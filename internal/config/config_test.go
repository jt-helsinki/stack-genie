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
  cpu_limit: 4
microsandbox:
  idle_timeout: 12h
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
	if cfg.Workspace.CPULimit != 4 { // from global, untouched by project
		test.Errorf("cpu_limit = %d, want 4", cfg.Workspace.CPULimit)
	}
	if cfg.Microsandbox.IdleTimeout != "12h" { // from global, untouched by project
		test.Errorf("microsandbox.idle_timeout = %q, want 12h", cfg.Microsandbox.IdleTimeout)
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

func TestDefaultMicrosandboxIdleTimeout(t *testing.T) {
	cfg := Default()
	if got := cfg.Microsandbox.IdleTimeout; got != DefaultMicrosandboxIdleTimeout {
		t.Fatalf("default microsandbox.idle_timeout = %q, want %q", got, DefaultMicrosandboxIdleTimeout)
	}
	if got := (MicrosandboxConfig{}).ResolvedIdleTimeout(); got != DefaultMicrosandboxIdleTimeout {
		t.Fatalf("empty resolved idle timeout = %q, want %q", got, DefaultMicrosandboxIdleTimeout)
	}
}

func TestMicrosandboxIdleTimeoutValidation(t *testing.T) {
	for _, value := range []string{"30s", "5m", "24h"} {
		if err := ValidateIdleTimeout(value); err != nil {
			t.Fatalf("%s should be valid: %v", value, err)
		}
	}
	for _, value := range []string{"0", "0s", "-1h", "soon"} {
		if err := ValidateIdleTimeout(value); err == nil {
			t.Fatalf("%s should be invalid", value)
		}
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
	for _, value := range []string{"fish", "sh", "BASH", "zsh ", "powershell"} {
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
	valid := NetworkConfig{
		AllowHostServices: []HostService{{Host: "gateway", Port: 5432}},
		PublishPorts:      []PortMapping{{Guest: 3000, Host: 3000}},
	}
	if err := valid.Validate(); err != nil {
		test.Fatalf("valid network rejected: %v", err)
	}

	badPort := NetworkConfig{AllowHostServices: []HostService{{Port: 70000}}}
	if err := badPort.Validate(); err == nil {
		test.Fatal("expected out-of-range port to be rejected")
	}

	dupHost := NetworkConfig{PublishPorts: []PortMapping{{Guest: 3000, Host: 8080}, {Guest: 3001, Host: 8080}}}
	if err := dupHost.Validate(); err == nil {
		test.Fatal("expected duplicate host port to be rejected")
	}
}

func TestResolveHostServices(test *testing.T) {
	network := NetworkConfig{AllowHostServices: []HostService{
		{Host: "gateway", Port: 5432},  // gateway token → resolved address
		{Port: 6379},                   // empty host → resolved address
		{Host: "10.0.0.5", Port: 9092}, // explicit host preserved
	}}
	resolved := network.ResolveHostServices("192.0.2.1")
	want := []string{"192.0.2.1:5432", "192.0.2.1:6379", "10.0.0.5:9092"}
	if len(resolved) != len(want) {
		test.Fatalf("resolved = %v, want %v", resolved, want)
	}
	for index := range want {
		if resolved[index] != want[index] {
			test.Errorf("resolved[%d] = %q, want %q", index, resolved[index], want[index])
		}
	}
}

func TestAppsRoundTrip(test *testing.T) {
	root := test.TempDir()
	original := &Config{
		OS:   "debian-trixie",
		Apps: []AppEntry{{Key: "openwebui", Port: 21000}, {Key: "anythingllm", Port: 21001}},
	}
	if err := WriteProject(root, original); err != nil {
		test.Fatal(err)
	}
	loaded, err := LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if len(loaded.Apps) != 2 {
		test.Fatalf("loaded %d apps, want 2", len(loaded.Apps))
	}
	if loaded.Apps[0].Key != "openwebui" || loaded.Apps[0].Port != 21000 {
		test.Fatalf("app[0] = %+v", loaded.Apps[0])
	}
	if loaded.Apps[1].Key != "anythingllm" || loaded.Apps[1].Port != 21001 {
		test.Fatalf("app[1] = %+v", loaded.Apps[1])
	}
}

func TestAppsOmittedWhenEmpty(test *testing.T) {
	root := test.TempDir()
	if err := WriteProject(root, &Config{OS: "ubuntu"}); err != nil {
		test.Fatal(err)
	}
	loaded, err := LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if len(loaded.Apps) != 0 {
		test.Fatalf("apps = %v, want empty", loaded.Apps)
	}
}

// TestParseMemoryMiBBareNumberIsGB pins the memory semantics: a bare number is
// GIGABYTES (the config is "just a number, in GB"); unit suffixes still work.
func TestParseMemoryMiBBareNumberIsGB(test *testing.T) {
	cases := map[string]uint64{
		"8":     8192,  // 8 GB
		"24":    24576, // 24 GB
		"1":     1024,  // 1 GB
		"4G":    4096,  // suffix still honored
		"2Gi":   2048,
		"512M":  512,
		"512Mi": 512,
	}
	for in, want := range cases {
		got, err := ParseMemoryMiB(in)
		if err != nil {
			test.Errorf("ParseMemoryMiB(%q): %v", in, err)
			continue
		}
		if got != want {
			test.Errorf("ParseMemoryMiB(%q) = %d MiB, want %d", in, got, want)
		}
	}
}

// TestValidateMemoryMinimum guards the boot-minimum (and the old unit footgun: a
// tiny value like "0.1" or "256M" is rejected).
func TestValidateMemoryMinimum(test *testing.T) {
	for _, ok := range []string{"", "8", "24", "1", "4G", "512M"} {
		if err := ValidateMemory(ok); err != nil {
			test.Errorf("ValidateMemory(%q) should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"256M", "0.1", "0.25"} {
		if err := ValidateMemory(bad); err == nil {
			test.Errorf("ValidateMemory(%q) should be rejected (below %d MiB)", bad, MinWorkspaceMemoryMiB)
		}
	}
}

// TestUsableHostMemoryMiB verifies the workspace memory ceiling always leaves host
// headroom (strictly below host RAM) — a microVM given all host RAM cannot boot.
func TestUsableHostMemoryMiB(test *testing.T) {
	cases := map[uint64]uint64{
		24576: 18432, // 24 GiB → reserve max(2048, 6144)=6144 → 18432 (the reported bug)
		16384: 12288, // 16 GiB → reserve 4096 → 12288
		8192:  6144,  // 8 GiB  → reserve max(2048, 2048)=2048 → 6144
		4096:  2048,  // 4 GiB  → reserve 2048 → 2048
	}
	for host, want := range cases {
		if got := UsableHostMemoryMiB(host); got != want {
			test.Errorf("UsableHostMemoryMiB(%d) = %d, want %d", host, got, want)
		}
		if UsableHostMemoryMiB(host) >= host {
			test.Errorf("UsableHostMemoryMiB(%d) must be strictly below host RAM", host)
		}
	}
	// A tiny host still yields the bootable minimum, never zero.
	if got := UsableHostMemoryMiB(2048); got != MinWorkspaceMemoryMiB {
		test.Errorf("UsableHostMemoryMiB(2048) = %d, want the %d MiB minimum", got, MinWorkspaceMemoryMiB)
	}
}

// TestAuthModeDefault verifies AuthMode defaults an absent/empty CLI to api-key.
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
