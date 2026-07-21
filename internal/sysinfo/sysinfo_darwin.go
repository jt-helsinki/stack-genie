//go:build darwin

package sysinfo

import "golang.org/x/sys/unix"

// hostMemoryMiB reads total physical RAM from the hw.memsize sysctl.
func hostMemoryMiB() (uint64, bool) {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil || bytes == 0 {
		return 0, false
	}
	return bytes / 1024 / 1024, true
}
