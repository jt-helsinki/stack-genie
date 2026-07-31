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

// ensureVLLMInstalled installs only when vLLM is ABSENT (Detect-gated), and a failed
// install is swallowed (best-effort — never fails the reconcile).
func TestEnsureVLLMInstalled(test *testing.T) {
	origDetect, origInstall := vllmDetect, installVLLMFn
	test.Cleanup(func() { vllmDetect, installVLLMFn = origDetect, origInstall })

	test.Run("already present skips install", func(test *testing.T) {
		vllmDetect = func() (bool, string) { return true, "mlx" }
		installed := false
		installVLLMFn = func(...string) error { installed = true; return nil }
		var lines []string
		ensureVLLMInstalled(func(line string) { lines = append(lines, line) })
		if installed {
			test.Error("must NOT install when vLLM is already present")
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "present") {
			test.Errorf("want a single 'present' line, got %v", lines)
		}
	})

	test.Run("absent triggers install then detects", func(test *testing.T) {
		calls := 0
		vllmDetect = func() (bool, string) { calls++; return calls > 1, "mlx" } // absent, then present
		installedWith := 0
		installVLLMFn = func(specs ...string) error { installedWith = len(specs) + 1; return nil }
		var lines []string
		ensureVLLMInstalled(func(line string) { lines = append(lines, line) })
		if installedWith == 0 {
			test.Fatal("must call the installer when vLLM is absent")
		}
		if !strings.Contains(lines[len(lines)-1], "installed") {
			test.Errorf("last line must report installed, got %v", lines)
		}
	})

	test.Run("install failure is swallowed", func(test *testing.T) {
		vllmDetect = func() (bool, string) { return false, "" }
		installVLLMFn = func(...string) error { return errors.New("no space left on device") }
		var lines []string
		ensureVLLMInstalled(func(line string) { lines = append(lines, line) })
		if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "skipped") {
			test.Errorf("a failed install must emit a 'skipped' line and not panic, got %v", lines)
		}
	})
}
