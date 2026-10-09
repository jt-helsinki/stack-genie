package config

import (
	"os"
	"testing"
)

// TestResolvedEgressDefault pins the security-posture default: an empty egress
// mode resolves to "public" (allow-outbound), while explicit modes pass through
// unchanged.
func TestResolvedEgressDefault(test *testing.T) {
	cases := []struct {
		egress string
		want   string
	}{
		{"", "public"},       // default flipped to allow-outbound
		{"public", "public"}, // pass-through
		{"deny", "deny"},     // pass-through
	}
	for _, testCase := range cases {
		network := NetworkConfig{Egress: testCase.egress}
		if got := network.ResolvedEgress(); got != testCase.want {
			test.Errorf("ResolvedEgress(%q) = %q, want %q", testCase.egress, got, testCase.want)
		}
	}
}

// TestEnsureGlobalDefault verifies the first call creates the global config and
// reports created=true, and a second call is a no-op reporting created=false.
func TestEnsureGlobalDefault(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	globalPath, err := GlobalPath()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(globalPath); err == nil {
		test.Fatalf("global config should not exist yet at %s", globalPath)
	}

	created, err := EnsureGlobalDefault()
	if err != nil {
		test.Fatalf("first EnsureGlobalDefault: %v", err)
	}
	if !created {
		test.Error("first call should report created=true")
	}
	if _, err := os.Stat(globalPath); err != nil {
		test.Fatalf("global config should exist after first call: %v", err)
	}
	// The written file must be a valid, loadable config.
	loaded, err := Load("")
	if err != nil {
		test.Fatalf("global config should load: %v", err)
	}
	if loaded.Workspace.Shell != Default().Workspace.Shell {
		test.Errorf("written config shell = %q, want default %q", loaded.Workspace.Shell, Default().Workspace.Shell)
	}

	created, err = EnsureGlobalDefault()
	if err != nil {
		test.Fatalf("second EnsureGlobalDefault: %v", err)
	}
	if created {
		test.Error("second call should report created=false (idempotent)")
	}
}

func boolPtr(value bool) *bool { return &value }

// TestContextToggleDefaults pins the three-state toggle semantics for each
// context tool: Caveman/Graphify default to TRUE when unset (nil), while
// code-review-graph/codebase-memory default to FALSE when unset (opt-in).
func TestContextToggleDefaults(test *testing.T) {
	cases := []struct {
		name    string
		fn      func(ContextConfig) bool
		set     func(*ContextConfig, *bool)
		nilWant bool
	}{
		{
			name:    "caveman",
			fn:      ContextConfig.CavemanEnabledOrDefault,
			set:     func(settings *ContextConfig, value *bool) { settings.CavemanEnabled = value },
			nilWant: true,
		},
		{
			name:    "graphify",
			fn:      ContextConfig.GraphifyEnabledOrDefault,
			set:     func(settings *ContextConfig, value *bool) { settings.GraphifyEnabled = value },
			nilWant: true,
		},
		{
			name:    "code-review-graph",
			fn:      ContextConfig.CodeReviewGraphEnabledOrDefault,
			set:     func(settings *ContextConfig, value *bool) { settings.CodeReviewGraphEnabled = value },
			nilWant: false,
		},
		{
			name:    "codebase-memory",
			fn:      ContextConfig.CodebaseMemoryEnabledOrDefault,
			set:     func(settings *ContextConfig, value *bool) { settings.CodebaseMemoryEnabled = value },
			nilWant: false,
		},
	}
	for _, testCase := range cases {
		// nil (unset) → tool's default.
		nilSettings := ContextConfig{}
		if got := testCase.fn(nilSettings); got != testCase.nilWant {
			test.Errorf("%s: nil default = %v, want %v", testCase.name, got, testCase.nilWant)
		}
		// explicit true → true.
		trueSettings := ContextConfig{}
		testCase.set(&trueSettings, boolPtr(true))
		if got := testCase.fn(trueSettings); !got {
			test.Errorf("%s: explicit true = %v, want true", testCase.name, got)
		}
		// explicit false → false.
		falseSettings := ContextConfig{}
		testCase.set(&falseSettings, boolPtr(false))
		if got := testCase.fn(falseSettings); got {
			test.Errorf("%s: explicit false = %v, want false", testCase.name, got)
		}
	}
}

// TestLoadProjectConfigMissing verifies a missing project config.yaml returns an
// empty Config with no error.
func TestLoadProjectConfigMissing(test *testing.T) {
	root := test.TempDir() // no .ai-platform/config.yaml written
	loaded, err := LoadProjectConfig(root)
	if err != nil {
		test.Fatalf("missing project config should not error: %v", err)
	}
	if loaded == nil {
		test.Fatal("missing project config should return a non-nil empty Config")
	}
	if loaded.OS != "" || len(loaded.Agent.Tools) != 0 || loaded.Workspace.Shell != "" {
		test.Fatalf("missing project config should be empty, got %+v", loaded)
	}
}

// TestLoadProjectConfigRoundTrip verifies a written project config round-trips.
func TestLoadProjectConfigRoundTrip(test *testing.T) {
	root := test.TempDir()
	original := &Config{
		OS:        "debian-trixie",
		Workspace: WorkspaceConfig{Shell: "zsh"},
		Network:   NetworkConfig{Egress: "deny"},
	}
	if err := WriteProject(root, original); err != nil {
		test.Fatal(err)
	}
	loaded, err := LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if loaded.OS != "debian-trixie" {
		test.Errorf("os = %q, want debian-trixie", loaded.OS)
	}
	if loaded.Workspace.Shell != "zsh" {
		test.Errorf("workspace round-trip = %+v", loaded.Workspace)
	}
	if loaded.Network.Egress != "deny" {
		test.Errorf("network.egress = %q, want deny", loaded.Network.Egress)
	}
}
