package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

func TestValidModelRuntime(t *testing.T) {
	if !config.ValidModelRuntime(string(config.RuntimeOllama)) {
		t.Errorf("RuntimeOllama should be valid")
	}
	if !config.ValidModelRuntime(string(config.RuntimeDockerModelRunner)) {
		t.Errorf("RuntimeDockerModelRunner should be valid")
	}
	if config.ValidModelRuntime("bogus") {
		t.Errorf("bogus runtime should be invalid")
	}
	if config.ValidModelRuntime("") {
		t.Errorf("empty runtime should be invalid")
	}
	if want := 2; len(config.ModelRuntimes()) != want {
		t.Errorf("ModelRuntimes() = %d, want %d", len(config.ModelRuntimes()), want)
	}
}

func TestLoadModelRuntimesAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	choices, err := config.LoadModelRuntimes()
	if err != nil {
		t.Fatalf("LoadModelRuntimes() error = %v", err)
	}
	if choices == nil {
		t.Fatal("LoadModelRuntimes() returned nil map for absent file")
	}
	if len(choices) != 0 {
		t.Errorf("LoadModelRuntimes() = %v, want empty", choices)
	}
	if _, ok := config.ModelRuntimeFor("nope"); ok {
		t.Errorf("ModelRuntimeFor on absent file should report not-found")
	}
}

func TestModelRuntimeRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	choice := config.ModelRuntimeChoice{
		Alias:    "ollama/llama3",
		Model:    "llama3",
		Runtime:  config.RuntimeOllama,
		Endpoint: "http://aip-ollama:11434",
		Status:   "running",
	}
	if err := config.SetModelRuntime(choice); err != nil {
		t.Fatalf("SetModelRuntime() error = %v", err)
	}

	got, ok := config.ModelRuntimeFor("ollama/llama3")
	if !ok {
		t.Fatal("ModelRuntimeFor() = not found after set")
	}
	if got != choice {
		t.Errorf("ModelRuntimeFor() = %+v, want %+v", got, choice)
	}

	// Overwrite with a different runtime.
	choice.Runtime = config.RuntimeDockerModelRunner
	choice.Endpoint = "http://model-runner:12434"
	if err := config.SetModelRuntime(choice); err != nil {
		t.Fatalf("SetModelRuntime() overwrite error = %v", err)
	}
	got, _ = config.ModelRuntimeFor("ollama/llama3")
	if got.Runtime != config.RuntimeDockerModelRunner {
		t.Errorf("after overwrite Runtime = %q, want %q", got.Runtime, config.RuntimeDockerModelRunner)
	}

	// Delete.
	if err := config.DeleteModelRuntime("ollama/llama3"); err != nil {
		t.Fatalf("DeleteModelRuntime() error = %v", err)
	}
	if _, ok := config.ModelRuntimeFor("ollama/llama3"); ok {
		t.Errorf("ModelRuntimeFor() still found after delete")
	}
	// Deleting an absent alias is a no-op, not an error.
	if err := config.DeleteModelRuntime("ollama/llama3"); err != nil {
		t.Errorf("DeleteModelRuntime() on absent alias error = %v", err)
	}
}

func TestSetModelRuntimeValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{Runtime: config.RuntimeOllama}); err == nil {
		t.Errorf("SetModelRuntime with empty alias should error")
	}
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{Alias: "x", Runtime: "bogus"}); err == nil {
		t.Errorf("SetModelRuntime with invalid runtime should error")
	}
}

func TestModelRuntimesPathIsYAML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := config.ModelRuntimesPath()
	if err != nil {
		t.Fatalf("ModelRuntimesPath() error = %v", err)
	}
	if filepath.Ext(path) != ".yaml" {
		t.Errorf("ModelRuntimesPath() = %q, want .yaml extension", path)
	}
	if base := filepath.Base(path); base != "model-runtimes.yaml" {
		t.Errorf("ModelRuntimesPath() base = %q, want model-runtimes.yaml", base)
	}
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{Alias: "a", Runtime: config.RuntimeOllama}); err != nil {
		t.Fatalf("SetModelRuntime() error = %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("expected %q to exist after write: %v", path, statErr)
	}
}

func TestLoadModelRuntimesRejectsUnknownFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := config.ModelRuntimesPath()
	if err != nil {
		t.Fatalf("ModelRuntimesPath() error = %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		t.Fatalf("MkdirAll error = %v", mkErr)
	}
	bad := "schema_version: 1\nchoices: {}\nbogus_field: true\n"
	if writeErr := os.WriteFile(path, []byte(bad), 0o644); writeErr != nil {
		t.Fatalf("WriteFile error = %v", writeErr)
	}
	if _, loadErr := config.LoadModelRuntimes(); loadErr == nil {
		t.Errorf("LoadModelRuntimes() should reject unknown fields")
	}
}
