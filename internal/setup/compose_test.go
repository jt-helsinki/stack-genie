package setup

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestServicesComposeYAML verifies the rendered compose covers every service-tier
// container with the right image/network/ports, and that the LiteLLM secrets are
// PASSTHROUGH (bare names, no values) so they never land in the file.
func TestServicesComposeYAML(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	raw, err := ServicesComposeYAML("127.0.0.1")
	if err != nil {
		test.Fatal(err)
	}
	out := string(raw)

	// Every service-tier container is present, with the shared network.
	for _, want := range []string{
		"aip-dns:", "aip-presidio-analyzer:", "aip-presidio-anonymizer:",
		"aip-litellm-db:", "aip-litellm:", "aip-headroom:", "aip-proxy:",
		"name: aip-net", "container_name: aip-litellm",
		"127.0.0.1:18787:80", // proxy publish
	} {
		if !strings.Contains(out, want) {
			test.Errorf("compose missing %q:\n%s", want, out)
		}
	}

	// Ollama is HOST-NATIVE — the reconcile runs no aip-ollama container, so the
	// debug compose artifact must NOT declare one (a containerized ollama here
	// would not match the running topology).
	for _, absent := range []string{"aip-ollama", "OLLAMA_MODELS", "ollama/ollama"} {
		if strings.Contains(out, absent) {
			test.Errorf("compose must not reference host-native Ollama (%q):\n%s", absent, out)
		}
	}

	// Secrets are PASSTHROUGH: the names appear, but never as KEY=VALUE.
	for _, secret := range []string{"UI_PASSWORD", "LITELLM_MASTER_KEY", "LITELLM_SALT_KEY"} {
		if !strings.Contains(out, secret) {
			test.Errorf("compose missing passthrough env %q", secret)
		}
		if strings.Contains(out, secret+"=") {
			test.Errorf("secret %q must be passthrough (no value in the file):\n%s", secret, out)
		}
	}

	// It must be valid YAML that round-trips to the compose shape.
	var parsed composeFile
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		test.Fatalf("rendered compose is not valid YAML: %v", err)
	}
	litellm, ok := parsed.Services["aip-litellm"]
	if !ok {
		test.Fatal("aip-litellm service missing after round-trip")
	}
	// LiteLLM waits on its db + both Presidio backends.
	for _, dep := range []string{"aip-litellm-db", "aip-presidio-analyzer", "aip-presidio-anonymizer"} {
		if !contains(litellm.DependsOn, dep) {
			test.Errorf("aip-litellm depends_on missing %q: %v", dep, litellm.DependsOn)
		}
	}
}

// TestWriteComposeFile writes the artifact to the platform dir and returns its path.
func TestWriteComposeFile(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	path, err := WriteComposeFile()
	if err != nil {
		test.Fatal(err)
	}
	if !strings.HasSuffix(path, "docker-compose.yaml") {
		test.Errorf("path = %q, want …/docker-compose.yaml", path)
	}
	if _, err := os.Stat(path); err != nil {
		test.Fatalf("compose file not written: %v", err)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
