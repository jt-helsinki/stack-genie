package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/doctor"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

type fakeDoctorSandbox struct {
	running      bool
	runningErr   error
	logTail      string
	logErr       error
	execResult   workspace.ExecResult
	execErr      error
	lastExecArgv []string
	lastLogLines int
}

func (sandbox *fakeDoctorSandbox) Create(string, string, string, string, workspace.VMResources, []string) error {
	return nil
}
func (sandbox *fakeDoctorSandbox) Start(string) error   { return nil }
func (sandbox *fakeDoctorSandbox) Stop(string) error    { return nil }
func (sandbox *fakeDoctorSandbox) Destroy(string) error { return nil }
func (sandbox *fakeDoctorSandbox) Exec(string, []string) (workspace.ExecResult, error) {
	return workspace.ExecResult{}, nil
}
func (sandbox *fakeDoctorSandbox) ExecContext(_ context.Context, _ string, argv []string) (workspace.ExecResult, error) {
	sandbox.lastExecArgv = argv
	return sandbox.execResult, sandbox.execErr
}
func (sandbox *fakeDoctorSandbox) ExecRoot(string, []string) (workspace.ExecResult, error) {
	return workspace.ExecResult{}, nil
}
func (sandbox *fakeDoctorSandbox) ExecRootContext(context.Context, string, []string) (workspace.ExecResult, error) {
	return workspace.ExecResult{}, nil
}
func (sandbox *fakeDoctorSandbox) ExecInteractive(string, []string) error { return nil }
func (sandbox *fakeDoctorSandbox) WriteFile(string, string, []byte) error { return nil }
func (sandbox *fakeDoctorSandbox) LogTail(string, int) (string, error) {
	return sandbox.logTail, sandbox.logErr
}
func (sandbox *fakeDoctorSandbox) LogTailContext(_ context.Context, _ string, lines int) (string, error) {
	sandbox.lastLogLines = lines
	return sandbox.logTail, sandbox.logErr
}
func (sandbox *fakeDoctorSandbox) InspectNetwork(string) (workspace.NetworkPolicy, error) {
	return workspace.NetworkPolicy{}, nil
}
func (sandbox *fakeDoctorSandbox) IsRunning(context.Context, string) (bool, error) {
	return sandbox.running, sandbox.runningErr
}
func (sandbox *fakeDoctorSandbox) SyncClock(string) error { return nil }

func cliCheckByName(checks []doctor.Check, name string) doctor.Check {
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	return doctor.Check{}
}

func doctorServiceByName(services []doctor.Service, name string) doctor.Service {
	for _, service := range services {
		if service.Name == name {
			return service
		}
	}
	return doctor.Service{}
}

// mapDoctorServices lists BOTH host-native inference backends (Ollama +
// docker-model-runner), skips the microVM runtime line, folds an actionable hint
// into a down Ollama's detail, and marks DMR optional (so a down DMR warns, not
// errors).
func TestMapDoctorServicesHostInference(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "ollama", Mode: "host", Healthy: false, State: "stopped"},
		{Name: "docker-model-runner", Mode: "host", Healthy: false, State: "stopped"},
		{Name: "litellm", Mode: "container", Healthy: true, State: "running"},
		{Name: "microsandbox", Mode: "runtime", Healthy: true},
	}
	services := mapDoctorServices(statuses, "darwin")

	if doctorServiceByName(services, "microsandbox").Name != "" {
		test.Fatal("the microVM runtime (Mode runtime) must not be listed as a service")
	}
	ollama := doctorServiceByName(services, "ollama")
	if ollama.Name == "" {
		test.Fatal("host-native Ollama must be listed")
	}
	if !strings.Contains(ollama.State, "brew install ollama") {
		test.Fatalf("down Ollama should carry an install hint, got State=%q", ollama.State)
	}
	dmr := doctorServiceByName(services, "docker-model-runner")
	if dmr.Name == "" {
		test.Fatal("Docker Model Runner must be listed")
	}
	if !dmr.Optional {
		test.Fatal("Docker Model Runner must be marked optional so a down DMR warns, not errors")
	}
	if !strings.Contains(dmr.State, "docker desktop enable model-runner") {
		test.Fatalf("down DMR should carry an enable hint, got State=%q", dmr.State)
	}
}

// A healthy host inference backend passes through without a synthetic hint (the
// detail stays as the real status detail).
func TestMapDoctorServicesHealthyPassthrough(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "ollama", Mode: "host", Healthy: true, State: "running"},
		{Name: "docker-model-runner", Mode: "host", Healthy: true, State: "running"},
	}
	services := mapDoctorServices(statuses, "linux")
	if state := doctorServiceByName(services, "ollama").State; state != "running" {
		test.Fatalf("healthy Ollama State should be untouched, got %q", state)
	}
	if !doctorServiceByName(services, "docker-model-runner").Optional {
		test.Fatal("Docker Model Runner should be optional even when healthy")
	}
}

func TestDoctorWorkspaceLiveChecksHealthy(test *testing.T) {
	sandbox := &fakeDoctorSandbox{running: true, logTail: "booted\n"}
	checks := doctorWorkspaceLiveChecks("demo", sandbox)
	if got := cliCheckByName(checks, "workspace microVM").Status; got != doctor.StatusOK {
		test.Fatalf("workspace microVM status = %q, want ok", got)
	}
	if got := cliCheckByName(checks, "workspace logs").Status; got != doctor.StatusOK {
		test.Fatalf("workspace logs status = %q, want ok", got)
	}
	if got := cliCheckByName(checks, "workspace exec").Status; got != doctor.StatusOK {
		test.Fatalf("workspace exec status = %q, want ok", got)
	}
	if sandbox.lastLogLines != 20 {
		test.Fatalf("live log check tailed %d lines, want 20", sandbox.lastLogLines)
	}
	if len(sandbox.lastExecArgv) != 1 || sandbox.lastExecArgv[0] != "true" {
		test.Fatalf("live exec check ran %v, want [true]", sandbox.lastExecArgv)
	}
}

func TestDoctorWorkspaceLiveChecksExecFailure(test *testing.T) {
	sandbox := &fakeDoctorSandbox{running: true, execErr: errors.New("context deadline exceeded")}
	checks := doctorWorkspaceLiveChecks("demo", sandbox)
	execCheck := cliCheckByName(checks, "workspace exec")
	if execCheck.Status != doctor.StatusError {
		test.Fatalf("workspace exec status = %q, want error", execCheck.Status)
	}
	if execCheck.Suggestion == "" {
		test.Fatal("workspace exec failure should include a recovery suggestion")
	}
}

func TestDoctorWorkspaceLiveChecksNotRunningSkipsExec(t *testing.T) {
	sandbox := &fakeDoctorSandbox{running: false}
	checks := doctorWorkspaceLiveChecks("demo", sandbox)
	if got := cliCheckByName(checks, "workspace microVM").Status; got != doctor.StatusError {
		t.Fatalf("workspace microVM status = %q, want error", got)
	}
	if check := cliCheckByName(checks, "workspace exec"); check.Name != "" {
		t.Fatalf("workspace exec should not run when microVM is absent: %+v", check)
	}
}
