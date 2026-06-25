// Package apps declares and orchestrates the opt-in AI applications that run as
// rootful nerdctl/containerd OCI containers INSIDE a workspace microVM (the
// in-VM container model, arch §7). Each app is described by a declarative
// Manifest (image, port, data dir, gateway env); the per-workspace lifecycle
// (install/remove/update/start/stop/restart/list) is driven by Manager over an
// injected in-VM exec surface so the host-side logic is unit-tested with fakes.
//
// The model: an app is a container the workspace runs locally, pointed at the
// SAME model gateway the agent CLIs use (http://host.microsandbox.internal:18787/v1
// via the workspace's scoped LiteLLM virtual key), with its data persisted on the
// workspace overlay so it survives restarts, and reachable from the host on a
// unique published port (the host:<port> → VM:<port> → container:<containerPort>
// chain; see the package doc on PublishedPort).
package apps

import (
	"fmt"
	"sort"
)

// Image pinning. The in-VM apps pin their OCI images by image+tag (no digest —
// digests are platform/arch specific), the same way the service tier pins its
// images in versions.yaml (internal/services VersionPins). These pins live with
// the manifest because the apps run inside the workspace, not in the service
// tier, so they are not part of the host versions.yaml.
const (
	openWebUIImage       = "ghcr.io/open-webui/open-webui:latest"
	anythingLLMImage     = "mintplexlabs/anythingllm:latest"
	defaultAppMemory     = "2g"
	openWebUIPortGuest   = 8080
	anythingLLMPortGuest = 3001
)

// Manifest is the declarative description of one in-VM app. Env is built per
// workspace by EnvFor so it can bake the resolved gateway URL, the workspace's
// scoped virtual key, and the default model.
type Manifest struct {
	// Key is the stable identifier used in state, CLI args, and the container
	// name (aip-app-<key>). Lowercase, no spaces.
	Key string
	// Name is the human-facing label (shown in `ai apps list` / the TUI).
	Name string
	// Image is the pinned OCI image reference (image:tag).
	Image string
	// ContainerPort is the port the app's web UI listens on inside the container.
	ContainerPort int
	// DataDir is the in-container directory that holds the app's persistent state.
	// It is bind-mounted to a per-app directory on the workspace overlay so the
	// data survives microVM restart/recreation.
	DataDir string
	// MountWorkspace controls whether the project source (/workspace in the VM) is
	// mounted into the container so the app can read the user's code.
	MountWorkspace bool
	// Memory is the container memory limit (nerdctl --memory value).
	Memory string
	// envFor builds the gateway-pointing environment for this app given the
	// resolved gateway base URL (".../v1"), the workspace's scoped LiteLLM virtual
	// key, and the default model handle.
	envFor func(gatewayURL, apiKey, defaultModel string) map[string]string
}

// Env returns the app's container environment for a workspace, pointing it at the
// model gateway with the workspace's scoped virtual key and the default model.
func (manifest Manifest) Env(gatewayURL, apiKey, defaultModel string) map[string]string {
	return manifest.envFor(gatewayURL, apiKey, defaultModel)
}

// EnvKeys returns the manifest's env keys, sorted, for deterministic argv and
// for tests that assert on the set of variables.
func (manifest Manifest) EnvKeys() []string {
	env := manifest.Env("", "", "")
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ContainerName is the nerdctl container name for the app inside the workspace
// microVM. One app per workspace, so the key alone is unique within a VM.
func (manifest Manifest) ContainerName() string {
	return "aip-app-" + manifest.Key
}

// catalogue is the ordered set of supported in-VM apps.
var catalogue = []Manifest{
	{
		Key:            "openwebui",
		Name:           "Open WebUI",
		Image:          openWebUIImage,
		ContainerPort:  openWebUIPortGuest,
		DataDir:        "/app/backend/data",
		MountWorkspace: true,
		Memory:         defaultAppMemory,
		// Open WebUI talks to an OpenAI-compatible endpoint via OPENAI_API_BASE_URL
		// + OPENAI_API_KEY; ENABLE_OLLAMA_API=false keeps it routed only through the
		// gateway, and WEBUI_AUTH=false runs it as a single-user per-workspace
		// instance (no login wall) — verified against the platform's own
		// aip-open-webui service launch (internal/setup/setup_real.go).
		envFor: func(gatewayURL, apiKey, _ string) map[string]string {
			return map[string]string{
				"OPENAI_API_BASE_URL": gatewayURL,
				"OPENAI_API_KEY":      apiKey,
				"ENABLE_OLLAMA_API":   "false",
				"WEBUI_AUTH":          "false",
			}
		},
	},
	{
		Key:            "anythingllm",
		Name:           "AnythingLLM",
		Image:          anythingLLMImage,
		ContainerPort:  anythingLLMPortGuest,
		DataDir:        "/app/server/storage",
		MountWorkspace: true,
		Memory:         defaultAppMemory,
		// AnythingLLM's GENERIC OpenAI provider (LLM_PROVIDER=generic-openai) points
		// at a base path + key + model with GENERIC_OPEN_AI_* — verified against the
		// project's docker/.env.example. STORAGE_DIR matches the persisted DataDir.
		envFor: func(gatewayURL, apiKey, defaultModel string) map[string]string {
			return map[string]string{
				"STORAGE_DIR":                       "/app/server/storage",
				"LLM_PROVIDER":                      "generic-openai",
				"GENERIC_OPEN_AI_BASE_PATH":         gatewayURL,
				"GENERIC_OPEN_AI_API_KEY":           apiKey,
				"GENERIC_OPEN_AI_MODEL_PREF":        defaultModel,
				"GENERIC_OPEN_AI_MODEL_TOKEN_LIMIT": "4096",
			}
		},
	},
}

// All returns the supported app manifests in display order.
func All() []Manifest {
	out := make([]Manifest, len(catalogue))
	copy(out, catalogue)
	return out
}

// Keys returns the supported app keys, sorted (for validation messages and
// shell completion).
func Keys() []string {
	keys := make([]string, 0, len(catalogue))
	for _, manifest := range catalogue {
		keys = append(keys, manifest.Key)
	}
	sort.Strings(keys)
	return keys
}

// Lookup returns the manifest for an app key and whether it is known.
func Lookup(key string) (Manifest, bool) {
	for _, manifest := range catalogue {
		if manifest.Key == key {
			return manifest, true
		}
	}
	return Manifest{}, false
}

// ErrUnknownApp is returned when an app key is not in the catalogue (→ exit 2).
var ErrUnknownApp = fmt.Errorf("unknown app")

// Validate returns ErrUnknownApp (wrapped with the offending key + valid set)
// when key is not a known app.
func Validate(key string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("%w: %q (one of: %v)", ErrUnknownApp, key, Keys())
	}
	return nil
}
