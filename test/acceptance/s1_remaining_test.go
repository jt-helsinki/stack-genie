package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// This file completes the [S1] acceptance suite (docs/HARDWARE-BRINGUP.md §3):
//
//   - §2.1–§2.3 setup + idempotency  — hardware-gated (real service tier).
//   - §7.1 model status / §7.2 model test — hardware-gated (running LiteLLM).
//
// The old §6.1/§6.3/§6.4 file-inspection probes (OS base image, agent-CLI /
// software-stack Dockerfile snippets) no longer apply: there is no OS choice,
// no Dockerfile, and no stacks concept in the nono model — a workspace is a
// registered project directory sandboxed directly on the host.

// --- §2.1–§2.3 setup + idempotency (hardware-gated) -------------------------

// TestSetupBringsServiceTierUpOnHardware covers AT §2.1 [S1]: `ai setup` exits 0
// and brings the service tier up (the containers report running/healthy), then
// AT §2.2: a SECOND `ai setup` is idempotent — still exit 0, no error, the
// services still report running. Asserted via the setup envelope plus a follow-up
// `ai services status`.
func TestSetupBringsServiceTierUpOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + nono — runs on a provisioned host; set AIP_HARDWARE_TESTS=1")
	}
	harness := New(test)

	// §2.1: first setup brings the tier up.
	first, code := harness.Run(test, "setup")
	AssertOK(test, first, code, "setup")
	if !setupReportsRunningTier(test, first) {
		test.Fatal("first `ai setup` did not bring the service tier up (no running services in the envelope)")
	}

	// §2.2: a second setup is idempotent — exit 0, no error, still running.
	second, code := harness.Run(test, "setup")
	AssertOK(test, second, code, "setup")
	if second.Error != nil {
		test.Fatalf("second `ai setup` reported an error: %+v", second.Error)
	}
	if !setupReportsRunningTier(test, second) {
		test.Fatal("second `ai setup` left the service tier not running (not idempotent)")
	}

	// And `ai services status` confirms the tier is still up after both runs.
	status, code := harness.Run(test, "services", "status")
	AssertOK(test, status, code, "services.status")
	if !servicesStatusReportsRunning(test, status) {
		test.Fatal("`ai services status` reports no running services after setup")
	}
}

// TestSetupUpgradePreservesStateOnHardware covers AT §2.3 [S1]: `ai setup
// --upgrade` exits 0, preserves existing state (a created project survives), and
// keeps the projects index intact.
func TestSetupUpgradePreservesStateOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + nono — runs on a provisioned host; set AIP_HARDWARE_TESTS=1")
	}
	harness := New(test)

	setupEnvelope, code := harness.Run(test, "setup")
	AssertOK(test, setupEnvelope, code, "setup")

	// Create a project so we can prove --upgrade preserves it.
	created, code := harness.CreateProject(test, "upgrade-survivor")
	AssertOK(test, created, code, "project.create")

	upgrade, code := harness.Run(test, "setup", "--upgrade")
	AssertOK(test, upgrade, code, "setup")

	// The project must still be in the index after the upgrade.
	list, code := harness.Run(test, "list")
	AssertOK(test, list, code, "project.list")
	var listData struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	list.dataInto(test, &listData)
	found := false
	for _, project := range listData.Projects {
		if project.Name == "upgrade-survivor" {
			found = true
		}
	}
	if !found {
		test.Fatalf("`ai setup --upgrade` lost the project (index = %+v)", listData)
	}
}

// setupReportsRunningTier reports whether a setup envelope shows at least one
// running service (the service tier came up). Mirrors setup.ServiceStatus.
func setupReportsRunningTier(test *testing.T, envelope Envelope) bool {
	test.Helper()
	var data struct {
		Services []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"services"`
	}
	envelope.dataInto(test, &data)
	for _, service := range data.Services {
		if service.State == "running" {
			return true
		}
	}
	return false
}

// servicesStatusReportsRunning reports whether an `ai services status` envelope
// shows at least one running service.
func servicesStatusReportsRunning(test *testing.T, envelope Envelope) bool {
	test.Helper()
	var data struct {
		Services []struct {
			State string `json:"state"`
		} `json:"services"`
	}
	envelope.dataInto(test, &data)
	for _, service := range data.Services {
		if service.State == "running" {
			return true
		}
	}
	return false
}

// --- §7.1 model status / §7.2 model test (hardware-gated) -------------------

// TestModelStatusOnHardware covers AT §7.1 [S1]: against a running gateway,
// `ai models status` returns a clean envelope reporting the routing — the default
// model, the providers, and a healthy gateway.
func TestModelStatusOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires a running LiteLLM gateway (run `ai setup`); set AIP_HARDWARE_TESTS=1")
	}
	harness := New(test)
	setupEnvelope, code := harness.Run(test, "setup")
	AssertOK(test, setupEnvelope, code, "setup")

	status, code := harness.Run(test, "models", "status")
	AssertOK(test, status, code, "models.status")
	var data struct {
		Healthy   bool     `json:"healthy"`
		Providers []string `json:"providers"`
		Default   string   `json:"default"`
	}
	status.dataInto(test, &data)
	if !data.Healthy {
		test.Fatal("models status: gateway is not healthy")
	}
	// NB: the model set is catalog-driven and DB-backed — litellm.DefaultRouting()
	// returns the zero Routing, so `default` is intentionally always empty. Do NOT
	// assert a non-empty default. Assert only gateway health + a non-empty served
	// provider set.
	_ = data.Default
	if len(data.Providers) == 0 {
		test.Fatal("models status: no providers listed")
	}
}

// TestModelTestOnHardware covers AT §7.2 [S1]: `ai models test <model>` against
// the running gateway returns ok with a latency. It uses the TLS mock-provider
// fixture so no real provider key is needed — the gateway is pointed at the mock
// via --provider-config and the model alias it exposes (gpt-5) is probed.
func TestModelTestOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires a running LiteLLM gateway (run `ai setup`); set AIP_HARDWARE_TESTS=1")
	}
	mock := startMockProvider(test)
	harness := New(test)

	providerConfig := filepath.Join(harness.Home, "provider.yaml")
	writeProviderConfig(test, providerConfig, mock.url())
	mock.writeCertPEM(test, filepath.Join(harness.Home, "mock-ca.pem"))

	setupEnvelope, code := harness.Run(test, "setup", "--provider-config", providerConfig)
	AssertOK(test, setupEnvelope, code, "setup")

	result, code := harness.Run(test, "models", "test", "gpt-5")
	AssertOK(test, result, code, "models.test")
	var data struct {
		Model     string `json:"model"`
		OK        bool   `json:"ok"`
		LatencyMS int    `json:"latency_ms"`
	}
	result.dataInto(test, &data)
	if !data.OK {
		test.Fatalf("models test: probe not ok: %+v", data)
	}
	if data.LatencyMS < 0 {
		test.Fatalf("models test: negative latency %d", data.LatencyMS)
	}
}

// writeProviderConfig writes a minimal LiteLLM model_list pointing a model alias
// at the mock provider. The api_key here is a virtual-key placeholder — the real
// provider key lives only in the LiteLLM gateway container (env passthrough at
// launch) and is attached when LiteLLM calls the provider (arch §17). Exact
// LiteLLM schema verified on hardware.
func writeProviderConfig(test *testing.T, path, providerURL string) {
	test.Helper()
	config := fmt.Sprintf(`model_list:
  - model_name: gpt-5
    litellm_params:
      model: openai/gpt-5
      api_base: %s/v1
      api_key: sk-litellm_virtual_key_placeholder
`, providerURL)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		test.Fatal(err)
	}
}
