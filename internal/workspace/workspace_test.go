package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/overlay"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/state"
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
	// keyInfoErr, when set, makes KeyInfo report the key as invalid (orphaned),
	// driving the attach-path self-heal. keyInfoCalls records the probe count.
	keyInfoErr   error
	keyInfoCalls int
	lastKeyInfo  string
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

func (minter *fakeKeyMinter) KeyInfo(key string) (litellm.KeyDetails, error) {
	minter.keyInfoCalls++
	minter.lastKeyInfo = key
	if minter.keyInfoErr != nil {
		return litellm.KeyDetails{}, minter.keyInfoErr
	}
	return litellm.KeyDetails{}, nil
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

func TestIsSandboxNotFound(test *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{err: nil, want: false},
		{err: errors.New("sandbox not found"), want: true},
		{err: errors.New("Sandbox Not Found"), want: true}, // case-insensitive
		{err: errors.New("connection refused"), want: false},
	}
	for _, tc := range cases {
		if got := isSandboxNotFound(tc.err); got != tc.want {
			test.Errorf("isSandboxNotFound(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestWorkspaceLogTail(test *testing.T) {
	seedProject(test, "my-app")

	// A not-yet-created sandbox ("not found") is an EMPTY log, not an error.
	sandbox := &fakeSandbox{logTailErr: errors.New("sandbox not found")}
	manager := newManager(&fakeBuilder{}, sandbox)
	out, err := manager.WorkspaceLogTail("my-app", 50)
	if err != nil || out != "" {
		test.Errorf("not-found should yield (\"\", nil), got (%q, %v)", out, err)
	}

	// A real read failure surfaces.
	sandbox.logTailErr = errors.New("relay exploded")
	if _, err := manager.WorkspaceLogTail("my-app", 50); err == nil {
		test.Error("a real LogTail failure must surface")
	}

	// The happy path returns the tail and passes the line count through.
	sandbox.logTailErr = nil
	sandbox.logTail = "line1\nline2"
	out, err = manager.WorkspaceLogTail("my-app", 25)
	if err != nil || out != "line1\nline2" {
		test.Errorf("LogTail = (%q, %v), want the tail text", out, err)
	}
	if sandbox.logTailLines != 25 {
		test.Errorf("lines passed to the sandbox = %d, want 25", sandbox.logTailLines)
	}

	// An unknown workspace errors before touching the sandbox.
	if _, err := manager.WorkspaceLogTail("nope", 10); err == nil {
		test.Error("unknown workspace must error")
	}
}

func TestIsRunningFalseWithoutHandle(test *testing.T) {
	seedProject(test, "my-app")
	manager := newManager(&fakeBuilder{}, &fakeSandbox{})
	// No workspace handle saved → not running (and never an error/panic).
	if manager.IsRunning("my-app") {
		test.Error("IsRunning without a started handle should be false")
	}
}

func TestAppGatewayKeyRotatesPerWorkspaceAlias(test *testing.T) {
	minter := &fakeKeyMinter{}
	manager := Manager{Keys: minter}
	key, err := manager.appGatewayKey("aip-my-app", "my-app")
	if err != nil {
		test.Fatalf("appGatewayKey: %v", err)
	}
	if key == "" {
		test.Error("appGatewayKey must return the minted key")
	}
	// Rotation: delete the previous key under the apps alias, then mint fresh.
	if minter.deleteCalls != 1 || minter.deletedAlias != "my-app-apps" {
		test.Errorf("delete calls/alias = %d/%q, want 1/my-app-apps", minter.deleteCalls, minter.deletedAlias)
	}
	if minter.scope.Alias != "my-app-apps" {
		test.Errorf("minted alias = %q, want my-app-apps (must not collide with the agent key)", minter.scope.Alias)
	}
}

func TestCurrentAgentKeyReadsFromVM(test *testing.T) {
	sandbox := &fakeSandbox{execResult: ExecResult{Stdout: "sk-live-r2wg\n"}}
	manager := newManager(&fakeBuilder{}, sandbox)
	if got := manager.currentAgentKey("aip-app"); got != "sk-live-r2wg" {
		test.Errorf("currentAgentKey = %q, want %q (trimmed VM stdout)", got, "sk-live-r2wg")
	}
	// A failed exec (wedged VM) yields no key, so the caller won't churn.
	sandbox.execResult = ExecResult{ExitCode: 1}
	if got := manager.currentAgentKey("aip-app"); got != "" {
		test.Errorf("currentAgentKey on nonzero exit = %q, want empty", got)
	}
}

func TestAgentKeyIsOrphaned(test *testing.T) {
	valid := &fakeKeyMinter{}
	orphaned := &fakeKeyMinter{keyInfoErr: errors.New("Unable to find token")}

	cases := []struct {
		name string
		keys KeyMinter
		key  string
		want bool
	}{
		{name: "no minter", keys: nil, key: "sk-x", want: false},
		{name: "empty key (unreadable VM)", keys: valid, key: "", want: false},
		{name: "valid key at gateway", keys: valid, key: "sk-x", want: false},
		{name: "orphaned key (401 from gateway)", keys: orphaned, key: "sk-x", want: true},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			manager := Manager{Keys: tc.keys}
			if got := manager.agentKeyIsOrphaned(tc.key); got != tc.want {
				test.Errorf("agentKeyIsOrphaned = %v, want %v", got, tc.want)
			}
		})
	}
	// A valid-key probe must actually reach the gateway with the exact key value.
	if valid.keyInfoCalls == 0 || valid.lastKeyInfo != "sk-x" {
		test.Errorf("KeyInfo probe not invoked with the VM key: calls=%d last=%q", valid.keyInfoCalls, valid.lastKeyInfo)
	}
}

func TestRegisterCavemanRespectsToggle(test *testing.T) {
	execedCaveman := func(argv [][]string) bool {
		for _, args := range argv {
			for _, token := range args {
				if strings.Contains(token, "caveman") {
					return true
				}
			}
		}
		return false
	}
	disabled := false
	enabled := true
	for _, tc := range []struct {
		name        string
		cavemanCfg  *bool
		tools       []string
		wantInstall bool
	}{
		{name: "disabled skips install", cavemanCfg: &disabled, tools: []string{"opencode"}, wantInstall: false},
		{name: "enabled with detectable CLI installs", cavemanCfg: &enabled, tools: []string{"opencode"}, wantInstall: true},
		{name: "enabled without detectable CLI skips", cavemanCfg: &enabled, tools: []string{"pi"}, wantInstall: false},
	} {
		test.Run(tc.name, func(test *testing.T) {
			sandbox := &fakeSandbox{}
			manager := newManager(&fakeBuilder{}, sandbox)
			projectConfig := &config.Config{
				Agent:   config.AgentConfig{Tools: tc.tools},
				Context: config.ContextConfig{CavemanEnabled: tc.cavemanCfg},
			}
			manager.registerCaveman("aip-app", projectConfig)
			if got := execedCaveman(sandbox.allExecArgv); got != tc.wantInstall {
				test.Errorf("caveman install exec = %v, want %v (execs: %v)", got, tc.wantInstall, sandbox.allExecArgv)
			}
		})
	}
}

// findCavemanScript returns the shell script from the single `bash -lc <script>`
// caveman-install exec recorded on the fake sandbox (empty if none).
// findCavemanScript returns the once-guarded install script STAGED in-VM (it is
// written to a file, then launched DETACHED via setsid — see registerCaveman).
func findCavemanScript(sandbox *fakeSandbox) string {
	return string(sandbox.written[cavemanScriptGuest])
}

// cavemanLaunched reports whether a detached-launch exec (setsid bash <script>) was
// issued for the staged install script.
func cavemanLaunched(argv [][]string) bool {
	for _, args := range argv {
		if len(args) == 3 && args[0] == "bash" && args[1] == "-lc" &&
			strings.Contains(args[2], "setsid bash") && strings.Contains(args[2], cavemanScriptGuest) {
			return true
		}
	}
	return false
}

// TestRegisterCavemanScript pins the shape of the in-VM install script: it must
// run Caveman's LOCAL installer (`git clone` + `node bin/install.js --only …`) from
// $HOME (never ~/project — a submodule .git file there dies git repo-discovery), and
// gate the once-guard marker on the caveman SKILL.md actually landing in the shared
// pool so a failed/empty install retries on the next start.
func TestRegisterCavemanScript(test *testing.T) {
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)
	enabled := true
	projectConfig := &config.Config{
		// Two detectable CLIs → two --only flags; pi is NOT detectable (shared pool only).
		Agent:   config.AgentConfig{Tools: []string{"opencode", "claude-code", "pi"}},
		Context: config.ContextConfig{CavemanEnabled: &enabled},
	}
	manager.registerCaveman("aip-app", projectConfig)

	script := findCavemanScript(sandbox)
	if script == "" {
		test.Fatalf("no caveman install script staged in-VM: %v", sandbox.written)
	}
	// The install is DETACHED: the script is staged to a file and launched via setsid
	// (so the multi-minute install neither blocks start nor wedges the msb relay).
	if !cavemanLaunched(sandbox.allExecArgv) {
		test.Errorf("install must be launched detached (setsid bash %s): %v", cavemanScriptGuest, sandbox.allExecArgv)
	}
	// Runs from $HOME, not the bind-mounted project dir.
	if !strings.Contains(script, `cd "$HOME";`) {
		test.Errorf("script must run from $HOME, got: %q", script)
	}
	if strings.Contains(script, "curl") || strings.Contains(script, "npx") {
		test.Errorf("script must not use the curl|bash / npx path: %q", script)
	}
	// Clone + local installer.
	if !strings.Contains(script, "git clone --depth 1 https://github.com/JuliusBrussee/caveman /tmp/caveman-src") {
		test.Errorf("script must clone caveman: %q", script)
	}
	if !strings.Contains(script, "node bin/install.js --non-interactive --with-hooks") {
		test.Errorf("script must run the local installer: %q", script)
	}
	// Both detectable CLIs mapped to --only tokens; pi absent.
	if !strings.Contains(script, "--only opencode") || !strings.Contains(script, "--only claude") {
		test.Errorf("script missing per-CLI --only flags: %q", script)
	}
	if strings.Contains(script, "--only pi") {
		test.Errorf("pi is not caveman-detectable and must not get an --only flag: %q", script)
	}
	// Marker is gated on SKILL.md landing in the shared pool, and only touched after
	// the install succeeded (installed exit code checked, chained with &&).
	if !strings.Contains(script, workspaceWorkdir+"/.ai-platform/skills/caveman/SKILL.md") {
		test.Errorf("script must gate on the caveman SKILL.md in the pool: %q", script)
	}
	if !strings.Contains(script, `[ "$installed" -eq 0 ] && { `) {
		test.Errorf("mirror + marker must be gated on install success in a brace group: %q", script)
	}
	if !strings.Contains(script, "touch "+workspaceWorkdir+"/.ai-platform/.caveman-installed") {
		test.Errorf("script must touch the once-guard marker: %q", script)
	}
	// The gate, the SKILL.md check, and the marker touch must be one &&-chain so the
	// marker is never recorded when the install failed or dropped nothing.
	gateIdx := strings.Index(script, `[ "$installed" -eq 0 ] &&`)
	skillIdx := strings.Index(script, "/skills/caveman/SKILL.md ] &&")
	touchIdx := strings.LastIndex(script, "touch "+workspaceWorkdir+"/.ai-platform/.caveman-installed")
	if gateIdx < 0 || gateIdx >= skillIdx || skillIdx >= touchIdx {
		test.Errorf("gate → SKILL.md check → marker touch must be an ordered &&-chain: %q", script)
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
	openCodeConfig := readProjectConfig(test, root, ".opencode", "opencode.json")
	piConfig := readGuestFile(test, sandbox, agentcfg.PiGlobalModelsGuest)
	// The project configs are KEYLESS (they reference the key via env interpolation,
	// not the literal value); pi must not carry the per-request Headroom knobs.
	if !strings.Contains(openCodeConfig, agentcfg.OpenCodeAPIKeyRef) {
		test.Error("opencode config is missing the env key-ref")
	}
	if strings.Contains(openCodeConfig, "sk-fake-workspace-key") {
		test.Error("opencode project config must be keyless")
	}
	if !strings.Contains(openCodeConfig, "headroom_keep_turns") {
		test.Error("opencode config is missing the Headroom knobs")
	}
	if strings.Contains(piConfig, "headroom_keep_turns") {
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
	if !strings.Contains(openCodeConfig, "http://host.microsandbox.internal:18787/v1") {
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

	// (1) Each CLI's config is written into its DEFAULT PROJECT location on host disk,
	// KEYLESS (base URL + an env key-ref), so the project is self-describing.
	opencode := readProjectConfig(test, root, ".opencode", "opencode.json")
	if !strings.Contains(opencode, agentcfg.OpenCodeAPIKeyRef) || !strings.Contains(opencode, "host.microsandbox.internal:18787/v1") {
		test.Errorf("opencode project config missing key-ref/baseURL:\n%s", opencode)
	}
	// pi reads the GLOBAL ~/.pi/agent/models.json (written into the VM), not a host file.
	pi := readGuestFile(test, sandbox, agentcfg.PiGlobalModelsGuest)
	if !strings.Contains(pi, agentcfg.PiAPIKeyRef) {
		test.Errorf("pi global models config missing key ref:\n%s", pi)
	}
	claude := readProjectConfig(test, root, ".claude", "settings.json")
	if !strings.Contains(claude, "ANTHROPIC_BASE_URL") || !strings.Contains(claude, "host.microsandbox.internal:18787") {
		test.Errorf("claude settings missing the base-URL env block:\n%s", claude)
	}
	codex := readProjectConfig(test, root, ".codex", "config.toml")
	if !strings.Contains(codex, "env_key") || !strings.Contains(codex, "wire_api") {
		test.Errorf("codex project config missing provider block:\n%s", codex)
	}

	// (2) codex's project-trust entry is written IN-VM (global ~/.codex/config.toml),
	// keyless — so codex loads the per-project provider block.
	if trust, wrote := sandbox.written[agentcfg.CodexConfigGuestPath]; !wrote || !strings.Contains(string(trust), "trust_level") {
		test.Errorf("codex trust entry not written in-VM: %q", trust)
	}

	// (3) The agent env file (IN-VM ONLY) carries the scoped key + gateway env vars +
	// OPENCODE_CONFIG (pointing opencode at its project config).
	agentEnv, wrote := sandbox.written[agentEnvGuestPath]
	if !wrote {
		test.Fatal("agent env file not written into the microVM")
	}
	envText := string(agentEnv)
	if !strings.Contains(envText, key) {
		test.Error("agent env file must carry the scoped key (in-VM only)")
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "GOOGLE_GEMINI_BASE_URL", "GEMINI_API_KEY", "AIP_GATEWAY_KEY", "OPENCODE_CONFIG"} {
		if !strings.Contains(envText, want) {
			test.Errorf("agent env file missing %q:\n%s", want, envText)
		}
	}

	// (4) The managed bash_profile sources the agent env (so `ai shell` routes too).
	if profile, wrote := sandbox.written[bashProfileGuestPath]; !wrote || !strings.Contains(string(profile), agentEnvGuestPath) {
		test.Errorf("managed bash_profile must source the agent env file: %q", profile)
	}

	// (5) The Headroom-wrap alias snippet is written IN-VM and the managed rc block is
	// appended to BOTH framework shells' rc so an interactive shell sources it (the alias
	// CONTENT for installed wrappable CLIs is asserted in TestStartAppliesZshLoginShell,
	// which seeds a config with agent tools).
	readGuestFile(test, sandbox, shellAliasesGuestPath)
	assertRCBlockAppended(test, sandbox, bashrcGuestPath)
	assertRCBlockAppended(test, sandbox, zshrcGuestPath)

	// (6) HARD security constraint: NO host-disk project config contains the scoped key.
	assertProjectConfigsKeyless(test, root, key)
}

// assertRCBlockAppended checks that Start staged the managed rc block for rcPath and ran an
// Exec that strips the prior block (sed on the markers) and appends the fresh one.
func assertRCBlockAppended(test *testing.T, sandbox *fakeSandbox, rcPath string) {
	test.Helper()
	staged, ok := sandbox.written[rcPath+".aip-block"]
	if !ok || !strings.Contains(string(staged), agentcfg.ShellRCMarkerBegin) {
		test.Errorf("managed rc block not staged for %s", rcPath)
	}
	found := false
	for _, argv := range sandbox.allExecArgv {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, rcPath) && strings.Contains(joined, agentcfg.ShellRCMarkerBegin) {
			found = true
			break
		}
	}
	if !found {
		test.Errorf("no Exec appended the managed rc block to %s:\n%v", rcPath, sandbox.allExecArgv)
	}
}

// TestStartAppliesZshLoginShell verifies that a zsh workspace runs a best-effort chsh to
// zsh as ROOT, while a bash (default) workspace does NOT — both still get the rc block +
// aliases so either shell is fully configured.
func TestStartAppliesZshLoginShell(test *testing.T) {
	chshRun := func(sandbox *fakeSandbox) bool {
		for _, argv := range sandbox.execRootArgv {
			if strings.Contains(strings.Join(argv, " "), "chsh -s") {
				return true
			}
		}
		return false
	}

	// zsh: a chsh -s to zsh for the workspace user is issued (as root). codex is set to
	// OAUTH so it (a Headroom-wrappable CLI bypassing the gateway) gets a wrap alias.
	rootZsh := seedProject(test, "zapp")
	if err := config.WriteProject(rootZsh, &config.Config{
		Agent:     config.AgentConfig{Tools: []string{"opencode", "codex", "pi"}, AuthModes: map[string]string{"codex": "oauth"}},
		Workspace: config.WorkspaceConfig{Shell: "zsh"},
	}); err != nil {
		test.Fatal(err)
	}
	sandboxZsh := &fakeSandbox{}
	managerZsh := Manager{Builder: &fakeBuilder{}, Sandbox: sandboxZsh, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := managerZsh.Start("zapp"); err != nil {
		test.Fatal(err)
	}
	if !chshRun(sandboxZsh) {
		test.Errorf("zsh workspace must chsh the login shell to zsh:\n%v", sandboxZsh.execRootArgv)
	}
	// Only OAUTH-mode wrappable CLIs get an alias: codex (oauth) does; opencode (gateway
	// only, never oauth) and pi (api-key + not wrappable) do not.
	aliases := readGuestFile(test, sandboxZsh, shellAliasesGuestPath)
	if !strings.Contains(aliases, "alias codex='headroom wrap codex'") {
		test.Errorf("oauth codex must be Headroom-wrap aliased:\n%s", aliases)
	}
	if strings.Contains(aliases, "alias opencode=") {
		test.Errorf("api-key/gateway opencode must NOT be aliased:\n%s", aliases)
	}
	if strings.Contains(aliases, "alias pi=") {
		test.Errorf("pi is not Headroom-wrappable and must not be aliased:\n%s", aliases)
	}

	// bash (default, unset shell): no chsh — the shell stays bash.
	seedProject(test, "bapp")
	sandboxBash := &fakeSandbox{}
	managerBash := Manager{Builder: &fakeBuilder{}, Sandbox: sandboxBash, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := managerBash.Start("bapp"); err != nil {
		test.Fatal(err)
	}
	if chshRun(sandboxBash) {
		test.Errorf("bash workspace must NOT chsh:\n%v", sandboxBash.execRootArgv)
	}
}

// TestStartOAuthAgentBypassesGateway verifies an OAuTH-mode claude-code: its gateway env
// is omitted (so its own subscription login wins), its cred dir ~/.claude is symlinked to
// /persist, it gets the `headroom wrap claude` alias, and the no-key-on-host invariant
// holds. api-key agents (opencode/pi) keep their gateway env.
func TestStartOAuthAgentBypassesGateway(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		Agent: config.AgentConfig{
			Tools:     []string{"opencode", "claude-code", "gemini"},
			AuthModes: map[string]string{"claude-code": "oauth", "gemini": "api-key"},
		},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/llama3.2:latest"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	const key = "sk-fake-workspace-key"

	// (1) The agent env file must NOT carry claude-code's gateway env (oauth), but MUST
	// keep gemini's (api-key) and the shared AIP_GATEWAY_KEY (opencode/pi).
	envText := readGuestFile(test, sandbox, agentEnvGuestPath)
	for _, absent := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if strings.Contains(envText, absent) {
			test.Errorf("oauth claude-code must not export %q:\n%s", absent, envText)
		}
	}
	for _, want := range []string{"GEMINI_API_KEY", "AIP_GATEWAY_KEY"} {
		if !strings.Contains(envText, want) {
			test.Errorf("agent env missing api-key/shared var %q:\n%s", want, envText)
		}
	}

	// (2) claude's project settings must carry NO gateway base URL (oauth), so its own
	// login reaches Anthropic directly.
	claude := readProjectConfig(test, root, ".claude", "settings.json")
	if strings.Contains(claude, "ANTHROPIC_BASE_URL") {
		test.Errorf("oauth claude settings must not carry the gateway base URL:\n%s", claude)
	}

	// (3) ~/.claude is symlinked to /persist so the native login survives restarts, and
	// the wrap alias is present for the oauth claude.
	linkedClaude := false
	for _, argv := range sandbox.allExecArgv {
		if strings.Contains(strings.Join(argv, " "), "ln -sfn /persist/agents/claude ~/.claude") {
			linkedClaude = true
		}
	}
	if !linkedClaude {
		test.Errorf("oauth claude-code must symlink ~/.claude to /persist:\n%v", sandbox.allExecArgv)
	}
	aliases := readGuestFile(test, sandbox, shellAliasesGuestPath)
	if !strings.Contains(aliases, "alias claude='headroom wrap claude'") {
		test.Errorf("oauth claude-code must be Headroom-wrap aliased:\n%s", aliases)
	}

	// (4) The scoped key never touches host disk.
	assertProjectConfigsKeyless(test, root, key)
}

// TestStartAPIKeyAgentRoutesThroughGateway verifies an api-key claude-code keeps its
// gateway env and gets NO wrap alias (it already routes through the gateway).
// TestStartRoutesOpenClawAndHermes verifies the two new gateway/api-key agent CLIs: each
// gets its GLOBAL config written IN-VM (off host disk) pointing at the gateway and KEYLESS
// (openclaw via ${AIP_GATEWAY_KEY}, hermes via key_env), and neither gets a Headroom wrap
// alias (they route through the gateway where the Headroom guardrail already applies).
func TestStartRoutesOpenClawAndHermes(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"openclaw", "hermes"}, DefaultTool: "openclaw"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/llama3.2:latest"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	openClaw := readGuestFile(test, sandbox, agentcfg.OpenClawConfigGuest)
	if !strings.Contains(openClaw, agentcfg.OpenClawAPIKeyRef) || !strings.Contains(openClaw, "host.microsandbox.internal:18787/v1") {
		test.Errorf("openclaw config missing key-ref/baseURL:\n%s", openClaw)
	}
	if !strings.Contains(openClaw, "ollama/llama3.2:latest") {
		test.Errorf("openclaw config must enumerate the served model:\n%s", openClaw)
	}
	hermes := readGuestFile(test, sandbox, agentcfg.HermesConfigGuest)
	if !strings.Contains(hermes, agentcfg.HermesKeyEnv) || !strings.Contains(hermes, "host.microsandbox.internal:18787/v1") {
		test.Errorf("hermes config missing key_env/base_url:\n%s", hermes)
	}
	// Both configs are keyless (the scoped key lives only in the agent env file).
	for name, content := range map[string]string{"openclaw": openClaw, "hermes": hermes} {
		if strings.Contains(content, "sk-fake-workspace-key") {
			test.Errorf("%s config must be keyless (no scoped key on disk):\n%s", name, content)
		}
	}
	// api-key agents get NO wrap alias.
	aliases := readGuestFile(test, sandbox, shellAliasesGuestPath)
	if strings.Contains(aliases, "openclaw") || strings.Contains(aliases, "hermes") {
		test.Errorf("openclaw/hermes route through the gateway and must NOT be wrap-aliased:\n%s", aliases)
	}
}

func TestStartAPIKeyAgentRoutesThroughGateway(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		Agent: config.AgentConfig{Tools: []string{"claude-code"}, AuthModes: map[string]string{"claude-code": "api-key"}},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	envText := readGuestFile(test, sandbox, agentEnvGuestPath)
	for _, want := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if !strings.Contains(envText, want) {
			test.Errorf("api-key claude-code must keep its gateway env %q:\n%s", want, envText)
		}
	}
	aliases := readGuestFile(test, sandbox, shellAliasesGuestPath)
	if strings.Contains(aliases, "alias claude=") {
		test.Errorf("api-key claude-code routes through the gateway and must NOT be wrap-aliased:\n%s", aliases)
	}
	claude := readProjectConfig(test, root, ".claude", "settings.json")
	if !strings.Contains(claude, "ANTHROPIC_BASE_URL") {
		test.Errorf("api-key claude settings must carry the gateway base URL:\n%s", claude)
	}
}

// TestStartCopilotForcedOAuth verifies a workspace with copilot (forced-oauth, gateway-
// incapable): it gets the `headroom wrap copilot` alias, its cred dir ~/.copilot is
// symlinked to /persist, Graphify is registered with --platform copilot, and NO gateway
// env or on-disk gateway config is written for it (it authenticates natively to GitHub).
func TestStartCopilotForcedOAuth(test *testing.T) {
	root := seedProject(test, "app")
	// No AuthModes recorded — copilot must be treated as oauth regardless.
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"opencode", "copilot"}, DefaultTool: "opencode"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	// (1) Headroom-wrap alias for copilot.
	aliases := readGuestFile(test, sandbox, shellAliasesGuestPath)
	if !strings.Contains(aliases, "alias copilot='headroom wrap copilot'") {
		test.Errorf("copilot must be Headroom-wrap aliased:\n%s", aliases)
	}

	// (2) ~/.copilot symlinked to /persist so the native GitHub login survives restarts.
	linkedCopilot := false
	sawGraphifyCopilot := false
	for _, argv := range sandbox.allExecArgv {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "ln -sfn /persist/agents/copilot ~/.copilot") {
			linkedCopilot = true
		}
		if strings.Contains(joined, "cd /home/workspace/project") && strings.Contains(joined, "graphify install --project --platform copilot") {
			sawGraphifyCopilot = true
		}
	}
	if !linkedCopilot {
		test.Errorf("copilot must symlink ~/.copilot to /persist:\n%v", sandbox.allExecArgv)
	}
	// (3) Graphify registered with --platform copilot at start.
	if !sawGraphifyCopilot {
		test.Errorf("Start must register Graphify with --platform copilot:\n%v", sandbox.allExecArgv)
	}

	// (4) No gateway env or gateway config is written for copilot (it can't route through
	// the gateway). The agent env script never mentions copilot/github.
	envText := readGuestFile(test, sandbox, agentEnvGuestPath)
	for _, absent := range []string{"copilot", "githubcopilot", "GH_TOKEN"} {
		if strings.Contains(envText, absent) {
			test.Errorf("copilot must have no gateway env (found %q):\n%s", absent, envText)
		}
	}
}

// readProjectConfig reads a per-CLI project config file written under the project root.
func readProjectConfig(test *testing.T, root string, parts ...string) string {
	test.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
	if err != nil {
		test.Fatalf("project config %v not written: %v", parts, err)
	}
	return string(content)
}

// readGuestFile reads a file written into the microVM via Sandbox.WriteFile (recorded
// by the fake). pi's models config lives at the GLOBAL in-VM path pi reads, not on host.
func readGuestFile(test *testing.T, sandbox *fakeSandbox, guestPath string) string {
	test.Helper()
	content, ok := sandbox.written[guestPath]
	if !ok {
		test.Fatalf("guest file %q not written via WriteFile", guestPath)
	}
	return string(content)
}

// runtimeExecRootCalls filters the recorded ExecRoot calls to the in-VM container-runtime
// bring-up (nerdctl/containerd), ignoring unrelated root ops such as the /persist agent-
// state prep, so containerd tests can assert the exact probe/boot sequence.
func runtimeExecRootCalls(sandbox *fakeSandbox) [][]string {
	var calls [][]string
	for _, argv := range sandbox.execRootArgv {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "nerdctl") || strings.Contains(joined, "containerd") {
			calls = append(calls, argv)
		}
	}
	return calls
}

// assertProjectConfigsKeyless fails if any on-disk (host, project) per-CLI config
// contains the scoped key (the key must live only in the in-VM agent env file).
func assertProjectConfigsKeyless(test *testing.T, root, key string) {
	test.Helper()
	for _, parts := range [][]string{
		{".opencode", "opencode.json"}, {".pi", "settings.json"},
		{".claude", "settings.json"}, {".codex", "config.toml"},
	} {
		content, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
		if err != nil {
			test.Fatalf("project config %v not written: %v", parts, err)
		}
		if strings.Contains(string(content), key) {
			test.Errorf("project config %v contains the scoped key — it MUST be keyless", parts)
		}
	}
}

// TestStartPreservesProjectConfigEdits verifies a user's edit to a project config
// (a custom top-level key) is preserved across start (deep-merged, not clobbered) and
// the merged result stays KEYLESS — the key is referenced via env, never embedded.
func TestStartPreservesProjectConfigEdits(test *testing.T) {
	root := seedProject(test, "app")
	dir := filepath.Join(root, ".opencode")
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

	merged := readProjectConfig(test, root, ".opencode", "opencode.json")
	if !strings.Contains(merged, "my-custom-theme") {
		test.Errorf("user edit not preserved in the merged project config:\n%s", merged)
	}
	if !strings.Contains(merged, "host.microsandbox.internal:18787/v1") {
		test.Errorf("merged config missing the gateway provider:\n%s", merged)
	}
	// KEYLESS: references the key via {env:…}, never the literal scoped key.
	if !strings.Contains(merged, agentcfg.OpenCodeAPIKeyRef) {
		test.Errorf("merged config must reference the key via env, not embed it:\n%s", merged)
	}
	if strings.Contains(merged, "sk-fake-workspace-key") {
		test.Errorf("merged project config must be keyless:\n%s", merged)
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
	runtimeCalls := runtimeExecRootCalls(sandbox)
	if len(runtimeCalls) != 2 {
		test.Fatalf("expected a probe + a boot ExecRoot call, got %d: %v", len(runtimeCalls), runtimeCalls)
	}
	probe := strings.Join(runtimeCalls[0], " ")
	// The probe is a quiet, bounded `nerdctl info` (output discarded — the
	// expected first-boot "cannot access containerd socket" fatal is not shown).
	if probe != "sh -c timeout 5 nerdctl info >/dev/null 2>&1" {
		test.Errorf("first ExecRoot must probe the runtime quietly (bounded), got %q", probe)
	}
	boot := strings.Join(runtimeCalls[1], " ")
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
	if runtimeCalls := runtimeExecRootCalls(sandbox); len(runtimeCalls) != 1 {
		test.Fatalf("expected only the probe ExecRoot when containerd is up, got %v", runtimeCalls)
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

// pinHostGatewayIPv4 must rewrite the guest /etc/hosts so the local msb gateway
// name resolves to IPv4 only — the fix for agents failing over the IPv6 record that
// msb's host-forward resets ("socket connection was closed unexpectedly").
func TestPinHostGatewayIPv4RewritesGuestHosts(test *testing.T) {
	sandbox := &fakeSandbox{}
	manager := Manager{Sandbox: sandbox}

	manager.pinHostGatewayIPv4("aip-app", runtime.DefaultGatewayHost)

	if len(sandbox.execRootArgv) != 1 {
		test.Fatalf("expected exactly one ExecRoot call, got %d: %v", len(sandbox.execRootArgv), sandbox.execRootArgv)
	}
	script := strings.Join(sandbox.execRootArgv[0], " ")
	for _, want := range []string{"getent ahostsv4", "/etc/hosts", "sed -i", runtime.DefaultGatewayHost} {
		if !strings.Contains(script, want) {
			test.Errorf("pin script must contain %q; got %q", want, script)
		}
	}
}

// A local-gateway start must issue the /etc/hosts pin; the failure of that
// best-effort step must not fail the start.
func TestStartPinsHostGatewayIPv4(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	manager := newManager(&fakeBuilder{}, sandbox)

	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start failed: %v", err)
	}
	pinned := false
	for _, argv := range sandbox.execRootArgv {
		if strings.Contains(strings.Join(argv, " "), "/etc/hosts") {
			pinned = true
			break
		}
	}
	if !pinned {
		test.Fatal("a local-gateway Start must issue the /etc/hosts IPv4 pin ExecRoot")
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

// Shell opens a PERSISTENT, reattachable tmux session named "shell" in ~/project
// running a login shell, via a single atomic `tmux new-session -A` (create-or-attach).
func TestShellOpensPersistentTmuxSession(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); err != nil {
		test.Fatal(err)
	}
	// One atomic create-or-attach in the interactive exec (`tmux new-session -A`),
	// running a login shell that starts in ~/project.
	want := append([]string{"tmux", "new-session", "-A", "-s", "shell", "-c", "/home/workspace/project"},
		projectLoginShell()...)
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
// running that CLI's launch command in ~/project (detached create, then attach).
func TestAgentStartsPerCLITmuxSession(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Agent("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	// The launch is wrapped in a login shell that sources the in-VM agent env file
	// (gateway env vars for the env-routed CLIs) then execs the CLI.
	want := append([]string{"tmux", "new-session", "-A", "-s", "opencode", "-c", "/home/workspace/project"},
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
	want := append([]string{"tmux", "new-session", "-A", "-s", "claude-code", "-c", "/home/workspace/project"},
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

// Attach create-or-attaches a named session running a login shell that starts in
// ~/project (a fresh session opens there); a blank session targets the default
// "shell".
func TestAttachSession(test *testing.T) {
	seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Attach("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	want := append([]string{"tmux", "new-session", "-A", "-s", "opencode", "-c", "/home/workspace/project"},
		projectLoginShell()...)
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Attach ran %v via ExecInteractive, want %v", got, want)
	}

	defaulted := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, defaulted).Attach("app", ""); err != nil {
		test.Fatal(err)
	}
	wantDefault := append([]string{"tmux", "new-session", "-A", "-s", "shell", "-c", "/home/workspace/project"},
		projectLoginShell()...)
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
	if !strings.Contains(string(conf), "status off") || !strings.Contains(string(conf), "mouse off") {
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

// TestStartToleratesRefreshInstallFailure: refresh-models is a CONVENIENCE helper, so
// a non-zero exit from its install must NOT fail Start or tear the microVM down — the
// workspace still comes up (warn + continue). The msb rootfs persists across starts,
// so an install hiccup ("File exists" on a leftover copy) must never wedge the start.
func TestStartToleratesRefreshInstallFailure(test *testing.T) {
	_ = seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 1, Stderr: "install: File exists"}}
	manager := newManager(&fakeBuilder{}, sandbox)
	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must tolerate a refresh-models install failure: %v", err)
	}
	if sandbox.destroyed {
		test.Fatal("a refresh-models install failure must NOT roll back the microVM")
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
	root := seedProject(test, "app")
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

	// opencode's config is on host; pi's is at the GLOBAL in-VM path pi reads.
	configs := map[string]string{
		"opencode": readProjectConfig(test, root, ".opencode", "opencode.json"),
		"pi":       readGuestFile(test, sandbox, agentcfg.PiGlobalModelsGuest),
	}
	for name, config := range configs {
		// Every served model is present.
		for _, want := range []string{"ollama/llama3.2:latest", "ollama/qwen2.5:7b", "anthropic/claude-opus-4-8"} {
			if !strings.Contains(config, want) {
				test.Errorf("%s config missing served model %q", name, want)
			}
		}
	}
	// The duplicate served entry is collapsed: in opencode's model map the model id
	// appears as a JSON key exactly once (`"anthropic/claude-opus-4-8":`), not twice.
	openCode := configs["opencode"]
	if got := strings.Count(openCode, `"anthropic/claude-opus-4-8":`); got != 1 {
		test.Errorf("opencode: served model keyed %d times, want 1 (deduped)", got)
	}
	// This project chose no setup model, so no top-level default is written (the default
	// is the workspace-setup model, tested in TestStartDefaultsToSetupModel).
	if strings.Contains(openCode, `"model":`) {
		test.Errorf("opencode config must not carry a top-level default model:\n%s", openCode)
	}
}

// TestStartDefaultsToSetupModel verifies the model chosen at workspace setup (stored as
// agent.graphify_model) becomes the DEFAULT model for every agent CLI — registered as
// ollama/<model> in the gateway. opencode gets a top-level model, pi's settings get a
// defaultModel, and codex gets a model line.
func TestStartDefaultsToSetupModel(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{Agent: config.AgentConfig{GraphifyModel: "qwen2.5-coder:7b"}}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/qwen2.5-coder:7b"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	openCode := readProjectConfig(test, root, ".opencode", "opencode.json")
	if !strings.Contains(openCode, `"aip-gateway/ollama/qwen2.5-coder:7b"`) {
		test.Errorf("opencode config missing the setup default model:\n%s", openCode)
	}
	piSettings := readProjectConfig(test, root, ".pi", "settings.json")
	if !strings.Contains(piSettings, `"ollama/qwen2.5-coder:7b"`) {
		test.Errorf("pi settings missing the setup default model:\n%s", piSettings)
	}
	codex := readProjectConfig(test, root, ".codex", "config.toml")
	if !strings.Contains(codex, `ollama/qwen2.5-coder:7b`) {
		test.Errorf("codex config missing the setup default model:\n%s", codex)
	}
}

// TestStartDropsSeededDefaultOnSecondStart verifies the seed-then-remember behavior:
// the first start pins the setup model, but a subsequent start DROPS the pinned model
// (opencode ranks config "model" above last-used, so leaving it would defeat the user's
// persisted /model choice). The seed marker under .ai-platform gates this.
func TestStartDropsSeededDefaultOnSecondStart(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{Agent: config.AgentConfig{GraphifyModel: "qwen2.5-coder:7b"}}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/qwen2.5-coder:7b"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}

	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if first := readProjectConfig(test, root, ".opencode", "opencode.json"); !strings.Contains(first, `"aip-gateway/ollama/qwen2.5-coder:7b"`) {
		test.Fatalf("first start should seed the default model:\n%s", first)
	}

	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if second := readProjectConfig(test, root, ".opencode", "opencode.json"); strings.Contains(second, `"model":`) {
		test.Errorf("second start must not pin a default model (remember last-used):\n%s", second)
	}
	if piSettings := readProjectConfig(test, root, ".pi", "settings.json"); strings.Contains(piSettings, "defaultModel") {
		test.Errorf("second start must drop pi's defaultModel:\n%s", piSettings)
	}
}

// TestAttachRefreshesModelList verifies attaching a shell to an ALREADY-RUNNING workspace
// refreshes the opencode + pi served-model lists from the live gateway (so a model added
// via `ai models`/`ai keys` since the last start is visible without a restart).
func TestAttachRefreshesModelList(test *testing.T) {
	root := seedStartedWorkspace(test, "app")
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/fresh:latest"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}

	if err := manager.Shell("app"); err != nil {
		test.Fatalf("Shell: %v", err)
	}
	if openCode := readProjectConfig(test, root, ".opencode", "opencode.json"); !strings.Contains(openCode, "ollama/fresh:latest") {
		test.Errorf("attach did not refresh the opencode model list:\n%s", openCode)
	}
	if pi := readGuestFile(test, sandbox, agentcfg.PiGlobalModelsGuest); !strings.Contains(pi, "ollama/fresh:latest") {
		test.Errorf("attach did not refresh the pi model list:\n%s", pi)
	}
	// The attach refresh must NOT rotate the scoped key (that would invalidate a running
	// agent) — the list-only refresh mints no key.
	if minter, ok := manager.Keys.(*fakeKeyMinter); ok && minter.calls != 0 {
		test.Errorf("attach refresh must not mint a key, got %d GenerateKey calls", minter.calls)
	}
}

// TestRestartDropsRemovedModel verifies the opencode list is REPLACED (not unioned)
// across restarts: a model removed upstream (via `ai models rm`/`ai keys remove`) between
// starts must disappear from the config, while still-served models remain.
func TestRestartDropsRemovedModel(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{}
	minter := &fakeKeyMinter{}
	first := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: minter, Now: func() string { return "t" },
		Served: fakeServedModels{models: []string{"ollama/a:latest", "ollama/b:latest"}}}
	if _, err := first.Start("app"); err != nil {
		test.Fatal(err)
	}
	if got := readProjectConfig(test, root, ".opencode", "opencode.json"); !strings.Contains(got, "ollama/b:latest") {
		test.Fatalf("first start should list b:\n%s", got)
	}

	// b removed upstream; the next start must drop it (not keep a stale union entry).
	second := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: minter, Now: func() string { return "t" },
		Served: fakeServedModels{models: []string{"ollama/a:latest"}}}
	if _, err := second.Start("app"); err != nil {
		test.Fatal(err)
	}
	got := readProjectConfig(test, root, ".opencode", "opencode.json")
	if strings.Contains(got, "ollama/b:latest") {
		test.Errorf("restart must drop the removed model b (list REPLACED, not unioned):\n%s", got)
	}
	if !strings.Contains(got, "ollama/a:latest") {
		test.Errorf("restart must keep the still-served model a:\n%s", got)
	}
}

// TestStartPickerDegradesWhenGatewayDown verifies the picker degrades gracefully:
// when ServedModels errors (the gateway is down at start), the picker is empty but
// the workspace still starts.
func TestStartPickerDegradesWhenGatewayDown(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{}
	served := fakeServedModels{err: errors.New("litellm: connection refused")}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Served: served, Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must NOT fail when the served-models lookup fails: %v", err)
	}

	config := readProjectConfig(test, root, ".opencode", "opencode.json")
	// The picker is empty — no model ids at all.
	if strings.Contains(config, "ollama/") || strings.Contains(config, "anthropic/") {
		test.Errorf("degraded config must have an empty picker:\n%s", config)
	}
}

// TestStartPickerNilSourceIsEmpty verifies a nil ServedModels source (Manager
// without the dep) also degrades to an empty picker without panicking.
func TestStartPickerNilSourceIsEmpty(test *testing.T) {
	root := seedProject(test, "app")
	sandbox := &fakeSandbox{}
	manager := Manager{
		Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{},
		Now: func() string { return "t" },
	}
	if _, err := manager.Start("app"); err != nil {
		test.Fatalf("Start must not fail with a nil ServedModels source: %v", err)
	}
	config := readProjectConfig(test, root, ".opencode", "opencode.json")
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
		if strings.Contains(strings.Join(argv, " "), "python3 -m venv /home/workspace/project/.venv-msb") {
			found = true
		}
	}
	if !found {
		test.Errorf("Start should create the .venv-msb virtualenv; execs: %v", sandbox.allExecArgv)
	}
}

// Start registers Graphify with each selected agent CLI at RUNTIME (not image
// build), running `graphify install --project …` in the mounted project dir
// (~/project) so its project-scoped files land where the CLI reads them.
func TestStartRegistersGraphify(test *testing.T) {
	root := seedProject(test, "app")
	// The selected agent CLIs come from the project config.yaml (written by
	// `ai create`); seed opencode + pi so registerGraphify has tools to register.
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"opencode", "pi"}, DefaultTool: "opencode"},
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
	var sawOpencode, sawPi bool
	for _, argv := range sandbox.allExecArgv {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "cd /home/workspace/project") {
			continue
		}
		if strings.Contains(joined, "graphify install --project --platform opencode") {
			sawOpencode = true
		}
		if strings.Contains(joined, "graphify install --project --platform pi") {
			sawPi = true
		}
	}
	if !sawOpencode || !sawPi {
		test.Errorf("Start should register Graphify (--project) for opencode + pi in ~/project; execs: %v", sandbox.allExecArgv)
	}
}

// TestStartInstallsGraphifyGitHook verifies the Graphify registration installs the git
// hook — gated on the project being a git repo (`[ -d .git ]`) and, per requirement, run
// on EVERY start with NO marker (hook install is idempotent), so the hook stays current.
func TestStartInstallsGraphifyGitHook(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"pi"}, DefaultTool: "pi"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	var found bool
	for _, argv := range sandbox.allExecArgv {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "graphify hook install") {
			continue
		}
		found = true
		if !strings.Contains(joined, "[ -d .git ]") {
			test.Errorf("`graphify hook install` must be gated on a git repo ([ -d .git ]): %s", joined)
		}
		if strings.Contains(joined, ".graphify-hook-installed") {
			test.Errorf("`graphify hook install` must NOT be once-guarded — it runs every start: %s", joined)
		}
	}
	if !found {
		test.Errorf("Start should include `graphify hook install` in the graphify exec; execs: %v", sandbox.allExecArgv)
	}
}

// TestStartInitsGitRepoBeforeGraphify verifies a non-git project is `git init`ed (guarded
// on [ ! -d .git ]) BEFORE graphify install/hook run, so the hook always has a repo.
func TestStartInitsGitRepoBeforeGraphify(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"pi"}, DefaultTool: "pi"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	var script string
	for _, argv := range sandbox.allExecArgv {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "graphify hook install") {
			script = joined
			break
		}
	}
	if script == "" {
		test.Fatalf("graphify exec not found; execs: %v", sandbox.allExecArgv)
	}
	if !strings.Contains(script, "[ ! -d .git ]") || !strings.Contains(script, "git init") {
		test.Errorf("script must `git init` guarded on a missing .git: %s", script)
	}
	gitInit := strings.Index(script, "git init")
	hook := strings.Index(script, "graphify hook install")
	if gitInit < 0 || hook < 0 || gitInit >= hook {
		test.Errorf("`git init` must precede `graphify hook install`: %s", script)
	}
}

// TestStartInstallsGraphifyGitHookForNonPlatformCLIs verifies the git hook still installs
// for a git repo even when NO selected CLI is a graphify platform (e.g. omp-only) — the
// hook step must not be gated behind the per-CLI install list.
func TestStartInstallsGraphifyGitHookForNonPlatformCLIs(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"omp"}, DefaultTool: "omp"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	var found bool
	for _, argv := range sandbox.allExecArgv {
		if strings.Contains(strings.Join(argv, " "), "graphify hook install") {
			found = true
		}
	}
	if !found {
		test.Error("a git repo with only non-platform CLIs must still install the graphify git hook")
	}
}

// TestStartRegistersCaveman verifies Start runs the real Caveman installer for each
// caveman-detectable selected CLI (with --only tokens + --non-interactive --with-hooks),
// then mirrors opencode's caveman dirs into the shared pool for pi/omp — all once-guarded.
func TestStartRegistersCaveman(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"opencode", "claude-code", "pi"}, DefaultTool: "opencode"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	// The install is staged as a script file and launched DETACHED (setsid).
	script := findCavemanScript(sandbox)
	if script == "" {
		test.Fatalf("Start should stage the Caveman install script; written: %v", sandbox.written)
	}
	if !cavemanLaunched(sandbox.allExecArgv) {
		test.Errorf("Start should launch the Caveman install detached; execs: %v", sandbox.allExecArgv)
	}
	for _, want := range []string{
		`cd "$HOME";`, // run from $HOME, never ~/project (submodule .git file kills git discovery)
		"git clone --depth 1 https://github.com/JuliusBrussee/caveman", // LOCAL clone (not curl|bash → npx)
		"node bin/install.js",
		"--non-interactive", "--with-hooks",
		"--only opencode", "--only claude", // pi is NOT caveman-detectable → no --only
		"skills/caveman/SKILL.md", // marker gated on the caveman skill landing in the pool
		".caveman-installed",      // once-guard marker
		`cp -a "$src/."`,          // pool mirror for pi/omp
	} {
		if !strings.Contains(script, want) {
			test.Errorf("caveman install script missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "--only pi") {
		test.Errorf("pi is not caveman-detectable and must not get an --only token: %s", script)
	}
}

// TestStartSkipsCavemanWhenNoDetectableCLI verifies that when only undetectable CLIs
// (pi/omp) are selected, Start makes no Caveman network call at all.
func TestStartSkipsCavemanWhenNoDetectableCLI(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"pi", "omp"}, DefaultTool: "pi"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}
	if script := findCavemanScript(sandbox); script != "" {
		test.Errorf("no caveman-detectable CLI selected — no install script must be staged: %s", script)
	}
	if cavemanLaunched(sandbox.allExecArgv) {
		test.Errorf("no caveman-detectable CLI selected — the installer must not launch: %v", sandbox.allExecArgv)
	}
}

// TestStartLinksSharedResources verifies the shared .ai-platform/{agents,skills,
// prompts,projects} pool is created and symlinked into each INSTALLED CLI's real
// per-project dirs (relative symlinks), skipping kinds a CLI has no concept for.
func TestStartLinksSharedResources(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"opencode", "claude-code", "pi", "gemini", "codex"}, DefaultTool: "opencode"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	// The shared pools exist.
	for _, kind := range []string{"agents", "skills", "prompts", "projects"} {
		if info, err := os.Stat(filepath.Join(root, ".ai-platform", kind)); err != nil || !info.IsDir() {
			test.Errorf(".ai-platform/%s pool not created: %v", kind, err)
		}
	}

	// Per-CLI symlinks resolve to the shared pools (relative, one level up).
	wantLinks := map[string]string{
		".opencode/skills":   "../.ai-platform/skills",
		".claude/skills":     "../.ai-platform/skills",
		".pi/skills":         "../.ai-platform/skills",
		".opencode/agents":   "../.ai-platform/agents",
		".claude/agents":     "../.ai-platform/agents",
		".pi/agents":         "../.ai-platform/agents",
		".opencode/commands": "../.ai-platform/prompts",
		".claude/commands":   "../.ai-platform/prompts",
		".gemini/commands":   "../.ai-platform/prompts",
		".pi/prompts":        "../.ai-platform/prompts",
	}
	for rel, wantTarget := range wantLinks {
		got, err := os.Readlink(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			test.Errorf("%s is not a symlink: %v", rel, err)
			continue
		}
		if got != filepath.FromSlash(wantTarget) {
			test.Errorf("%s -> %s, want %s", rel, got, wantTarget)
		}
	}

	// codex/gemini have no skills/agents concept — no such symlinks.
	for _, rel := range []string{".codex/skills", ".codex/agents", ".gemini/skills", ".gemini/agents"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			test.Errorf("%s must not exist (CLI has no such concept)", rel)
		}
	}

	// pi settings.json points its resource paths at the symlinked pools.
	piSettings := readProjectConfig(test, root, ".pi", "settings.json")
	for _, want := range []string{`"skills"`, `"prompts"`, `"defaultProvider"`} {
		if !strings.Contains(piSettings, want) {
			test.Errorf("pi settings.json missing %s:\n%s", want, piSettings)
		}
	}
}

// TestStartWiresOmpWhenSelected verifies selecting omp writes its keyless, discovery-based
// provider config (global models.yml, in-VM) + project config.yml (host), and symlinks the
// shared skills/agents/commands pools into omp's native .omp dirs.
func TestStartWiresOmpWhenSelected(test *testing.T) {
	root := seedProject(test, "app")
	if err := config.WriteProject(root, &config.Config{
		OS:    "debian-trixie",
		Agent: config.AgentConfig{Tools: []string{"omp"}, DefaultTool: "omp"},
	}); err != nil {
		test.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	served := fakeServedModels{models: []string{"ollama/qwen3:latest"}}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: sandbox, Keys: &fakeKeyMinter{}, Served: served, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err != nil {
		test.Fatal(err)
	}

	// Global models.yml (in-VM): keyless, openai-models-list discovery, no key literal.
	models := readGuestFile(test, sandbox, agentcfg.OmpGlobalModelsGuest)
	if !strings.Contains(models, "openai-models-list") || !strings.Contains(models, agentcfg.OmpAPIKeyRef) {
		test.Errorf("omp models.yml missing discovery/keyless ref:\n%s", models)
	}
	if strings.Contains(models, "sk-fake-workspace-key") {
		test.Errorf("omp models.yml must be keyless:\n%s", models)
	}
	// Project config.yml (host): the gateway provider order.
	if cfg := readProjectConfig(test, root, ".omp", "config.yml"); !strings.Contains(cfg, "modelProviderOrder") || !strings.Contains(cfg, "aip-gateway") {
		test.Errorf("omp config.yml missing provider order:\n%s", cfg)
	}
	// Shared pools symlinked into omp's native dirs (omp skips .claude/agents, so .omp/agents is required).
	for rel, want := range map[string]string{
		".omp/skills":   "../.ai-platform/skills",
		".omp/agents":   "../.ai-platform/agents",
		".omp/commands": "../.ai-platform/prompts",
	} {
		got, err := os.Readlink(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			test.Errorf("%s is not a symlink: %v", rel, err)
			continue
		}
		if got != filepath.FromSlash(want) {
			test.Errorf("%s -> %s, want %s", rel, got, want)
		}
	}
}
