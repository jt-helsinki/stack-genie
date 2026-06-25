package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// runApps drives a single `ai apps <args...>` invocation against a fresh command
// tree, returning the exit code the command set.
func runApps(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newAppsCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("apps %v returned error: %v", args, err)
	}
	return exit
}

// seedStoppedWorkspace registers a project with an app installed but no running
// microVM (so List works without a live VM, the lifecycle verbs report not-running).
func seedStoppedWorkspace(test *testing.T, project string, appKeys ...string) string {
	test.Helper()
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := filepath.Join(home, "projects", project)
	entries := make([]config.AppEntry, 0, len(appKeys))
	port := 21000
	for _, key := range appKeys {
		entries = append(entries, config.AppEntry{Key: key, Port: port})
		port++
	}
	if err := config.WriteProject(root, &config.Config{OS: "debian-trixie", Apps: entries}); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects[project] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
	return root
}

func TestAppsUnknownAction(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runApps(test, "frobnicate", "openwebui"); exit != output.ExitInvalidInput {
		test.Fatalf("unknown action exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestAppsUnknownApp(test *testing.T) {
	seedStoppedWorkspace(test, "demo")
	if exit := runApps(test, "add", "nope", "demo"); exit != output.ExitInvalidInput {
		test.Fatalf("unknown app exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestAppsMissingAppArg(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runApps(test, "add"); exit != output.ExitInvalidInput {
		test.Fatalf("missing app exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestAppsUnknownWorkspace(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runApps(test, "list", "ghost"); exit != output.ExitInvalidInput {
		test.Fatalf("unknown workspace exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestAppsListStoppedWorkspace(test *testing.T) {
	seedStoppedWorkspace(test, "demo", "openwebui")
	// List works against a stopped workspace (no live VM probe) — exits 0.
	if exit := runApps(test, "list", "demo"); exit != output.ExitOK {
		test.Fatalf("apps list exit = %d, want 0", exit)
	}
}

func TestAppsStartNeedsRunningWorkspace(test *testing.T) {
	seedStoppedWorkspace(test, "demo", "openwebui")
	// Lifecycle verbs need a running microVM → exit 3 (missing dep).
	if exit := runApps(test, "start", "openwebui", "demo"); exit != output.ExitMissingDep {
		test.Fatalf("apps start (stopped) exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

func TestAppsResultHuman(test *testing.T) {
	result := appsResult{Project: "demo", Apps: []apps.Status{
		{Key: "openwebui", Name: "Open WebUI", Installed: true, Running: true, Port: 21000, URL: "http://localhost:21000"},
		{Key: "anythingllm", Name: "AnythingLLM"},
	}}
	human := result.Human()
	for _, want := range []string{"Open WebUI", "running", "http://localhost:21000", "AnythingLLM", "not installed"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

func TestAppActionResultHumanRestartHint(test *testing.T) {
	result := appActionResult{Project: "demo", App: "openwebui", Action: "add", Port: 21000, RestartRequired: true}
	human := result.Human()
	if !strings.Contains(human, "host port 21000") {
		test.Fatalf("Human() missing port: %s", human)
	}
	if !strings.Contains(human, "ai restart demo") {
		test.Fatalf("Human() missing restart hint: %s", human)
	}
}
