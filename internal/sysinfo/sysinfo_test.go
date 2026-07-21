package sysinfo

import "testing"

func TestParseMemTotalKiB(test *testing.T) {
	const meminfo = "MemFree:         100 kB\nMemTotal:    16384000 kB\nBuffers: 1 kB\n"
	got, ok := parseMemTotalKiB(meminfo)
	if !ok {
		test.Fatal("expected MemTotal to parse")
	}
	if want := uint64(16384000 / 1024); got != want {
		test.Errorf("MemTotal MiB = %d, want %d", got, want)
	}
}

func TestParseMemTotalKiBMissing(test *testing.T) {
	if _, ok := parseMemTotalKiB("MemFree: 100 kB\n"); ok {
		test.Error("missing MemTotal should report not-ok")
	}
}

func TestCPUsPositive(test *testing.T) {
	if CPUs() < 1 {
		test.Errorf("CPUs() = %d, want >= 1", CPUs())
	}
}
