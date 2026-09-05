//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// Group 7: local inference runtime (host-native omlx).
//
// omlx is the SOLE local-inference backend — ONE shared host-native `omlx serve`
// process (not per-model like the old vLLM design), never an `aip-*` container.
// Model management lives entirely in omlx's own admin panel — this platform has
// no `ai models pull` equivalent any more, so these live checks are limited to
// what the CLI still owns: the services/doctor surfacing and `ai models refresh`
// (the on-demand LiteLLM sync). They self-skip whatever needs omlx actually
// running/serving, matching the suite's `hardware bring-up` self-skip convention.
func TestGroup07InferenceRuntime(test *testing.T) {
	requireStack(test)

	// omlxRunning reports whether `ai services status` sees the host-native omlx
	// backend as running (a single "omlx" host summary line).
	omlxRunning := func(test *testing.T) (present bool, running bool) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return false, false
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)
		for _, service := range result.Services {
			if service.Name == "omlx" {
				return true, service.State == "running"
			}
		}
		return false, false
	}

	test.Run("services status lists host-native omlx (no removed local runtime)", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)

		var omlx *setup.ServiceStatus
		for index := range result.Services {
			switch result.Services[index].Name {
			case "omlx":
				omlx = &result.Services[index]
			case "vllm", "ollama":
				// A removed local runtime must not be surfaced any more.
				test.Errorf("services status still lists a removed local-runtime service %q", result.Services[index].Name)
			}
		}
		if omlx == nil {
			test.Errorf("services status did not list omlx (it is always surfaced as a host service)")
		} else if omlx.Mode != "host" {
			test.Errorf("omlx mode = %q, want %q (host-side service)", omlx.Mode, "host")
		}
	})

	test.Run("doctor lists host-native omlx (no removed local runtime)", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "doctor")
		// doctor always exits 0 with a report; assert the envelope is ok and mentions omlx.
		if !assertOK(test, env, code, "doctor") {
			return
		}
		var report struct {
			Checks []struct {
				Name string `json:"name"`
			} `json:"checks"`
		}
		env.dataInto(test, &report)
		names := map[string]bool{}
		for _, check := range report.Checks {
			names[check.Name] = true
		}
		if !names["omlx"] {
			test.Errorf("doctor did not report an omlx check; got %v", names)
		}
		if names["vllm"] || names["ollama"] {
			test.Errorf("doctor still reports a removed local-runtime check; got %v", names)
		}
	})

	test.Run("models refresh with omlx unavailable fails clearly, never silently succeeds", func(test *testing.T) {
		if _, running := omlxRunning(test); running {
			test.Skip("omlx is running — the unavailable (negative) path is not applicable")
		}
		_, code, stderr := run(test, "", 60*time.Second, "models", "refresh")
		if code == 0 {
			test.Errorf("models refresh (omlx unavailable) exited 0 — it must fail, never silently succeed\nstderr:\n%s", stderr)
		}
	})

	test.Run("models refresh syncs omlx's live models into the gateway (skips when omlx is down)", func(test *testing.T) {
		present, running := omlxRunning(test)
		if !present || !running {
			test.Skip("omlx not running — omlx serving is a hardware bring-up path")
		}
		env, code, stderr := run(test, "", 60*time.Second, "models", "refresh")
		if !assertOK(test, env, code, "models.refresh") {
			test.Fatalf("models refresh failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		omlxServed := false
		for _, name := range servedModelNames(test) {
			if strings.HasPrefix(name, "omlx/") {
				omlxServed = true
				break
			}
		}
		if !omlxServed {
			test.Skip("omlx is running but currently serves no models — add one via its own admin panel (`ai services console omlx`)")
		}
	})
}
