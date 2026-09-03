//go:build e2e

// Boot smoke test. The golden gate proves a build is byte-deterministic and
// the target checks prove an artifact is a well-formed ELF; neither proves the
// kernel RUNS. This boots the freshly built kernel in a microVM with a
// one-binary initramfs and greps the guest console for a marker, which is the
// only check here that would catch a config that compiles and links into
// something unbootable.
//
// Firecracker on /dev/kvm when both are available (a second or two), else
// QEMU. It is skipped, not failed, when the host has no VMM: the suite still
// runs on a machine that cannot virtualize.
//
//	KBF_BOOT_VMLINUX=/path/to/vmlinux go test -tags e2e -run TestBootSmoke .
package kbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bootMarker is what testdata/bootinit prints once the guest reaches
// userspace. Keep the two in sync.
const bootMarker = "KBF-BOOT-OK"

func TestBootSmoke(t *testing.T) {
	vmlinux, bzImage := os.Getenv("KBF_BOOT_VMLINUX"), os.Getenv("KBF_BOOT_BZIMAGE")
	if vmlinux == "" && bzImage == "" {
		t.Skip("set KBF_BOOT_VMLINUX and/or KBF_BOOT_BZIMAGE to boot an existing kernel")
	}
	bootSmoke(t, vmlinux, bzImage)
}

// bootSmoke boots one of the given kernels and fails unless the guest reaches
// userspace. Either path may be empty; the VMM choice follows from which one
// is present.
func bootSmoke(t *testing.T, vmlinux, bzImage string) {
	t.Helper()
	initramfs := buildInitramfs(t)

	var out string
	var err error
	var via string
	switch {
	case vmlinux != "" && haveFirecracker():
		via = "firecracker/kvm"
		out, err = bootFirecracker(t, vmlinux, initramfs)
	case bzImage != "" && lookPath("qemu-system-x86_64") != "":
		via = "qemu"
		out, err = bootQEMU(t, bzImage, initramfs)
	case vmlinux != "" && lookPath("qemu-system-x86_64") != "":
		// QEMU's x86 loader takes the ELF vmlinux too; slower to set up than
		// bzImage but it keeps the test useful for a vmlinux-only build.
		via = "qemu"
		out, err = bootQEMU(t, vmlinux, initramfs)
	default:
		t.Skip("no usable VMM: need firecracker + /dev/kvm for vmlinux, or qemu-system-x86_64")
	}

	if !strings.Contains(out, bootMarker) {
		tail := out
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		t.Fatalf("kernel did not reach userspace via %s (err=%v); last console output:\n%s", via, err, tail)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, bootMarker) {
			t.Logf("boot OK via %s: %s", via, strings.TrimSpace(line))
			break
		}
	}
}

func lookPath(bin string) string {
	p, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	return p
}

func haveFirecracker() bool {
	if lookPath("firecracker") == "" {
		return false
	}
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// buildInitramfs compiles testdata/bootinit for the guest and packs it as a
// newc cpio archive holding exactly /init.
func buildInitramfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "init")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", bin, "./testdata/bootinit")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build bootinit: %v\n%s", err, o)
	}
	payload, err := os.ReadFile(bin)
	must(t, err)
	archive := filepath.Join(dir, "initramfs.cpio")
	// The device nodes are not decoration. PID 1 here gets no usable standard
	// fds, so the marker needs a device to write to, and /dev/console alone is
	// not enough: under Firecracker that write returns EIO while printk on the
	// same UART works, so /dev/kmsg is the channel that survives every VMM
	// (all measured, see testdata/bootinit). Writing the archive ourselves is
	// what lets a test create device nodes without being root.
	must(t, os.WriteFile(archive, cpioNewc(
		cpioEntry{name: "dev", mode: 0o040755},
		cpioEntry{name: "dev/console", mode: 0o020600, rdevMajor: 5, rdevMinor: 1},
		cpioEntry{name: "dev/kmsg", mode: 0o020600, rdevMajor: 1, rdevMinor: 11},
		cpioEntry{name: "init", mode: 0o100755, data: payload},
	), 0o644))
	return archive
}

// cpioEntry is one member of the initramfs: a regular file, a directory, or a
// character device, distinguished by the file-type bits in mode.
type cpioEntry struct {
	name                 string
	mode                 int
	rdevMajor, rdevMinor int
	data                 []byte
}

// cpioNewc renders a newc ("SVR4") cpio archive, the format the kernel's
// initramfs loader understands. Written here rather than shelling out to
// cpio(1) for the same reason kbuild-step packs its own tarballs: one less
// binary the test environment has to have - and mknod without root.
func cpioNewc(entries ...cpioEntry) []byte {
	var buf bytes.Buffer
	write := func(e cpioEntry) {
		nameBytes := append([]byte(e.name), 0)
		buf.WriteString("070701")
		for _, f := range []int{
			1,      // ino
			e.mode, // mode (file type + permissions)
			0, 0,   // uid, gid
			1,           // nlink
			0,           // mtime, fixed: this archive is reproducible like every other artifact here
			len(e.data), // filesize
			0, 0,        // dev major/minor
			e.rdevMajor, e.rdevMinor,
			len(nameBytes), // namesize
			0,              // check
		} {
			fmt.Fprintf(&buf, "%08X", f)
		}
		buf.Write(nameBytes)
		pad(&buf)
		buf.Write(e.data)
		pad(&buf)
	}
	for _, e := range entries {
		write(e)
	}
	write(cpioEntry{name: "TRAILER!!!"})
	return buf.Bytes()
}

// pad aligns the archive to the 4-byte boundary newc requires after every
// header+name and every file body.
func pad(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

func bootFirecracker(t *testing.T, vmlinux, initramfs string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"boot-source": map[string]string{
			"kernel_image_path": vmlinux,
			"initrd_path":       initramfs,
			"boot_args":         "console=ttyS0 reboot=k panic=1 pci=off rdinit=/init",
		},
		"drives":         []any{},
		"machine-config": map[string]int{"vcpu_count": 1, "mem_size_mib": 256},
	}
	dt, err := json.Marshal(cfg)
	must(t, err)
	cfgPath := filepath.Join(dir, "fc.json")
	must(t, os.WriteFile(cfgPath, dt, 0o644))
	return runVMM(t, 90*time.Second, "firecracker",
		"--api-sock", filepath.Join(dir, "fc.sock"), "--config-file", cfgPath, "--no-api")
}

func bootQEMU(t *testing.T, kernel, initramfs string) (string, error) {
	t.Helper()
	// The default machine, not -M microvm: microvm has no PIT/HPET, so a
	// kernel without a paravirt clock hangs in TSC calibration before it ever
	// reaches init (measured). This test must fail for the kernel's reasons,
	// not the machine model's.
	args := []string{
		"-m", "256", "-nographic", "-no-reboot",
		"-kernel", kernel, "-initrd", initramfs,
		"-append", "console=ttyS0 panic=1 rdinit=/init",
	}
	// -cpu host exists only under an accelerator ("CPU model 'host' requires
	// KVM or HVF"); without /dev/kvm QEMU falls back to TCG, where the widest
	// emulated model keeps a kernel built for a modern x86-64 level booting.
	if _, err := os.Stat("/dev/kvm"); err == nil {
		args = append([]string{"-enable-kvm", "-cpu", "host"}, args...)
	} else {
		args = append([]string{"-cpu", "max"}, args...)
	}
	return runVMM(t, 180*time.Second, "qemu-system-x86_64", args...)
}

// runVMM runs the VMM under a deadline and returns everything it printed. A
// timeout is not itself a failure: the caller decides based on the marker,
// because a guest that printed the marker and then failed to reset is a
// working kernel.
func runVMM(t *testing.T, timeout time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}
