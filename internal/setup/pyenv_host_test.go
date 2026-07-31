package setup

import (
	"errors"
	"strings"
	"testing"
)

// swapEnsurePlatformVenv captures the venv-ensure seam and restores it on cleanup.
func swapEnsurePlatformVenv(test *testing.T) {
	test.Helper()
	original := ensurePlatformVenvFn
	test.Cleanup(func() { ensurePlatformVenvFn = original })
}

// ensurePlatformVenv reports creation vs presence, and never panics/fails on a
// toolchain error — it just emits a progress line (the reconcile continues).
func TestEnsurePlatformVenv(test *testing.T) {
	swapEnsurePlatformVenv(test)

	cases := []struct {
		name    string
		fn      func() (bool, error)
		wantSub string
	}{
		{"created", func() (bool, error) { return true, nil }, "created"},
		{"present", func() (bool, error) { return false, nil }, "present"},
		{"skipped", func() (bool, error) { return false, errors.New("no uv/python3") }, "skipped"},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			ensurePlatformVenvFn = testCase.fn
			var lines []string
			ensurePlatformVenv(func(line string) { lines = append(lines, line) })
			if len(lines) != 1 {
				test.Fatalf("want exactly one progress line, got %v", lines)
			}
			if !strings.Contains(lines[0], testCase.wantSub) {
				test.Errorf("progress line %q must contain %q", lines[0], testCase.wantSub)
			}
		})
	}
}
