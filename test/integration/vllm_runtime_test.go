//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// vllmAlias is the gateway alias used for the positive vLLM routing path (served as
// vllm/<vllmAlias>). The repo is localModelRepo, shared with models_keys_test.go.
const vllmAlias = "aip-it-vllm"

// Group 7: local inference runtime (host-native vLLM).
//
// vLLM is the SOLE local-inference backend — there is no per-model `--runtime`
// choice (the flag is gone) and the local inference tier never runs as an
// `aip-*` container. These live checks exercise the parts that do NOT require a
// running vLLM (the services/doctor surfacing and the vLLM-unavailable guard),
// and self-skip the positive routing path when vLLM is not up — matching the
// suite's `hardware bring-up` self-skip convention.
func TestGroup07InferenceRuntime(test *testing.T) {
	requireStack(test)

	// vllmRunning reports whether `ai services status` sees the host-native vLLM backend
	// as running (a single "vllm" host summary line), so the positive path runs only when
	// vLLM is actually up.
	vllmRunning := func(test *testing.T) (present bool, running bool) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return false, false
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)
		for _, service := range result.Services {
			if service.Name == "vllm" {
				return true, service.State == "running"
			}
		}
		return false, false
	}

	test.Run("services status lists host-native vLLM (no removed local runtime)", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)

		var vllm *setup.ServiceStatus
		for index := range result.Services {
			switch result.Services[index].Name {
			case "vllm":
				vllm = &result.Services[index]
			case "ollama":
				// The removed local runtime must not be surfaced any more.
				test.Errorf("services status still lists a removed local-runtime service")
			}
		}
		if vllm == nil {
			test.Errorf("services status did not list vllm (it is always surfaced as a host service)")
		} else if vllm.Mode != "host" {
			test.Errorf("vllm mode = %q, want %q (host-side service)", vllm.Mode, "host")
		}
	})

	test.Run("doctor lists host-native vLLM (no removed local runtime)", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "doctor")
		// doctor always exits 0 with a report; assert the envelope is ok and mentions vllm.
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
		if !names["vllm"] {
			test.Errorf("doctor did not report a vllm check; got %v", names)
		}
		if names["ollama"] {
			test.Errorf("doctor still reports a removed local-runtime check; got %v", names)
		}
	})

	// Retargeted from the removed "invalid --runtime is rejected (exit 2)" test: the
	// `--runtime` flag no longer exists (vLLM is the sole local runtime), so we assert the
	// remaining contract — a pull when vLLM cannot serve MUST fail non-zero, never a silent
	// success or a fallback. When vLLM is not installed the code is 3 (missing dependency);
	// when installed-but-not-serving it still fails non-zero. (Also folds in the old
	// "vLLM unavailable" negative test.)
	test.Run("pull with vLLM unavailable fails clearly, never silently succeeds", func(test *testing.T) {
		if _, running := vllmRunning(test); running {
			test.Skip("vLLM is running — the unavailable (negative) path is not applicable")
		}
		_, code, stderr := run(test, "", 2*time.Minute, "models", "pull", localModelRepo)
		if code == 0 {
			test.Errorf("pull (vLLM unavailable) exited 0 — it must fail (exit 3 when not installed), never silently succeed\nstderr:\n%s", stderr)
		}
	})

	test.Run("vLLM registration + routing (skips when vLLM is down)", func(test *testing.T) {
		present, running := vllmRunning(test)
		if !present || !running {
			test.Skip("vLLM not running — vLLM serving is a hardware bring-up path")
		}
		// vLLM is up: pull the small curated model via the vLLM runtime under a fixed alias,
		// assert it is served under the vllm/ public handle, then de-register it.
		test.Cleanup(func() {
			run(test, "", 90*time.Second, "models", "rm", localModelRepo)
		})
		env, code, stderr := run(test, "", 15*time.Minute,
			"models", "pull", localModelRepo, "--alias", vllmAlias)
		if !assertOK(test, env, code, "models.pull") {
			test.Fatalf("vLLM pull failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		if !waitForServedModel(test, "vllm/"+vllmAlias, 60*time.Second) {
			test.Errorf("vllm/%s not served by the gateway after a vLLM pull", vllmAlias)
		}
	})
}
