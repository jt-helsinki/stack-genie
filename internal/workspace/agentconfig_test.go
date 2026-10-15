package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
)

func TestHeadroomParamsPerStrategy(test *testing.T) {
	cases := map[string]struct {
		keepTurns int
		buffer    int
	}{
		"conservative": {8, 12000},
		"aggressive":   {2, 4000},
		"balanced":     {5, 8000},
		"":             {5, 8000}, // default
		"unknown":      {5, 8000}, // falls through to default
	}
	for strategy, want := range cases {
		keepTurns, buffer := headroomParams(strategy)
		if keepTurns != want.keepTurns || buffer != want.buffer {
			test.Errorf("headroomParams(%q) = (%d,%d), want (%d,%d)", strategy, keepTurns, buffer, want.keepTurns, want.buffer)
		}
	}
}

// TestWriteAgentConfigDispatchesPerCLI verifies each known CLI writes to its own
// default on-disk location (or none, for gemini — env-only), and that an unknown
// CLI is simply a no-op (gemini's own path).
func TestWriteAgentConfigDispatchesPerCLI(test *testing.T) {
	manager := Manager{Served: fakeServedModels{}}
	cases := map[string]string{
		"opencode":    filepath.Join(".opencode", "opencode.json"),
		"claude-code": filepath.Join(".claude", "settings.json"),
		"codex":       filepath.Join(".codex", "config.toml"),
	}
	for cli, relPath := range cases {
		root := test.TempDir()
		projectConfig := &config.Config{Agent: config.AgentConfig{Tools: []string{cli}}}
		if err := manager.writeAgentConfig(root, cli, projectConfig, "http://127.0.0.1:18787/v1", "sk-key"); err != nil {
			test.Fatalf("writeAgentConfig(%q): %v", cli, err)
		}
		if _, err := os.Stat(filepath.Join(root, relPath)); err != nil {
			test.Errorf("%s: expected %s to be written: %v", cli, relPath, err)
		}
	}

	// gemini is env-only — no config file, no error.
	root := test.TempDir()
	if err := manager.writeAgentConfig(root, "gemini", &config.Config{}, "http://127.0.0.1:18787/v1", "sk-key"); err != nil {
		test.Fatalf("writeAgentConfig(gemini): %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		test.Errorf("gemini should write nothing, found %v", entries)
	}
}

// TestWriteAgentConfigInjectsMCPServersForManagedConfigCLIs is a regression
// test: codex/omp/hermes own configs the platform rewrites wholesale each
// launch, so an enabled AI tool (code-review-graph here) must reach them via
// MCP-server injection (agentcfg.EnabledMCPServers/AppendCodexMCP/
// OmpMcpConfig/InjectHermesMCP) rather than that tool's own native
// per-platform install, which would be clobbered on the next launch. These
// injection calls existed in internal/agentcfg with full test coverage but
// were never wired into internal/workspace's config writers until this test
// was added.
func TestWriteAgentConfigInjectsMCPServersForManagedConfigCLIs(test *testing.T) {
	test.Setenv("HOME", test.TempDir()) // codex/omp/hermes also write GLOBAL home-scoped files
	manager := Manager{Served: fakeServedModels{}}
	enabled := true
	projectConfig := &config.Config{
		Agent:   config.AgentConfig{Tools: []string{"codex", "omp", "hermes"}},
		Context: config.ContextConfig{CodeReviewGraphEnabled: &enabled},
	}

	root := test.TempDir()
	if err := manager.writeAgentConfig(root, "codex", projectConfig, "http://127.0.0.1:18787/v1", "sk-key"); err != nil {
		test.Fatalf("writeAgentConfig(codex): %v", err)
	}
	codexConfig, err := os.ReadFile(filepath.Join(root, ".codex", "config.toml"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(codexConfig), "[mcp_servers.code-review-graph]") {
		test.Errorf("codex config.toml missing injected code-review-graph MCP server:\n%s", codexConfig)
	}

	if err := manager.writeAgentConfig(root, "omp", projectConfig, "http://127.0.0.1:18787/v1", "sk-key"); err != nil {
		test.Fatalf("writeAgentConfig(omp): %v", err)
	}
	ompMCP, err := os.ReadFile(filepath.Join(root, ".omp", "mcp.json"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(ompMCP), "code-review-graph") {
		test.Errorf(".omp/mcp.json missing injected code-review-graph MCP server:\n%s", ompMCP)
	}

	if err := manager.writeAgentConfig(root, "hermes", projectConfig, "http://127.0.0.1:18787/v1", "sk-key"); err != nil {
		test.Fatalf("writeAgentConfig(hermes): %v", err)
	}
	homeDir, _ := os.UserHomeDir()
	hermesConfig, err := os.ReadFile(filepath.Join(homeDir, ".hermes", "config.yaml"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(hermesConfig), "code-review-graph") {
		test.Errorf("hermes config.yaml missing injected code-review-graph MCP server:\n%s", hermesConfig)
	}
}

// TestWriteClaudeConfigOAuthVsAPIKey verifies the oauth/api-key mode shape
// difference: oauth never writes a base-URL env block, api-key always does.
func TestWriteClaudeConfigOAuthVsAPIKey(test *testing.T) {
	root := test.TempDir()
	if err := writeClaudeConfig(root, "http://127.0.0.1:18787/v1", true); err != nil {
		test.Fatal(err)
	}
	apiKeyRaw, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(apiKeyRaw), "ANTHROPIC_BASE_URL") {
		test.Errorf("api-key claude settings missing ANTHROPIC_BASE_URL:\n%s", apiKeyRaw)
	}

	if err := writeClaudeConfig(root, "http://127.0.0.1:18787/v1", false); err != nil {
		test.Fatal(err)
	}
	oauthRaw, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(oauthRaw), "ANTHROPIC_BASE_URL") {
		test.Errorf("oauth claude settings must not carry the gateway base URL:\n%s", oauthRaw)
	}
}

// TestAppendCodexTrustIsIdempotent verifies the GLOBAL ~/.codex/config.toml trust
// file only gains ONE entry per project root no matter how many times it runs
// (it is shared across every project on the host, unlike the old per-VM-isolated
// world, so a blind overwrite would destroy other projects' entries).
func TestAppendCodexTrustIsIdempotent(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	projectRoot := filepath.Join(test.TempDir(), "proj")

	for range 3 {
		if err := appendCodexTrust(projectRoot); err != nil {
			test.Fatalf("appendCodexTrust: %v", err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		test.Fatal(err)
	}
	marker := agentcfg.CodexTrustMarker(projectRoot)
	if count := strings.Count(string(raw), marker); count != 1 {
		test.Fatalf("trust entry appended %d times, want exactly 1:\n%s", count, raw)
	}
}

// TestAppendCodexTrustPreservesOtherProjects verifies a SECOND project's trust
// entry does not disturb the FIRST project's already-written entry.
func TestAppendCodexTrustPreservesOtherProjects(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	first := filepath.Join(test.TempDir(), "first")
	second := filepath.Join(test.TempDir(), "second")

	if err := appendCodexTrust(first); err != nil {
		test.Fatal(err)
	}
	if err := appendCodexTrust(second); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		test.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, agentcfg.CodexTrustMarker(first)) || !strings.Contains(text, agentcfg.CodexTrustMarker(second)) {
		test.Fatalf("both projects' trust entries should be present:\n%s", text)
	}
}

// opencode only waits 5s for an MCP server and codebase-memory-mcp needs ~8s to answer
// `initialize`, so its entry must carry a longer timeout (and be complete, or opencode rejects it).
func TestWithCodebaseMemoryMCPAddsCompleteEntryWithTimeout(test *testing.T) {
	binDir := test.TempDir()
	binary := filepath.Join(binDir, "codebase-memory-mcp")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		test.Fatal(err)
	}
	test.Setenv("PATH", binDir)
	out, err := withCodebaseMemoryMCP([]byte(`{"theme":"x"}`))
	if err != nil {
		test.Fatal(err)
	}
	var decoded struct {
		Theme string `json:"theme"`
		MCP   map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Timeout int      `json:"timeout"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		test.Fatal(err)
	}
	entry := decoded.MCP["codebase-memory-mcp"]
	if decoded.Theme != "x" || entry.Type != "local" || len(entry.Command) != 1 || entry.Command[0] != binary || entry.Timeout != codebaseMemoryMCPTimeoutMS {
		test.Fatalf("entry = %+v theme=%q", entry, decoded.Theme)
	}
	test.Setenv("PATH", test.TempDir())
	if untouched, _ := withCodebaseMemoryMCP([]byte(`{"a":1}`)); string(untouched) != `{"a":1}` {
		test.Fatalf("a missing binary must leave the config untouched: %s", untouched)
	}
}
