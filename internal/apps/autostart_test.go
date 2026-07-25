package apps

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// TestAppsAutostartScriptEmpty: no installed apps → no script (nothing to auto-start).
func TestAppsAutostartScriptEmpty(test *testing.T) {
	if got := AppsAutostartScript(nil, testAppsGateway); got != "" {
		test.Errorf("nil config → empty script, got: %q", got)
	}
	if got := AppsAutostartScript(&config.Config{}, testAppsGateway); got != "" {
		test.Errorf("no apps → empty script, got: %q", got)
	}
	// A dashboard-only workspace has no APP containers, so the app script is still empty
	// (dashboards are launched separately by the workspace, not by this container script).
	dashOnly := &config.Config{AgentDashboards: []config.AppEntry{{Key: "hermes", Port: 9119}}}
	if got := AppsAutostartScript(dashOnly, testAppsGateway); got != "" {
		test.Errorf("dashboard-only → empty app script, got: %q", got)
	}
}

// testAppsGateway is the (non-secret) resolved gateway base URL baked into the script.
const testAppsGateway = "http://host.microsandbox.internal:18787/v1"

// TestAppsAutostartScript pins the shape of the app-container autostart script and,
// critically, that NO literal scoped-key value appears — the gateway env is passed by
// shell-variable reference sourced from the in-VM agent env file.
func TestAppsAutostartScript(test *testing.T) {
	projectConfig := &config.Config{
		Apps: []config.AppEntry{
			{Key: "openwebui", Port: 8080},
			{Key: "anythingllm", Port: 3001},
			{Key: "bogus", Port: 9999}, // unknown → skipped
		},
	}
	script := AppsAutostartScript(projectConfig, testAppsGateway)
	if script == "" {
		test.Fatal("expected a non-empty script for installed apps")
	}

	// Sources the in-VM agent env file so ${OPENAI_*} resolve at run time.
	for _, want := range []string{
		"set -a",
		"[ -f /home/workspace/.config/aip/agent-env.sh ] && . /home/workspace/.config/aip/agent-env.sh",
		`: "${OPENAI_API_KEY:=${AIP_GATEWAY_KEY}}"`,
		"nerdctl info", // bounded containerd-ready wait
	} {
		if !strings.Contains(script, want) {
			test.Errorf("script missing %q:\n%s", want, script)
		}
	}

	// Open WebUI: idempotent recreate, run line, container name, port, restart policy,
	// volume, /workspace mount, memory, and env-by-REFERENCE.
	for _, want := range []string{
		"nerdctl rm -f aip-app-openwebui 2>/dev/null || true",
		"nerdctl run -d",
		"--name aip-app-openwebui",
		"--restart always",
		"-p 8080:8080",
		"-v /persist/apps/openwebui:/app/backend/data",
		"-v /home/workspace/project:/workspace",
		"--memory 2g",
		"-e ENABLE_OLLAMA_API=false",
		`-e OPENAI_API_BASE_URL=` + testAppsGateway,
		`-e "OPENAI_API_KEY=${OPENAI_API_KEY}"`,
		"-e WEBUI_AUTH=false",
		openWebUIImage,
	} {
		if !strings.Contains(script, want) {
			test.Errorf("openwebui line missing %q:\n%s", want, script)
		}
	}

	// AnythingLLM: its GENERIC_OPEN_AI_* env by reference + the static ones.
	for _, want := range []string{
		"nerdctl rm -f aip-app-anythingllm 2>/dev/null || true",
		"--name aip-app-anythingllm",
		"-p 3001:3001",
		`-e "GENERIC_OPEN_AI_API_KEY=${OPENAI_API_KEY}"`,
		`-e GENERIC_OPEN_AI_BASE_PATH=` + testAppsGateway,
		`-e "GENERIC_OPEN_AI_MODEL_PREF=${OPENAI_MODEL}"`,
		"-e GENERIC_OPEN_AI_MODEL_TOKEN_LIMIT=4096",
		"-e LLM_PROVIDER=generic-openai",
		"-e STORAGE_DIR=/app/server/storage",
		anythingLLMImage,
	} {
		if !strings.Contains(script, want) {
			test.Errorf("anythingllm line missing %q:\n%s", want, script)
		}
	}

	// The unknown app key is skipped entirely.
	if strings.Contains(script, "bogus") {
		test.Errorf("unknown app key must be skipped:\n%s", script)
	}

	// HARD invariant: no literal secret value is ever staged — only ${…} references. The
	// generator is handed no key at all, so any "sk-" prefix would be a regression.
	if strings.Contains(script, "sk-") {
		test.Errorf("script must not contain any literal key value:\n%s", script)
	}
}
