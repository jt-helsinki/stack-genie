package workspace

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
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
	written                              map[string][]byte
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
func (sandbox *fakeSandbox) Exec(string, []string) (ExecResult, error) {
	return sandbox.execResult, sandbox.execErr
}
func (sandbox *fakeSandbox) WriteFile(_, guestPath string, content []byte) error {
	if sandbox.written == nil {
		sandbox.written = map[string][]byte{}
	}
	sandbox.written[guestPath] = content
	return nil
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
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil || len(workspaces) != 1 || workspaces[0].Status != state.StatusStarted {
		test.Fatalf("persisted workspaces=%+v err=%v", workspaces, err)
	}
}

func TestStartFailsWhenKeyMintFails(test *testing.T) {
	seedProject(test, "app")
	minter := &fakeKeyMinter{lastErr: errors.New("litellm gateway is not reachable")}
	manager := Manager{Builder: &fakeBuilder{}, Sandbox: &fakeSandbox{}, Keys: minter, Now: func() string { return "t" }}
	if _, err := manager.Start("app"); err == nil {
		test.Fatal("Start must fail when the virtual key cannot be minted")
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
