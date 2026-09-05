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

// ensureOmlxInstalled installs only when omlx is ABSENT (Detect-gated), and a
// failed install is swallowed (best-effort — never fails the reconcile).
func TestEnsureOmlxInstalled(test *testing.T) {
	origDetect, origInstall := omlxDetectFn, installOmlxFn
	test.Cleanup(func() { omlxDetectFn, installOmlxFn = origDetect, origInstall })

	test.Run("already present skips install", func(test *testing.T) {
		omlxDetectFn = func() bool { return true }
		installed := false
		installOmlxFn = func() error { installed = true; return nil }
		var lines []string
		ensureOmlxInstalled(func(line string) { lines = append(lines, line) })
		if installed {
			test.Error("must NOT install when omlx is already present")
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "present") {
			test.Errorf("want a single 'present' line, got %v", lines)
		}
	})

	test.Run("absent triggers install then detects", func(test *testing.T) {
		calls := 0
		omlxDetectFn = func() bool { calls++; return calls > 1 } // absent, then present
		installedWith := 0
		installOmlxFn = func() error { installedWith++; return nil }
		var lines []string
		ensureOmlxInstalled(func(line string) { lines = append(lines, line) })
		if installedWith == 0 {
			test.Fatal("must call the installer when omlx is absent")
		}
		if !strings.Contains(lines[len(lines)-1], "installed") {
			test.Errorf("last line must report installed, got %v", lines)
		}
	})

	test.Run("install failure is swallowed", func(test *testing.T) {
		omlxDetectFn = func() bool { return false }
		installOmlxFn = func() error { return errors.New("no space left on device") }
		var lines []string
		ensureOmlxInstalled(func(line string) { lines = append(lines, line) })
		if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "skipped") {
			test.Errorf("a failed install must emit a 'skipped' line and not panic, got %v", lines)
		}
	})
}
