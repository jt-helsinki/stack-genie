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

// Group 7: per-model inference runtime (host-native Ollama + Docker Model Runner).
//
// The inference tier is now host-native Ollama (no aip-ollama container) plus an
// always-available Docker Model Runner (DMR) backend, and the ENGINE is chosen
// per-model at add time via `ai models pull --runtime ollama|docker-model-runner`.
// These live checks exercise the parts that do NOT require a running host Ollama /
// DMR (flag validation, the DMR-unavailable guard, and the services/doctor
// surfacing), and self-skip the positive DMR routing path when DMR is not up —
// matching the suite's `hardware bring-up` self-skip convention.
func TestGroup07InferenceRuntime(test *testing.T) {
	requireStack(test)

	// dmrRunning reports whether the gateway currently sees the docker-model-runner
	// backend as running (from `ai services status`), so the positive path runs only
	// when DMR is actually up and the negative path only when it is down.
	dmrRunning := func(test *testing.T) (present bool, running bool) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return false, false
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)
		for _, service := range result.Services {
			if service.Name == "docker-model-runner" {
				return true, service.State == "running"
			}
		}
		return false, false
	}

	test.Run("services status lists host-native Ollama and Docker Model Runner", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return
		}
		var result struct {
			Services []setup.ServiceStatus `json:"services"`
		}
		env.dataInto(test, &result)

		var ollama, dmr *setup.ServiceStatus
		for index := range result.Services {
			switch result.Services[index].Name {
			case "ollama":
				ollama = &result.Services[index]
			case "docker-model-runner":
				dmr = &result.Services[index]
			}
		}
		if ollama == nil {
			test.Errorf("services status did not list ollama")
		} else if ollama.Mode != "host" {
			// Ollama is host-native now — it must not report as a container/runtime mode.
			test.Errorf("ollama mode = %q, want %q (host-native)", ollama.Mode, "host")
		}
		if dmr == nil {
			test.Errorf("services status did not list docker-model-runner (it is always surfaced)")
		} else if dmr.Mode != "host" {
			test.Errorf("docker-model-runner mode = %q, want %q (host-side service)", dmr.Mode, "host")
		}
	})

	test.Run("doctor lists host-native Ollama and Docker Model Runner", func(test *testing.T) {
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
		if !names["docker-model-runner"] {
			test.Errorf("doctor did not report a docker-model-runner check; got %v", names)
		}
	})

	test.Run("invalid --runtime is rejected (exit 2)", func(test *testing.T) {
		// Flag validation happens before any download, so this is fast and needs no backend.
		_, code, stderr := run(test, "", 60*time.Second, "models", "pull", localModelRef, "--runtime", "bogus")
		if code != 2 {
			test.Errorf("pull --runtime bogus: exit %d, want 2 (invalid input)\nstderr:\n%s", code, stderr)
		}
	})

	test.Run("DMR runtime with DMR down fails clearly, never silently falls back (exit 3)", func(test *testing.T) {
		if present, running := dmrRunning(test); present && running {
			test.Skip("Docker Model Runner is running — negative (unavailable) path not applicable")
		}
		// DMR is not running: an explicit DMR pull must fail with a missing-dependency
		// error (exit 3), NOT silently register/serve via Ollama.
		_, code, stderr := run(test, "", 2*time.Minute, "models", "pull", localModelRef, "--runtime", "docker-model-runner")
		if code != 3 {
			test.Errorf("pull --runtime docker-model-runner (DMR down): exit %d, want 3 (missing dependency)\nstderr:\n%s", code, stderr)
		}
	})

	test.Run("DMR runtime registration + routing (skips when DMR is down)", func(test *testing.T) {
		present, running := dmrRunning(test)
		if !present || !running {
			test.Skip("Docker Model Runner not running — DMR routing is a hardware bring-up path")
		}
		// DMR is up: pull the tiny model via the DMR runtime, assert it is served under
		// the docker-model-runner/ public handle, then de-register it.
		const alias = "aip-it-dmr"
		test.Cleanup(func() {
			run(test, "", 90*time.Second, "models", "rm", localModelRef)
		})
		env, code, stderr := run(test, "", 5*time.Minute,
			"models", "pull", localModelRef, "--runtime", "docker-model-runner", "--alias", alias)
		if !assertOK(test, env, code, "models.pull") {
			test.Fatalf("DMR pull failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		if !waitForServedModel(test, "docker-model-runner/"+alias, 60*time.Second) {
			test.Errorf("docker-model-runner/%s not served by the gateway after a DMR pull", alias)
		}
	})
}
