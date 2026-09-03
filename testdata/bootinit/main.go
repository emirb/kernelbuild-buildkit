//go:build linux

// bootinit is the /init of the boot smoke test's initramfs. It mounts /proc,
// announces the kernel it is running on plus the marker the test greps for,
// and reboots.
//
// The marker goes to /dev/kmsg first, deliberately. PID 1 in an initramfs is
// not handed usable standard fds (measured: writes to fd 1 fail), and a
// write to /dev/console fails with EIO under Firecracker even though the
// console tty is registered and printk reaches the same UART. The kernel log
// is the one channel every VMM console shows, so that is where the marker
// goes; console and stdout follow as best effort for VMMs where they work.
//
// RESTART, not POWER_OFF: with reboot=k (x86) or PSCI (arm64) the VMM sees
// the reset and exits immediately, so a passing boot costs a second instead
// of idling until the timeout. A power-off without ACPI would just halt.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Marker is what the test looks for on the guest console.
const Marker = "KBF-BOOT-OK"

func main() {
	_ = os.Mkdir("/proc", 0o555)
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	version, _ := os.ReadFile("/proc/version")
	line := fmt.Sprintf("%s %s\n", Marker, strings.TrimSpace(string(version)))

	for _, dev := range []string{"/dev/kmsg", "/dev/console"} {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}
	_, _ = os.Stdout.WriteString(line)

	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	select {}
}
