//go:build e2e

// End-to-end test of the REAL #syntax= delegation path: build the frontend
// image, then run stock `docker build -f Kernelfile` and assert the frontend
// is actually invoked and produces the requested artifacts. The client-API
// integration suite never exercises this path (it drives KernelLLB directly),
// so a broken gateway entrypoint, opt-mapping, or image packaging would slip
// through everything else.
//
// The default case uses `TARGETS config`, which compiles NOTHING: it runs the
// whole delegation + fetch + extract + olddefconfig chain in ~1 min instead
// of a full kernel build. Set KBF_E2E_COMPILE=1 to also build a real vmlinux.
//
//	docker must be available with BuildKit (the default).
//	go test -tags e2e -timeout 30m -run TestE2E .
package kbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// e2eImage is the #syntax= ref. On a remote builder (Namespace nsc-remote)
// it must be a registry the builder can pull, set via KBF_E2E_IMAGE; locally
// a --load'd tag on the docker driver suffices. KBF_E2E_PREBUILT=1 says the
// ref already exists and must be tested as-is instead of rebuilt here: the
// release workflow stages the exact image it is about to publish and runs
// this suite against it before promoting it.
func e2eImageRef() string {
	if v := os.Getenv("KBF_E2E_IMAGE"); v != "" {
		return v
	}
	return "kernelbuild-buildkit:e2e"
}

func run(t *testing.T, dir, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func buildFrontendImage(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if os.Getenv("KBF_E2E_PREBUILT") == "1" {
		if os.Getenv("KBF_E2E_IMAGE") == "" {
			t.Fatal("KBF_E2E_PREBUILT=1 needs KBF_E2E_IMAGE")
		}
		t.Logf("using prebuilt frontend image %s", e2eImageRef())
		return
	}
	root, err := os.Getwd()
	must(t, err)

	// Assemble a tiny build context: Dockerfile.frontend + the two amd64
	// binaries it COPYs. Single-arch (the runner is amd64) so --load needs no
	// containerd image store.
	ctx := t.TempDir()
	// Build for every platform the image manifest will carry (see below):
	// the pushed image is multi-arch so the gateway resolves for the worker
	// and the compile vertex resolves the amd64 helper.
	arches := []string{"amd64"}
	if strings.Contains(e2eImageRef(), "/") {
		arches = []string{"amd64", "arm64"}
	}
	for _, arch := range arches {
		dist := filepath.Join(ctx, "dist", "linux", arch)
		must(t, os.MkdirAll(dist, 0o755))
		for _, bin := range []string{"kbuild-frontend", "kbuild-step"} {
			cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(dist, bin), "./cmd/"+bin)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build %s/%s: %v\n%s", arch, bin, err, out)
			}
		}
	}
	copyFileE2E(t, filepath.Join(root, "Dockerfile.frontend"), filepath.Join(ctx, "Dockerfile.frontend"))

	img := e2eImageRef()
	// A registry ref (host/path) is pushed MULTI-ARCH so the gateway resolves
	// for the worker's platform and the compile vertex resolves the amd64
	// helper, whatever the builder's arch. A bare tag is --load'd single-arch
	// on the local docker driver (worker must be amd64; the GOARCH guard in
	// TestE2E enforces that).
	args := []string{"buildx", "build", "--platform", "linux/amd64", "--load", "-f", "Dockerfile.frontend", "-t", img, "."}
	if strings.Contains(img, "/") {
		args = []string{"buildx", "build", "--platform", "linux/amd64,linux/arm64", "--push", "-f", "Dockerfile.frontend", "-t", img, "."}
	}
	if out, err := run(t, ctx, "docker", args...); err != nil {
		t.Fatalf("build frontend image %s: %v\n%s", img, err, out)
	}
}

func writeCtx(t *testing.T, kernelfile string) string {
	return writeCtxConfig(t, kernelfile, "# defaults\n")
}

func writeCtxConfig(t *testing.T, kernelfile, config string) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "Kernelfile"), []byte(kernelfile), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "kernel.config"), []byte(config), 0o644))
	if ca := os.Getenv("KBF_E2E_PROXY_CA"); ca != "" {
		copyFileE2E(t, ca, filepath.Join(dir, e2eProxyCAName))
	}
	return dir
}

// e2eProxyCAName is the context filename the Kernelfile's PROXY_CA line
// names when KBF_E2E_PROXY_CA is set: the path of a CA bundle to trust inside
// the build steps, for hosts whose egress is TLS-intercepted (the same knob
// the integration suite exposes as KBF_PROXY_CA). Without it the tarball
// fetch behind such a gateway fails on certificate verification, which is a
// property of the host, not of the frontend.
const e2eProxyCAName = "proxy-ca.crt"

func proxyCALine() string {
	if os.Getenv("KBF_E2E_PROXY_CA") == "" {
		return ""
	}
	return "PROXY_CA   " + e2eProxyCAName + "\n"
}

// kernelfile renders the test Kernelfile for a TARGETS value. Built from a
// parameter rather than string-replaced after the fact: the replacement had
// silently stopped matching when the file was realigned, so the compile leg
// was building TARGETS config and then failing on the missing vmlinux.
func kernelfile(targets string) string {
	return "#syntax=" + e2eImageRef() + `
KERNEL     6.18.20
SOURCE_URL https://mirrors.edge.kernel.org/pub/linux/kernel/v6.x/linux-6.18.20.tar.gz
CONFIG     kernel.config
SHA256     a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c
EPOCH      1785542400
` + proxyCALine() + `TARGETS    ` + targets + `
`
}

func goodKernelfile() string { return kernelfile("config") }

func TestE2E(t *testing.T) {
	if runtime.GOARCH != "amd64" && !strings.Contains(e2eImageRef(), "/") {
		t.Skipf("local e2e image is single-platform; set KBF_E2E_IMAGE to a registry ref to test on %s", runtime.GOARCH)
	}
	buildFrontendImage(t)

	t.Run("delegation-config-only", func(t *testing.T) {
		// TARGETS config: exercises #syntax= -> frontend -> fetch -> extract
		// -> olddefconfig, and exports the resolved .config. No compile.
		dir := writeCtx(t, goodKernelfile())
		out := filepath.Join(dir, "out")
		if o, err := run(t, dir, "docker", "build", "-f", "Kernelfile", "--output", "type=local,dest="+out, "."); err != nil {
			t.Fatalf("docker build: %v\n%s", err, o)
		}
		cfg, err := os.ReadFile(filepath.Join(out, "config"))
		if err != nil {
			t.Fatalf("no resolved config exported: %v", err)
		}
		if !strings.Contains(string(cfg), "CONFIG_") {
			t.Error("exported config does not look like a kernel .config")
		}
	})

	t.Run("negative-unknown-key", func(t *testing.T) {
		// A Kernelfile the frontend must reject; docker build has to fail.
		bad := "#syntax=" + e2eImageRef() + "\nKERNEL 6.18.20\nFROBNICATE yes\n"
		dir := writeCtx(t, bad)
		o, err := run(t, dir, "docker", "build", "-f", "Kernelfile", "--output", "type=local,dest="+filepath.Join(dir, "out"), ".")
		if err == nil {
			t.Fatalf("build with an unknown Kernelfile key succeeded; want failure\n%s", o)
		}
		if !strings.Contains(o, "FROBNICATE") && !strings.Contains(o, "unknown key") {
			t.Fatalf("build failed but not for the unknown key (wrong-reason failure):\n%s", o)
		}
	})

	if os.Getenv("KBF_E2E_COMPILE") == "1" {
		t.Run("full-vmlinux", func(t *testing.T) {
			// vmlinux for Firecracker, bzImage for QEMU: the boot smoke below
			// picks whichever VMM the host can run. The config matters as much
			// as the targets - the kbuild defaults leave a microVM guest with
			// no initramfs support and no serial console, so it boots to
			// silence (measured).
			cfg, err := os.ReadFile("testdata/boot.config")
			must(t, err)
			dir := writeCtxConfig(t, kernelfile("vmlinux image"), string(cfg))
			out := filepath.Join(dir, "out")
			if o, err := run(t, dir, "docker", "build", "-f", "Kernelfile", "--output", "type=local,dest="+out, "."); err != nil {
				t.Fatalf("full build: %v\n%s", err, o)
			}
			b, err := os.ReadFile(filepath.Join(out, "vmlinux"))
			if err != nil || len(b) < 4 || string(b[:4]) != "\x7fELF" {
				t.Fatalf("vmlinux is not an ELF: %v", err)
			}
			// A well-formed ELF is not a working kernel. Keep the boot result as
			// its own subtest so a host without a VMM does not hide a successful
			// full build behind a SKIP status.
			t.Run("boot", func(t *testing.T) {
				bootSmoke(t, filepath.Join(out, "vmlinux"), filepath.Join(out, "bzImage"))
			})
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func copyFileE2E(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	must(t, err)
	must(t, os.WriteFile(dst, b, 0o644))
}
