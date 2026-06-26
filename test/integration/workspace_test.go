//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// execResult mirrors the `ai exec` data payload.
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// appActionResult mirrors the `ai apps add` data payload (port is the published
// host port).
type appActionResult struct {
	Project         string `json:"project"`
	App             string `json:"app"`
	Action          string `json:"action"`
	Port            int    `json:"port,omitempty"`
	RestartRequired bool   `json:"restart_required,omitempty"`
}

// Group 4: Workspace lifecycle — the deepest bring-up path.
//
// Create the workspace in a fresh dir, start the microVM, exec a command, verify
// the in-VM container runtime, the refresh-models helper, an in-VM app reachable
// on its published host port, and the egress policy (deny blocks / public
// allows). Each step asserts and, on failure, logs the exact `ai` output — those
// are the bring-up seams the user wants surfaced.
//
// The whole group runs in ONE workspace it owns end-to-end; cleanup (stop +
// delete --purge) runs even if a step fails.
func TestGroup04WorkspaceLifecycle(test *testing.T) {
	requireStack(test)

	name := fmt.Sprintf("aip-int-%d", time.Now().Unix())
	work := test.TempDir() // a fresh dir is the workspace root

	// Cleanup: tear down the microVM and remove the workspace + its source.
	test.Cleanup(func() {
		run(test, work, 3*time.Minute, "stop", name)
		run(test, work, 3*time.Minute, "delete", name, "--purge", "--yes")
	})

	// --- create -------------------------------------------------------------
	if !test.Run("create", func(test *testing.T) {
		env, code, stderr := run(test, work, 2*time.Minute, "create", "--os", "debian-trixie", "--name", name)
		if !assertOK(test, env, code, "project.create") {
			test.Fatalf("create failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
	}) {
		return // no point continuing without a workspace
	}

	// --- start --------------------------------------------------------------
	if !test.Run("start", func(test *testing.T) {
		// Building the OCI image + booting the microVM is slow on a cold cache.
		env, code, stderr := run(test, work, 15*time.Minute, "start", name)
		if !assertOK(test, env, code, "workspace.start") {
			test.Fatalf("start failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
	}) {
		return
	}

	// --- exec echo ----------------------------------------------------------
	test.Run("exec echo ok", func(test *testing.T) {
		env, code, stderr := runExec(test, work, 90*time.Second, name, "echo", "ok")
		if !assertOK(test, env, code, "workspace.exec") {
			test.Fatalf("exec failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
		var result execResult
		env.dataInto(test, &result)
		if result.ExitCode != 0 || !strings.Contains(result.Stdout, "ok") {
			test.Errorf("exec echo: inner exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
		}
	})

	// --- in-VM container runtime (containerd via nerdctl) -------------------
	test.Run("in-VM nerdctl works", func(test *testing.T) {
		env, code, stderr := runExec(test, work, 2*time.Minute, name, "sudo", "nerdctl", "version")
		if !assertOK(test, env, code, "workspace.exec") {
			test.Fatalf("exec nerdctl version failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
		var result execResult
		env.dataInto(test, &result)
		// nerdctl reaching the in-VM containerd is the bring-up signal. A non-zero
		// inner exit means containerd is not up (a known bring-up seam).
		if result.ExitCode != 0 {
			test.Errorf("in-VM `nerdctl version` exit=%d\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Stdout, result.Stderr)
		} else if !strings.Contains(strings.ToLower(result.Stdout), "client") &&
			!strings.Contains(strings.ToLower(result.Stdout), "server") {
			test.Errorf("nerdctl version output unexpected:\n%s", result.Stdout)
		}
	})

	// --- refresh-models runs in the VM --------------------------------------
	test.Run("refresh-models present + runs", func(test *testing.T) {
		// The script is installed on PATH at /usr/local/bin/refresh-models. Running
		// it re-pulls the local model into the gateway; a clean exit is the signal.
		env, code, stderr := runExec(test, work, 4*time.Minute, name, "refresh-models")
		if !assertOK(test, env, code, "workspace.exec") {
			test.Fatalf("exec refresh-models failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
		var result execResult
		env.dataInto(test, &result)
		if result.ExitCode != 0 {
			test.Errorf("in-VM `refresh-models` exit=%d\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Stdout, result.Stderr)
		}
	})

	// --- in-VM app on a published host port ---------------------------------
	test.Run("apps add openwebui reachable on host port", func(test *testing.T) {
		env, code, stderr := run(test, work, 6*time.Minute, "apps", "add", "openwebui", name)
		if !assertOK(test, env, code, "apps.add") {
			test.Fatalf("apps add openwebui failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
		var result appActionResult
		env.dataInto(test, &result)
		if result.Port <= 0 {
			test.Fatalf("apps add did not report a published host port: %+v", result)
		}
		url := fmt.Sprintf("http://127.0.0.1:%d/", result.Port)
		// Generous warm-up: the app container must pull + boot inside the VM, then
		// the port forward must come up. Accept any non-5xx (200/redirect/auth).
		reached := waitFor(func() bool {
			client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Get(url)
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode < 500
		}, 5*time.Minute)
		if !reached {
			test.Errorf("openwebui not reachable on %s within timeout (bring-up seam: in-VM app port forward)", url)
		}
	})

	// --- egress policy: deny blocks, public allows --------------------------
	test.Run("egress deny blocks, public allows", func(test *testing.T) {
		// The egress policy is applied as msb net-rules at workspace CREATE, so a
		// change needs a restart to take effect. Probe a stable public host.
		const probeHost = "https://example.com"

		// deny: set + restart, then an in-VM curl should fail.
		if env, code, _ := run(test, work, 30*time.Second, "network", "egress", "deny", name); !assertOK(test, env, code, "network.egress") {
			test.Fatalf("network egress deny failed: %+v", env.Error)
		}
		if env, code, stderr := run(test, work, 6*time.Minute, "restart", name); !assertOK(test, env, code, "workspace.restart") {
			test.Fatalf("restart after deny failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		denyEnv, _, _ := runExec(test, work, 60*time.Second, name,
			"curl", "-sS", "--max-time", "15", "-o", "/dev/null", "-w", "%{http_code}", probeHost)
		var denyResult execResult
		denyEnv.dataInto(test, &denyResult)
		if denyResult.ExitCode == 0 {
			test.Errorf("egress deny: in-VM curl to %s SUCCEEDED (exit 0) — egress not blocked\nstdout:%q stderr:%q",
				probeHost, denyResult.Stdout, denyResult.Stderr)
		}

		// public: set + restart, then the same curl should succeed.
		if env, code, _ := run(test, work, 30*time.Second, "network", "egress", "public", name); !assertOK(test, env, code, "network.egress") {
			test.Fatalf("network egress public failed: %+v", env.Error)
		}
		if env, code, stderr := run(test, work, 6*time.Minute, "restart", name); !assertOK(test, env, code, "workspace.restart") {
			test.Fatalf("restart after public failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		publicEnv, _, _ := runExec(test, work, 60*time.Second, name,
			"curl", "-sS", "--max-time", "20", "-o", "/dev/null", "-w", "%{http_code}", probeHost)
		var publicResult execResult
		publicEnv.dataInto(test, &publicResult)
		if publicResult.ExitCode != 0 {
			test.Errorf("egress public: in-VM curl to %s FAILED (exit %d) — egress not allowed\nstdout:%q stderr:%q",
				probeHost, publicResult.ExitCode, publicResult.Stdout, publicResult.Stderr)
		}
	})

	// --- stop ---------------------------------------------------------------
	test.Run("stop", func(test *testing.T) {
		env, code, stderr := run(test, work, 3*time.Minute, "stop", name)
		if !assertOK(test, env, code, "workspace.stop") {
			test.Errorf("stop failed (exit %d):\nstderr:\n%s", code, stderr)
		}
	})

	// delete --purge is handled by t.Cleanup above (so it runs even on failure).
}
