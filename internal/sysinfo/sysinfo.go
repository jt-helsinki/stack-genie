// Package sysinfo reports the host's CPU and memory totals so `ai create` can cap a
// workspace's requested resources at what the machine actually has. Detection is
// per-OS (darwin/linux); an unsupported host or read failure reports "unknown" so the
// caller skips the cap rather than guessing.
package sysinfo

import (
	"runtime"
	"strconv"
	"strings"
)

// CPUs returns the number of logical CPUs on the host.
func CPUs() int { return runtime.NumCPU() }

// MemoryMiB returns the host's total RAM in MiB and whether it could be determined
// (false on an unsupported platform or a read failure — the caller then skips the
// memory cap).
func MemoryMiB() (uint64, bool) { return hostMemoryMiB() }

// parseMemTotalKiB extracts the "MemTotal: <n> kB" line from /proc/meminfo content and
// returns it converted to MiB. Pure (no I/O) so it is testable on any host.
func parseMemTotalKiB(meminfo string) (uint64, bool) {
	for _, line := range strings.Split(meminfo, "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kib / 1024, true
	}
	return 0, false
}
