package workspace

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// TestAppManagerForUnknownProject verifies AppManagerFor maps an unknown project
// to ErrUnknownProject (→ exit 2) before wiring anything.
func TestAppManagerForUnknownProject(test *testing.T) {
	seedProject(test, "app") // sets HOME + registers "app", so "nope" is genuinely unknown
	manager := newManager(&fakeBuilder{}, &fakeSandbox{})
	if _, err := manager.AppManagerFor("nope"); !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("AppManagerFor(unknown) err = %v, want ErrUnknownProject", err)
	}
}

// TestAppManagerForNotRunning covers buildAppManager's forceExec=false branch: a
// project whose workspace is NOT running yields an apps.Manager with no in-VM exec
// wired, so List degrades to installed-but-not-running without touching the sandbox.
func TestAppManagerForNotRunning(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:   "debian-trixie",
		Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)

	appManager, err := manager.AppManagerFor("app")
	if err != nil {
		test.Fatalf("AppManagerFor(not running): %v", err)
	}
	if appManager == nil {
		test.Fatal("AppManagerFor returned a nil manager")
	}
	statuses, err := appManager.List()
	if err != nil {
		test.Fatalf("List on a not-running workspace: %v", err)
	}
	// Exec was not wired (forceExec=false), so no in-VM probe ran.
	if len(sandbox.execRootArgv) != 0 {
		test.Errorf("not-running AppManager must not probe the VM, got exec calls %v", sandbox.execRootArgv)
	}
	var found bool
	for _, status := range statuses {
		if status.Key == "openwebui" {
			found = true
			if !status.Installed {
				test.Error("openwebui should report installed")
			}
			if status.Running {
				test.Error("a not-running workspace must report its app as not running")
			}
		}
	}
	if !found {
		test.Errorf("List did not include the installed openwebui app: %+v", statuses)
	}
}

// TestAppManagerForRunningWiresExec covers buildAppManager's forceExec=true branch:
// a RUNNING workspace wires the in-VM exec + bounded probe, so List runs the
// `nerdctl ps` probe through the sandbox (exercising the probeExec closure).
func TestAppManagerForRunningWiresExec(test *testing.T) {
	root := seedStartedWorkspace(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:   "debian-trixie",
		Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}},
	}); err != nil {
		test.Fatal(err)
	}
	// The probe reports one running container by its container name.
	sandbox := &fakeSandbox{execRootCtxResult: ExecResult{Stdout: "aip-app-openwebui\n"}}
	manager := newManager(&fakeBuilder{}, sandbox)

	appManager, err := manager.AppManagerFor("app")
	if err != nil {
		test.Fatalf("AppManagerFor(running): %v", err)
	}
	statuses, err := appManager.List()
	if err != nil {
		test.Fatalf("List on a running workspace: %v", err)
	}
	// The wired probeExec closure must have run the in-VM `nerdctl ps` probe.
	if len(sandbox.execRootArgv) == 0 {
		test.Fatal("running AppManager must probe the VM via the wired exec closure")
	}
	var sawProbe bool
	for _, call := range sandbox.execRootArgv {
		if strings.Contains(strings.Join(call, " "), "nerdctl ps") {
			sawProbe = true
		}
	}
	if !sawProbe {
		test.Errorf("expected a `nerdctl ps` probe, got %v", sandbox.execRootArgv)
	}
	var running bool
	for _, status := range statuses {
		if status.Key == "openwebui" {
			running = status.Running
		}
	}
	if !running {
		test.Error("openwebui should report running when the probe lists its container")
	}
}

// TestAppManagerRunningStartsAppContainer covers buildAppManager's forceExec=true
// exec closure: a running workspace wires Exec to the sandbox's root exec, so an app
// Start issues `nerdctl start` in the VM and succeeds on a zero exit.
func TestAppManagerRunningStartsAppContainer(test *testing.T) {
	root := seedStartedWorkspace(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:   "debian-trixie",
		Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{execRootResult: ExecResult{ExitCode: 0}}
	manager := newManager(&fakeBuilder{}, sandbox)

	appManager, err := manager.AppManagerFor("app")
	if err != nil {
		test.Fatalf("AppManagerFor(running): %v", err)
	}
	if err := appManager.Start("openwebui"); err != nil {
		test.Fatalf("app Start: %v", err)
	}
	// The exec closure ran `nerdctl start <container>` through the sandbox root exec.
	var started bool
	for _, call := range sandbox.execRootArgv {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "nerdctl start") && strings.Contains(joined, "aip-app-openwebui") {
			started = true
		}
	}
	if !started {
		test.Errorf("expected a `nerdctl start aip-app-openwebui` exec, got %v", sandbox.execRootArgv)
	}
}

// TestRefreshAgentModelsUnknownProject verifies the resolve-error guard: an unknown
// project short-circuits with no panic and no writes.
func TestRefreshAgentModelsUnknownProject(test *testing.T) {
	seedProject(test, "app") // sets HOME; "nope" stays unknown
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)
	manager.refreshAgentModels("nope") // must not panic
	if len(sandbox.written) != 0 {
		test.Errorf("unknown project must not write any config, got %v", sandbox.written)
	}
}

// TestRefreshAgentModelsSelfHealsOrphanedKey covers the self-heal branch: the baked
// gateway key is read from the VM but the gateway reports it invalid (KeyInfo errors),
// so refresh re-registers the providers (re-minting the scoped key) and returns early.
func TestRefreshAgentModelsSelfHealsOrphanedKey(test *testing.T) {
	root := seedProject(test, "app")
	// currentAgentKey reads a non-empty key from the VM; KeyInfo then errors → orphaned.
	sandbox := &fakeSandbox{execResult: ExecResult{Stdout: "sk-dead\n"}}
	minter := &fakeKeyMinter{keyInfoErr: errors.New("key not found")}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: minter,
		Served: fakeServedModels{models: []string{"vllm/heal:1"}},
		Now:    func() string { return "t" },
	}

	manager.refreshAgentModels("app")

	// Self-heal must re-mint the scoped key (registerAgentProviders path).
	if minter.calls == 0 {
		test.Error("self-heal must re-mint the scoped gateway key")
	}
	if minter.keyInfoCalls == 0 {
		test.Error("self-heal must probe the key via KeyInfo")
	}
	// registerAgentProviders rewrote the opencode config with the served model.
	openCode := readProjectConfig(test, root, ".opencode", "opencode.json")
	if !strings.Contains(openCode, "vllm/heal:1") {
		test.Errorf("self-heal did not rewrite the opencode model list:\n%s", openCode)
	}
}

// TestRefreshAgentModelsSelfHealFailureFallsThrough covers the branch where the key
// looks orphaned but the re-mint fails: refresh warns and falls through to the
// list-only writeModelListConfigs, which still refreshes the served list.
func TestRefreshAgentModelsSelfHealFailureFallsThrough(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{Stdout: "sk-dead\n"}}
	// orphaned (KeyInfo errors) AND the re-mint fails (GenerateKey errors).
	minter := &fakeKeyMinter{keyInfoErr: errors.New("key not found"), lastErr: errors.New("mint gateway down")}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: minter,
		Served: fakeServedModels{models: []string{"vllm/fallthrough:1"}},
		Now:    func() string { return "t" },
	}

	manager.refreshAgentModels("app") // must not panic despite the failed re-mint

	// The fall-through list-only refresh still writes the served list.
	openCode := readProjectConfig(test, root, ".opencode", "opencode.json")
	if !strings.Contains(openCode, "vllm/fallthrough:1") {
		test.Errorf("fall-through refresh did not write the served list:\n%s", openCode)
	}
}

// TestRefreshAgentModelsEmptyServedLeavesUntouched covers the healthy-key + empty
// picker path: with no ServedModels source the picker is empty, and the opencode
// list from a prior start is LEFT UNTOUCHED (a transient outage never wipes a good
// list).
func TestRefreshAgentModelsEmptyServedLeavesUntouched(test *testing.T) {
	root := seedProject(test, "app")

	// Prime a good opencode list via a full start with a served model.
	primed := Manager{
		Builder: &fakeBuilder{}, Sandbox: &fakeSandbox{}, Keys: &fakeKeyMinter{},
		Served: fakeServedModels{models: []string{"vllm/keep:1"}},
		Now:    func() string { return "t" },
	}
	if _, err := primed.Start("app"); err != nil {
		test.Fatalf("prime Start: %v", err)
	}
	if openCode := readProjectConfig(test, root, ".opencode", "opencode.json"); !strings.Contains(openCode, "vllm/keep:1") {
		test.Fatalf("prime did not seed the model list:\n%s", openCode)
	}

	// Refresh with NO ServedModels source (nil) → empty picker → must not wipe the list.
	// A fresh sandbox with empty exec stdout makes currentAgentKey return "" (healthy,
	// self-heal skipped), so this exercises the list-only path.
	refresher := Manager{
		Builder: &fakeBuilder{}, Sandbox: &fakeSandbox{}, Keys: &fakeKeyMinter{},
		Now: func() string { return "t" },
	}
	refresher.refreshAgentModels("app")

	openCode := readProjectConfig(test, root, ".opencode", "opencode.json")
	if !strings.Contains(openCode, "vllm/keep:1") {
		test.Errorf("empty-served refresh must leave the existing list untouched:\n%s", openCode)
	}
}
