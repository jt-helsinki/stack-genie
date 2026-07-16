//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the black-box, NON-INTERACTIVE workspace create/teardown suite. Unlike
// workspace_test.go (which does a full create→start→…→delete against a live microVM and
// takes many minutes), these scenarios exercise ONLY `ai create` (scaffold-only — it
// writes <project>/.ai-platform and registers the project; it does NOT build or boot a
// microVM) and `ai delete`/`destroy` (teardown). They therefore need no running service
// tier or msb — only that `ai setup` has installed the on-disk templates — so they run
// fast and everywhere the platform is set up. Everything is driven through the built `ai`
// binary with `--json`; no TUI/PTY is used, and each scenario cleans up after itself.

// requireSetup skips unless `ai setup` has installed the on-disk templates that `ai
// create` composes the project Dockerfile from. Create/teardown need those templates but
// NOT a running stack (no docker containers, no microVM), so this is a lighter gate than
// requireStack.
func requireSetup(test *testing.T) {
	test.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		test.Skipf("cannot resolve HOME: %v", err)
	}
	base := filepath.Join(home, ".ai-platform", "templates", "dockerfiles", "debian-trixie", "Dockerfile")
	if _, err := os.Stat(base); err != nil {
		test.Skipf("platform templates not installed (run `ai setup`): %v", err)
	}
}

// nameCounter makes per-call workspace names unique within a run.
var nameCounter int

// uniqueName returns a short, projects.yaml-safe workspace name: workspace names must be
// lowercase alphanumeric + hyphens, 2-40 chars, no leading/trailing hyphen (invalid → the
// CLI exits 2). The pid + counter keep it unique both within a run and across runs, while
// staying well under the 40-char cap (leaving room for callers that append e.g. `-outer`).
func uniqueName(test *testing.T) string {
	test.Helper()
	nameCounter++
	return fmt.Sprintf("ittest-%d-%d", os.Getpid(), nameCounter)
}

// cleanupWorkspace best-effort tears a workspace down (deregister + remove .ai-platform),
// tolerating "already gone". Registered as a t.Cleanup so a failing assertion never leaks
// a project into the real projects index.
func cleanupWorkspace(test *testing.T, name string) {
	test.Helper()
	test.Cleanup(func() {
		run(test, "", 3*time.Minute, "delete", name, "--purge", "--yes")
	})
}

// createWorkspace runs a non-interactive `ai create` at loc with the given extra flags.
func createWorkspace(test *testing.T, name, loc string, extra ...string) (Envelope, int, string) {
	test.Helper()
	args := append([]string{"create", "--name", name, "--os", "debian-trixie", "--location", loc}, extra...)
	return run(test, "", 4*time.Minute, args...)
}

// listNames returns the registered workspace names from `ai list`.
func listNames(test *testing.T) []string {
	test.Helper()
	env, code, _ := run(test, "", time.Minute, "list")
	assertOK(test, env, code, "project.list")
	var data struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	env.dataInto(test, &data)
	names := make([]string, 0, len(data.Projects))
	for _, project := range data.Projects {
		names = append(names, project.Name)
	}
	return names
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// TestWorkspaceCreateTeardown drives the create/teardown surface through the built binary
// with --json, one scenario per subtest.
func TestWorkspaceCreateTeardown(test *testing.T) {
	requireSetup(test)

	// 1. create scaffolds the tracked .ai-platform files, registers the project, and a
	//    plain delete deregisters it + removes .ai-platform while LEAVING the user's own
	//    files untouched.
	test.Run("create_scaffolds_then_delete_keeps_user_files", func(test *testing.T) {
		name := uniqueName(test)
		loc := test.TempDir()
		sentinel := filepath.Join(loc, "USER_FILE.txt")
		if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
			test.Fatal(err)
		}
		cleanupWorkspace(test, name)

		env, code, stderr := createWorkspace(test, name, loc)
		if !assertOK(test, env, code, "project.create") {
			test.Fatalf("create failed: %s", stderr)
		}
		for _, rel := range []string{"config.yaml", "Dockerfile", "project.yaml"} {
			if _, err := os.Stat(filepath.Join(loc, ".ai-platform", rel)); err != nil {
				test.Errorf("create did not scaffold .ai-platform/%s: %v", rel, err)
			}
		}
		if !contains(listNames(test), name) {
			test.Errorf("created workspace %q not in `ai list`", name)
		}

		env, code, _ = run(test, "", 3*time.Minute, "delete", name, "--yes")
		assertOK(test, env, code, "project.delete")
		if _, err := os.Stat(filepath.Join(loc, ".ai-platform")); !os.IsNotExist(err) {
			test.Errorf("delete should remove .ai-platform, stat err=%v", err)
		}
		if _, err := os.Stat(sentinel); err != nil {
			test.Errorf("delete (no --purge) must keep the user's files: %v", err)
		}
		if contains(listNames(test), name) {
			test.Errorf("deleted workspace %q still registered", name)
		}
	})

	// 2. delete --purge removes the whole project directory (including the user's files).
	test.Run("delete_purge_removes_project_dir", func(test *testing.T) {
		name := uniqueName(test)
		// A dir we own and can let --purge delete (NOT t.TempDir, which the framework
		// also cleans — we assert the purge removed it).
		loc := filepath.Join(test.TempDir(), "proj")
		if err := os.MkdirAll(loc, 0o755); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(loc, "app.py"), []byte("x=1"), 0o644); err != nil {
			test.Fatal(err)
		}
		cleanupWorkspace(test, name)

		if env, code, stderr := createWorkspace(test, name, loc); !assertOK(test, env, code, "project.create") {
			test.Fatalf("create failed: %s", stderr)
		}
		env, code, _ := run(test, "", 3*time.Minute, "delete", name, "--purge", "--yes")
		assertOK(test, env, code, "project.delete")
		if _, err := os.Stat(loc); !os.IsNotExist(err) {
			test.Errorf("--purge should remove the project dir %q, stat err=%v", loc, err)
		}
	})

	// 3. non-interactive create REQUIRES --os; a missing --os is invalid input (exit 2).
	test.Run("missing_os_exits_2", func(test *testing.T) {
		loc := test.TempDir()
		env, code, _ := run(test, "", time.Minute, "create", "--name", uniqueName(test), "--location", loc)
		assertError(test, env, code, 2)
		if _, err := os.Stat(filepath.Join(loc, ".ai-platform")); !os.IsNotExist(err) {
			test.Errorf("a rejected create must not scaffold anything")
		}
	})

	// 4. an unknown --os is invalid input (exit 2).
	test.Run("unknown_os_exits_2", func(test *testing.T) {
		loc := test.TempDir()
		env, code, _ := run(test, "", time.Minute, "create",
			"--name", uniqueName(test), "--os", "no-such-os", "--location", loc)
		assertError(test, env, code, 2)
	})

	// 5. selected agents + opt-in tools persist to the tracked config.yaml.
	test.Run("flags_persist_to_config", func(test *testing.T) {
		name := uniqueName(test)
		loc := test.TempDir()
		cleanupWorkspace(test, name)
		env, code, stderr := createWorkspace(test, name, loc,
			"--agents", "opencode,codex", "--code-review-graph", "--codebase-memory")
		if !assertOK(test, env, code, "project.create") {
			test.Fatalf("create failed: %s", stderr)
		}
		configBytes, err := os.ReadFile(filepath.Join(loc, ".ai-platform", "config.yaml"))
		if err != nil {
			test.Fatal(err)
		}
		config := string(configBytes)
		for _, want := range []string{"opencode", "codex", "code_review_graph_enabled: true", "codebase_memory_enabled: true"} {
			if !strings.Contains(config, want) {
				test.Errorf("config.yaml missing %q:\n%s", want, config)
			}
		}
	})

	// 6. a location that is (or is nested inside) an existing workspace is rejected (exit 2).
	test.Run("nested_location_rejected", func(test *testing.T) {
		outer := uniqueName(test) + "-outer"
		loc := test.TempDir()
		cleanupWorkspace(test, outer)
		if env, code, stderr := createWorkspace(test, outer, loc); !assertOK(test, env, code, "project.create") {
			test.Fatalf("outer create failed: %s", stderr)
		}
		nested := filepath.Join(loc, "sub")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			test.Fatal(err)
		}
		env, code, _ := run(test, "", time.Minute, "create",
			"--name", uniqueName(test)+"-inner", "--os", "debian-trixie", "--location", nested)
		assertError(test, env, code, 2)
	})

	// 7. creating a second workspace at the same location is rejected (already a workspace).
	test.Run("duplicate_location_rejected", func(test *testing.T) {
		name := uniqueName(test)
		loc := test.TempDir()
		cleanupWorkspace(test, name)
		if env, code, stderr := createWorkspace(test, name, loc); !assertOK(test, env, code, "project.create") {
			test.Fatalf("first create failed: %s", stderr)
		}
		env, code, _ := createWorkspace(test, name+"-again", loc)
		if env.OK || code == 0 {
			test.Errorf("creating a second workspace at an existing location must fail, got ok=%v exit=%d", env.OK, code)
		}
	})

	// 8. --dry-run emits the plan but writes NOTHING.
	test.Run("dry_run_has_no_side_effects", func(test *testing.T) {
		name := uniqueName(test)
		loc := test.TempDir()
		env, code, stderr := createWorkspace(test, name, loc, "--dry-run")
		if !assertOK(test, env, code, "project.create") {
			test.Fatalf("dry-run create failed: %s", stderr)
		}
		if _, err := os.Stat(filepath.Join(loc, ".ai-platform")); !os.IsNotExist(err) {
			test.Errorf("--dry-run must not scaffold .ai-platform")
		}
		if contains(listNames(test), name) {
			test.Errorf("--dry-run must not register the workspace")
		}
	})

	// 9. deleting an unknown workspace is an error, not a silent success.
	test.Run("delete_unknown_errors", func(test *testing.T) {
		env, code, _ := run(test, "", time.Minute, "delete", "ittest-does-not-exist-"+uniqueName(test), "--yes")
		if env.OK || code == 0 {
			test.Errorf("deleting an unknown workspace must fail, got ok=%v exit=%d", env.OK, code)
		}
	})

	// 10. `destroy` is an alias of `delete` (same teardown).
	test.Run("destroy_alias_tears_down", func(test *testing.T) {
		name := uniqueName(test)
		loc := test.TempDir()
		cleanupWorkspace(test, name)
		if env, code, stderr := createWorkspace(test, name, loc); !assertOK(test, env, code, "project.create") {
			test.Fatalf("create failed: %s", stderr)
		}
		env, code, _ := run(test, "", 3*time.Minute, "destroy", name, "--purge", "--yes")
		assertOK(test, env, code, "project.delete")
		if contains(listNames(test), name) {
			test.Errorf("destroy should deregister %q", name)
		}
	})
}
