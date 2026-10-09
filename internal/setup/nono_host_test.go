package setup

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/nono"
)

// EnsureNonoInstalled installs only when nono is ABSENT (Detect-gated), and a
// failed install is swallowed (best-effort — never fails `ai setup`).
func TestEnsureNonoInstalled(test *testing.T) {
	origDetect, origInstall := nonoDetectFn, installNonoFn
	test.Cleanup(func() { nonoDetectFn, installNonoFn = origDetect, origInstall })

	test.Run("already present skips install", func(test *testing.T) {
		nonoDetectFn = func() nono.Info { return nono.Info{Installed: true, Path: "/usr/local/bin/nono"} }
		installed := false
		installNonoFn = func() error { installed = true; return nil }
		var lines []string
		EnsureNonoInstalled(func(line string) { lines = append(lines, line) })
		if installed {
			test.Error("must NOT install when nono is already present")
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "present") {
			test.Errorf("want a single 'present' line, got %v", lines)
		}
	})

	test.Run("absent triggers install then detects", func(test *testing.T) {
		calls := 0
		nonoDetectFn = func() nono.Info {
			calls++
			return nono.Info{Installed: calls > 1} // absent, then present
		}
		installedWith := 0
		installNonoFn = func() error { installedWith++; return nil }
		var lines []string
		EnsureNonoInstalled(func(line string) { lines = append(lines, line) })
		if installedWith == 0 {
			test.Fatal("must call the installer when nono is absent")
		}
		if !strings.Contains(lines[len(lines)-1], "installed") {
			test.Errorf("last line must report installed, got %v", lines)
		}
	})

	test.Run("install failure is swallowed", func(test *testing.T) {
		nonoDetectFn = func() nono.Info { return nono.Info{} }
		installNonoFn = func() error { return errors.New("network unreachable") }
		var lines []string
		EnsureNonoInstalled(func(line string) { lines = append(lines, line) })
		if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "skipped") {
			test.Errorf("a failed install must emit a 'skipped' line and not panic, got %v", lines)
		}
	})
}
