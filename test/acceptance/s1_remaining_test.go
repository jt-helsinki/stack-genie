package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file completes the [S1] acceptance suite (docs/HARDWARE-BRINGUP.md §3):
//
//   - §2.1–§2.3 setup + idempotency  — hardware-gated (real service tier).
//   - §7.1 model status / §7.2 model test — hardware-gated (running LiteLLM).
//   - §6.1 / §6.3 / §6.4 environment probes — FILE-INSPECTION: they assert the
//     scaffolded .ai-platform/ artifacts on disk (Dockerfile / config.yaml /
//     profile.yaml), so they need only `git` + the bundled templates and RUN NOW
//     (no microVM). The spec's in-VM `command -v` half of §6.3/§6.4 needs a real
//     workspace and lives in the hardware-gated build/exec tests; here we assert
//     the equivalent on-disk selection that drives that build.

// projectArtifact reads a file under the scaffolded project's .ai-platform/ dir.
// root is the project root returned in the project.create envelope's data.
func projectArtifact(test *testing.T, root, name string) string {
	test.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".ai-platform", name))
	if err != nil {
		test.Fatalf("read .ai-platform/%s: %v", name, err)
	}
	return string(data)
}

// createRoot decodes the project root from a project.create envelope.
func createRoot(test *testing.T, envelope Envelope) string {
	test.Helper()
	var data struct {
		Root string `json:"root"`
	}
	envelope.dataInto(test, &data)
	if data.Root == "" {
		test.Fatal("project.create envelope did not report a root")
	}
	return data.Root
}

// --- §6.1 / §6.3 / §6.4 environment probes (file-inspection, runnable now) ---

// TestProjectDockerfileReflectsOS covers AT §6.1 [S1]: the scaffolded
// .ai-platform/Dockerfile is seeded from the chosen base-OS template. The in-VM
// `cat /etc/os-release` half of §6.1 needs a real image, so here we assert the
// on-disk base image (the FROM line) that the workspace is built from — for each
// of the four supported OS templates.
func TestProjectDockerfileReflectsOS(test *testing.T) {
	requireGit(test)
	cases := map[string]string{
		"debian-trixie":   "FROM debian:trixie-slim",
		"debian-bookworm": "FROM debian:bookworm-slim",
		"ubuntu":          "FROM ubuntu:24.04",
		"alma":            "FROM almalinux:10",
	}
	for osKey, wantFROM := range cases {
		test.Run(osKey, func(test *testing.T) {
			harness := New(test)
			harness.installTemplates(test)

			created, code := harness.CreateProjectWithOS(test, "os-"+osKey, osKey)
			AssertOK(test, created, code, "project.create")

			dockerfile := projectArtifact(test, createRoot(test, created), "Dockerfile")
			if !strings.Contains(dockerfile, wantFROM) {
				test.Fatalf("Dockerfile for %s missing base image %q:\n%s", osKey, wantFROM, dockerfile)
			}
		})
	}
}

// TestAgentCLISelectionReflected covers AT §6.3 [S1]: the selected agent CLIs are
// recorded in config.yaml (agent.tools / agent.default_tool) and each appears as
// an `# agent CLI:` snippet in the Dockerfile, while an unselected CLI does NOT.
func TestAgentCLISelectionReflected(test *testing.T) {
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	// Select a non-default CLI (codex) alongside the default (opencode).
	created, code := harness.CreateProjectFull(test, "cli-test", "debian-trixie",
		[]string{"opencode", "codex"}, nil)
	AssertOK(test, created, code, "project.create")

	// The create envelope reports the selected tools.
	var createData struct {
		Tools []string `json:"tools"`
		Root  string   `json:"root"`
	}
	created.dataInto(test, &createData)
	if !equalStrings(createData.Tools, []string{"opencode", "codex"}) {
		test.Fatalf("create reported tools %v, want [opencode codex]", createData.Tools)
	}

	// config.yaml records the tools and the default tool.
	config := projectArtifact(test, createData.Root, "config.yaml")
	for _, want := range []string{"opencode", "codex"} {
		if !strings.Contains(config, want) {
			test.Errorf("config.yaml missing agent CLI %q:\n%s", want, config)
		}
	}

	// Both selected CLIs appear in the Dockerfile; the unselected one does not.
	dockerfile := projectArtifact(test, createData.Root, "Dockerfile")
	for _, want := range []string{"# agent CLI: opencode", "# agent CLI: codex"} {
		if !strings.Contains(dockerfile, want) {
			test.Errorf("Dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
	if strings.Contains(dockerfile, "# agent CLI: gemini") {
		test.Errorf("Dockerfile installs an unselected agent CLI (gemini):\n%s", dockerfile)
	}
}

// TestAgentCLIDefaultSelection covers the §6.3/§3.1 default: a plain create
// (no --agents) yields the default agent CLIs (opencode + pi).
func TestAgentCLIDefaultSelection(test *testing.T) {
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProject(test, "cli-default")
	AssertOK(test, created, code, "project.create")
	var createData struct {
		Tools []string `json:"tools"`
	}
	created.dataInto(test, &createData)
	if !equalStrings(createData.Tools, []string{"opencode", "pi"}) {
		test.Fatalf("default create reported tools %v, want [opencode pi]", createData.Tools)
	}
}

// TestSoftwareStackSelectionReflected covers AT §6.4 [S1]: the selected software
// stacks are recorded in profile.yaml and each appears as a `# stack:` snippet in
// the Dockerfile, while an unselected stack does NOT.
func TestSoftwareStackSelectionReflected(test *testing.T) {
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProjectFull(test, "stack-test", "debian-trixie",
		nil, []string{"go", "node"})
	AssertOK(test, created, code, "project.create")

	var createData struct {
		Stacks []string `json:"stacks"`
		Root   string   `json:"root"`
	}
	created.dataInto(test, &createData)
	if !equalStrings(createData.Stacks, []string{"go", "node"}) {
		test.Fatalf("create reported stacks %v, want [go node]", createData.Stacks)
	}

	// profile.yaml records the selected stacks.
	profile := projectArtifact(test, createData.Root, "profile.yaml")
	for _, want := range []string{"go", "node"} {
		if !strings.Contains(profile, want) {
			test.Errorf("profile.yaml missing stack %q:\n%s", want, profile)
		}
	}

	// Both selected stacks appear in the Dockerfile; the unselected one does not.
	dockerfile := projectArtifact(test, createData.Root, "Dockerfile")
	for _, want := range []string{"# stack: go", "# stack: node"} {
		if !strings.Contains(dockerfile, want) {
			test.Errorf("Dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
	if strings.Contains(dockerfile, "# stack: python") {
		test.Errorf("Dockerfile installs an unselected stack (python):\n%s", dockerfile)
	}
}

// TestSoftwareStackDefaultEmpty covers the §6.4 default: a plain create installs
// no stacks beyond the base image (profile.yaml stacks: []).
func TestSoftwareStackDefaultEmpty(test *testing.T) {
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProject(test, "stack-default")
	AssertOK(test, created, code, "project.create")
	var createData struct {
		Stacks []string `json:"stacks"`
		Root   string   `json:"root"`
	}
	created.dataInto(test, &createData)
	if len(createData.Stacks) != 0 {
		test.Fatalf("default create reported stacks %v, want none", createData.Stacks)
	}
	dockerfile := projectArtifact(test, createData.Root, "Dockerfile")
	if strings.Contains(dockerfile, "# stack:") {
		test.Errorf("default create installed a stack snippet:\n%s", dockerfile)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// --- §2.1–§2.3 setup + idempotency (hardware-gated) -------------------------

// TestSetupBringsServiceTierUpOnHardware covers AT §2.1 [S1]: `ai setup` exits 0
// and brings the service tier up (the containers report running/healthy), then
// AT §2.2: a SECOND `ai setup` is idempotent — still exit 0, no error, the
// services still report running. Asserted via the setup envelope plus a follow-up
// `ai services status`.
func TestSetupBringsServiceTierUpOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host; set AIP_HARDWARE_TESTS=1")
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
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host; set AIP_HARDWARE_TESTS=1")
	}
	requireGit(test)
	harness := New(test)

	setupEnvelope, code := harness.Run(test, "setup")
	AssertOK(test, setupEnvelope, code, "setup")

	// Create a project so we can prove --upgrade preserves it.
	created, code := harness.CreateProject(test, "upgrade-survivor")
	AssertOK(test, created, code, "project.create")

	upgrade, code := harness.Run(test, "setup", "--upgrade")
	AssertOK(test, upgrade, code, "setup")

	// The project must still be in the index after the upgrade.
	list, code := harness.Run(test, "project", "list")
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
	if data.Default == "" {
		test.Fatal("models status: no default model in the routing")
	}
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
