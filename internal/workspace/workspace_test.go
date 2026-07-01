package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/agentcfg"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/state"
)

type fakeBuilder struct {
	built bool
	err   error
}

func (builder *fakeBuilder) Build(string, string) error {
	builder.built = true
	return builder.err
}

type fakeSandbox struct {
	created, started, stopped, destroyed bool
	projectMount                         string
	overlayMount                         string
	resources                            VMResources
	netArgs                              []string
	execResult                           ExecResult
	execErr                              error
	execArgv                             []string   // the LAST Exec/ExecContext argv
	allExecArgv                          [][]string // every Exec/ExecContext argv, in order
	interactiveArgv                      []string
	execRootArgv                         [][]string // every ExecRoot call's argv, in order
	execRootResult                       ExecResult
	execRootErr                          error
	execRootCtxResult                    ExecResult
	execRootCtxErr                       error
	// isRunning controls the IsRunning liveness probe: it reports this value (default
	// true — VM present) unless isRunningErr is set. The classification path uses it
	// AFTER an in-VM exec fails to tell a stale VM (false) from an overloaded one
	// (true).
	isRunning        bool
	isRunningSet     bool
	isRunningErr     error
	clockSynced      bool
	execCtxCalls     int
	execCtxFailFirst int
	written          map[string][]byte
	inspectPolicy    NetworkPolicy
	inspectErr       error
	logTail          string
	logTailLines     int
	logTailErr       error
}

func (sandbox *fakeSandbox) Create(_, _, projectMount, overlayPath string, resources VMResources, netArgs []string) error {
	sandbox.created = true
	sandbox.projectMount = projectMount
	sandbox.overlayMount = overlayPath
	sandbox.resources = resources
	sandbox.netArgs = netArgs
	return nil
}
func (sandbox *fakeSandbox) Start(string) error   { sandbox.started = true; return nil }
func (sandbox *fakeSandbox) Stop(string) error    { sandbox.stopped = true; return nil }
func (sandbox *fakeSandbox) Destroy(string) error { sandbox.destroyed = true; return nil }
func (sandbox *fakeSandbox) Exec(_ string, argv []string) (ExecResult, error) {
	sandbox.execArgv = argv
	sandbox.allExecArgv = append(sandbox.allExecArgv, argv)
	return sandbox.execResult, sandbox.execErr
}
func (sandbox *fakeSandbox) ExecContext(_ context.Context, _ string, argv []string) (ExecResult, error) {
	sandbox.execArgv = argv
	sandbox.allExecArgv = append(sandbox.allExecArgv, argv)
	sandbox.execCtxCalls++
	// Simulate a post-sleep stale connection: the first execCtxFailFirst calls time
	// out (ErrWorkspaceUnresponsive), exercising probeInVM's retry/recovery.
	if sandbox.execCtxCalls <= sandbox.execCtxFailFirst {
		return ExecResult{}, ErrWorkspaceUnresponsive
	}
	return sandbox.execResult, sandbox.execErr
}
func (sandbox *fakeSandbox) ExecRoot(_ string, argv []string) (ExecResult, error) {
	sandbox.execRootArgv = append(sandbox.execRootArgv, argv)
	return sandbox.execRootResult, sandbox.execRootErr
}
func (sandbox *fakeSandbox) ExecRootContext(_ context.Context, _ string, argv []string) (ExecResult, error) {
	sandbox.execRootArgv = append(sandbox.execRootArgv, argv)
	return sandbox.execRootCtxResult, sandbox.execRootCtxErr
}
func (sandbox *fakeSandbox) SyncClock(string) error { sandbox.clockSynced = true; return nil }
func (sandbox *fakeSandbox) IsRunning(context.Context, string) (bool, error) {
	if sandbox.isRunningErr != nil {
		return false, sandbox.isRunningErr
	}
	// Default to "present" so the happy-path classification (an exec that fails for a
	// reason other than a missing VM) reports unresponsive, not stale.
	if !sandbox.isRunningSet {
		return true, nil
	}
	return sandbox.isRunning, nil
}
func (sandbox *fakeSandbox) ExecInteractive(_ string, argv []string) error {
	sandbox.interactiveArgv = argv
	return nil
}
func (sandbox *fakeSandbox) WriteFile(_, guestPath string, content []byte) error {
	if sandbox.written == nil {
		sandbox.written = map[string][]byte{}
	}
	sandbox.written[guestPath] = content
	return nil
}
func (sandbox *fakeSandbox) InspectNetwork(string) (NetworkPolicy, error) {
	return sandbox.inspectPolicy, sandbox.inspectErr
}
func (sandbox *fakeSandbox) LogTail(_ string, lines int) (string, error) {
	sandbox.logTailLines = lines
	return sandbox.logTail, sandbox.logTailErr
}
func (sandbox *fakeSandbox) LogTailContext(_ context.Context, _ string, lines int) (string, error) {
	sandbox.logTailLines = lines
	return sandbox.logTail, sandbox.logTailErr
}

// fakeKeyMinter records GenerateKey/DeleteKeyByAlias calls and returns a fixed key
// (or an error).
type fakeKeyMinter struct {
	calls        int
	lastErr      error
	scope        litellm.KeyScope
	deletedAlias string
	deleteCalls  int
}

func (minter *fakeKeyMinter) GenerateKey(scope litellm.KeyScope) (string, error) {
	minter.calls++
	minter.scope = scope
	if minter.lastErr != nil {
		return "", minter.lastErr
	}
	return "sk-fake-workspace-key", nil
}

func (minter *fakeKeyMinter) DeleteKeyByAlias(alias string) error {
	minter.deleteCalls++
	minter.deletedAlias = alias
	return nil
}

func seedProject(test *testing.T, project string) string {
	test.Helper()
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := filepath.Join(home, "projects", project)
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: project, OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects[project] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
	return root
}

func writeGlobalConfig(test *testing.T, content string) {
	test.Helper()
	path, err := config.GlobalPath()
	if err != nil {
		test.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

// seedStartedWorkspace seeds a project AND a started lifecycle handle, so the
// interactive entry points (which require a running workspace) proceed.
func seedStartedWorkspace(test *testing.T, project string) string {
	test.Helper()
	root := seedProject(test, project)
	handle := &state.Workspace{
		ID: Name(project), Project: project,
		Status: state.StatusStarted, Created: "t", LastStarted: "t",
	}
	if err := state.OpenStore(root).SaveWorkspace(handle); err != nil {
		test.Fatal(err)
	}
	return root
}

func newManager(builder Builder, sandbox Sandbox) Manager {
	return Manager{
		Builder: builder,
		Sandbox: sandbox,
		Keys:    &fakeKeyMinter{},
		Now:     func() string { return "2026-06-18T00:00:00Z" },
	}
}

func TestName(test *testing.T) {
	if got := Name("app"); got != "aip-app" {
		test.Errorf("workspace name = %q", got)
	}
}

func TestStartBuildsAndRecordsStartedHandle(test *testing.T) {
	root := seedProject(test, "app")
	builder := &fakeBuilder{}
	sandbox := &fakeSandbox{}
	minter := &fakeKeyMinter{}
	served := fakeServedModels{models: []string{"ollama/gemma4:latest"}}
	manager := Manager{Builder: builder, Sandbox: sandbox, Keys: minter, Served: served, Now: func() string { return "2026-06-18T00:00:00Z" }}

	handle, err := manager.Start("app")
	if err != nil {
		test.Fatal(err)
	}
	if !builder.built || !sandbox.created || !sandbox.started {
		test.Fatalf("lifecycle not driven: builder=%v sandbox=%+v", builder.built, sandbox)
	}
	if sandbox.resources.IdleTimeout != config.DefaultMicrosandboxIdleTimeout {
		test.Fatalf("Start should pass default idle timeout %q, got %q", config.DefaultMicrosandboxIdleTimeout, sandbox.resources.IdleTimeout)
	}
	// Start must mint a scoped virtual key tied to this workspace and write both
	// agent provider configs into the microVM (arch §15, §17).
	if minter.calls != 1 {
		test.Fatalf("GenerateKey called %d times, want 1", minter.calls)
	}
	if minter.scope.Alias != "app" || minter.scope.Metadata["workspace"] != "aip-app" {
		test.Fatalf("key scope = %+v, want alias=app workspace=aip-app", minter.scope)
	}
	// Start must ROTATE the key: revoke any prior key for the project's alias before
	// minting, so a re-start does not fail on LiteLLM's unique-alias requirement.
	if minter.deleteCalls != 1 || minter.deletedAlias != "app" {
		test.Fatalf("expected a delete-by-alias rotate for %q, got %d call(s) for %q",
			"app", minter.deleteCalls, minter.deletedAlias)
	}
	openCodeConfig, wrote := sandbox.written["/home/workspace/.config/opencode/opencode.json"]
	if !wrote {
		test.Fatal("opencode provider config not written into the microVM")
	}
	piConfig, wrote := sandbox.written["/home/workspace/.pi/agent/models.json"]
	if !wrote {
		test.Fatal("pi provider config not written into the microVM")
	}
	// The minted key must reach the agent configs (host→VM only) and pi must not
	// carry the per-request Headroom knobs.
	if !strings.Contains(string(openCodeConfig), "sk-fake-workspace-key") {
		test.Error("opencode config is missing the virtual key")
	}
	if !strings.Contains(string(openCodeConfig), "headroom_keep_turns") {
		test.Error("opencode config is missing the Headroom knobs")
	}
	if strings.Contains(string(piConfig), "headroom_keep_turns") {
		test.Error("pi config must not carry the Headroom knobs")
	}
	if handle.ID != "aip-app" || handle.Status != state.StatusStarted {
		test.Fatalf("handle: %+v", handle)
	}
	// Start must ensure the persistent overlay and mount it (arch §26).
	present, err := overlay.Exists("aip-app")
	if err != nil || !present {
		test.Fatalf("overlay not ensured: present=%v err=%v", present, err)
	}
	if sandbox.overlayMount == "" {
		test.Fatal("overlay path not passed to Sandbox.Create")
	}
	// The microVM mounts the host project path directly (no translation; hosts
	// are macOS and Linux).
	if sandbox.projectMount != root {
		test.Fatalf("project mount = %q, want host path %q", sandbox.projectMount, root)
	}
	// Start must translate the project egress policy into msb network args and
	// pass them to Create (the always-on host-gateway allow rule is present).
	if len(sandbox.netArgs) == 0 {
		test.Fatal("egress network args not passed to Sandbox.Create")
	}
	// With a temp HOME and no runtime.yaml, the gateway resolves to the local
	// standalone default (host.microsandbox.internal:18787). The always-on allow
	// rule targets msb's `host` GROUP token (which engages host-forwarding — a host
	// NAME target does not), while the agent configs still use the resolved gateway
	// URL the guest connects to.
	if !strings.Contains(strings.Join(sandbox.netArgs, " "), "allow:egress@host:tcp:18787") {
		test.Fatalf("netArgs missing local gateway allow rule: %#v", sandbox.netArgs)
	}
	if !strings.Contains(string(openCodeConfig), "http://host.microsandbox.internal:18787/v1") {
		test.Error("opencode config is missing the local gateway URL")
	}
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil || len(workspaces) != 1 || workspaces[0].Status != state.StatusStarted {
		test.Fatalf("persisted workspaces=%+v err=%v", workspaces, err)
	}
}

func TestStartReadsConfiguredMicrosandboxIdleTimeout(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{Microsandbox: config.MicrosandboxConfig{IdleTimeout: "2h"}}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if sandbox.resources.IdleTimeout != "2h" {
		test.Fatalf("Start should pass configured idle timeout 2h, got %q", sandbox.resources.IdleTimeout)
	}
}

func TestStartUsesMergedConfigWithProjectPriority(test *testing.T) {
	root := seedProject(test, "app")
	writeGlobalConfig(test, `
workspace:
  cpu_limit: 2
  memory_limit: 16G
microsandbox:
  idle_timeout: 30m
network:
  publish_ports:
    - guest: 8080
      host: 18080
`)
	if err := config.WriteProject(root, &config.Config{
		Workspace:    config.WorkspaceConfig{CPULimit: 6},
		Microsandbox: config.MicrosandboxConfig{IdleTimeout: "2h"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if sandbox.resources.CPUs != 6 {
		test.Fatalf("project cpu_limit should override global cpu_limit, got %d", sandbox.resources.CPUs)
	}
	if sandbox.resources.Memory != "16G" {
		test.Fatalf("global memory_limit should survive when project omits it, got %q", sandbox.resources.Memory)
	}
	if sandbox.resources.IdleTimeout != "2h" {
		test.Fatalf("project idle_timeout should override global idle_timeout, got %q", sandbox.resources.IdleTimeout)
	}
	if !strings.Contains(strings.Join(sandbox.netArgs, " "), "-p 18080:8080") {
		test.Fatalf("global publish_ports should be passed to msb when project omits them: %#v", sandbox.netArgs)
	}
}

func TestStartRejectsInvalidMicrosandboxIdleTimeout(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{Microsandbox: config.MicrosandboxConfig{IdleTimeout: "0s"}}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err == nil || !strings.Contains(err.Error(), "microsandbox.idle_timeout") {
		test.Fatalf("Start should reject invalid microsandbox.idle_timeout, got %v", err)
	}
	if sandbox.created {
		test.Fatal("Start must validate idle timeout before creating the microVM")
	}
}

// TestStartRoutesAllFiveAgentCLIs verifies Start wires every agent CLI through the
// gateway with the scoped key written ONLY into the microVM, and scaffolds KEYLESS
// host-side templates — the scoped key must never touch host disk.
func TestStartRoutesAllFiveAgentCLIs(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/llama3.2:latest"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	const key = "sk-fake-workspace-key"

	// (1) The in-VM final configs must each carry the scoped key.
	for _, guestPath := range []string{
		"/home/workspace/.config/opencode/opencode.json",
		"/home/workspace/.pi/agent/models.json",
	} {
		content, wrote := sandbox.written[guestPath]
		if !wrote {
			test.Fatalf("no in-VM config written to %s", guestPath)
		}
		if !strings.Contains(string(content), key) {
			test.Errorf("in-VM config %s is missing the scoped key", guestPath)
		}
	}

	// (2) codex.toml is written into the VM but is KEYLESS (the key is env-supplied
	// via env_key); the agent env file carries the key for claude-code/codex/gemini.
	codex, wrote := sandbox.written[codexGuestPath]
	if !wrote {
		test.Fatal("codex config not written into the microVM")
	}
	if strings.Contains(string(codex), key) {
		test.Error("codex config.toml must be keyless (key comes from env_key)")
	}
	if !strings.Contains(string(codex), "env_key") || !strings.Contains(string(codex), "wire_api") {
		test.Errorf("codex config.toml missing provider block:\n%s", codex)
	}

	agentEnv, wrote := sandbox.written[agentEnvGuestPath]
	if !wrote {
		test.Fatal("agent env file not written into the microVM")
	}
	envText := string(agentEnv)
	if !strings.Contains(envText, key) {
		test.Error("agent env file must carry the scoped key (in-VM only)")
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "GOOGLE_GEMINI_BASE_URL", "GEMINI_API_KEY", "AIP_GATEWAY_KEY"} {
		if !strings.Contains(envText, want) {
			test.Errorf("agent env file missing %q:\n%s", want, envText)
		}
	}

	// (3) The managed bash_profile sources the agent env (so `ai shell` routes too).
	if profile, wrote := sandbox.written[bashProfileGuestPath]; !wrote || !strings.Contains(string(profile), agentEnvGuestPath) {
		test.Errorf("managed bash_profile must source the agent env file: %q", profile)
	}

	// (4) HARD security constraint: the KEYLESS host-side templates must contain NO
	// key anywhere under <project>/.ai-platform/agents/.
	assertHostTemplatesKeyless(test, root, key)
}

// assertHostTemplatesKeyless fails if any host-side agent template contains the
// scoped key (the key must live only in the microVM).
func assertHostTemplatesKeyless(test *testing.T, root, key string) {
	test.Helper()
	dir := filepath.Join(root, ".ai-platform", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		test.Fatalf("agents template dir not scaffolded: %v", err)
	}
	if len(entries) == 0 {
		test.Fatal("no host-side agent templates scaffolded")
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if strings.Contains(string(content), key) {
			test.Errorf("host template %s contains the scoped key — it MUST be keyless", entry.Name())
		}
	}
}

// TestStartPreservesAgentTemplateEdits verifies a user's edit to a host template is
// preserved across start (not clobbered) and merged into the in-VM config, while the
// scoped key is still injected only into the in-VM result (not the host template).
func TestStartPreservesAgentTemplateEdits(test *testing.T) {
	root := seedProject(test, "app")
	dir := filepath.Join(root, ".ai-platform", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		test.Fatal(err)
	}
	// A user edit: a custom top-level key opencode would keep (e.g. a theme).
	edited := []byte(`{"theme":"my-custom-theme","provider":{}}`)
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), edited, 0o644); err != nil {
		test.Fatal(err)
	}

	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	// The host template is untouched (still the user's edit, still keyless).
	onDisk, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		test.Fatal(err)
	}
	if string(onDisk) != string(edited) {
		test.Errorf("host template was clobbered: %s", onDisk)
	}
	// The in-VM config merges the user's key in AND carries the gateway provider +
	// the scoped key (dynamic values win, user key survives).
	inVM := string(sandbox.written["/home/workspace/.config/opencode/opencode.json"])
	if !strings.Contains(inVM, "my-custom-theme") {
		test.Errorf("user template edit not merged into the in-VM config:\n%s", inVM)
	}
	if !strings.Contains(inVM, "sk-fake-workspace-key") {
		test.Errorf("in-VM config missing the scoped key:\n%s", inVM)
	}
	if !strings.Contains(inVM, "host.microsandbox.internal:18787/v1") {
		test.Errorf("in-VM config missing the gateway provider:\n%s", inVM)
	}
}

// TestStartEnsuresContainerdWhenDown drives the in-VM container-runtime bring-up:
// when the `nerdctl info` probe reports the daemon is NOT up, Start must boot
// containerd as ROOT (ExecRoot) detached via setsid.
func TestStartEnsuresContainerdWhenDown(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{
		// Probe (and the boot) report a non-zero exit → containerd not yet up, so
		// ensureContainerd proceeds to the boot. A non-zero boot exit is swallowed
		// (best-effort) so it never fails the start.
		execRootResult: ExecResult{ExitCode: 1, Stderr: "down"},
	}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}

	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must succeed even when containerd is down (best-effort): %v", err)
	}
	if len(sandbox.execRootArgv) != 2 {
		test.Fatalf("expected a probe + a boot ExecRoot call, got %d: %v", len(sandbox.execRootArgv), sandbox.execRootArgv)
	}
	probe := strings.Join(sandbox.execRootArgv[0], " ")
	// The probe is a quiet, bounded `nerdctl info` (output discarded — the
	// expected first-boot "cannot access containerd socket" fatal is not shown).
	if probe != "sh -c timeout 5 nerdctl info >/dev/null 2>&1" {
		test.Errorf("first ExecRoot must probe the runtime quietly (bounded), got %q", probe)
	}
	boot := strings.Join(sandbox.execRootArgv[1], " ")
	if !strings.Contains(boot, "setsid") || !strings.Contains(boot, "containerd") {
		test.Errorf("second ExecRoot must boot containerd detached via setsid, got %q", boot)
	}
}

// TestStartSkipsContainerdBootWhenUp confirms the probe short-circuits the boot:
// when `nerdctl info` succeeds (exit 0), only the single probe ExecRoot is issued.
func TestStartSkipsContainerdBootWhenUp(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{} // zero-value execRootResult → exit 0 → already up
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}

	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if len(sandbox.execRootArgv) != 1 {
		test.Fatalf("expected only the probe ExecRoot when containerd is up, got %v", sandbox.execRootArgv)
	}
}

// TestStartSucceedsWhenContainerdEnsureErrors confirms the bring-up is BEST-EFFORT:
// an infrastructure error from ExecRoot must not fail the workspace start.
func TestStartSucceedsWhenContainerdEnsureErrors(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{execRootErr: errors.New("msb exec failed")}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}

	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must not fail when containerd-ensure errors (best-effort): %v", err)
	}
	// The handle must still be saved (the start completed) and the microVM kept.
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil || len(workspaces) != 1 || workspaces[0].Status != state.StatusStarted {
		test.Fatalf("a best-effort containerd failure must still save a started handle, got %+v err=%v", workspaces, err)
	}
	if sandbox.destroyed {
		test.Fatal("a best-effort containerd failure must NOT roll back the microVM")
	}
}

func TestStartFailsWhenKeyMintFails(test *testing.T) {
	root := seedProject(test, "app")
	minter := &fakeKeyMinter{lastErr: errors.New("litellm gateway is not reachable")}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: minter, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err == nil {
		test.Fatal("Start must fail when the virtual key cannot be minted")
	}
	// A post-start failure must roll back the running microVM (no orphan) and
	// leave no saved handle, so a retry starts cleanly.
	if !sandbox.destroyed {
		test.Fatal("Start must destroy the microVM when post-start registration fails (orphan rollback)")
	}
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil {
		test.Fatal(err)
	}
	if len(workspaces) != 0 {
		test.Fatalf("failed Start must not save a handle, got %+v", workspaces)
	}
}

// A failure on the FINAL post-start step — the state-handle write, AFTER the
// microVM is up and agent-provider registration succeeded — must also roll back
// the running microVM, not just the registration step. (Previously this path
// leaked an untracked running VM that no saved handle could later reap.) Force
// SaveWorkspace to fail by planting a regular file where the workspaces state
// directory must be, so the atomic write cannot create
// <root>/.ai-platform/run/workspaces/<id>.yaml.
func TestStartRollsBackWhenHandleSaveFails(test *testing.T) {
	root := seedProject(test, "app")
	runDir := filepath.Join(root, ".ai-platform", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "workspaces"), []byte("x"), 0o644); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)

	if _, err := manager.Start("app"); err == nil {
		test.Fatal("Start must fail when the state handle cannot be saved")
	}
	// The microVM was created+started and registration succeeded; only the handle
	// save failed — the deferred rollback must still destroy the orphan.
	if !sandbox.destroyed {
		test.Fatal("Start must destroy the microVM when the handle save fails (orphan rollback)")
	}
}

func TestRestartStopsThenStartsExistingMicroVM(test *testing.T) {
	root := seedProject(test, "app")
	manager := newManager(&fakeBuilder{}, &fakeSandbox{})

	// Establish an existing started handle.
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	// Restart stops the existing microVM then runs the FULL start path again, so it
	// recreates (Sandbox.Create) and re-applies the network/published-port set —
	// otherwise a newly added/removed in-VM app's host port would never re-publish.
	restartBuilder := &fakeBuilder{}
	restartSandbox := &fakeSandbox{}
	restartManager := newManager(restartBuilder, restartSandbox)

	handle, err := restartManager.Restart("app")
	if err != nil {
		test.Fatal(err)
	}
	if !restartSandbox.stopped || !restartSandbox.started {
		test.Fatalf("restart must stop then start the microVM: %+v", restartSandbox)
	}
	if !restartBuilder.built || !restartSandbox.created {
		test.Fatalf("restart must rebuild + recreate to re-apply published ports: builder=%v sandbox=%+v", restartBuilder.built, restartSandbox)
	}
	if handle.Status != state.StatusStarted {
		test.Fatalf("handle status = %q, want started", handle.Status)
	}
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil || len(workspaces) != 1 || workspaces[0].Status != state.StatusStarted {
		test.Fatalf("persisted workspaces=%+v err=%v", workspaces, err)
	}
}

func TestRestartWithoutExistingHandle(test *testing.T) {
	seedProject(test, "app")
	_, err := newManager(&fakeBuilder{}, &fakeSandbox{}).Restart("app")
	if !errors.Is(err, ErrNotStarted) {
		test.Fatalf("want ErrNotStarted, got %v", err)
	}
}

func TestRestartUnknownProject(test *testing.T) {
	seedProject(test, "app")
	_, err := newManager(&fakeBuilder{}, &fakeSandbox{}).Restart("nope")
	if !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}

func TestStartUnknownProject(test *testing.T) {
	seedProject(test, "app")
	_, err := newManager(&fakeBuilder{}, &fakeSandbox{}).Start("nope")
	if !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}

// Shell opens a PERSISTENT, reattachable tmux session named "shell" in /workspace
// running a login shell, via a single atomic `tmux new-session -A` (create-or-attach).
func TestShellOpensPersistentTmuxSession(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); err != nil {
		test.Fatal(err)
	}
	// One atomic create-or-attach in the interactive exec (`tmux new-session -A`).
	want := []string{"tmux", "new-session", "-A", "-s", "shell", "-c", "/workspace", "bash", "-l"}
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Shell ran %v via ExecInteractive, want %v", got, want)
	}
}

// Before handing the user's real terminal to `msb exec -t`, Shell warms up tmux with
// a bounded, buffered probe. This prevents the flaky first post-start tmux/server/PTY
// path from becoming a blank/frozen real terminal.
func TestShellPreflightsTmuxBeforeInteractiveAttach(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); err != nil {
		test.Fatal(err)
	}
	if sandbox.execCtxCalls < 2 {
		test.Fatalf("Shell should run requireTmux + readiness probes before interactive attach, got %d ExecContext calls", sandbox.execCtxCalls)
	}
	if got := strings.Join(sandbox.execArgv, " "); !strings.Contains(got, "tmux new-session -d") || !strings.Contains(got, "aip-probe-aip_app") {
		test.Fatalf("last preflight argv should create/kill a probe tmux session, got %v", sandbox.execArgv)
	}
	if sandbox.interactiveArgv == nil {
		test.Fatal("Shell should still hand off to the interactive tmux attach after preflight")
	}
}

func TestShellReadinessProbeRetriesTransientTimeout(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{execCtxFailFirst: 1}
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); err != nil {
		test.Fatal(err)
	}
	if sandbox.execCtxCalls < 3 {
		test.Fatalf("transient preflight timeout should be retried before interactive attach, got %d calls", sandbox.execCtxCalls)
	}
	if sandbox.interactiveArgv == nil {
		test.Fatal("Shell should continue to interactive attach after the retry succeeds")
	}
}

// Agent starts (or reattaches to) a per-CLI tmux session named after the CLI,
// running that CLI's launch command in /workspace (detached create, then attach).
func TestAgentStartsPerCLITmuxSession(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Agent("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	// The launch is wrapped in a login shell that sources the in-VM agent env file
	// (gateway env vars for the env-routed CLIs) then execs the CLI.
	want := append([]string{"tmux", "new-session", "-A", "-s", "opencode", "-c", "/workspace"},
		wrapWithAgentEnv([]string{"opencode"})...)
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Agent ran %v via ExecInteractive, want %v", got, want)
	}
}

// claude-code maps to the `claude` launch command but keeps its own session name.
func TestAgentMapsClaudeCodeLaunch(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Agent("app", "claude-code"); err != nil {
		test.Fatal(err)
	}
	// claude-code maps to the `claude` binary, wrapped to source the agent env.
	want := append([]string{"tmux", "new-session", "-A", "-s", "claude-code", "-c", "/workspace"},
		wrapWithAgentEnv([]string{"claude"})...)
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Agent(claude-code) ran %v via ExecInteractive, want %v", got, want)
	}
}

// An unknown CLI is rejected with ErrUnknownAgentCLI (→ exit 2) and never starts
// a session.
func TestAgentUnknownCLI(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	err := newManager(&fakeBuilder{}, sandbox).Agent("app", "nope")
	if !errors.Is(err, ErrUnknownAgentCLI) {
		test.Fatalf("want ErrUnknownAgentCLI, got %v", err)
	}
	if sandbox.interactiveArgv != nil {
		test.Fatal("an unknown CLI must not start a session")
	}
}

// Attach create-or-attaches a named session with no command (so a fresh session opens
// the default shell); a blank session targets the default "shell".
func TestAttachSession(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Attach("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "new-session", "-A", "-s", "opencode", "-c", "/workspace"}
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Attach ran %v via ExecInteractive, want %v", got, want)
	}

	defaulted := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, defaulted).Attach("app", ""); err != nil {
		test.Fatal(err)
	}
	wantDefault := []string{"tmux", "new-session", "-A", "-s", "shell", "-c", "/workspace"}
	if got := defaulted.interactiveArgv; !equalStrings(got, wantDefault) {
		test.Fatalf("Attach(\"\") ran %v, want the default shell session %v", got, wantDefault)
	}
}

// ListSessions parses tmux's '|'-separated list-sessions output (name, attached,
// activity), reading the attached flag and raw activity epoch. The delimiter is '|'
// not a tab because `msb exec` mangles tab bytes in argv.
func TestListSessionsParsesOutput(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{
		ExitCode: 0,
		Stdout:   "shell|1|1700000000\nopencode|0|1700000500\n",
	}}
	sessions, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if err != nil {
		test.Fatal(err)
	}
	if len(sandbox.execArgv) != 3 || sandbox.execArgv[0] != "sh" || sandbox.execArgv[1] != "-c" || !strings.Contains(sandbox.execArgv[2], "tmux list-sessions") {
		test.Fatalf("ListSessions ran unexpected argv: %v", sandbox.execArgv)
	}
	if len(sessions) != 2 {
		test.Fatalf("parsed %d sessions, want 2: %+v", len(sessions), sessions)
	}
	if sessions[0].Name != "shell" || !sessions[0].Attached || sessions[0].Activity != "1700000000" {
		test.Fatalf("session[0] = %+v", sessions[0])
	}
	if sessions[1].Name != "opencode" || sessions[1].Attached {
		test.Fatalf("session[1] = %+v", sessions[1])
	}
}

// A transient post-sleep timeout on the FIRST in-VM probe is retried (probeInVM /
// sleep recovery): the second attempt succeeds and ListSessions returns the
// sessions instead of erroring.
func TestListSessionsRetriesPastTransientTimeout(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{
		execCtxFailFirst: 1, // first probe "hangs" (timeout), retry succeeds
		execResult:       ExecResult{ExitCode: 0, Stdout: "shell|1|1700000000\n"},
	}
	sessions, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if err != nil {
		test.Fatalf("ListSessions should recover via retry, got %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "shell" {
		test.Fatalf("want one session 'shell' after retry, got %+v", sessions)
	}
	if sandbox.execCtxCalls != 2 {
		test.Fatalf("expected 2 probe attempts (1 timeout + 1 success), got %d", sandbox.execCtxCalls)
	}
}

// msb reporting "sandbox not found" (a stale handle: started in state, but the
// microVM is gone) must surface as ErrWorkspaceStale ("run ai restart"), NOT as a
// raw tmux error or "no sessions".
func TestListSessionsSandboxNotFoundIsStale(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{
		ExitCode: 1,
		Stderr:   "error: sandbox not found: aip-app",
	}}
	_, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if !errors.Is(err, ErrWorkspaceStale) {
		test.Fatalf("want ErrWorkspaceStale for a gone sandbox, got %v", err)
	}
}

// Start syncs the guest clock (so a post-sleep clock skew doesn't break in-VM TLS).
func TestStartSyncsGuestClock(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if _, err := newManager(&fakeBuilder{}, sandbox).Start("app"); err != nil {
		test.Fatal(err)
	}
	if !sandbox.clockSynced {
		test.Fatal("Start must sync the guest clock (SyncClock)")
	}
}

// With no tmux server running yet, `tmux list-sessions` exits non-zero with "no
// server running" on stderr — that is ZERO sessions, not an error. Exec carries
// the inner non-zero exit as data, so the check is on the ExecResult.
func TestListSessionsNoServerIsEmpty(test *testing.T) {
	seedStartedWorkspace(test, "app")
	// Different tmux builds word "no server running yet" differently — both must be
	// treated as ZERO sessions, not an error.
	for _, stderr := range []string{
		"no server running on /tmp/tmux-1000/default",
		"error connecting to /tmp/tmux-1000/default (No such file or directory)",
	} {
		sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 1, Stderr: stderr}}
		sessions, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
		if err != nil {
			test.Fatalf("no running tmux server (%q) must be empty, not an error: %v", stderr, err)
		}
		if len(sessions) != 0 {
			test.Fatalf("want zero sessions for %q, got %+v", stderr, sessions)
		}
	}
}

// A non-zero tmux exit that is NOT "no server running" is a real failure.
func TestListSessionsOtherFailureIsError(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 1, Stderr: "tmux: command not found"}}
	if _, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app"); err == nil {
		test.Fatal("a non-'no server running' failure must be an error")
	}
}

// When the in-VM session listing fails AND the liveness probe confirms the microVM
// is GONE (handle says started, but no VM), ListSessions reports ErrWorkspaceStale
// so the user is told the state is stale and to `ai restart`.
func TestListSessionsStaleHandleClassified(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{
		execErr:      ErrWorkspaceUnresponsive, // the in-VM exec timed out
		isRunningSet: true, isRunning: false,   // liveness probe: VM not present
	}
	_, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if !errors.Is(err, ErrWorkspaceStale) {
		test.Fatalf("stale VM: want ErrWorkspaceStale, got %v", err)
	}
}

// When the in-VM session listing fails but the liveness probe confirms the microVM
// IS present (just slow), ListSessions reports ErrWorkspaceUnresponsive — overloaded,
// not stale.
func TestListSessionsUnresponsiveClassified(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{
		execErr:      ErrWorkspaceUnresponsive, // the in-VM exec timed out
		isRunningSet: true, isRunning: true,    // liveness probe: VM present
	}
	_, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if !errors.Is(err, ErrWorkspaceUnresponsive) {
		test.Fatalf("present-but-slow VM: want ErrWorkspaceUnresponsive, got %v", err)
	}
	if errors.Is(err, ErrWorkspaceStale) {
		test.Fatal("a present VM must NOT be classified stale")
	}
}

// When the liveness probe ITSELF can't determine the VM state (its own error/
// timeout), the listing degrades to ErrWorkspaceUnresponsive — never a confident
// "stale" on uncertainty.
func TestListSessionsIndeterminateLivenessIsUnresponsive(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{
		execErr:      ErrWorkspaceUnresponsive,
		isRunningErr: errors.New("liveness probe timed out"),
	}
	_, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if !errors.Is(err, ErrWorkspaceUnresponsive) {
		test.Fatalf("indeterminate liveness: want ErrWorkspaceUnresponsive, got %v", err)
	}
}

// A missing msb surfaced by the liveness probe stays ErrMsbMissing (a missing dep,
// not a runtime failure), even on the classification path.
func TestListSessionsMsbMissingPreserved(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{
		execErr:      ErrWorkspaceUnresponsive,
		isRunningErr: ErrMsbMissing,
	}
	_, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if !errors.Is(err, ErrMsbMissing) {
		test.Fatalf("missing msb: want ErrMsbMissing, got %v", err)
	}
}

// KillSession kills the named session via tmux kill-session -t.
func TestKillSession(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).KillSession("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "kill-session", "-t", "opencode"}
	if !equalStrings(sandbox.execArgv, want) {
		test.Fatalf("KillSession ran %v, want %v", sandbox.execArgv, want)
	}
}

// Start writes the managed tmux.conf into the microVM so the session model is
// transparent (alongside the agent provider configs).
func TestStartWritesTmuxConf(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if _, err := newManager(&fakeBuilder{}, sandbox).Start("app"); err != nil {
		test.Fatal(err)
	}
	conf, wrote := sandbox.written["/home/workspace/.tmux.conf"]
	if !wrote {
		test.Fatal("Start must write the managed tmux.conf into the microVM")
	}
	if !strings.Contains(string(conf), "status off") || !strings.Contains(string(conf), "mouse on") {
		test.Errorf("tmux.conf missing transparent settings:\n%s", conf)
	}
}

// TestStartInstallsRefreshScript verifies Start stages the per-workspace
// `refresh-models` script via WriteFile and installs it onto PATH executable
// (sudo install -m 0755 → /usr/local/bin/refresh-models). The staged script must
// bake in the resolved gateway URL, the minted scoped key, and the Headroom knobs —
// but NOT any model list: the script fetches the served models live from /v1/models.
func TestStartInstallsRefreshScript(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/llama3.2:latest"}}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Served: served, Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	staged, wrote := sandbox.written[refreshScriptStagePath]
	if !wrote {
		test.Fatalf("refresh-models script not staged at %s", refreshScriptStagePath)
	}
	script := string(staged)

	// The staged script must be EXACTLY what agentcfg.RefreshScript produces for the
	// resolved gateway, the minted key, the empty default model, and the project's
	// Headroom knobs — pinning the full wiring (gateway URL, scoped key, knobs). With
	// a temp HOME and no runtime.yaml the gateway resolves to the local standalone
	// default, and the default Headroom strategy yields its knobs.
	keepTurns, outputBufferTokens := contextopt.HeadroomParams("")
	wantScript, err := agentcfg.RefreshScript(
		"http://host.microsandbox.internal:18787/v1",
		"sk-fake-workspace-key",
		"", // no built-in default model
		keepTurns, outputBufferTokens,
	)
	if err != nil {
		test.Fatal(err)
	}
	if script != string(wantScript) {
		test.Fatalf("staged refresh-models script does not match RefreshScript for the resolved wiring")
	}

	// The script fetches the served models live from /v1/models — it must NOT bake in
	// any model list (no served model is embedded).
	if !strings.Contains(script, "MODELS_URL=") {
		test.Error("refresh-models script missing the served-models endpoint")
	}
	if strings.Contains(script, "ollama/llama3.2:latest") {
		test.Error("refresh-models must not bake in any models (it fetches the served list)")
	}

	// It must be installed onto PATH executable via sudo install -m 0755, then the
	// staging copy removed. Search ALL execs (later best-effort steps — venv creation
	// — run after it, so it is not necessarily the last exec).
	var installed string
	for _, argv := range sandbox.allExecArgv {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "install -m 0755") && strings.Contains(joined, refreshScriptStagePath) && strings.Contains(joined, refreshScriptBinPath) {
			installed = joined
		}
	}
	if installed == "" {
		test.Errorf("refresh-models not installed executable onto PATH; execs were: %v", sandbox.allExecArgv)
	}
}

// TestStartFailsWhenRefreshInstallFails: a non-zero exit from the install step
// fails Start (and the orphan rollback tears the microVM down) — the user is not
// left with a half-provisioned workspace silently missing refresh-models.
func TestStartFailsWhenRefreshInstallFails(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 1, Stderr: "install: permission denied"}}
	manager := newManager(&fakeBuilder{}, sandbox)
	if _, err := manager.Start("app"); err == nil {
		test.Fatal("Start must fail when the refresh-models install exits non-zero")
	}
	if !sandbox.destroyed {
		test.Fatal("a failed refresh-models install must roll back the microVM (orphan rollback)")
	}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestExecInteractiveUnknownProject(test *testing.T) {
	seedProject(test, "app")
	err := newManager(&fakeBuilder{}, &fakeSandbox{}).ExecInteractive("nope", []string{"bash"})
	if !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}

// TestInteractiveRequiresRunningWorkspace: a shell/agent/attach/sessions request
// against a workspace that was never started fails cleanly with ErrNotStarted
// (rather than poking msb — `msb exec -t` against a missing VM can corrupt the
// terminal).
func TestInteractiveRequiresRunningWorkspace(test *testing.T) {
	seedProject(test, "app") // no lifecycle handle → never started
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)

	if err := manager.Shell("app"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Shell on a never-started workspace: want ErrNotStarted, got %v", err)
	}
	if err := manager.Agent("app", "opencode"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Agent on a never-started workspace: want ErrNotStarted, got %v", err)
	}
	if err := manager.Attach("app", "shell"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Attach on a never-started workspace: want ErrNotStarted, got %v", err)
	}
	if _, err := manager.ListSessions("app"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("ListSessions on a never-started workspace: want ErrNotStarted, got %v", err)
	}
	if sandbox.interactiveArgv != nil {
		test.Fatalf("a not-running shell must not attach a PTY, ran %v", sandbox.interactiveArgv)
	}
}

// TestStoppedWorkspaceShellFailsCleanly is the regression for the hang: a STOPPED
// (but existing) workspace must fail fast with ErrNotStarted, not invoke `msb exec`
// (which hangs on a stopped microVM).
func TestStoppedWorkspaceShellFailsCleanly(test *testing.T) {
	root := seedProject(test, "app")
	stopped := &state.Workspace{ID: Name("app"), Project: "app", Status: state.StatusStopped, Created: "t"}
	if err := state.OpenStore(root).SaveWorkspace(stopped); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Shell on a stopped workspace must fail cleanly, got %v", err)
	}
	if sandbox.interactiveArgv != nil {
		test.Fatal("a stopped-workspace shell must not attach a PTY (it would hang)")
	}
}

// TestSessionRequiresTmuxInImage: a tmux-backed session against a running
// workspace whose image lacks tmux fails with ErrTmuxMissing (a clear remediation)
// and never attaches the PTY — instead of msb's raw "failed to exec tmux" leak.
func TestSessionRequiresTmuxInImage(test *testing.T) {
	seedStartedWorkspace(test, "app")
	// Running workspace (started handle), but the tmux probe exits non-zero (not installed).
	noTmux := func() *fakeSandbox { return &fakeSandbox{execResult: ExecResult{ExitCode: 127}} }

	sandbox := noTmux()
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); !errors.Is(err, ErrTmuxMissing) {
		test.Fatalf("Shell with no tmux in image: want ErrTmuxMissing, got %v", err)
	}
	if sandbox.interactiveArgv != nil {
		test.Fatalf("a tmux-less shell must not attach a PTY, ran %v", sandbox.interactiveArgv)
	}
	if err := newManager(&fakeBuilder{}, noTmux()).Agent("app", "opencode"); !errors.Is(err, ErrTmuxMissing) {
		test.Fatalf("Agent with no tmux in image: want ErrTmuxMissing, got %v", err)
	}
	if err := newManager(&fakeBuilder{}, noTmux()).Attach("app", "shell"); !errors.Is(err, ErrTmuxMissing) {
		test.Fatalf("Attach with no tmux in image: want ErrTmuxMissing, got %v", err)
	}
}

func TestExecCarriesInnerResult(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 7, Stdout: "hi"}}
	result, err := newManager(&fakeBuilder{}, sandbox).Exec("app", []string{"false"})
	if err != nil {
		test.Fatalf("inner non-zero exit must not be a platform error: %v", err)
	}
	if result.ExitCode != 7 || result.Stdout != "hi" {
		test.Fatalf("exec result: %+v", result)
	}
}

func TestExecPlatformError(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{execErr: errors.New("microVM not running")}
	if _, err := newManager(&fakeBuilder{}, sandbox).Exec("app", []string{"ls"}); err == nil {
		test.Fatal("expected a platform error")
	}
}

func TestDestroyIsNonDestructiveAndRecordsStatus(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Destroy("app"); err != nil {
		test.Fatal(err)
	}
	if !sandbox.destroyed {
		test.Fatal("sandbox.Destroy not called")
	}
	workspaces, _ := state.OpenStore(root).ListWorkspaces()
	if len(workspaces) != 1 || workspaces[0].Status != state.StatusDestroyed {
		test.Fatalf("status not recorded destroyed: %+v", workspaces)
	}
}

// DestroyIfPresent tears down a started workspace's microVM (the teardown `ai
// project delete` layers on, CLI §3.4) and stamps the handle destroyed.
func TestDestroyIfPresentDestroysStartedWorkspace(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if err := manager.DestroyIfPresent("app"); err != nil {
		test.Fatal(err)
	}
	if !sandbox.destroyed {
		test.Fatal("sandbox.Destroy not called for a started workspace")
	}
	workspaces, _ := state.OpenStore(root).ListWorkspaces()
	if len(workspaces) != 1 || workspaces[0].Status != state.StatusDestroyed {
		test.Fatalf("status not recorded destroyed: %+v", workspaces)
	}
}

// DestroyIfPresent is a no-op when the project never started a workspace (no
// handle), so `ai project delete` on a never-started project still works.
func TestDestroyIfPresentNoHandleIsNoop(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).DestroyIfPresent("app"); err != nil {
		test.Fatal(err)
	}
	if sandbox.destroyed {
		test.Fatal("sandbox.Destroy called for a never-started workspace")
	}
}

// resolveGateway falls back to the local standalone gateway when no runtime.yaml
// exists (the common test/fresh-host case).
func TestResolveGatewayDefaultsToLocalWithoutRuntime(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	host, port, url := resolveGateway()
	if host != "host.microsandbox.internal" || port != 18787 {
		test.Fatalf("default gateway = %s:%d, want host.microsandbox.internal:18787", host, port)
	}
	if url != "http://host.microsandbox.internal:18787/v1" {
		test.Fatalf("default gateway url = %q", url)
	}
}

// resolveGateway reads the machine-wide AIPlatformHost from runtime.yaml (client
// mode), resolving host-only and host:port forms.
func TestResolveGatewayFromRuntime(test *testing.T) {
	cases := []struct {
		configured string
		wantHost   string
		wantPort   int
		wantURL    string
	}{
		{"", "host.microsandbox.internal", 18787, "http://host.microsandbox.internal:18787/v1"},
		{"srv", "srv", 18787, "http://srv:18787/v1"},
		{"srv:9999", "srv", 9999, "http://srv:9999/v1"},
	}
	for _, testCase := range cases {
		test.Setenv("HOME", test.TempDir())
		if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, AIPlatformHost: testCase.configured}); err != nil {
			test.Fatal(err)
		}
		host, port, url := resolveGateway()
		if host != testCase.wantHost || port != testCase.wantPort || url != testCase.wantURL {
			test.Fatalf("resolveGateway(%q) = %s:%d %q, want %s:%d %q",
				testCase.configured, host, port, url, testCase.wantHost, testCase.wantPort, testCase.wantURL)
		}
	}
}

func TestDestroyKeepsOverlayForRecovery(test *testing.T) {
	seedProject(test, "app")
	manager := newManager(&fakeBuilder{}, &fakeSandbox{})

	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if err := manager.Destroy("app"); err != nil {
		test.Fatal(err)
	}
	// Destroy is non-destructive (arch §26): the overlay survives so a later
	// `start` fully recovers the workspace.
	present, err := overlay.Exists("aip-app")
	if err != nil || !present {
		test.Fatalf("destroy must keep the overlay: present=%v err=%v", present, err)
	}
}

// fakeServedModels is a workspace-local ServedModels fake: it returns a fixed set of
// served model names (or an error to simulate the gateway being down at start).
type fakeServedModels struct {
	models []string
	err    error
}

func (source fakeServedModels) ServedModels() ([]string, error) {
	return source.models, source.err
}

// TestStartPickerIsServedModels verifies the in-VM agent model picker is EXACTLY
// the set of models the gateway currently serves (its DB-backed models), deduped +
// sorted, and that it is written into both agent provider configs.
func TestStartPickerIsServedModels(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{
		"ollama/qwen2.5:7b",
		"anthropic/claude-opus-4-8",
		"ollama/llama3.2:latest",
		"anthropic/claude-opus-4-8", // duplicate — must be collapsed
	}}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Served: served, Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	for _, guestPath := range []string{
		"/home/workspace/.config/opencode/opencode.json",
		"/home/workspace/.pi/agent/models.json",
	} {
		config := string(sandbox.written[guestPath])
		if config == "" {
			test.Fatalf("no config written to %s", guestPath)
		}
		// Every served model is present.
		for _, want := range []string{"ollama/llama3.2:latest", "ollama/qwen2.5:7b", "anthropic/claude-opus-4-8"} {
			if !strings.Contains(config, want) {
				test.Errorf("%s missing served model %q", guestPath, want)
			}
		}
	}
	// The duplicate served entry is collapsed: in opencode's model map the model id
	// appears as a JSON key exactly once (`"anthropic/claude-opus-4-8":`), not twice.
	openCode := string(sandbox.written["/home/workspace/.config/opencode/opencode.json"])
	if got := strings.Count(openCode, `"anthropic/claude-opus-4-8":`); got != 1 {
		test.Errorf("opencode: served model keyed %d times, want 1 (deduped)", got)
	}
	// No built-in default model is written (the catalog-driven system has none).
	if strings.Contains(openCode, `"model":`) {
		test.Errorf("opencode config must not carry a top-level default model:\n%s", openCode)
	}
}

// TestStartPickerDegradesWhenGatewayDown verifies the picker degrades gracefully:
// when ServedModels errors (the gateway is down at start), the picker is empty but
// the workspace still starts.
func TestStartPickerDegradesWhenGatewayDown(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{}
	served := fakeServedModels{err: errors.New("litellm: connection refused")}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Served: served, Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must NOT fail when the served-models lookup fails: %v", err)
	}

	config := string(sandbox.written["/home/workspace/.config/opencode/opencode.json"])
	if config == "" {
		test.Fatal("no opencode config written")
	}
	// The picker is empty — no model ids at all.
	if strings.Contains(config, "ollama/") || strings.Contains(config, "anthropic/") {
		test.Errorf("degraded config must have an empty picker:\n%s", config)
	}
}

// TestStartPickerNilSourceIsEmpty verifies a nil ServedModels source (Manager
// without the dep) also degrades to an empty picker without panicking.
func TestStartPickerNilSourceIsEmpty(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must not fail with a nil ServedModels source: %v", err)
	}
	config := string(sandbox.written["/home/workspace/.config/opencode/opencode.json"])
	if config == "" {
		test.Fatal("no opencode config written")
	}
	if strings.Contains(config, "ollama/") || strings.Contains(config, "anthropic/") {
		test.Errorf("config must have an empty picker with a nil source:\n%s", config)
	}
}

func TestMergePublishPorts(test *testing.T) {
	declared := []config.PortMapping{{Guest: 8000, Host: 9000}}
	appPorts := []config.PortMapping{{Guest: 21000, Host: 21000}}
	merged := mergePublishPorts(declared, appPorts)
	if len(merged) != 2 {
		test.Fatalf("merged = %v, want 2", merged)
	}
	// App mapping wins on a host-port clash.
	conflict := mergePublishPorts(
		[]config.PortMapping{{Guest: 1, Host: 21000}},
		[]config.PortMapping{{Guest: 21000, Host: 21000}},
	)
	if len(conflict) != 1 || conflict[0].Guest != 21000 {
		test.Fatalf("clash resolution = %v, want the app mapping to win", conflict)
	}
}

// Start passes the project config's workspace resource limits through to
// Sandbox.Create (→ msb --memory/--cpus); an empty memory_limit is left for the
// real Create to default.
func TestStartAppliesConfiguredResourceLimits(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:        "debian-trixie",
		Workspace: config.WorkspaceConfig{CPULimit: 6, MemoryLimit: "8G"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if sandbox.resources.CPUs != 6 || sandbox.resources.Memory != "8G" {
		test.Fatalf("Start passed resources %+v, want {CPUs:6 Memory:8G}", sandbox.resources)
	}
}

// TestStartPublishesInstalledAppPorts checks that an installed app's allocated
// port is published as an msb -p mapping (apps are NOT auto-started anymore).
func TestStartPublishesInstalledAppPorts(test *testing.T) {
	root := seedProject(test, "app")
	// Record an installed app in the project config before start.
	if err := config.WriteProject(root, &config.Config{
		OS:   "debian-trixie",
		Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	// The app's host port is still published at create (host:guest same number) so an
	// on-demand `ai apps start` is reachable — even though apps are NOT auto-started.
	if !strings.Contains(strings.Join(sandbox.netArgs, " "), "-p 21000:21000") {
		test.Fatalf("app port not published in netArgs: %#v", sandbox.netArgs)
	}
	// Apps are NOT auto-started during workspace start (a heavy pull would block it) —
	// they start on demand, so no `nerdctl run` for the app container happens here.
	if execRootRan(sandbox, "aip-app-openwebui") {
		test.Fatalf("app container must NOT be auto-run during start (apps are on-demand): %#v", sandbox.execRootArgv)
	}
}

// TestStartNoAppPortsWhenNoneInstalled confirms nothing extra is published when
// no app is installed.
func TestStartNoAppPortsWhenNoneInstalled(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if strings.Contains(strings.Join(sandbox.netArgs, " "), "-p ") {
		test.Fatalf("unexpected published port when no apps installed: %#v", sandbox.netArgs)
	}
}

func execRootRan(sandbox *fakeSandbox, name string) bool {
	for _, call := range sandbox.execRootArgv {
		for _, arg := range call {
			if arg == name {
				return true
			}
		}
	}
	return false
}

// fakeSleepInhibitor records Inhibit/Release calls for optional lifecycle hooks.
type fakeSleepInhibitor struct{ inhibited, released int }

func (inhibitor *fakeSleepInhibitor) Inhibit(_, _ string) error { inhibitor.inhibited++; return nil }
func (inhibitor *fakeSleepInhibitor) Release(_, _ string) error { inhibitor.released++; return nil }

// Start/Stop still honour an injected SleepInhibitor for tests/future opt-in hooks,
// but RealManager does not wire host sleep prevention by default.
func TestStartInhibitsSleepStopReleases(test *testing.T) {
	seedProject(test, "app")
	sleep := &fakeSleepInhibitor{}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: &fakeSandbox{}, Keys: &fakeKeyMinter{},
		Now: func() string { return "t" }, Sleep: sleep,
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if sleep.inhibited != 1 {
		test.Fatalf("Start must take the keep-awake assertion once, got %d", sleep.inhibited)
	}
	if err := manager.Stop("app"); err != nil {
		test.Fatal(err)
	}
	if sleep.released != 1 {
		test.Fatalf("Stop must release the keep-awake assertion once, got %d", sleep.released)
	}
}

// TestStartCreatesVenv: workspace start creates the per-project .venv-msb virtualenv
// with the guest's baked-in Python (best-effort, guarded by pyvenv.cfg).
func TestStartCreatesVenv(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	found := false
	for _, argv := range sandbox.allExecArgv {
		if strings.Contains(strings.Join(argv, " "), "python3 -m venv /workspace/.venv-msb") {
			found = true
		}
	}
	if !found {
		test.Errorf("Start should create the .venv-msb virtualenv; execs: %v", sandbox.allExecArgv)
	}
}
