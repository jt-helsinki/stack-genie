package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(test *testing.T, path, content string) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

func TestMergeProjectOverGlobal(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	gp, _ := GlobalPath()
	writeFile(test, gp, `
workspace:
  shell: zsh
context:
  caveman_level: full
  strategy: balanced
`)
	projectRoot := filepath.Join(home, "projects", "app")
	writeFile(test, ProjectPath(projectRoot), `
os: alma
context:
  strategy: aggressive
`)

	cfg, err := Load(projectRoot)
	if err != nil {
		test.Fatal(err)
	}
	if cfg.Workspace.Shell != "zsh" { // from global, untouched by project
		test.Errorf("shell = %q, want zsh", cfg.Workspace.Shell)
	}
	if cfg.OS != "alma" { // only in project
		test.Errorf("os = %q, want alma", cfg.OS)
	}
	if cfg.Context.CavemanLevel != "full" { // global survives a nested partial override
		test.Errorf("caveman_level = %q, want full", cfg.Context.CavemanLevel)
	}
	if cfg.Context.Strategy != "aggressive" { // project wins on the overlapping key
		test.Errorf("strategy = %q, want aggressive", cfg.Context.Strategy)
	}
}

func TestDefaultShellIsBash(test *testing.T) {
	if got := Default().Workspace.Shell; got != "bash" {
		test.Fatalf("default workspace.shell = %q, want %q", got, "bash")
	}
}

func TestValidateShell(test *testing.T) {
	for _, value := range []string{"", "bash", "zsh"} {
		if err := ValidateShell(value); err != nil {
			test.Errorf("ValidateShell(%q) should be valid: %v", value, err)
		}
	}
	for _, value := range []string{"tcsh", "sh", "BASH", "zsh ", "ksh"} {
		if err := ValidateShell(value); err == nil {
			test.Errorf("ValidateShell(%q) should be rejected", value)
		}
	}
}

func TestUnknownFieldRejected(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	gp, _ := GlobalPath()
	writeFile(test, gp, "os: alma\nbogus: true\n")
	if _, err := Load(""); err == nil {
		test.Fatal("expected unknown-field rejection")
	}
}

func TestMissingLayersOK(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	cfg, err := Load("")
	if err != nil {
		test.Fatalf("missing layers should not error: %v", err)
	}
	if cfg.OS != "" || cfg.Agent.DefaultTool != "" {
		test.Fatalf("expected empty config, got %+v", cfg)
	}
}

func TestNetworkValidate(test *testing.T) {
	for _, mode := range []string{"", "deny", "public"} {
		if err := (NetworkConfig{Egress: mode}).Validate(); err != nil {
			test.Errorf("egress %q should be valid: %v", mode, err)
		}
	}
	if err := (NetworkConfig{Egress: "unrestricted"}).Validate(); err == nil {
		test.Fatal("unrestricted is no longer a supported egress mode")
	}
	if err := (NetworkConfig{Egress: "nonsense"}).Validate(); err == nil {
		test.Fatal("expected an unknown egress mode to be rejected")
	}
}

func TestAuthModeDefault(test *testing.T) {
	agent := AgentConfig{AuthModes: map[string]string{"claude-code": "oauth", "codex": ""}}
	cases := map[string]string{
		"claude-code": "oauth",   // explicit
		"codex":       "api-key", // empty → default
		"gemini":      "api-key", // absent → default
	}
	for cli, want := range cases {
		if got := agent.AuthMode(cli); got != want {
			test.Errorf("AuthMode(%q) = %q, want %q", cli, got, want)
		}
	}
	// A nil map still defaults cleanly.
	if got := (AgentConfig{}).AuthMode("claude-code"); got != "api-key" {
		test.Errorf("AuthMode on nil map = %q, want api-key", got)
	}
	// A forced-oauth CLI (ForcedOAuthCLIs) is ALWAYS oauth, even absent from the map or
	// if a bad edit set it to api-key — it has no api-key mode. Currently no supported
	// CLI is forced-oauth, so an unknown/hypothetical one must fall through to the
	// ordinary default instead.
	if got := (AgentConfig{}).AuthMode("some-native-cli"); got != "api-key" {
		test.Errorf("AuthMode(unknown) on nil map = %q, want api-key (not forced)", got)
	}
}

// TestForcedOAuthCLIs pins the forced-oauth set: currently empty (no supported CLI is
// forced-oauth).
func TestForcedOAuthCLIs(test *testing.T) {
	if got := ForcedOAuthCLIs(); len(got) != 0 {
		test.Fatalf("ForcedOAuthCLIs() = %v, want empty", got)
	}
	for _, cli := range []string{"claude-code", "codex", "gemini"} {
		if !IsOAuthEligible(cli) {
			test.Errorf("%s must be OAuth-eligible", cli)
		}
	}
	for _, cli := range []string{"opencode", "omp", "hermes"} {
		if IsOAuthEligible(cli) {
			test.Errorf("%s is a gateway-only agent and must not be OAuth-eligible", cli)
		}
		if (&AgentConfig{}).AuthMode(cli) != "api-key" {
			test.Errorf("%s must default to api-key auth mode", cli)
		}
	}
}

// TestValidateAuthMode covers the accepted values and a rejection.
func TestValidateAuthMode(test *testing.T) {
	for _, ok := range []string{"", "api-key", "oauth"} {
		if err := ValidateAuthMode(ok); err != nil {
			test.Errorf("ValidateAuthMode(%q) should be valid: %v", ok, err)
		}
	}
	if err := ValidateAuthMode("sso"); err == nil {
		test.Error("ValidateAuthMode(\"sso\") should be rejected")
	}
}

// TestOAuthCapableCLIs pins the single source of truth for the OAuth-capable set.
func TestOAuthCapableCLIs(test *testing.T) {
	got := OAuthCapableCLIs()
	want := []string{"claude-code", "codex", "gemini"}
	if len(got) != len(want) {
		test.Fatalf("OAuthCapableCLIs() = %v, want %v", got, want)
	}
	for index, cli := range want {
		if got[index] != cli {
			test.Errorf("OAuthCapableCLIs()[%d] = %q, want %q", index, got[index], cli)
		}
	}
}

// TestAuthModesRoundTrip verifies auth_modes survives a project config write/read.
func TestAuthModesRoundTrip(test *testing.T) {
	root := test.TempDir()
	want := map[string]string{"claude-code": "oauth", "codex": "api-key"}
	if err := WriteProject(root, &Config{Agent: AgentConfig{Tools: []string{"claude-code", "codex"}, AuthModes: want}}); err != nil {
		test.Fatal(err)
	}
	loaded, err := LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	for cli, mode := range want {
		if got := loaded.Agent.AuthModes[cli]; got != mode {
			test.Errorf("round-trip auth_modes[%q] = %q, want %q", cli, got, mode)
		}
	}
}
