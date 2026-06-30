//go:build linux

package sysinfo

import "os"

// hostMemoryMiB reads total RAM from /proc/meminfo (MemTotal, in kB).
func hostMemoryMiB() (uint64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemTotalKiB(string(data))
}
