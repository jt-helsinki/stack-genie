//go:build !darwin && !linux

package sysinfo

// hostMemoryMiB cannot determine RAM on unsupported platforms.
func hostMemoryMiB() (uint64, bool) { return 0, false }
