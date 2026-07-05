package cli

import (
	"errors"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// TestSpecFromFlagsCarriesAuthModes verifies --auth-mode is parsed into Spec.AuthModes for
// the selected OAuth-capable CLIs.
func TestSpecFromFlagsCarriesAuthModes(test *testing.T) {
	spec, err := specFromFlags(createFlags{
		name:        "demo",
		osKey:       "ubuntu",
		agents:      []string{"opencode", "claude-code", "codex"},
		authMode:    "claude-code=oauth,codex=api-key",
		defaultName: "demo",
	})
	if err != nil {
		test.Fatal(err)
	}
	if spec.AuthModes["claude-code"] != "oauth" {
		test.Errorf("AuthModes[claude-code] = %q, want oauth", spec.AuthModes["claude-code"])
	}
	if spec.AuthModes["codex"] != "api-key" {
		test.Errorf("AuthModes[codex] = %q, want api-key", spec.AuthModes["codex"])
	}
}

// TestParseAuthModesRejectsBadInput covers the invalid-input (exit 2) cases: a
// non-OAuth-capable CLI, a CLI that is not selected, a bad mode, and malformed syntax.
func TestParseAuthModesRejectsBadInput(test *testing.T) {
	agents := []string{"opencode", "claude-code"}
	cases := []struct {
		name, raw string
	}{
		{"not oauth-capable", "opencode=oauth"},
		{"not selected", "codex=oauth"},
		{"bad mode", "claude-code=sso"},
		{"missing mode", "claude-code="},
		{"no equals", "claude-code"},
	}
	for _, tc := range cases {
		_, err := parseAuthModes(tc.raw, agents)
		if err == nil {
			test.Errorf("%s: parseAuthModes(%q) should error", tc.name, tc.raw)
			continue
		}
		var platformErr *output.Error
		if !errors.As(err, &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Errorf("%s: parseAuthModes(%q) error should be exit %d, got %v", tc.name, tc.raw, output.ExitInvalidInput, err)
		}
	}
}

// TestParseAuthModesEmpty yields a nil map (no per-agent modes recorded).
func TestParseAuthModesEmpty(test *testing.T) {
	modes, err := parseAuthModes("", []string{"claude-code"})
	if err != nil {
		test.Fatal(err)
	}
	if modes != nil {
		test.Errorf("empty --auth-mode should yield nil, got %v", modes)
	}
}

// TestSpecFromFlagsRejectsUnselectedAuthMode verifies specFromFlags maps a bad --auth-mode
// to exit 2 via validateProvidedCreateFlags.
func TestSpecFromFlagsRejectsUnselectedAuthMode(test *testing.T) {
	_, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", authMode: "gemini=oauth", defaultName: "demo"})
	if err == nil {
		test.Fatal("expected an error for a non-selected --auth-mode agent")
	}
	var platformErr *output.Error
	if !errors.As(err, &platformErr) || platformErr.Code != output.ExitInvalidInput {
		test.Errorf("error should be exit %d, got %v", output.ExitInvalidInput, err)
	}
}
