//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/doctor"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// Group 1: Setup / health.
//
// `ai setup --mode standalone` is idempotent and non-interactive; `ai doctor`
// reports every check ok/healthy; the catalog file exists on disk; the gateway
// is reachable (models status healthy).
func TestGroup01SetupHealth(test *testing.T) {
	requireStack(test)

	test.Run("setup standalone is idempotent", func(test *testing.T) {
		// The stack is already up; setup must reconcile to the same healthy state
		// without prompting. --json makes it non-interactive. Generous timeout: a
		// reconcile may re-pull/restart containers.
		env, code, stderr := run(test, "", 5*time.Minute, "setup", "--mode", "standalone")
		if !assertOK(test, env, code, "setup") {
			test.Logf("setup stderr:\n%s", stderr)
			return
		}
		var report setup.Report
		env.dataInto(test, &report)
		if report.Runtime == nil || !report.Runtime.Microsandbox.Available {
			test.Errorf("setup report: microVM runtime should be available, got %+v", report.Runtime)
		}
	})

	test.Run("doctor every check ok/warn (no errors)", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "doctor")
		// doctor always exits 0 with a report; the per-check status conveys health.
		if !assertOK(test, env, code, "doctor") {
			return
		}
		var report doctor.Report
		env.dataInto(test, &report)
		if !report.OK {
			test.Errorf("doctor report.ok is false")
		}
		for _, check := range report.Checks {
			if check.Status == doctor.StatusError {
				test.Errorf("doctor check %q errored: %s", check.Name, check.Detail)
			}
		}
		// Sanity: the core service-tier checks must be present.
		want := map[string]bool{"litellm": false, "vllm": false, "presidio": false, "proxy": false, "dns": false, "headroom": false}
		for _, check := range report.Checks {
			if _, ok := want[check.Name]; ok {
				want[check.Name] = true
			}
		}
		for name, found := range want {
			if !found {
				test.Errorf("doctor report missing expected check %q", name)
			}
		}
	})

	test.Run("catalog file exists", func(test *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			test.Fatalf("home: %v", err)
		}
		catalogPath := filepath.Join(home, ".ai-platform", "cache", "catalog.yaml")
		if _, err := os.Stat(catalogPath); err != nil {
			test.Errorf("catalog file %s not present: %v", catalogPath, err)
		}
	})

	test.Run("gateway reachable (models status healthy)", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "models", "status")
		if !assertOK(test, env, code, "models.status") {
			return
		}
		var status litellm.StatusInfo
		env.dataInto(test, &status)
		if !status.Healthy {
			test.Errorf("models status: gateway not healthy: %+v", status)
		}
		// Local-model connectivity is an environmental dependency (host-native vLLM must
		// be installed + serving at least one model — there is no aip-ollama container).
		// When it's down, skip rather than fail — the suite self-skips absent stack pieces,
		// not error on an un-provisioned host.
		if !status.Local {
			test.Skip("models status: no local (vLLM) model served — skipping (install vLLM + pull a model to exercise this)")
		}
	})
}

// modelsStatus returns the live gateway StatusInfo (used by several groups).
func modelsStatus(test *testing.T) litellm.StatusInfo {
	test.Helper()
	env, code, _ := run(test, "", 60*time.Second, "models", "status")
	if !env.OK || code != 0 {
		test.Fatalf("models status failed: exit=%d err=%+v", code, env.Error)
	}
	var status litellm.StatusInfo
	if err := json.Unmarshal(env.Data, &status); err != nil {
		test.Fatalf("decode models status: %v", err)
	}
	return status
}

// servedModelNames returns the names the gateway currently serves.
func servedModelNames(test *testing.T) []string {
	status := modelsStatus(test)
	names := make([]string, 0, len(status.Models))
	for _, model := range status.Models {
		names = append(names, model.Name)
	}
	return names
}

// hasModel reports whether name is in the served set.
func hasModel(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// waitForServedModel polls models status until the named model appears.
func waitForServedModel(test *testing.T, name string, timeout time.Duration) bool {
	return waitFor(func() bool {
		return hasModel(servedModelNames(test), name)
	}, timeout)
}
