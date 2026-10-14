package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/nono"
)

// TestMain stubs the nono-registry pack pull and the AI tools' detached
// script launcher for the whole package's test run — ensureProfile's `nono
// pull` and tools.go's runDetachedScript are best-effort side effects that
// must never make a real network call (git clone, curl, npx, …), depend on a
// real binary, or leave a background process running past the test (which
// races t.TempDir()'s cleanup) in unit tests. The stub still WRITES the
// staged script to disk (mirroring the real implementation's layout) so
// tests can assert on its content/syntax — it just never executes it.
func TestMain(m *testing.M) {
	pullPackFn = func(context.Context, string) error { return nil }
	runDetachedScriptFn = func(root, tool, script string) error {
		runDir := filepath.Join(toolPool(root), "run")
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(runDir, tool+"-install.sh"), []byte(script), 0o755)
	}
	os.Exit(m.Run())
}

func readProfile(test *testing.T, path string) nono.Profile {
	test.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		test.Fatalf("read profile: %v", err)
	}
	var profile nono.Profile
	if err := json.Unmarshal(raw, &profile); err != nil {
		test.Fatalf("unmarshal profile: %v", err)
	}
	return profile
}

// TestEnsureProfileExtendsKnownPackForCLI verifies each CLI with a dedicated nono
// registry pack extends it, while Shell (empty cli) and an unlisted CLI fall back
// to nono's own built-in "default" profile.
func TestEnsureProfileExtendsKnownPackForCLI(test *testing.T) {
	cases := map[string]string{
		"opencode":    "nolabs-ai/opencode",
		"claude-code": "nolabs-ai/claude",
		"codex":       "nolabs-ai/codex",
		"":            "default", // Shell
		"gemini":      "default", // no dedicated pack
	}
	for cli, wantExtends := range cases {
		root := test.TempDir()
		path, err := ensureProfile(root, cli, &config.Config{}, "")
		if err != nil {
			test.Fatalf("ensureProfile(%q): %v", cli, err)
		}
		profile := readProfile(test, path)
		if profile.Extends != wantExtends {
			test.Errorf("ensureProfile(%q).Extends = %q, want %q", cli, profile.Extends, wantExtends)
		}
	}
}

// TestEnsureProfileNetworkCapabilitiesMirrorConfig verifies the profile's network
// block (block/allow/deny) is derived directly from the project's declared egress
// policy — a config.yaml change takes effect on the very next launch.
func TestEnsureProfileNetworkCapabilitiesMirrorConfig(test *testing.T) {
	root := test.TempDir()
	projectConfig := &config.Config{Network: config.NetworkConfig{
		Egress: "deny", AllowDomain: []string{"api.github.com"}, DenyDomain: []string{"evil.example.com"},
	}}
	path, err := ensureProfile(root, "opencode", projectConfig, "")
	if err != nil {
		test.Fatal(err)
	}
	profile := readProfile(test, path)
	if !profile.Network.Block {
		test.Error("deny egress should set Network.Block = true")
	}
	if len(profile.Network.AllowDomain) != 1 || profile.Network.AllowDomain[0] != "api.github.com" {
		test.Errorf("AllowDomain = %v, want [api.github.com]", profile.Network.AllowDomain)
	}
	if len(profile.Network.DenyDomain) != 1 || profile.Network.DenyDomain[0] != "evil.example.com" {
		test.Errorf("DenyDomain = %v, want [evil.example.com]", profile.Network.DenyDomain)
	}

	// public egress → Block is false.
	projectConfig.Network.Egress = "public"
	path, err = ensureProfile(root, "opencode", projectConfig, "")
	if err != nil {
		test.Fatal(err)
	}
	if readProfile(test, path).Network.Block {
		test.Error("public egress should set Network.Block = false")
	}
}

// TestEnsureProfileDenyModeStillAllowsGateway verifies a `deny` egress policy
// never cuts the agent off from its own model gateway — only the open
// internet is meant to be fenced off (regression for a bug where `ai network
// egress deny` silently broke every agent CLI's own LiteLLM traffic).
func TestEnsureProfileDenyModeStillAllowsGateway(test *testing.T) {
	root := test.TempDir()
	projectConfig := &config.Config{Network: config.NetworkConfig{Egress: "deny"}}
	path, err := ensureProfile(root, "opencode", projectConfig, "http://127.0.0.1:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	profile := readProfile(test, path)
	if !profile.Network.Block {
		test.Fatal("deny egress should set Network.Block = true")
	}
	found := false
	for _, domain := range profile.Network.AllowDomain {
		if domain == "127.0.0.1" {
			found = true
		}
	}
	if !found {
		test.Errorf("AllowDomain = %v, want it to include the gateway host 127.0.0.1", profile.Network.AllowDomain)
	}

	// A Shell launch (no gatewayURL) must not fabricate a gateway entry.
	shellPath, err := ensureProfile(root, "", projectConfig, "")
	if err != nil {
		test.Fatal(err)
	}
	if shell := readProfile(test, shellPath); len(shell.Network.AllowDomain) != 0 {
		test.Errorf("Shell profile AllowDomain = %v, want empty (no gatewayURL passed)", shell.Network.AllowDomain)
	}
}

// A loopback gateway is only reachable from the sandbox through an explicit open port; a
// remote gateway is reached through the proxy's domain allow-list and needs none.
func TestEnsureProfileOpensLoopbackGatewayPort(test *testing.T) {
	root := test.TempDir()
	path, err := ensureProfile(root, "opencode", &config.Config{}, "http://127.0.0.1:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	if got := readProfile(test, path).Network.OpenPort; len(got) != 1 || got[0] != 18787 {
		test.Fatalf("OpenPort = %v, want [18787]", got)
	}
	remote, err := ensureProfile(root, "opencode", &config.Config{}, "http://gateway.example:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	if got := readProfile(test, remote).Network.OpenPort; len(got) != 0 {
		test.Fatalf("remote gateway OpenPort = %v, want none", got)
	}
}

// TestEnsureProfileGeminiSetsBaseURLEnvVar verifies gemini — which has no
// config-file base-URL setting — gets the gateway ROOT (no /v1 suffix) as a
// non-secret profile environment variable; every other CLI gets no such var.
func TestEnsureProfileGeminiSetsBaseURLEnvVar(test *testing.T) {
	root := test.TempDir()
	path, err := ensureProfile(root, "gemini", &config.Config{}, "http://127.0.0.1:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	profile := readProfile(test, path)
	if got := profile.Environment.SetVars[agentcfg.GeminiBaseURLEnvVar]; got != "http://127.0.0.1:18787" {
		test.Errorf("gemini base URL env = %q, want http://127.0.0.1:18787 (no /v1)", got)
	}

	opencodePath, err := ensureProfile(root, "opencode", &config.Config{}, "http://127.0.0.1:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	if opencode := readProfile(test, opencodePath); len(opencode.Environment.SetVars) != 0 {
		test.Errorf("opencode profile should carry no set_vars, got %v", opencode.Environment.SetVars)
	}
}

// TestEnsureProfileWritesToProjectDir verifies the profile path is per-CLI and
// lives under the project's .ai-platform directory.
func TestEnsureProfileWritesToProjectDir(test *testing.T) {
	root := test.TempDir()
	path, err := ensureProfile(root, "codex", &config.Config{}, "")
	if err != nil {
		test.Fatal(err)
	}
	want := filepath.Join(root, ".ai-platform", "nono-profile-codex.json")
	if path != want {
		test.Errorf("profile path = %q, want %q", path, want)
	}
}
