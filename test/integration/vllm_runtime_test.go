//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// localModelRef is the bare Ollama pull ref for the tiny test model (localModel is
// its served `ollama/`-prefixed handle).
const localModelRef = "smollm:135m"

// Group 7: per-model inference runtime (host-native Ollama + host-native vLLM).
//
// The inference tier is host-native Ollama (no aip-ollama container) plus a
// host-native vLLM backend; the serving ENGINE is chosen per-model at add time via
// `ai models pull --runtime ollama|vllm`. These live checks exercise the parts that
// do NOT require a running vLLM (flag validation, the vLLM-unavailable guard, and the
// services/doctor surfacing), and self-skip the positive vLLM routing path when vLLM
// is not up — matching the suite's `hardware bring-up` self-skip convention.
func TestGroup07InferenceRuntime(test *testing.T) {
	requireStack(test)

	// vllmRunning reports whether the gateway currently sees the host-native vLLM
	// backend as running (from `ai services status` — a single "vllm" summary line),
	// so the positive path runs only when vLLM is actually up.
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

	test.Run("services status lists host-native Ollama and vLLM", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)

		var ollama, vllm *setup.ServiceStatus
		for index := range result.Services {
			switch result.Services[index].Name {
			case "ollama":
				ollama = &result.Services[index]
			case "vllm":
				vllm = &result.Services[index]
			}
		}
		if ollama == nil {
			test.Errorf("services status did not list ollama")
		} else if ollama.Mode != "host" {
			// Ollama is host-native now — it must not report as a container/runtime mode.
			test.Errorf("ollama mode = %q, want %q (host-native)", ollama.Mode, "host")
		}
		if vllm == nil {
			test.Errorf("services status did not list vllm (it is always surfaced as a host service)")
		} else if vllm.Mode != "host" {
			test.Errorf("vllm mode = %q, want %q (host-side service)", vllm.Mode, "host")
		}
	})

	test.Run("doctor lists host-native Ollama and vLLM", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "doctor")
		// doctor always exits 0 with a report; assert the envelope is ok and mentions both.
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
		if !names["ollama"] {
			test.Errorf("doctor did not report an ollama check; got %v", names)
		}
		if !names["vllm"] {
			test.Errorf("doctor did not report a vllm check; got %v", names)
		}
	})

	test.Run("invalid --runtime is rejected (exit 2)", func(test *testing.T) {
		// Flag validation happens before any download, so this is fast and needs no backend.
		_, code, stderr := run(test, "", 60*time.Second, "models", "pull", localModelRef, "--runtime", "bogus")
		if code != 2 {
			test.Errorf("pull --runtime bogus: exit %d, want 2 (invalid input)\nstderr:\n%s", code, stderr)
		}
	})

	test.Run("vLLM runtime with vLLM unavailable fails clearly, never silently falls back", func(test *testing.T) {
		if _, running := vllmRunning(test); running {
			test.Skip("vLLM is running — negative (unavailable) path not applicable")
		}
		// vLLM is not serving on this host: an explicit vLLM pull must FAIL (non-zero) —
		// never a silent success or a downgrade to Ollama. When vLLM is not installed the
		// code is 3 (missing dependency); when it is installed but the serve path is a
		// hardware-bring-up stub the pull still fails non-zero. Either way it must not be 0.
		_, code, stderr := run(test, "", 2*time.Minute, "models", "pull", localModelRef, "--runtime", "vllm")
		if code == 0 {
			test.Errorf("pull --runtime vllm (vLLM unavailable) exited 0 — it must fail, never silently fall back to Ollama\nstderr:\n%s", stderr)
		}
	})

	test.Run("vLLM runtime registration + routing (skips when vLLM is down)", func(test *testing.T) {
		present, running := vllmRunning(test)
		if !present || !running {
			test.Skip("vLLM not running — vLLM serving is a hardware bring-up path")
		}
		// vLLM is up: pull the tiny model via the vLLM runtime, assert it is served under
		// the vllm/ public handle, then de-register it.
		const alias = "aip-it-vllm"
		test.Cleanup(func() {
			run(test, "", 90*time.Second, "models", "rm", localModelRef)
		})
		env, code, stderr := run(test, "", 5*time.Minute,
			"models", "pull", localModelRef, "--runtime", "vllm", "--alias", alias)
		if !assertOK(test, env, code, "models.pull") {
			test.Fatalf("vLLM pull failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		if !waitForServedModel(test, "vllm/"+alias, 60*time.Second) {
			test.Errorf("vllm/%s not served by the gateway after a vLLM pull", alias)
		}
	})
}
