package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// checkScriptSyntax runs `bash -n` against the staged script for tool under
// root (written by runDetachedScript to <root>/.ai-platform/run/<tool>-install.sh)
// and fails the test with the script content on a syntax error. This is a
// regression sweep: two real "empty { }" bash syntax bugs in teardown scripts
// were caught by running the actual functions against a config with no
// compatible CLI selected (an under-tested combination) — this test exercises
// every register* function (which stage DETACHED scripts whose exit status is
// never observed by the caller, so a syntax error there would otherwise fail
// silently in the background) across both an empty-tools and a full-tools
// config, to catch the same class of bug anywhere else in the file.
func checkScriptSyntax(test *testing.T, root, tool string) {
	test.Helper()
	scriptPath := filepath.Join(root, ".ai-platform", "run", tool+"-install.sh")
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		test.Fatalf("expected %s to be staged: %v", tool, err)
	}
	cmd := exec.Command("bash", "-n", scriptPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		test.Errorf("%s install script has a bash syntax error: %v\n%s\n--- script ---\n%s", tool, err, output, content)
	}
}

// allToolsEnabledConfig returns a config with every opt-in AI tool enabled and
// every agent CLI this platform supports selected, so every register*
// function's install path (not just its disabled/no-op path) actually stages
// a script.
func allToolsEnabledConfig() *config.Config {
	enabled := true
	return &config.Config{
		Agent: config.AgentConfig{
			Tools:       []string{"opencode", "claude-code", "codex", "gemini", "omp"},
			DefaultTool: "opencode",
		},
		Context: config.ContextConfig{
			CavemanEnabled:         &enabled,
			GraphifyEnabled:        &enabled,
			CodeReviewGraphEnabled: &enabled,
			CodebaseMemoryEnabled:  &enabled,
			OpenVikingEnabled:      &enabled,
			TokenSaveEnabled:       &enabled,
			AgentMemoryEnabled:     &enabled,
			PonytailEnabled:        &enabled,
			AgentSkillsEnabled:     &enabled,
		},
	}
}

func TestGeneratedToolScriptsAreValidBash(test *testing.T) {
	requireBash(test)
	if _, err := exec.LookPath("python3"); err != nil {
		test.Skip("python3 not on PATH (graphify's AGENTS.md cleanup step needs it for the disabled branch, exercised separately)")
	}
	root := seedProject(test, "all-tools-syntax", allToolsEnabledConfig())
	manager := Manager{}
	projectConfig := allToolsEnabledConfig()

	manager.registerCaveman(root, projectConfig)
	checkScriptSyntax(test, root, "caveman")

	manager.registerCodeReviewGraph(root, projectConfig)
	checkScriptSyntax(test, root, "code-review-graph")

	manager.registerOpenViking(root, projectConfig)
	checkScriptSyntax(test, root, "openviking")

	manager.registerTokenSave(root, projectConfig)
	checkScriptSyntax(test, root, "tokensave")

	manager.registerAgentMemory(root, projectConfig)
	checkScriptSyntax(test, root, "agentmemory")

	manager.registerPonytail(root, projectConfig)
	checkScriptSyntax(test, root, "ponytail")

	manager.registerAgentSkills(root, projectConfig)
	checkScriptSyntax(test, root, "agent-skills")

	manager.setupGraphifyMCP(root)
	checkScriptSyntax(test, root, "graphify-mcp")
}

// TestGeneratedToolScriptsAreValidBashWithSingleCLI exercises the SAME
// register* functions with only one, less-commonly-tested agent CLI selected
// (codex — which every one of these tools treats as "not natively supported,
// MCP-injection only"), the exact shape that produced the two empty-"{ }"
// syntax bugs this test suite caught (an optional per-CLI uninstall/install
// list reduced to empty). Every tool must still either skip cleanly or stage
// syntactically valid bash.
func TestGeneratedToolScriptsAreValidBashWithSingleCLI(test *testing.T) {
	requireBash(test)
	enabled := true
	projectConfig := &config.Config{
		Agent: config.AgentConfig{Tools: []string{"codex"}, DefaultTool: "codex"},
		Context: config.ContextConfig{
			CavemanEnabled:         &enabled,
			CodeReviewGraphEnabled: &enabled,
			OpenVikingEnabled:      &enabled,
			TokenSaveEnabled:       &enabled,
			AgentMemoryEnabled:     &enabled,
			PonytailEnabled:        &enabled,
			AgentSkillsEnabled:     &enabled,
		},
	}
	root := seedProject(test, "codex-only-syntax", projectConfig)
	manager := Manager{}

	// codex IS a caveman-compatible CLI (cavemanOnlyAgent), so it stages a
	// script like the full-tools sweep above — just re-verified here under
	// the single-CLI shape that produced the earlier empty-"{ }" bugs.
	manager.registerCaveman(root, projectConfig)
	checkScriptSyntax(test, root, "caveman")

	// codex is NOT in codeReviewGraphPlatformFlag, so this is the genuine
	// no-compatible-CLI path: it must warn and stage nothing.
	output := captureStderr(test, func() {
		manager.registerCodeReviewGraph(root, projectConfig)
	})
	if output == "" {
		test.Error("registerCodeReviewGraph with no compatible CLI selected should warn")
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "run", "code-review-graph-install.sh")); err == nil {
		test.Error("registerCodeReviewGraph should not stage a script with no compatible CLI selected")
	}

	// tokensave/agentmemory/ponytail/agent-skills still stage a script even
	// with no natively-integrated CLI selected (tokensave always runs `init`;
	// ponytail/agent-skills always drop the shared-pool skill file;
	// agentmemory has nothing to do and returns early) — assert whichever
	// path each takes produces no syntax error.
	manager.registerTokenSave(root, projectConfig)
	checkScriptSyntax(test, root, "tokensave")

	manager.registerPonytail(root, projectConfig)
	checkScriptSyntax(test, root, "ponytail")

	manager.registerAgentSkills(root, projectConfig)
	checkScriptSyntax(test, root, "agent-skills")
}
