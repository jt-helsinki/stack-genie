package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These full-stack [S1] tests need a provisioned host (real microVM + ClawPatrol
// + LiteLLM), so they are gated by hardwareAvailable() (AIP_HARDWARE_TESTS=1).
// Off-hardware they skip; on-hardware they are the falsifiable checklist for the
// credential, isolation, and egress guarantees.

// innerExit returns the inner command's exit code from a workspace.exec envelope
// (§4.5: a non-zero inner exit is data, not a platform error).
func innerExit(test *testing.T, envelope Envelope) int {
	test.Helper()
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	envelope.dataInto(test, &result)
	return result.ExitCode
}

// writeProviderConfig writes a minimal LiteLLM model_list pointing a model alias
// at the mock provider. The api_key is a ClawPatrol placeholder — the real value
// is injected on egress (arch §17). Exact LiteLLM schema verified on hardware.
func writeProviderConfig(test *testing.T, path, providerURL string) {
	test.Helper()
	config := fmt.Sprintf(`model_list:
  - model_name: gpt-5
    litellm_params:
      model: openai/gpt-5
      api_base: %s/v1
      api_key: sk-clawpatrol_placeholder_do_not_use
`, providerURL)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		test.Fatal(err)
	}
}

// TestCredentialedRequestOnHardware — AT §9.1: the agent holds only a placeholder,
// yet an outbound request reaches the provider carrying the real credential.
func TestCredentialedRequestOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires a provisioned host (msb + Docker + ClawPatrol); set AIP_HARDWARE_TESTS=1")
	}
	requireGit(test)
	const sentinel = "sk-aip-test-sentinel-DO-NOT-LEAK"

	mock := startMockProvider(test)
	harness := New(test)
	harness.installTemplates(test)

	providerConfig := filepath.Join(harness.Home, "provider.yaml")
	writeProviderConfig(test, providerConfig, mock.url())
	mock.writeCertPEM(test, filepath.Join(harness.Home, "mock-ca.pem"))

	setupEnvelope, code := harness.Run(test, "setup", "--provider-config", providerConfig)
	AssertOK(test, setupEnvelope, code, "setup")

	// Load the real credential into ClawPatrol; the agent only ever sees a placeholder.
	set, code := harness.Run(test, "secrets", "set", "OPENAI_API_KEY", "--value", sentinel)
	AssertOK(test, set, code, "secrets.set")

	created, code := harness.CreateProject(test, "cred-test")
	AssertOK(test, created, code, "project.create")
	mapped, code := harness.Run(test, "secrets", "map", "OPENAI_API_KEY", "--env", "OPENAI_API_KEY")
	AssertOK(test, mapped, code, "secrets.map")

	// A model call must reach the provider carrying the REAL credential.
	resp, code := harness.Run(test, "models", "test", "gpt-5", "--project", "cred-test")
	AssertOK(test, resp, code, "models.test")
	if !mock.sawCredential(sentinel) {
		test.Fatal("mock provider never received the real credential — ClawPatrol wire injection failed")
	}

	// The workspace env must hold only the placeholder, never the sentinel.
	envOut, _ := harness.Exec(test, "cred-test", "env")
	var execData struct {
		Stdout string `json:"stdout"`
	}
	envOut.dataInto(test, &execData)
	if strings.Contains(execData.Stdout, sentinel) {
		test.Fatal("the real credential leaked into the workspace env")
	}
	if !strings.Contains(execData.Stdout, "clawpatrol_placeholder") {
		test.Fatal("workspace env is missing the placeholder credential")
	}
}

// TestWorkspaceIsolationOnHardware — AT §16.2: the host filesystem and Docker
// socket are unreachable from inside the microVM workspace.
func TestWorkspaceIsolationOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires a provisioned host; set AIP_HARDWARE_TESTS=1")
	}
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProject(test, "iso-test")
	AssertOK(test, created, code, "project.create")
	start, code := harness.Run(test, "workspace", "start", "iso-test")
	AssertOK(test, start, code, "workspace.start")

	// A host-only marker placed outside any workspace mount.
	marker := filepath.Join(harness.Home, "host-only-marker")
	if err := os.WriteFile(marker, []byte("HOST_ONLY"), 0o644); err != nil {
		test.Fatal(err)
	}
	if catMarker, _ := harness.Exec(test, "iso-test", "cat", marker); innerExit(test, catMarker) == 0 {
		test.Fatal("host filesystem reachable from the workspace (marker readable)")
	}
	if socket, _ := harness.Exec(test, "iso-test", "test", "-e", "/var/run/docker.sock"); innerExit(test, socket) == 0 {
		test.Fatal("host Docker socket is present inside the workspace")
	}
}

// TestEgressConfinementOnHardware — AT §16.3: trusted host services and the
// allow-listed provider are reachable; everything else is denied (via the proxy
// and directly).
func TestEgressConfinementOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires a provisioned host; set AIP_HARDWARE_TESTS=1")
	}
	requireGit(test)
	mock := startMockProvider(test)
	harness := New(test)
	harness.installTemplates(test)

	providerConfig := filepath.Join(harness.Home, "provider.yaml")
	writeProviderConfig(test, providerConfig, mock.url())
	setupEnvelope, code := harness.Run(test, "setup", "--provider-config", providerConfig)
	AssertOK(test, setupEnvelope, code, "setup")

	created, code := harness.CreateProject(test, "egress-test")
	AssertOK(test, created, code, "project.create")
	start, code := harness.Run(test, "workspace", "start", "egress-test")
	AssertOK(test, start, code, "workspace.start")

	// Trusted host service (LiteLLM) reachable via AI_PLATFORM_HOST (guest vars).
	litellm, _ := harness.Exec(test, "egress-test", "sh", "-c",
		`curl -fsS "http://$AI_PLATFORM_HOST:$LITELLM_PORT/health" >/dev/null`)
	if innerExit(test, litellm) != 0 {
		test.Fatal("trusted host service (LiteLLM) not reachable from the workspace")
	}
	// Non-allow-listed destination denied through the proxy.
	viaProxy, _ := harness.Exec(test, "egress-test", "sh", "-c",
		"curl -fsS --max-time 5 https://example.com >/dev/null")
	if innerExit(test, viaProxy) == 0 {
		test.Fatal("non-allow-listed destination should be denied by ClawPatrol")
	}
	// And denied directly (Microsandbox default-deny network policy).
	direct, _ := harness.Exec(test, "egress-test", "sh", "-c",
		`curl -fsS --max-time 5 --noproxy "*" https://example.com >/dev/null`)
	if innerExit(test, direct) == 0 {
		test.Fatal("direct (non-proxy) egress should be denied by the network policy")
	}
}
