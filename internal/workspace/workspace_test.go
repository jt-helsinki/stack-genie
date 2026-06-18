package workspace

import (
	"errors"
	"path/filepath"
	"testing"

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
	execResult                           ExecResult
	execErr                              error
}

func (sandbox *fakeSandbox) Create(string, string, string) error { sandbox.created = true; return nil }
func (sandbox *fakeSandbox) Start(string) error                  { sandbox.started = true; return nil }
func (sandbox *fakeSandbox) Stop(string) error                   { sandbox.stopped = true; return nil }
func (sandbox *fakeSandbox) Destroy(string) error                { sandbox.destroyed = true; return nil }
func (sandbox *fakeSandbox) Exec(string, []string) (ExecResult, error) {
	return sandbox.execResult, sandbox.execErr
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
	return Manager{Builder: builder, Sandbox: sandbox, Now: func() string { return "2026-06-18T00:00:00Z" }}
}

func TestName(test *testing.T) {
	if got := Name("app", ""); got != "aip-app" {
		test.Errorf("project name = %q", got)
	}
	if got := Name("app", "review"); got != "aip-app-review" {
		test.Errorf("agent name = %q", got)
	}
}

func TestStartBuildsAndRecordsStartedHandle(test *testing.T) {
	root := seedProject(test, "app")
	builder := &fakeBuilder{}
	sandbox := &fakeSandbox{}

	handle, err := newManager(builder, sandbox).Start("app")
	if err != nil {
		test.Fatal(err)
	}
	if !builder.built || !sandbox.created || !sandbox.started {
		test.Fatalf("lifecycle not driven: builder=%v sandbox=%+v", builder.built, sandbox)
	}
	if handle.ID != "aip-app" || handle.Status != state.StatusStarted {
		test.Fatalf("handle: %+v", handle)
	}
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil || len(workspaces) != 1 || workspaces[0].Status != state.StatusStarted {
		test.Fatalf("persisted workspaces=%+v err=%v", workspaces, err)
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
	result, err := newManager(&fakeBuilder{}, sandbox).Exec("app", "", []string{"false"})
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
	if _, err := newManager(&fakeBuilder{}, sandbox).Exec("app", "", []string{"ls"}); err == nil {
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
