package apps

import (
	"errors"
	"strings"
	"testing"
)

func TestAllAndKeys(test *testing.T) {
	all := All()
	if len(all) != 2 {
		test.Fatalf("All() = %d apps, want 2", len(all))
	}
	keys := Keys()
	if len(keys) != 2 {
		test.Fatalf("Keys() = %v, want 2 keys", keys)
	}
	// Keys is sorted.
	if keys[0] != "anythingllm" || keys[1] != "openwebui" {
		test.Fatalf("Keys() = %v, want sorted [anythingllm openwebui]", keys)
	}
}

func TestLookupAndValidate(test *testing.T) {
	manifest, ok := Lookup("openwebui")
	if !ok {
		test.Fatal("Lookup(openwebui) not found")
	}
	if manifest.ContainerPort != 8080 || manifest.DataDir != "/app/backend/data" {
		test.Fatalf("openwebui manifest port/dir = %d/%q, want 8080 / /app/backend/data", manifest.ContainerPort, manifest.DataDir)
	}
	if _, ok := Lookup("nope"); ok {
		test.Fatal("Lookup(nope) should not be found")
	}
	if err := Validate("openwebui"); err != nil {
		test.Fatalf("Validate(openwebui) = %v, want nil", err)
	}
	err := Validate("nope")
	if !errors.Is(err, ErrUnknownApp) {
		test.Fatalf("Validate(nope) = %v, want ErrUnknownApp", err)
	}
}

func TestOpenWebUIEnvPointsAtGateway(test *testing.T) {
	manifest, _ := Lookup("openwebui")
	env := manifest.Env("http://host.microsandbox.internal:18787/v1", "sk-key", "gemma4")
	if env["OPENAI_API_BASE_URL"] != "http://host.microsandbox.internal:18787/v1" {
		test.Fatalf("OPENAI_API_BASE_URL = %q", env["OPENAI_API_BASE_URL"])
	}
	if env["OPENAI_API_KEY"] != "sk-key" {
		test.Fatalf("OPENAI_API_KEY = %q", env["OPENAI_API_KEY"])
	}
	if env["ENABLE_OLLAMA_API"] != "false" {
		test.Fatalf("ENABLE_OLLAMA_API = %q, want false", env["ENABLE_OLLAMA_API"])
	}
	if env["WEBUI_AUTH"] != "false" {
		test.Fatalf("WEBUI_AUTH = %q, want false (single-user in-VM)", env["WEBUI_AUTH"])
	}
}

func TestAnythingLLMEnvPointsAtGateway(test *testing.T) {
	manifest, _ := Lookup("anythingllm")
	env := manifest.Env("http://host.microsandbox.internal:18787/v1", "sk-key", "gemma4")
	if env["LLM_PROVIDER"] != "generic-openai" {
		test.Fatalf("LLM_PROVIDER = %q, want generic-openai", env["LLM_PROVIDER"])
	}
	if env["GENERIC_OPEN_AI_BASE_PATH"] != "http://host.microsandbox.internal:18787/v1" {
		test.Fatalf("GENERIC_OPEN_AI_BASE_PATH = %q (must be the gateway)", env["GENERIC_OPEN_AI_BASE_PATH"])
	}
	if env["GENERIC_OPEN_AI_API_KEY"] != "sk-key" {
		test.Fatalf("GENERIC_OPEN_AI_API_KEY = %q", env["GENERIC_OPEN_AI_API_KEY"])
	}
	if env["GENERIC_OPEN_AI_MODEL_PREF"] != "gemma4" {
		test.Fatalf("GENERIC_OPEN_AI_MODEL_PREF = %q, want gemma4", env["GENERIC_OPEN_AI_MODEL_PREF"])
	}
	if env["STORAGE_DIR"] != "/app/server/storage" {
		test.Fatalf("STORAGE_DIR = %q, want /app/server/storage", env["STORAGE_DIR"])
	}
}

func TestContainerName(test *testing.T) {
	manifest, _ := Lookup("openwebui")
	if got := manifest.ContainerName(); got != "aip-app-openwebui" {
		test.Fatalf("ContainerName = %q, want aip-app-openwebui", got)
	}
}

func TestEnvKeysSorted(test *testing.T) {
	manifest, _ := Lookup("anythingllm")
	keys := manifest.EnvKeys()
	for index := 1; index < len(keys); index++ {
		if keys[index-1] > keys[index] {
			test.Fatalf("EnvKeys not sorted: %v", keys)
		}
	}
}

func TestRunArgsShape(test *testing.T) {
	manifest, _ := Lookup("openwebui")
	argv := runArgs(manifest, 21000, "http://gw/v1", "sk-key", "gemma4")
	joined := strings.Join(argv, " ")
	want := []string{
		"nerdctl run -d",
		"--name aip-app-openwebui",
		"--restart always",
		"-p 21000:8080",
		"-v /persist/apps/openwebui:/app/backend/data",
		"-v /workspace:/workspace",
		"--memory 2g",
		"-e OPENAI_API_BASE_URL=http://gw/v1",
		"ghcr.io/open-webui/open-webui:latest",
	}
	for _, fragment := range want {
		if !strings.Contains(joined, fragment) {
			test.Fatalf("runArgs missing %q\ngot: %s", fragment, joined)
		}
	}
	// Image is last.
	if argv[len(argv)-1] != manifest.Image {
		test.Fatalf("last arg = %q, want image %q", argv[len(argv)-1], manifest.Image)
	}
}
