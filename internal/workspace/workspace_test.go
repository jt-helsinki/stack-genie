package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	netArgs                              []string
	execResult                           ExecResult
	execErr                              error
	execArgv                             []string
	interactiveArgv                      []string
	written                              map[string][]byte
	inspectPolicy                        NetworkPolicy
	inspectErr                           error
}

func (sandbox *fakeSandbox) Create(_, _, projectMount, overlayPath string, netArgs []string) error {
	sandbox.created = true
	sandbox.projectMount = projectMount
	sandbox.overlayMount = overlayPath
	sandbox.netArgs = netArgs
	return nil
}
func (sandbox *fakeSandbox) Start(string) error   { sandbox.started = true; return nil }
func (sandbox *fakeSandbox) Stop(string) error    { sandbox.stopped = true; return nil }
func (sandbox *fakeSandbox) Destroy(string) error { sandbox.destroyed = true; return nil }
func (sandbox *fakeSandbox) Exec(_ string, argv []string) (ExecResult, error) {
	sandbox.execArgv = argv
	return sandbox.execResult, sandbox.execErr
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

// fakeKeyMinter records GenerateKey calls and returns a fixed key (or an error).
type fakeKeyMinter struct {
	calls   int
	lastErr error
	scope   litellm.KeyScope
}

func (minter *fakeKeyMinter) GenerateKey(scope litellm.KeyScope) (string, error) {
	minter.calls++
	minter.scope = scope
	if minter.lastErr != nil {
		return "", minter.lastErr
	}
	return "sk-fake-workspace-key", nil
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
	manager := Manager{Builder: builder, Sandbox: sandbox, Keys: minter, Now: func() string { return "2026-06-18T00:00:00Z" }}

	handle, err := manager.Start("app")
	if err != nil {
		test.Fatal(err)
	}
	if !builder.built || !sandbox.created || !sandbox.started {
		test.Fatalf("lifecycle not driven: builder=%v sandbox=%+v", builder.built, sandbox)
	}
	// Start must mint a scoped virtual key tied to this workspace and write both
	// agent provider configs into the microVM (arch §15, §17).
	if minter.calls != 1 {
		test.Fatalf("GenerateKey called %d times, want 1", minter.calls)
	}
	if minter.scope.Alias != "app" || minter.scope.Metadata["workspace"] != "aip-app" {
		test.Fatalf("key scope = %+v, want alias=app workspace=aip-app", minter.scope)
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
	// standalone default (host.microsandbox.internal:18787), so the always-on
	// allow rule and the agent configs target that gateway.
	if !strings.Contains(strings.Join(sandbox.netArgs, " "), "allow:egress@host.microsandbox.internal:tcp:18787") {
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

	// Restart drives the EXISTING microVM: no rebuild, just stop then start.
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
	if restartBuilder.built || restartSandbox.created {
		test.Fatalf("restart must not rebuild or recreate: builder=%v sandbox=%+v", restartBuilder.built, restartSandbox)
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
// running a login shell (tmux new-session -A makes it create-or-attach).
func TestShellOpensPersistentTmuxSession(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Shell("app"); err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "new-session", "-A", "-s", "shell", "-c", "/workspace", "bash", "-l"}
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Shell ran %v via ExecInteractive, want %v", got, want)
	}
}

// Agent starts (or reattaches to) a per-CLI tmux session named after the CLI,
// running that CLI's launch command in /workspace.
func TestAgentStartsPerCLITmuxSession(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Agent("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "new-session", "-A", "-s", "opencode", "-c", "/workspace", "opencode"}
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Agent ran %v via ExecInteractive, want %v", got, want)
	}
}

// claude-code maps to the `claude` launch command but keeps its own session name.
func TestAgentMapsClaudeCodeLaunch(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Agent("app", "claude-code"); err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "new-session", "-A", "-s", "claude-code", "-c", "/workspace", "claude"}
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Agent(claude-code) ran %v, want %v", got, want)
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

// Attach attaches to (or creates) a named session with no command (so a fresh
// session opens the default shell); a blank session attaches the default "shell".
func TestAttachSession(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{}
	if err := newManager(&fakeBuilder{}, sandbox).Attach("app", "opencode"); err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "new-session", "-A", "-s", "opencode", "-c", "/workspace"}
	if got := sandbox.interactiveArgv; !equalStrings(got, want) {
		test.Fatalf("Attach ran %v, want %v", got, want)
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

// ListSessions parses tmux's tab-separated list-sessions output (name, attached,
// activity), reading the attached flag and raw activity epoch.
func TestListSessionsParsesOutput(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{
		ExitCode: 0,
		Stdout:   "shell\t1\t1700000000\nopencode\t0\t1700000500\n",
	}}
	sessions, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if err != nil {
		test.Fatal(err)
	}
	want := []string{"tmux", "list-sessions", "-F", "#{session_name}\t#{session_attached}\t#{session_activity}"}
	if !equalStrings(sandbox.execArgv, want) {
		test.Fatalf("ListSessions ran %v, want %v", sandbox.execArgv, want)
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

// With no tmux server running yet, `tmux list-sessions` exits non-zero with "no
// server running" on stderr — that is ZERO sessions, not an error. Exec carries
// the inner non-zero exit as data, so the check is on the ExecResult.
func TestListSessionsNoServerIsEmpty(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 1, Stderr: "no server running on /tmp/tmux-1000/default"}}
	sessions, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app")
	if err != nil {
		test.Fatalf("no running tmux server must be empty, not an error: %v", err)
	}
	if len(sessions) != 0 {
		test.Fatalf("want zero sessions, got %+v", sessions)
	}
}

// A non-zero tmux exit that is NOT "no server running" is a real failure.
func TestListSessionsOtherFailureIsError(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{execResult: ExecResult{ExitCode: 1, Stderr: "tmux: command not found"}}
	if _, err := newManager(&fakeBuilder{}, sandbox).ListSessions("app"); err == nil {
		test.Fatal("a non-'no server running' failure must be an error")
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

// TestInteractiveRequiresRunningWorkspace: a shell/agent/sessions request against
// a workspace whose microVM is not running fails cleanly with ErrNotStarted
// (rather than attaching a PTY to a missing VM, which can corrupt the terminal).
func TestInteractiveRequiresRunningWorkspace(test *testing.T) {
	seedProject(test, "app")
	// InspectNetwork reports no sandbox (the workspace was never started).
	notRunning := func() *fakeSandbox { return &fakeSandbox{inspectErr: ErrNotRunning} }

	if err := newManager(&fakeBuilder{}, notRunning()).Shell("app"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Shell on a not-running workspace: want ErrNotStarted, got %v", err)
	}
	if err := newManager(&fakeBuilder{}, notRunning()).Agent("app", "opencode"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Agent on a not-running workspace: want ErrNotStarted, got %v", err)
	}
	if err := newManager(&fakeBuilder{}, notRunning()).Attach("app", "shell"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("Attach on a not-running workspace: want ErrNotStarted, got %v", err)
	}
	if _, err := newManager(&fakeBuilder{}, notRunning()).ListSessions("app"); !errors.Is(err, ErrNotStarted) {
		test.Fatalf("ListSessions on a not-running workspace: want ErrNotStarted, got %v", err)
	}
	// A not-running shell must NOT have attached a PTY (no msb exec -t).
	sandbox := notRunning()
	_ = newManager(&fakeBuilder{}, sandbox).Shell("app")
	if sandbox.interactiveArgv != nil {
		test.Fatalf("a not-running shell must not attach a PTY, ran %v", sandbox.interactiveArgv)
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
