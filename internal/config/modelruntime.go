package config

// modelruntime.go records the user's chosen SERVING RUNTIME per model (host-native
// vLLM — the sole local-inference backend) in a small MACHINE-WIDE store at
// ~/.ai-platform/config/model-runtimes.yaml.
//
// This is a THIN SELECTION RECORD, NOT a parallel model registry: it remembers
// only HOW each model should be served, keyed by its gateway alias. The source of
// truth for WHAT is served remains LiteLLM's live DB-backed model set (surfaced
// via litellm.KeyManager.ListModels) — this store never dictates the served set,
// it only annotates it. Because models are global (machine-wide), so is this
// store; it lives under ~/.ai-platform and is therefore removed wholesale by
// `ai uninstall --purge` (RemoveAll of ~/.ai-platform), needing no extra teardown.
//
// It mirrors the internal/versions global-store pattern exactly: a Path() derived
// from paths.ConfigDir, atomic writes via internal/conffile, and unknown-field-
// rejecting reads.

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// ModelRuntime names a serving backend for a model.
type ModelRuntime string

const (
	// RuntimeVLLM serves the model through host-native vLLM (a per-model
	// `vllm serve` OpenAI endpoint). It is the ONLY local-inference runtime. A
	// legacy non-vLLM entry recorded by an older build is ignored on
	// load (LoadModelRuntimes drops it) rather than crashing.
	RuntimeVLLM ModelRuntime = "vllm"
)

// ModelRuntimeSchemaVersion is stamped on config/model-runtimes.yaml.
const ModelRuntimeSchemaVersion = 1

// ModelRuntimes returns the selectable serving runtimes, in a stable order
// suitable for a UI picker. vLLM is the only local-inference runtime.
func ModelRuntimes() []ModelRuntime {
	return []ModelRuntime{RuntimeVLLM}
}

// ValidModelRuntime reports whether value is a recognised serving runtime.
func ValidModelRuntime(value string) bool {
	return ModelRuntime(value) == RuntimeVLLM
}

// ModelRuntimeChoice is a single selection record: how one served model (keyed by
// its gateway Alias) should be served. It is NOT a registry entry — LiteLLM's DB
// remains the source of truth for what is served; these fields only annotate an
// already-served model with the user's chosen runtime and, for convenience, the
// endpoint/status observed when the choice was made.
type ModelRuntimeChoice struct {
	// Alias is the gateway model alias — the store key.
	Alias string `json:"alias" yaml:"alias"`
	// Model is the underlying model name (e.g. a Hugging Face model id).
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// Runtime is the chosen serving backend.
	Runtime ModelRuntime `json:"runtime" yaml:"runtime"`
	// Endpoint is the serving endpoint observed for the choice (advisory).
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	// Status is the last-observed status for the choice (advisory).
	Status string `json:"status,omitempty" yaml:"status,omitempty"`
	// GPUMemoryUtilization, when > 0, is the `--gpu-memory-utilization` fraction
	// (0-1) the user pinned for this model's `vllm serve` — recorded so a later
	// restart (`ai setup` / `ai services restart vllm`) relaunches it with the same
	// value instead of falling back to vLLM's own default (0.9, sized off TOTAL
	// device memory rather than the model's weight size). Zero means unset.
	GPUMemoryUtilization float64 `json:"gpu_memory_utilization,omitempty" yaml:"gpu_memory_utilization,omitempty"`
	// MaxModelLen, when > 0, is the `--max-model-len` (tokens) the user pinned for
	// this model, recorded for the same restart-preserving reason. Zero means unset
	// (the model's own default context length).
	MaxModelLen int `json:"max_model_len,omitempty" yaml:"max_model_len,omitempty"`
	// Disabled marks a model that should NOT be auto-started by `ai setup` / `ai
	// services start vllm` (ensureVLLMServers starts every recorded choice
	// unconditionally otherwise) — e.g. to keep a second local model pulled and
	// registered without it fighting another for GPU/unified memory (see
	// vllmGPUMemoryUtilizationBudgetWarning). The weights and this record are
	// untouched; `ai models enable` starts it again with no re-download. Set via
	// `ai models disable`/`enable`, never by `pull`/`configure`.
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
}

// ModelRuntimesFile is config/model-runtimes.yaml.
type ModelRuntimesFile struct {
	SchemaVersion int                           `json:"schema_version" yaml:"schema_version"`
	Choices       map[string]ModelRuntimeChoice `json:"choices" yaml:"choices"`
}

// ModelRuntimesPath returns config/model-runtimes.yaml.
func ModelRuntimesPath() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "model-runtimes.yaml"), nil
}

// LoadModelRuntimes reads the runtime-choice store, keyed by alias. An absent file
// is not an error — it returns an empty (non-nil) map.
func LoadModelRuntimes() (map[string]ModelRuntimeChoice, error) {
	path, err := ModelRuntimesPath()
	if err != nil {
		return nil, err
	}
	var file ModelRuntimesFile
	if readErr := conffile.Read(path, &file); readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return map[string]ModelRuntimeChoice{}, nil
		}
		return nil, readErr
	}
	if file.Choices == nil {
		return map[string]ModelRuntimeChoice{}, nil
	}
	// Drop any legacy non-vLLM entry recorded by an older build:
	// vLLM is now the sole local runtime, so such a record is stale — ignore it
	// rather than surface an unusable runtime to callers.
	for alias, choice := range file.Choices {
		if !ValidModelRuntime(string(choice.Runtime)) {
			delete(file.Choices, alias)
		}
	}
	return file.Choices, nil
}

// SetModelRuntime records (or replaces) the serving-runtime choice for
// choice.Alias, load-modify-atomic-save. choice.Alias must be non-empty and
// choice.Runtime must be a recognised runtime.
func SetModelRuntime(choice ModelRuntimeChoice) error {
	if choice.Alias == "" {
		return errors.New("model runtime choice: empty alias")
	}
	if !ValidModelRuntime(string(choice.Runtime)) {
		return errors.New("model runtime choice: invalid runtime")
	}
	choices, err := LoadModelRuntimes()
	if err != nil {
		return err
	}
	choices[choice.Alias] = choice
	return writeModelRuntimes(choices)
}

// DeleteModelRuntime removes the choice for alias, load-modify-atomic-save. It is
// a no-op (no error) when the alias is absent.
func DeleteModelRuntime(alias string) error {
	choices, err := LoadModelRuntimes()
	if err != nil {
		return err
	}
	if _, ok := choices[alias]; !ok {
		return nil
	}
	delete(choices, alias)
	return writeModelRuntimes(choices)
}

// ModelRuntimeFor returns the recorded choice for alias, and whether one exists.
func ModelRuntimeFor(alias string) (ModelRuntimeChoice, bool) {
	choices, err := LoadModelRuntimes()
	if err != nil {
		return ModelRuntimeChoice{}, false
	}
	choice, ok := choices[alias]
	return choice, ok
}

// writeModelRuntimes atomically persists the choice map.
func writeModelRuntimes(choices map[string]ModelRuntimeChoice) error {
	path, err := ModelRuntimesPath()
	if err != nil {
		return err
	}
	return conffile.WriteAtomic(path, ModelRuntimesFile{
		SchemaVersion: ModelRuntimeSchemaVersion,
		Choices:       choices,
	})
}
