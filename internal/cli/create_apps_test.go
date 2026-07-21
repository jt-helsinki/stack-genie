package cli

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

func TestSpecFromFlagsCarriesApps(test *testing.T) {
	spec, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", apps: []string{"openwebui"}, defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if len(spec.Apps) != 1 || spec.Apps[0] != "openwebui" {
		test.Fatalf("spec.Apps = %v, want [openwebui]", spec.Apps)
	}
}

func TestSpecFromFlagsAppsDefaultEmpty(test *testing.T) {
	spec, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if len(spec.Apps) != 0 {
		test.Fatalf("spec.Apps = %v, want empty (apps opt-in)", spec.Apps)
	}
}

func TestSpecFromFlagsRejectsUnknownApp(test *testing.T) {
	_, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", apps: []string{"nope"}, defaultName: "demo"})
	platformErr, ok := err.(*output.Error)
	if !ok || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("err = %v, want exit-2 *output.Error", err)
	}
}

func TestSeedSpecAppsEmptyByDefault(test *testing.T) {
	spec := seedSpec(createFlags{name: "demo", osKey: "ubuntu", defaultName: "demo"})
	if len(spec.Apps) != 0 {
		test.Fatalf("seedSpec apps = %v, want empty", spec.Apps)
	}
}

func TestSeedSpecDefaultToolFromSelectedAgents(test *testing.T) {
	spec := seedSpec(createFlags{name: "demo", osKey: "ubuntu", agents: []string{"codex", "gemini"}, defaultName: "demo"})
	if spec.DefaultTool != "codex" {
		test.Fatalf("seedSpec default tool = %q, want first selected agent codex", spec.DefaultTool)
	}
	if got := formatAgentCLIs(spec.AgentCLIs); got != "codex, gemini" {
		test.Fatalf("formatted selected agents = %q, want codex, gemini", got)
	}
}

func TestSpecFromFlagsIdleTimeoutDefaultAndOverride(test *testing.T) {
	defaulted, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if defaulted.IdleTimeout != config.DefaultMicrosandboxIdleTimeout {
		test.Fatalf("default idle timeout = %q, want %q", defaulted.IdleTimeout, config.DefaultMicrosandboxIdleTimeout)
	}
	overridden, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", idleTimeout: "2h", defaultName: "demo"})
	if err != nil {
		test.Fatal(err)
	}
	if overridden.IdleTimeout != "2h" {
		test.Fatalf("overridden idle timeout = %q, want 2h", overridden.IdleTimeout)
	}
}

func TestSpecFromFlagsRejectsBadIdleTimeout(test *testing.T) {
	_, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", idleTimeout: "0s", defaultName: "demo"})
	platformErr, ok := err.(*output.Error)
	if !ok || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("err = %v, want exit-2 *output.Error", err)
	}
}

func TestSpecFromFlagsCarriesResourcesAndPorts(test *testing.T) {
	spec, err := specFromFlags(createFlags{
		name: "demo", osKey: "ubuntu", defaultName: "demo",
		cpus: 1, memory: "2G", ports: []string{"8080", "9000:3000"},
	})
	if err != nil {
		test.Fatal(err)
	}
	if spec.CPUs != 1 || spec.Memory != "2G" {
		test.Fatalf("resources = %d/%q, want 1/2G", spec.CPUs, spec.Memory)
	}
	if len(spec.PublishPorts) != 2 {
		test.Fatalf("ports = %v, want 2 mappings", spec.PublishPorts)
	}
	if spec.PublishPorts[0].Host != 8080 || spec.PublishPorts[0].Guest != 8080 {
		test.Fatalf("port 0 = %+v, want 8080->8080", spec.PublishPorts[0])
	}
	if spec.PublishPorts[1].Host != 9000 || spec.PublishPorts[1].Guest != 3000 {
		test.Fatalf("port 1 = %+v, want 9000->3000", spec.PublishPorts[1])
	}
}

func TestSpecFromFlagsRejectsBadPort(test *testing.T) {
	_, err := specFromFlags(createFlags{name: "demo", osKey: "ubuntu", defaultName: "demo", ports: []string{"abc"}})
	platformErr, ok := err.(*output.Error)
	if !ok || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("err = %v, want exit-2 *output.Error", err)
	}
}

func TestParsePublishPorts(test *testing.T) {
	ports, err := parsePublishPorts([]string{"8080", "9000:3000", " "})
	if err != nil {
		test.Fatal(err)
	}
	if len(ports) != 2 {
		test.Fatalf("ports = %v, want 2 (blank skipped)", ports)
	}
	for _, bad := range []string{"0", "70000", "80:0", "x:1"} {
		if _, err := parsePublishPorts([]string{bad}); err == nil {
			test.Errorf("port %q should be rejected", bad)
		}
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
	if got := normalizeDefaultAgentCLI("omp", []string{"opencode", "omp"}); got != "omp" {
		test.Fatalf("valid default changed to %q", got)
	}
	if got := normalizeDefaultAgentCLI("opencode", []string{"codex", "gemini"}); got != "codex" {
		test.Fatalf("stale default normalized to %q, want codex", got)
	}
	if got := normalizeDefaultAgentCLI("opencode", nil); got != "" {
		test.Fatalf("empty agents default = %q, want empty", got)
	}
}

// --app-port <app>=<port> flows into project.Spec.AppPorts and is validated.
func TestSpecFromFlagsCarriesAppPorts(test *testing.T) {
	spec, err := specFromFlags(createFlags{
		name: "demo", osKey: "ubuntu", defaultName: "demo",
		apps: []string{"openwebui"}, appPorts: map[string]int{"openwebui": 8080},
	})
	if err != nil {
		test.Fatalf("specFromFlags: %v", err)
	}
	if spec.AppPorts["openwebui"] != 8080 {
		test.Errorf("spec.AppPorts[openwebui] = %d, want 8080", spec.AppPorts["openwebui"])
	}
}

func TestParseAppPortFlags(test *testing.T) {
	ports := parseAppPortFlags([]string{"openwebui=8080", "anythingllm=3001", "bad-no-eq", "x=notnum"})
	if ports["openwebui"] != 8080 || ports["anythingllm"] != 3001 {
		test.Errorf("parsed = %v, want openwebui=8080 anythingllm=3001", ports)
	}
	if len(ports) != 2 {
		test.Errorf("malformed entries must be skipped, got %v", ports)
	}
}

func TestValidateRejectsUnknownAppPort(test *testing.T) {
	if err := validateProvidedCreateFlags(createFlags{appPorts: map[string]int{"nope": 8080}}); err == nil {
		test.Error("an --app-port for an unknown app must be rejected")
	}
}

func TestValidateRejectsOutOfRangeAppPort(test *testing.T) {
	if err := validateProvidedCreateFlags(createFlags{appPorts: map[string]int{"openwebui": 70000}}); err == nil {
		test.Error("an out-of-range --app-port must be rejected")
	}
}

// --app-port accepts a dashboard-capable agent CLI (hermes), not just in-VM apps.
func TestValidateAcceptsHermesDashboardPort(test *testing.T) {
	if err := validateProvidedCreateFlags(createFlags{appPorts: map[string]int{"hermes": 9119}}); err != nil {
		test.Errorf("--app-port hermes=9119 must be accepted (hermes ships a dashboard): %v", err)
	}
	if err := validateProvidedCreateFlags(createFlags{appPorts: map[string]int{"hermes": 70000}}); err == nil {
		test.Error("an out-of-range hermes dashboard port must be rejected")
	}
}

func TestSpecFromFlagsCarriesHermesDashboardPort(test *testing.T) {
	spec, err := specFromFlags(createFlags{
		name: "demo", osKey: "ubuntu", defaultName: "demo",
		agents: []string{"opencode", "hermes"}, appPorts: map[string]int{"hermes": 9119},
	})
	if err != nil {
		test.Fatalf("specFromFlags: %v", err)
	}
	if spec.AppPorts["hermes"] != 9119 {
		test.Errorf("spec.AppPorts[hermes] = %d, want 9119", spec.AppPorts["hermes"])
	}
}
