//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// servicesResult mirrors the `ai services status` data payload.
type servicesResult struct {
	Services []setup.ServiceStatus `json:"services"`
}

// Group 2: Services.
//
// `ai services status` reports the core services running; restarting one returns
// it to running; `ai logs --service litellm` returns output.
func TestGroup02Services(test *testing.T) {
	requireStack(test)

	test.Run("status all core services running", func(test *testing.T) {
		env, code, _ := run(test, "", 60*time.Second, "services", "status")
		if !assertOK(test, env, code, "services.status") {
			return
		}
		var result servicesResult
		env.dataInto(test, &result)
		if len(result.Services) == 0 {
			test.Fatalf("services status returned no services")
		}
		for _, service := range result.Services {
			// A disabled service is allowed to be non-running: either an optional
			// service turned off, or a guardrail-gated one (e.g. presidio when the
			// secret-masking guardrail isn't selected at setup). Core services must
			// be healthy.
			if service.State == "disabled" {
				continue
			}
			// An OPTIONAL service is allowed to be idle/stopped: e.g. the host-native
			// omlx backend reports "stopped" until it's installed/started (it is
			// auto-installed best-effort at `ai setup`, never a hard requirement).
			// Core services (Optional=false) must be healthy.
			if service.Optional {
				continue
			}
			if !service.Healthy {
				test.Errorf("service %q not healthy: state=%q detail=%q", service.Name, service.State, service.Detail)
			}
		}
	})

	test.Run("restart presidio returns to running", func(test *testing.T) {
		// presidio only runs when the secret-masking guardrail is selected at setup; it
		// reports state "disabled" otherwise. Skip the bounce when it's not enabled.
		statusEnv, statusCode, _ := run(test, "", 30*time.Second, "services", "status")
		if statusCode == 0 {
			var status servicesResult
			statusEnv.dataInto(test, &status)
			for _, service := range status.Services {
				if service.Name == "presidio" && service.State == "disabled" {
					test.Skip("presidio disabled (secret-masking guardrail not selected)")
				}
			}
		}
		// presidio is a safe service to bounce — it has no dependents that need a
		// coordinated restart, and it is quick to come back.
		env, code, stderr := run(test, "", 4*time.Minute, "services", "restart", "presidio")
		if !assertOK(test, env, code, "services.restart") {
			test.Logf("restart stderr:\n%s", stderr)
			return
		}
		// Poll until presidio is healthy again.
		healthy := waitFor(func() bool {
			statusEnv, statusCode, _ := run(test, "", 30*time.Second, "services", "status")
			if !statusEnv.OK || statusCode != 0 {
				return false
			}
			var result servicesResult
			if err := jsonUnmarshal(statusEnv.Data, &result); err != nil {
				return false
			}
			for _, service := range result.Services {
				if service.Name == "presidio" {
					return service.Healthy
				}
			}
			return false
		}, 2*time.Minute)
		if !healthy {
			test.Errorf("presidio did not return to healthy after restart")
		}
	})

	test.Run("logs --service litellm returns output", func(test *testing.T) {
		// logs is human-rendered by default; --json wraps the captured lines.
		env, code, stderr := run(test, "", 60*time.Second, "logs", "--service", "litellm", "--tail")
		if !assertOK(test, env, code, "") {
			test.Logf("logs stderr:\n%s", stderr)
			return
		}
		if len(env.Data) == 0 || string(env.Data) == "null" {
			test.Errorf("logs --service litellm returned empty data payload")
		}
	})
}
