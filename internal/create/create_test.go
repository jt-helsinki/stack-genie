package create

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
)

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
	if err := ValidateResourcesWithinHost(1<<20, ""); err == nil {
		test.Error("an over-host CPU request must be rejected")
	}
	// A negative CPU count is invalid.
	if err := ValidateResourcesWithinHost(-1, ""); err == nil {
		test.Error("a negative CPU count must be rejected")
	}
	// A reasonable request (1 CPU, 1 GB) passes on any host.
	if err := ValidateResourcesWithinHost(1, "1"); err != nil {
		test.Errorf("1 CPU / 1 GB should be valid: %v", err)
	}
	// Below the boot minimum is rejected (also guards the unit footgun).
	if err := ValidateResourcesWithinHost(1, "256M"); err == nil {
		test.Error("256M (below the 512 MiB minimum) must be rejected")
	}
}

func TestModelInstalled(test *testing.T) {
	installed := []ollama.Model{{Name: "llama3.2:latest"}, {Name: "qwen2.5-coder:7b"}}
	if !modelInstalled(installed, "qwen2.5-coder:7b") {
		test.Error("exact ref should be installed")
	}
	if !modelInstalled(installed, "llama3.2") {
		test.Error("bare name should match name:latest")
	}
	if modelInstalled(installed, "mistral") {
		test.Error("absent model should not be installed")
	}
}

// pullGraphifyModelIfAbsent skips the pull entirely when the model is already
// installed (no Pull call, no warning).
func TestPullGraphifyModelSkipsWhenInstalled(test *testing.T) {
	fake := &ollama.Fake{ListModels: []ollama.Model{{Name: "llama3.2:latest"}}}
	restore := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restore }()

	if warnings := pullGraphifyModelIfAbsent("llama3.2"); len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none (already installed)", warnings)
	}
	if fake.PulledNames != nil {
		test.Fatalf("Pull should not run for an installed model; pulled %v", fake.PulledNames)
	}
}

// A blank model ref is a no-op (Graphify has no configured model).
func TestPullGraphifyModelBlankNoOp(test *testing.T) {
	fake := &ollama.Fake{}
	restore := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restore }()

	if warnings := pullGraphifyModelIfAbsent("  "); warnings != nil {
		test.Fatalf("blank ref should be a no-op, got %v", warnings)
	}
	if fake.PulledNames != nil {
		test.Fatalf("blank ref must not pull; pulled %v", fake.PulledNames)
	}
}

// An absent model is pulled and then registered in the gateway.
func TestPullGraphifyModelPullsAndRegisters(test *testing.T) {
	fake := &ollama.Fake{}
	restoreClient := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restoreClient }()

	registrar := &fakeModelRegistrar{}
	restoreReg := newRegistrar
	newRegistrar = func() modelRegistrar { return registrar }
	defer func() { newRegistrar = restoreReg }()

	if warnings := pullGraphifyModelIfAbsent("qwen2.5-coder:7b"); len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none", warnings)
	}
	if len(fake.PulledNames) != 1 || fake.PulledNames[0] != "qwen2.5-coder:7b" {
		test.Fatalf("Pulled = %v, want [qwen2.5-coder:7b]", fake.PulledNames)
	}
	if len(registrar.registered) != 1 || registrar.registered[0] != "qwen2.5-coder:7b" {
		test.Fatalf("registered = %v, want [qwen2.5-coder:7b]", registrar.registered)
	}
}

// fakeModelRegistrar records gateway registrations for the pull test.
type fakeModelRegistrar struct {
	registered []string
}

func (fake *fakeModelRegistrar) RegisterOllamaModel(name string) error {
	fake.registered = append(fake.registered, name)
	return nil
}
