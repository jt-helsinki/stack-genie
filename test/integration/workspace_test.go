//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// Group 4: Workspace lifecycle against the real nono + Docker stack.
//
// Unlike the old Microsandbox microVM model, a workspace has no image to build and
// no persistent process to boot — `ai create` just scaffolds a project directory,
// and every agent/shell launch is its own nono session. The JSON-envelope harness
// here can drive every NON-interactive verb (create/list/sessions/network/doctor/
// delete); actually launching and exec'ing INSIDE a sandboxed session (`ai agent`/
// `ai shell`) is interactive-only (no envelope — it owns the real terminal), so
// verifying a live session's gateway auth/network enforcement end-to-end needs a
// PTY-driving harness this suite does not yet have — a hardware bring-up item,
// not covered here (see docs/HARDWARE-BRINGUP.md).
func TestGroup04WorkspaceLifecycle(test *testing.T) {
	requireStack(test)

	name := fmt.Sprintf("aip-int-%d", time.Now().Unix())
	work := test.TempDir() // a fresh dir is the workspace root

	// Cleanup: stop any live sessions and remove the workspace + its source.
	test.Cleanup(func() {
		run(test, work, 3*time.Minute, "delete", name, "--purge", "--yes")
	})

	// --- create -------------------------------------------------------------
	if !test.Run("create", func(test *testing.T) {
		env, code, stderr := run(test, work, time.Minute, "create", "--name", name)
		if !assertOK(test, env, code, "project.create") {
			test.Fatalf("create failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
	}) {
		return // no point continuing without a workspace
	}

	// --- sessions (empty before any agent/shell has run) ---------------------
	test.Run("sessions empty before any launch", func(test *testing.T) {
		env, code, stderr := run(test, work, 30*time.Second, "sessions", name)
		if !assertOK(test, env, code, "workspace.sessions") {
			test.Fatalf("sessions failed (exit %d):\nstderr:\n%s\ndata:\n%s", code, stderr, env.Data)
		}
		var data struct {
			Sessions []struct {
				SessionID string `json:"session_id"`
			} `json:"sessions"`
		}
		env.dataInto(test, &data)
		if len(data.Sessions) != 0 {
			test.Errorf("a freshly-created workspace should have no sessions yet, got %+v", data.Sessions)
		}
	})

	// --- egress policy round-trips instantly (no restart needed) -----------
	test.Run("egress deny/public round-trips", func(test *testing.T) {
		if env, code, stderr := run(test, work, 30*time.Second, "network", "egress", "deny", name); !assertOK(test, env, code, "network.egress") {
			test.Fatalf("network egress deny failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		show, code, stderr := run(test, work, 30*time.Second, "network", "show", name)
		if !assertOK(test, show, code, "network.show") {
			test.Fatalf("network show failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		var deniedState struct {
			Egress string `json:"egress"`
		}
		show.dataInto(test, &deniedState)
		if deniedState.Egress != "deny" {
			test.Errorf("network show after deny: egress = %q, want deny", deniedState.Egress)
		}

		if env, code, stderr := run(test, work, 30*time.Second, "network", "egress", "public", name); !assertOK(test, env, code, "network.egress") {
			test.Fatalf("network egress public failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		show, code, stderr = run(test, work, 30*time.Second, "network", "show", name)
		if !assertOK(test, show, code, "network.show") {
			test.Fatalf("network show failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		var publicState struct {
			Egress string `json:"egress"`
		}
		show.dataInto(test, &publicState)
		if publicState.Egress != "public" {
			test.Errorf("network show after public: egress = %q, want public", publicState.Egress)
		}
	})

	// --- doctor reports the nono runtime posture for this workspace --------
	test.Run("doctor reports nono runtime ok", func(test *testing.T) {
		env, code, stderr := run(test, work, 30*time.Second, "doctor", name)
		if !assertOK(test, env, code, "doctor") {
			test.Fatalf("doctor failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		var data struct {
			Checks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"checks"`
		}
		env.dataInto(test, &data)
		found := false
		for _, check := range data.Checks {
			if check.Name == "workspace runtime" {
				found = true
				if check.Status != "ok" {
					test.Errorf("workspace runtime check = %q, want ok", check.Status)
				}
			}
		}
		if !found {
			test.Error("doctor report missing the workspace runtime check")
		}
	})

	// delete --purge is handled by t.Cleanup above (so it runs even on failure).
}
