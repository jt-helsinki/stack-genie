package setup

import (
	"errors"
	"strings"
	"testing"
)

func swapHostAgentSeams(test *testing.T) {
	test.Helper()
	origRegistry, origLookPath := hostAgentCLIsFn, agentCLILookPathFn
	test.Cleanup(func() { hostAgentCLIsFn, agentCLILookPathFn = origRegistry, origLookPath })
}

// EnsureHostAgentCLIsInstalled installs only when a CLI's binary is ABSENT from
// PATH (Detect-gated), skips unknown keys silently, and swallows install
// failures (best-effort — never fails `ai setup`).
func TestEnsureHostAgentCLIsInstalled(test *testing.T) {
	swapHostAgentSeams(test)

	test.Run("already present skips install", func(test *testing.T) {
		installed := false
		hostAgentCLIsFn = func() map[string]hostAgentCLI {
			return map[string]hostAgentCLI{
				HostAgentOpenCode: {binary: "opencode", install: func() error { installed = true; return nil }},
			}
		}
		agentCLILookPathFn = func(string) (string, error) { return "/usr/local/bin/opencode", nil }
		var lines []string
		EnsureHostAgentCLIsInstalled([]string{HostAgentOpenCode}, func(line string) { lines = append(lines, line) })
		if installed {
			test.Error("must NOT install when the binary is already on PATH")
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "present") {
			test.Errorf("want a single 'present' line, got %v", lines)
		}
	})

	test.Run("absent triggers install then detects", func(test *testing.T) {
		calls := 0
		installedWith := 0
		hostAgentCLIsFn = func() map[string]hostAgentCLI {
			return map[string]hostAgentCLI{
				HostAgentClaudeCode: {binary: "claude", install: func() error { installedWith++; return nil }},
			}
		}
		agentCLILookPathFn = func(string) (string, error) {
			calls++
			if calls > 1 {
				return "/usr/local/bin/claude", nil
			}
			return "", errors.New("not found")
		}
		var lines []string
		EnsureHostAgentCLIsInstalled([]string{HostAgentClaudeCode}, func(line string) { lines = append(lines, line) })
		if installedWith == 0 {
			test.Fatal("must call the installer when the binary is absent")
		}
		if !strings.Contains(lines[len(lines)-1], "installed") {
			test.Errorf("last line must report installed, got %v", lines)
		}
	})

	test.Run("install failure is swallowed", func(test *testing.T) {
		hostAgentCLIsFn = func() map[string]hostAgentCLI {
			return map[string]hostAgentCLI{
				HostAgentCodex: {binary: "codex", install: func() error { return errors.New("network unreachable") }},
			}
		}
		agentCLILookPathFn = func(string) (string, error) { return "", errors.New("not found") }
		var lines []string
		EnsureHostAgentCLIsInstalled([]string{HostAgentCodex}, func(line string) { lines = append(lines, line) })
		if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "skipped") {
			test.Errorf("a failed install must emit a 'skipped' line and not panic, got %v", lines)
		}
	})

	test.Run("unknown key is skipped silently", func(test *testing.T) {
		hostAgentCLIsFn = func() map[string]hostAgentCLI { return map[string]hostAgentCLI{} }
		var lines []string
		EnsureHostAgentCLIsInstalled([]string{"bogus-cli"}, func(line string) { lines = append(lines, line) })
		if len(lines) != 0 {
			test.Errorf("unknown key must produce no progress lines, got %v", lines)
		}
	})
}

func TestSupportedHostAgentCLIs(test *testing.T) {
	got := SupportedHostAgentCLIs()
	want := []string{HostAgentClaudeCode, HostAgentCodex, HostAgentOpenCode}
	if len(got) != len(want) {
		test.Fatalf("SupportedHostAgentCLIs() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			test.Errorf("SupportedHostAgentCLIs()[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}
