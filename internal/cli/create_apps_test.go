package cli

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

func TestSpecFromFlagsCarriesApps(test *testing.T) {
	spec, err := specFromFlags("demo", "ubuntu", nil, nil, []string{"openwebui"}, "", "demo")
	if err != nil {
		test.Fatal(err)
	}
	if len(spec.Apps) != 1 || spec.Apps[0] != "openwebui" {
		test.Fatalf("spec.Apps = %v, want [openwebui]", spec.Apps)
	}
}

func TestSpecFromFlagsAppsDefaultEmpty(test *testing.T) {
	spec, err := specFromFlags("demo", "ubuntu", nil, nil, nil, "", "demo")
	if err != nil {
		test.Fatal(err)
	}
	if len(spec.Apps) != 0 {
		test.Fatalf("spec.Apps = %v, want empty (apps opt-in)", spec.Apps)
	}
}

func TestSpecFromFlagsRejectsUnknownApp(test *testing.T) {
	_, err := specFromFlags("demo", "ubuntu", nil, nil, []string{"nope"}, "", "demo")
	platformErr, ok := err.(*output.Error)
	if !ok || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("err = %v, want exit-2 *output.Error", err)
	}
}

func TestSeedSpecAppsEmptyByDefault(test *testing.T) {
	spec := seedSpec("demo", "ubuntu", nil, nil, nil, "", "demo")
	if len(spec.Apps) != 0 {
		test.Fatalf("seedSpec apps = %v, want empty", spec.Apps)
	}
}

func TestSeedSpecDefaultToolFromSelectedAgents(test *testing.T) {
	spec := seedSpec("demo", "ubuntu", []string{"codex", "gemini"}, nil, nil, "", "demo")
	if spec.DefaultTool != "codex" {
		test.Fatalf("seedSpec default tool = %q, want first selected agent codex", spec.DefaultTool)
	}
	if got := formatAgentCLIs(spec.AgentCLIs); got != "codex, gemini" {
		test.Fatalf("formatted selected agents = %q, want codex, gemini", got)
	}
}

func TestSpecFromFlagsIdleTimeoutDefaultAndOverride(test *testing.T) {
	defaulted, err := specFromFlags("demo", "ubuntu", nil, nil, nil, "", "demo")
	if err != nil {
		test.Fatal(err)
	}
	if defaulted.IdleTimeout != config.DefaultMicrosandboxIdleTimeout {
		test.Fatalf("default idle timeout = %q, want %q", defaulted.IdleTimeout, config.DefaultMicrosandboxIdleTimeout)
	}
	overridden, err := specFromFlags("demo", "ubuntu", nil, nil, nil, "2h", "demo")
	if err != nil {
		test.Fatal(err)
	}
	if overridden.IdleTimeout != "2h" {
		test.Fatalf("overridden idle timeout = %q, want 2h", overridden.IdleTimeout)
	}
}

func TestSpecFromFlagsRejectsBadIdleTimeout(test *testing.T) {
	_, err := specFromFlags("demo", "ubuntu", nil, nil, nil, "0s", "demo")
	platformErr, ok := err.(*output.Error)
	if !ok || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("err = %v, want exit-2 *output.Error", err)
	}
}

func TestAgentCLIOptionsReflectSelectedAgents(test *testing.T) {
	options := agentCLIOptions([]string{"codex", "gemini"})
	if len(options) != 2 {
		test.Fatalf("options = %d, want 2", len(options))
	}
	if options[0].Value != "codex" || options[1].Value != "gemini" {
		test.Fatalf("options = %#v, want codex/gemini", options)
	}
}

func TestNormalizeDefaultAgentCLI(test *testing.T) {
	if got := normalizeDefaultAgentCLI("pi", []string{"opencode", "pi"}); got != "pi" {
		test.Fatalf("valid default changed to %q", got)
	}
	if got := normalizeDefaultAgentCLI("opencode", []string{"codex", "gemini"}); got != "codex" {
		test.Fatalf("stale default normalized to %q, want codex", got)
	}
	if got := normalizeDefaultAgentCLI("opencode", nil); got != "" {
		test.Fatalf("empty agents default = %q, want empty", got)
	}
}
