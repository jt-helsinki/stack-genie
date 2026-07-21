package apps

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// fakeExec records the nerdctl argv it is handed and returns a scripted result.
type fakeExec struct {
	calls    [][]string
	result   ExecResult
	err      error
	psOutput string // returned for `nerdctl ps`
}

func (exec *fakeExec) run(argv []string) (ExecResult, error) {
	exec.calls = append(exec.calls, argv)
	if len(argv) >= 2 && argv[0] == "nerdctl" && argv[1] == "ps" {
		return ExecResult{Stdout: exec.psOutput}, nil
	}
	if len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "nerdctl ps") {
		return ExecResult{Stdout: exec.psOutput}, nil
	}
	return exec.result, exec.err
}

// newTestDeps builds Deps over an in-memory project config and a controllable
// always-free port checker, returning the Deps plus a pointer to the stored
// config so tests can assert persistence.
func newTestDeps(initial *config.Config, exec ExecRunner) (Deps, *config.Config) {
	stored := initial
	if stored == nil {
		stored = &config.Config{}
	}
	return Deps{
		LoadConfig: func() (*config.Config, error) {
			// Return a copy so the manager mutating it does not surprise the test
			// before SaveConfig.
			copied := *stored
			copied.Apps = append([]config.AppEntry(nil), stored.Apps...)
			return &copied, nil
		},
		SaveConfig: func(updated *config.Config) error {
			*stored = *updated
			return nil
		},
		ReservedPorts: func() (map[int]bool, error) { return map[int]bool{}, nil },
		PortFree:      func(int) bool { return true },
		Exec:          exec,
		Gateway: func() (string, string, string, error) {
			return "http://gw/v1", "sk-key", "gemma4", nil
		},
	}, stored
}

func TestInstallAllocatesPortAndPersists(test *testing.T) {
	exec := &fakeExec{}
	deps, stored := newTestDeps(nil, exec.run)
	manager := NewManager(deps)

	port, restart, err := manager.Install("openwebui")
	if err != nil {
		test.Fatal(err)
	}
	if port < portRangeStart || port > portRangeEnd {
		test.Fatalf("port %d outside allocation window", port)
	}
	if !restart {
		test.Fatal("Install should report restartRequired (published-port set changed)")
	}
	if len(stored.Apps) != 1 || stored.Apps[0].Key != "openwebui" || stored.Apps[0].Port != port {
		test.Fatalf("config not persisted correctly: %+v", stored.Apps)
	}
	// Running with the VM up should have launched the container.
	if !ranContainer(exec, "aip-app-openwebui") {
		test.Fatal("Install with a running VM should run the container")
	}
}

func TestInstallRejectsDuplicate(test *testing.T) {
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, nil)
	manager := NewManager(deps)
	_, _, err := manager.Install("openwebui")
	if !errors.Is(err, ErrAlreadyInstalled) {
		test.Fatalf("err = %v, want ErrAlreadyInstalled", err)
	}
}

func TestInstallRejectsUnknown(test *testing.T) {
	deps, _ := newTestDeps(nil, nil)
	manager := NewManager(deps)
	_, _, err := manager.Install("nope")
	if !errors.Is(err, ErrUnknownApp) {
		test.Fatalf("err = %v, want ErrUnknownApp", err)
	}
}

func TestInstallNothingPublishedWhenVMDown(test *testing.T) {
	// Exec nil = VM down; Install still records + allocates but starts nothing.
	deps, stored := newTestDeps(nil, nil)
	deps.Exec = nil
	manager := NewManager(deps)
	port, _, err := manager.Install("anythingllm")
	if err != nil {
		test.Fatal(err)
	}
	if len(stored.Apps) != 1 || stored.Apps[0].Port != port {
		test.Fatalf("entry not persisted: %+v", stored.Apps)
	}
}

func TestRemoveFreesPortAndContainer(test *testing.T) {
	exec := &fakeExec{}
	deps, stored := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	manager := NewManager(deps)
	restart, err := manager.Remove("openwebui")
	if err != nil {
		test.Fatal(err)
	}
	if !restart {
		test.Fatal("Remove should report restartRequired")
	}
	if len(stored.Apps) != 0 {
		test.Fatalf("app not removed: %+v", stored.Apps)
	}
	if !calledWith(exec, "nerdctl", "rm", "-f", "aip-app-openwebui") {
		test.Fatal("Remove should rm -f the container")
	}
}

func TestRemoveNotInstalled(test *testing.T) {
	deps, _ := newTestDeps(nil, nil)
	manager := NewManager(deps)
	_, err := manager.Remove("openwebui")
	if !errors.Is(err, ErrNotInstalled) {
		test.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

func TestLifecycleNeedsRunningWorkspace(test *testing.T) {
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, nil)
	deps.Exec = nil // VM down
	manager := NewManager(deps)
	for _, action := range []func(string) error{manager.Start, manager.Stop, manager.Restart, manager.Update} {
		if err := action("openwebui"); !errors.Is(err, ErrWorkspaceNotRunning) {
			test.Fatalf("action err = %v, want ErrWorkspaceNotRunning", err)
		}
	}
}

func TestUpdatePullsAndRecreates(test *testing.T) {
	exec := &fakeExec{}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	manager := NewManager(deps)
	if err := manager.Update("openwebui"); err != nil {
		test.Fatal(err)
	}
	if !calledWith(exec, "nerdctl", "pull", "ghcr.io/open-webui/open-webui:latest") {
		test.Fatal("Update should pull the image")
	}
	if !ranContainer(exec, "aip-app-openwebui") {
		test.Fatal("Update should recreate the container")
	}
}

func TestStopUsesNerdctlStop(test *testing.T) {
	exec := &fakeExec{}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	manager := NewManager(deps)
	if err := manager.Stop("openwebui"); err != nil {
		test.Fatal(err)
	}
	if !calledWith(exec, "nerdctl", "stop", "aip-app-openwebui") {
		test.Fatal("Stop should call nerdctl stop")
	}
}

func TestListReportsInstalledAndRunning(test *testing.T) {
	exec := &fakeExec{psOutput: "aip-app-openwebui\n"}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	manager := NewManager(deps)
	statuses, err := manager.List()
	if err != nil {
		test.Fatal(err)
	}
	if len(statuses) != 2 {
		test.Fatalf("List returned %d, want 2 (all catalogue apps)", len(statuses))
	}
	var openwebui, anythingllm Status
	for _, status := range statuses {
		switch status.Key {
		case "openwebui":
			openwebui = status
		case "anythingllm":
			anythingllm = status
		}
	}
	if !openwebui.Installed || !openwebui.Running {
		test.Fatalf("openwebui status = %+v, want installed+running", openwebui)
	}
	if openwebui.URL != "http://localhost:21000" {
		test.Fatalf("openwebui URL = %q", openwebui.URL)
	}
	if anythingllm.Installed {
		test.Fatalf("anythingllm should not be installed: %+v", anythingllm)
	}
}

// When the running-status probe fails (a busy/wedged VM), List returns the probe
// error so the caller can classify it — rather than silently misreporting every app
// as stopped.
func TestListPropagatesProbeError(test *testing.T) {
	probeErr := errors.New("workspace is running but not responding")
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, func(argv []string) (ExecResult, error) {
		return ExecResult{}, nil // unbounded Exec is never the probe here
	})
	deps.ProbeExec = func(argv []string) (ExecResult, error) {
		return ExecResult{}, probeErr
	}
	manager := NewManager(deps)
	if _, err := manager.List(); !errors.Is(err, probeErr) {
		test.Fatalf("List must surface the probe error, got %v", err)
	}
}

// When ProbeExec is set, List's running-status probe uses it (the bounded path), not
// the unbounded Exec.
func TestListUsesProbeExecWhenSet(test *testing.T) {
	probe := &fakeExec{psOutput: "aip-app-openwebui\n"}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, func(argv []string) (ExecResult, error) {
		test.Fatalf("Exec must not be used for the running-status probe when ProbeExec is set; got %v", argv)
		return ExecResult{}, nil
	})
	deps.ProbeExec = probe.run
	manager := NewManager(deps)
	statuses, err := manager.List()
	if err != nil {
		test.Fatal(err)
	}
	var openwebui Status
	for _, status := range statuses {
		if status.Key == "openwebui" {
			openwebui = status
		}
	}
	if !openwebui.Running {
		test.Fatalf("openwebui should be running via the ProbeExec result: %+v", openwebui)
	}
}

func TestStartInstalledBestEffort(test *testing.T) {
	// The second app's run fails; the first must still be attempted and no error
	// returned (best-effort), with a warning collected.
	exec := &failOnExec{failContainer: "aip-app-anythingllm"}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{
		{Key: "openwebui", Port: 21000},
		{Key: "anythingllm", Port: 21001},
	}}, exec.run)
	manager := NewManager(deps)
	warnings, err := manager.StartInstalled()
	if err != nil {
		test.Fatalf("StartInstalled returned error %v, want best-effort nil", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "anythingllm") {
		test.Fatalf("warnings = %v, want one mentioning anythingllm", warnings)
	}
	if !ranContainer(&fakeExec{calls: exec.calls}, "aip-app-openwebui") {
		test.Fatal("openwebui should still have been started")
	}
}

func TestPublishedPortsOnlyInstalled(test *testing.T) {
	cfg := &config.Config{Apps: []config.AppEntry{
		{Key: "anythingllm", Port: 21005},
		{Key: "openwebui", Port: 21002},
		{Key: "ghost", Port: 21099}, // unknown key — not published
	}}
	mappings := PublishedPorts(cfg)
	if len(mappings) != 2 {
		test.Fatalf("PublishedPorts = %v, want 2 (unknown key skipped)", mappings)
	}
	// Sorted by host port.
	if mappings[0].Host != 21002 || mappings[1].Host != 21005 {
		test.Fatalf("mappings not sorted by host port: %v", mappings)
	}
	// Host == guest (same number both sides of the msb publish).
	for _, mapping := range mappings {
		if mapping.Host != mapping.Guest {
			test.Fatalf("mapping host %d != guest %d", mapping.Host, mapping.Guest)
		}
	}
}

func TestPublishedPortsEmptyForNoApps(test *testing.T) {
	if mappings := PublishedPorts(&config.Config{}); len(mappings) != 0 {
		test.Fatalf("PublishedPorts = %v, want empty when no apps installed", mappings)
	}
}

// --- helpers ---

// failOnExec fails the `nerdctl run` for a specific container name and records
// every call.
type failOnExec struct {
	calls         [][]string
	failContainer string
}

func (exec *failOnExec) run(argv []string) (ExecResult, error) {
	exec.calls = append(exec.calls, argv)
	if len(argv) >= 2 && argv[0] == "nerdctl" && argv[1] == "run" {
		for _, arg := range argv {
			if arg == exec.failContainer {
				return ExecResult{ExitCode: 1, Stderr: "boom"}, nil
			}
		}
	}
	return ExecResult{}, nil
}

func ranContainer(exec *fakeExec, name string) bool {
	for _, call := range exec.calls {
		if len(call) >= 2 && call[0] == "nerdctl" && call[1] == "run" {
			for _, arg := range call {
				if arg == name {
					return true
				}
			}
		}
	}
	return false
}

func calledWith(exec *fakeExec, want ...string) bool {
	for _, call := range exec.calls {
		if equalPrefix(call, want) {
			return true
		}
	}
	return false
}

func equalPrefix(call, want []string) bool {
	if len(call) < len(want) {
		return false
	}
	for index := range want {
		if call[index] != want[index] {
			return false
		}
	}
	return true
}
