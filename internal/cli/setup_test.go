package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/uihosts"
)

// parseOptionalFlag turns the --optional CSV (the non-interactive setup contract)
// into a validated service list: "none" disables all. There are currently NO
// optional host services (Open WebUI moved to a per-workspace in-VM app and
// Odysseus was removed), so the optional universe is empty and ANY named value is
// exit 2.
func TestParseOptionalFlag(test *testing.T) {
	valid := []struct {
		in   string
		want []string
	}{
		{"none", []string{}},
		{"NONE", []string{}},
	}
	for _, testCase := range valid {
		got, err := parseOptionalFlag(testCase.in)
		if err != nil {
			test.Errorf("parseOptionalFlag(%q) unexpected error: %v", testCase.in, err)
			continue
		}
		if !slices.Equal(got, testCase.want) {
			test.Errorf("parseOptionalFlag(%q) = %v, want %v", testCase.in, got, testCase.want)
		}
	}

	// Any named value is rejected — there are no optional host services.
	for _, bad := range []string{"bogus", "open-webui", "odysseus", "llm-guard"} {
		_, err := parseOptionalFlag(bad)
		var platformErr *output.Error
		if !errors.As(err, &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Errorf("parseOptionalFlag(%q) = %v, want exit %d", bad, err, output.ExitInvalidInput)
		}
	}
}

// optionalServicesFromFlag is the non-interactive (--json / no-TTY) resolution:
// an empty flag leaves the set unspecified (set=false, keep persisted/default);
// "none" makes an explicit empty choice (set=true). Any named value is exit 2
// (there are no optional host services).
func TestOptionalServicesFromFlag(test *testing.T) {
	if set, services, err := optionalServicesFromFlag(""); set || services != nil || err != nil {
		test.Errorf("empty flag = (set=%v, %v, %v), want (false, nil, nil) — unspecified", set, services, err)
	}
	if set, services, err := optionalServicesFromFlag("none"); !set || len(services) != 0 || err != nil {
		test.Errorf("\"none\" = (set=%v, %v, %v), want (true, [], nil)", set, services, err)
	}
	if _, _, err := optionalServicesFromFlag("bogus"); err == nil {
		test.Error("unknown --optional name must error (exit 2)")
	}
}

// The server-hostname seed defaults to localhost when nothing is persisted, and
// reuses the configured domain when set.
func TestDefaultServerDomain(test *testing.T) {
	if got := defaultServerDomain(nil); got != "localhost" {
		test.Errorf("no persisted info: got %q, want localhost", got)
	}
	if got := defaultServerDomain(&runtime.Info{}); got != "localhost" {
		test.Errorf("empty persisted domain: got %q, want localhost", got)
	}
	if got := defaultServerDomain(&runtime.Info{Domain: "aip.example.com"}); got != "aip.example.com" {
		test.Errorf("persisted domain: got %q, want aip.example.com", got)
	}
}

// The server hostname must pass the SHARED `ai domain` hostname validator
// (validateDomain) — including the localhost default (a bare label is valid).
func TestServerHostnameUsesSharedDomainValidator(test *testing.T) {
	if err := validateDomain("localhost"); err != nil {
		test.Fatalf("localhost (the server default) must validate: %v", err)
	}
	if err := validateDomain("aip.example.com"); err != nil {
		test.Fatalf("a real hostname must validate: %v", err)
	}
	if err := validateDomain("http://aip.example.com"); err == nil {
		test.Fatal("a URL must be rejected by the domain validator")
	}
}

// Under --json / no TTY only the server role carries a hostname; standalone/client
// leave it empty so Run's load-modify-save preserves or defaults the domain.
func TestNonInteractiveServerDomain(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if got := nonInteractiveServerDomain(runtime.RoleStandalone); got != "" {
		test.Errorf("standalone: got %q, want empty", got)
	}
	if got := nonInteractiveServerDomain(runtime.RoleClient); got != "" {
		test.Errorf("client: got %q, want empty", got)
	}
	// Server with no persisted runtime.yaml → empty (Run defaults it).
	if got := nonInteractiveServerDomain(runtime.RoleServer); got != "" {
		test.Errorf("server, no persisted domain: got %q, want empty", got)
	}
	// Server reuses a persisted domain.
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Domain: "kept.example.com"}); err != nil {
		test.Fatal(err)
	}
	if got := nonInteractiveServerDomain(runtime.RoleServer); got != "kept.example.com" {
		test.Errorf("server, persisted domain: got %q, want kept.example.com", got)
	}
}

// When host-native Ollama is healthy AND vLLM is installed there is no local-inference
// guidance to print.
func TestLocalInferenceGuidanceBothHealthy(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "ollama", Mode: "host", Healthy: true},
	}
	if lines := localInferenceGuidanceLines("darwin", statuses, "/models", true); len(lines) != 0 {
		test.Fatalf("healthy: want no guidance, got %v", lines)
	}
}

// vLLM being absent adds an OPTIONAL, actionable install note (never a setup failure),
// even when the required Ollama backend is healthy.
func TestLocalInferenceGuidanceVLLMOptionalNote(test *testing.T) {
	statuses := []setup.ServiceStatus{{Name: "ollama", Mode: "host", Healthy: true}}
	joined := strings.Join(localInferenceGuidanceLines("darwin", statuses, "/models", false), "\n")
	for _, want := range []string{"vLLM is an OPTIONAL local runtime", "Metal plugin"} {
		if !strings.Contains(joined, want) {
			test.Fatalf("vLLM-absent guidance missing %q:\n%s", want, joined)
		}
	}
	// Linux emits the CUDA/pip guidance instead.
	linux := strings.Join(localInferenceGuidanceLines("linux", statuses, "/x", false), "\n")
	if !strings.Contains(linux, "NVIDIA GPU") {
		test.Fatalf("linux vLLM-absent guidance missing CUDA note:\n%s", linux)
	}
}

// A down host-native Ollama surfaces the per-OS install/start block plus the
// required OLLAMA_CONTEXT_LENGTH + OLLAMA_MODELS environment and a re-run hint.
func TestLocalInferenceGuidanceOllamaDown(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "ollama", Mode: "host", Healthy: false, State: "stopped"},
	}
	joined := strings.Join(localInferenceGuidanceLines("darwin", statuses, "/vol/models/ollama", true), "\n")
	for _, want := range []string{
		"host-native Ollama is not reachable",
		"brew install ollama",
		"OLLAMA_CONTEXT_LENGTH=16384",
		"OLLAMA_MODELS=/vol/models/ollama",
		"ai setup",
	} {
		if !strings.Contains(joined, want) {
			test.Fatalf("Ollama-down guidance missing %q:\n%s", want, joined)
		}
	}
	// Linux variant emits the install.sh guidance.
	linux := strings.Join(localInferenceGuidanceLines("linux", statuses, "/x", true), "\n")
	if !strings.Contains(linux, "install.sh") {
		test.Fatalf("linux Ollama-down guidance missing install.sh:\n%s", linux)
	}
}

// printLocalInferenceGuidance stays silent under --json (clean envelope on stdout)
// and prints via the injectable status seam otherwise.
func TestPrintLocalInferenceGuidanceJSONSilent(test *testing.T) {
	restore := localInferenceStatusFn
	defer func() { localInferenceStatusFn = restore }()
	localInferenceStatusFn = func() ([]setup.ServiceStatus, error) {
		return []setup.ServiceStatus{{Name: "ollama", Mode: "host", Healthy: false, State: "stopped"}}, nil
	}

	var jsonErr bytes.Buffer
	printLocalInferenceGuidance(&output.Emitter{Out: &bytes.Buffer{}, Err: &jsonErr, JSON: true})
	if jsonErr.Len() != 0 {
		test.Fatalf("--json: guidance must be silent, got:\n%s", jsonErr.String())
	}

	var humanErr bytes.Buffer
	printLocalInferenceGuidance(&output.Emitter{Out: &bytes.Buffer{}, Err: &humanErr})
	if !strings.Contains(humanErr.String(), "host-native Ollama is not reachable") {
		test.Fatalf("human run: expected the Ollama guidance, got:\n%s", humanErr.String())
	}
}

// Server mode produces the per-UI credential guide (the post-setup admin-access
// instructions): the LiteLLM admin UI URL plus how to set/rotate its password.
// litellm is now the ONLY host UI (Open WebUI moved in-VM; Odysseus was removed).
func TestServerModeProducesCredentialsGuide(test *testing.T) {
	guide := uihosts.ServerCredentialsGuide("aip.example.com")
	if !strings.Contains(guide, "ai litellm password") {
		test.Fatalf("server credentials guide must mention `ai litellm password`:\n%s", guide)
	}
	if !strings.Contains(guide, "http://litellm.aip.example.com:18787/ui") {
		test.Fatalf("server credentials guide must list the LiteLLM admin UI:\n%s", guide)
	}
	// No chat./odysseus. UIs anymore.
	if strings.Contains(guide, "chat.aip.example.com") || strings.Contains(guide, "odysseus.aip.example.com") {
		test.Fatalf("server credentials guide must not list chat/odysseus UIs:\n%s", guide)
	}
}
