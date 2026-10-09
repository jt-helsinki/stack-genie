package workspace

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written to it — used to assert registerAITools' best-effort warnf calls
// stay silent on the paths that are expected to be clean no-ops.
func captureStderr(test *testing.T, fn func()) string {
	test.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		test.Fatal(err)
	}
	os.Stderr = writer
	fn()
	_ = writer.Close()
	os.Stderr = original
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, reader)
	return buf.String()
}

func requireBash(test *testing.T) {
	test.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		test.Skip("bash not on PATH")
	}
}

// TestTeardownTokenSaveNoCompatibleCLIIsValidScript is a regression test: with
// no claude-code/opencode/gemini selected, the generated teardown script must
// still be syntactically valid bash (a bare "{ }" with nothing inside it is a
// syntax error) and must not warn.
func TestTeardownTokenSaveNoCompatibleCLIIsValidScript(test *testing.T) {
	requireBash(test)
	root := seedProject(test, "tokensave-empty", &config.Config{
		Agent: config.AgentConfig{Tools: []string{"codex"}, DefaultTool: "codex"},
	})
	manager := Manager{}
	output := captureStderr(test, func() {
		manager.teardownTokenSave(root, &config.Config{Agent: config.AgentConfig{Tools: []string{"codex"}}})
	})
	if output != "" {
		test.Errorf("teardownTokenSave with no compatible CLI warned: %q", output)
	}
}

// TestTeardownAgentMemoryNoCompatibleCLIIsValidScript mirrors the tokensave
// regression above for agentmemory's teardown script.
func TestTeardownAgentMemoryNoCompatibleCLIIsValidScript(test *testing.T) {
	requireBash(test)
	root := seedProject(test, "agentmemory-empty", &config.Config{
		Agent: config.AgentConfig{Tools: []string{"codex"}, DefaultTool: "codex"},
	})
	manager := Manager{}
	output := captureStderr(test, func() {
		manager.teardownAgentMemory(root, &config.Config{Agent: config.AgentConfig{Tools: []string{"codex"}}})
	})
	if output != "" {
		test.Errorf("teardownAgentMemory with no compatible CLI warned: %q", output)
	}
}

// TestRegisterCavemanNoCompatibleCLISkipsSilently verifies that with only
// gemini selected (explicitly skipped — see registerCaveman's comment) there
// is nothing to install and no warning fires.
func TestRegisterCavemanNoCompatibleCLISkipsSilently(test *testing.T) {
	root := seedProject(test, "caveman-gemini-only", nil)
	enabled := true
	projectConfig := &config.Config{
		Agent:   config.AgentConfig{Tools: []string{"gemini"}, DefaultTool: "gemini"},
		Context: config.ContextConfig{CavemanEnabled: &enabled},
	}
	manager := Manager{}
	output := captureStderr(test, func() {
		manager.registerCaveman(root, projectConfig)
	})
	if output != "" {
		test.Errorf("registerCaveman with only gemini selected warned: %q", output)
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "run", "caveman-install.sh")); err == nil {
		test.Error("registerCaveman should not stage an install script with no compatible CLI")
	}
}

// TestRegisterAIToolsNilConfigNoop verifies registerAITools tolerates a nil
// config (Agent/Shell both load a real config, but the guard must hold).
func TestRegisterAIToolsNilConfigNoop(test *testing.T) {
	manager := Manager{}
	manager.registerAITools(test.TempDir(), nil) // must not panic
}

// TestLinkSharedResourcesCreatesExpectedSymlinks verifies the shared pool
// dirs are created and symlinked into each installed CLI's real per-project
// dir, and that re-running it is idempotent.
func TestLinkSharedResourcesCreatesExpectedSymlinks(test *testing.T) {
	root := test.TempDir()
	if err := linkSharedResources(root, []string{"opencode", "claude-code", "omp"}); err != nil {
		test.Fatal(err)
	}
	for _, want := range []string{".opencode/skills", ".claude/skills", ".omp/skills", ".omp/agents"} {
		target, err := os.Readlink(filepath.Join(root, want))
		if err != nil {
			test.Errorf("expected %s to be a symlink: %v", want, err)
			continue
		}
		if target == "" {
			test.Errorf("%s symlink has empty target", want)
		}
	}
	// codex has no skills/agents concept — nothing should be created for it.
	if _, err := os.Lstat(filepath.Join(root, ".codex", "skills")); err == nil {
		test.Error("codex should get no skills symlink")
	}
	// Idempotent re-run must not error or change an already-correct symlink.
	if err := linkSharedResources(root, []string{"opencode", "claude-code", "omp"}); err != nil {
		test.Fatalf("second linkSharedResources call failed: %v", err)
	}
}

// TestRegisterOpenVikingNoCompatibleCLIWarns verifies the explicit warning
// path when OpenViking is enabled but no supported CLI is selected.
func TestRegisterOpenVikingNoCompatibleCLIWarns(test *testing.T) {
	root := seedProject(test, "openviking-none", nil)
	enabled := true
	projectConfig := &config.Config{
		Agent:   config.AgentConfig{Tools: []string{"gemini"}, DefaultTool: "gemini"},
		Context: config.ContextConfig{OpenVikingEnabled: &enabled},
	}
	manager := Manager{}
	output := captureStderr(test, func() {
		manager.registerOpenViking(root, projectConfig)
	})
	if output == "" {
		test.Error("registerOpenViking with no supported CLI should warn")
	}
}
