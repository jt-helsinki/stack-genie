package uninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripManagedRemovesOnlyOurLines(test *testing.T) {
	input := strings.Join([]string{
		"# my real config",
		"export EDITOR=vim",
		"",
		`export PATH="X:$PATH"  ` + pathMarker,
		"",
		completionMarker,
		`fpath=("$HOME/.zsh/completions" $fpath)`,
		"autoload -Uz compinit && compinit",
		`alias gs="git status"`,
		"",
	}, "\n")

	got := stripManaged(input)

	for _, keep := range []string{"# my real config", "export EDITOR=vim", `alias gs="git status"`} {
		if !strings.Contains(got, keep) {
			test.Errorf("stripped output dropped a user line %q:\n%s", keep, got)
		}
	}
	for _, gone := range []string{pathMarker, completionMarker, "fpath=(", "compinit"} {
		if strings.Contains(got, gone) {
			test.Errorf("stripped output still contains %q:\n%s", gone, got)
		}
	}
}

func TestStripManagedPowerShellBlock(test *testing.T) {
	input := completionMarker + "\n. \"/home/me/.config/powershell/ai.completion.ps1\"\nWrite-Host hi\n"
	got := stripManaged(input)
	if strings.Contains(got, "ai.completion.ps1") || strings.Contains(got, completionMarker) {
		test.Errorf("powershell completion block not removed:\n%s", got)
	}
	if !strings.Contains(got, "Write-Host hi") {
		test.Errorf("user line after the block was dropped:\n%s", got)
	}
}

func TestStripManagedNoMarkersUnchanged(test *testing.T) {
	input := "export EDITOR=vim\nalias gs=\"git status\"\n"
	if got := stripManaged(input); got != input {
		test.Errorf("a file with no managed lines must be unchanged:\n%q", got)
	}
}

// fakeProber records the commands it is asked to run and returns canned output.
type fakeProber struct {
	present map[string]bool   // binaries on PATH
	output  map[string][]byte // keyed by "name arg0 arg1 ..."
	ran     []string          // every Run invocation, joined
}

func (prober *fakeProber) LookPath(file string) (string, error) {
	if prober.present[file] {
		return "/usr/bin/" + file, nil
	}
	return "", os.ErrNotExist
}

func (prober *fakeProber) Run(name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	prober.ran = append(prober.ran, key)
	return prober.output[key], nil
}

func (prober *fakeProber) Exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRunFullTeardown(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	// Keep XDG dirs under the temp HOME so completion-file paths are scoped.
	test.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	test.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	test.Setenv("ZDOTDIR", "")

	// Lay down a prior install: binary, platform state, a project, completion
	// files, and a .zshrc carrying both managed blocks plus user lines.
	binaryPath := filepath.Join(home, ".ai-platform", "bin", "ai")
	mustWrite(test, binaryPath, "binary")
	mustWrite(test, filepath.Join(home, ".ai-platform", "config", "config.yaml"), "os: alma")
	mustWrite(test, filepath.Join(home, ".clawpatrol", "gateway.hcl"), "state_dir = \".\"")
	projectFile := filepath.Join(home, "projects", "keepme", "main.go")
	mustWrite(test, projectFile, "package main")
	completionFile := filepath.Join(home, ".zsh", "completions", "_ai")
	mustWrite(test, completionFile, "#compdef ai")
	mustWrite(test, filepath.Join(home, ".zshrc"), strings.Join([]string{
		"export EDITOR=vim",
		`export PATH="x"  ` + pathMarker,
		completionMarker,
		"fpath=(x $fpath)",
		"autoload -Uz compinit && compinit",
		`alias gs="git status"`,
		"",
	}, "\n"))

	prober := &fakeProber{
		present: map[string]bool{"docker": true},
		output:  map[string][]byte{"docker ps -aq --filter name=aip-": []byte("abc123\ndef456\n")},
	}
	var steps []string
	report, err := Run(Options{Purge: true, BinaryPath: binaryPath}, prober, func(line string) {
		steps = append(steps, line)
	})
	if err != nil {
		test.Fatalf("Run: %v", err)
	}

	if report.RemovedContainers != 2 {
		test.Errorf("RemovedContainers = %d, want 2", report.RemovedContainers)
	}
	if report.RemovedBinary != binaryPath {
		test.Errorf("RemovedBinary = %q, want %q", report.RemovedBinary, binaryPath)
	}
	if !report.Purged {
		test.Error("Purged = false, want true")
	}
	// docker rm -f was issued with both ids.
	if !containsLine(prober.ran, "docker rm -f abc123 def456") {
		test.Errorf("expected `docker rm -f abc123 def456`, ran: %v", prober.ran)
	}
	// Binary, completion file, and platform state are gone; project survives.
	for _, gone := range []string{binaryPath, completionFile, filepath.Join(home, ".ai-platform"), filepath.Join(home, ".clawpatrol")} {
		if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
			test.Errorf("expected %q removed, but it still exists", gone)
		}
	}
	if _, statErr := os.Stat(projectFile); statErr != nil {
		test.Errorf("project source must survive uninstall: %v", statErr)
	}
	// rc keeps the user's lines, drops ours.
	rc, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if !strings.Contains(string(rc), `alias gs="git status"`) || !strings.Contains(string(rc), "export EDITOR=vim") {
		test.Errorf("user rc lines were dropped:\n%s", rc)
	}
	if strings.Contains(string(rc), pathMarker) || strings.Contains(string(rc), "compinit") {
		test.Errorf("managed rc lines survived:\n%s", rc)
	}
	if len(steps) == 0 {
		test.Error("expected progress lines, got none")
	}
	// A transcript is written to ~/ai-uninstall.log and survives --purge.
	if report.LogPath != filepath.Join(home, LogName) {
		test.Errorf("LogPath = %q, want %q", report.LogPath, filepath.Join(home, LogName))
	}
	logBytes, logErr := os.ReadFile(report.LogPath)
	if logErr != nil {
		test.Fatalf("uninstall log not written: %v", logErr)
	}
	logText := string(logBytes)
	for _, want := range []string{"=== ai uninstall", "Removed platform containers", "Purged ~/.ai-platform", "=== uninstall finished ==="} {
		if !strings.Contains(logText, want) {
			test.Errorf("uninstall log missing %q:\n%s", want, logText)
		}
	}
}

func TestRunIdempotentOnCleanHome(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	prober := &fakeProber{present: map[string]bool{}}
	report, err := Run(Options{BinaryPath: filepath.Join(home, "nope", "ai")}, prober, nil)
	if err != nil {
		test.Fatalf("Run on clean home should not error: %v", err)
	}
	if report.RemovedContainers != 0 || report.RemovedBinary != "" || len(report.CleanedRC) != 0 {
		test.Errorf("expected an empty report on a clean home, got %+v", report)
	}
}

func TestExternalDepsPresenceAndRemoval(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("MSB_HOME", "")
	test.Setenv("CLAWPATROL_PREFIX", "")

	// msb installed: ~/.microsandbox dir + a ~/.local/bin/msb symlink target.
	msbHome := filepath.Join(home, ".microsandbox")
	mustWrite(test, filepath.Join(msbHome, "bin", "msb"), "x")
	mustWrite(test, filepath.Join(home, ".local", "bin", "msb"), "x")
	// clawpatrol NOT installed.

	prober := &fakeProber{present: map[string]bool{}} // nothing on PATH

	deps := ExternalDeps()
	var msb, claw ExternalDep
	for _, dep := range deps {
		switch dep.Binary {
		case "msb":
			msb = dep
		case "clawpatrol":
			claw = dep
		}
	}
	if !msb.Present(prober) {
		test.Error("msb should be detected present (install dir exists)")
	}
	if claw.Present(prober) {
		test.Error("clawpatrol should not be detected (nothing installed)")
	}

	// Removing msb via Run clears its targets but leaves clawpatrol untouched.
	report, err := Run(Options{RemoveDeps: []ExternalDep{msb}}, prober, nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(report.RemovedDeps) != 1 || report.RemovedDeps[0] != msb.Name {
		test.Errorf("RemovedDeps = %v, want [%q]", report.RemovedDeps, msb.Name)
	}
	if _, statErr := os.Stat(msbHome); !os.IsNotExist(statErr) {
		test.Error("~/.microsandbox should be removed")
	}
	if _, statErr := os.Stat(filepath.Join(home, ".local", "bin", "msb")); !os.IsNotExist(statErr) {
		test.Error("~/.local/bin/msb symlink should be removed")
	}
}

func mustWrite(test *testing.T, path, content string) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}
