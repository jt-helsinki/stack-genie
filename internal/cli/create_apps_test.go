package cli

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
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

func TestCappedResourcesCapsDefaultAtHost(test *testing.T) {
	// Small host: the 4-cpu / 8G defaults are capped down to the host's 2 cpu and the
	// USABLE memory (4096 MiB host → reserve max(2048, 4096/4=1024)=2048 → 2048 usable).
	cpus, memory := cappedResources(0, "", 2, 4096, true)
	if cpus != 2 {
		test.Errorf("cpus = %d, want capped to host 2", cpus)
	}
	if memory != "2048M" {
		test.Errorf("memory = %q, want capped to usable 2048M", memory)
	}
	// Large host: defaults fit, so they pass through unchanged.
	cpus, memory = cappedResources(0, "", 16, 32768, true)
	if cpus != config.Default().Workspace.CPULimit || memory != config.Default().Workspace.MemoryLimit {
		test.Errorf("large host: got %d/%q, want defaults %d/%q", cpus, memory, config.Default().Workspace.CPULimit, config.Default().Workspace.MemoryLimit)
	}
	// Explicit values are preserved (capping only fills unset).
	cpus, memory = cappedResources(1, "2G", 2, 4096, true)
	if cpus != 1 || memory != "2G" {
		test.Errorf("explicit values changed: got %d/%q, want 1/2G", cpus, memory)
	}
	// Unknown host RAM: the memory default is left as-is (no cap when we can't tell).
	if _, memory = cappedResources(0, "", 2, 0, false); memory != config.Default().Workspace.MemoryLimit {
		test.Errorf("unknown host RAM: memory = %q, want default unchanged", memory)
	}
}

func TestValidateResourcesWithinHostRejectsOverCommit(test *testing.T) {
	// A clearly-impossible CPU request must be rejected (host has far fewer).
	if err := validateResourcesWithinHost(1<<20, ""); err == nil {
		test.Error("an over-host CPU request must be rejected")
	}
	// A negative CPU count is invalid.
	if err := validateResourcesWithinHost(-1, ""); err == nil {
		test.Error("a negative CPU count must be rejected")
	}
	// A reasonable request (1 CPU, 1 GB) passes on any host.
	if err := validateResourcesWithinHost(1, "1"); err != nil {
		test.Errorf("1 CPU / 1 GB should be valid: %v", err)
	}
	// Below the boot minimum is rejected (also guards the unit footgun).
	if err := validateResourcesWithinHost(1, "256M"); err == nil {
		test.Error("256M (below the 512 MiB minimum) must be rejected")
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
