package acceptance

import (
	"os"
	"os/exec"
	"testing"
)

// hardwareAvailable gates the full-stack [S1] tests. They require a provisioned
// host that can actually launch the Docker service tier (and, for workspace
// tests, has nono installed) — so just detecting the binaries on PATH is not
// enough to prove the stack actually comes up. Require an explicit opt-in
// (AIP_HARDWARE_TESTS=1), set on the provisioned host, so `go test ./...` stays
// green on a tooled-but-not-provisioned machine.
func hardwareAvailable() bool {
	if os.Getenv("AIP_HARDWARE_TESTS") != "1" {
		return false
	}
	_, docker := exec.LookPath("docker")
	_, nono := exec.LookPath("nono")
	return docker == nil && nono == nil
}

// --- runnable [S1] subset ---------------------------------------------------

func TestVersion(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "--version")
	AssertOK(test, envelope, code, "version")
	var data struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	envelope.dataInto(test, &data)
	if data.Name != "ai" || data.Version == "" {
		test.Fatalf("version data: %+v", data)
	}
}

func TestUnknownCommandExits2(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "bogus")
	AssertError(test, envelope, code, 2)
}

func TestDoctorRunsAndReports(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "doctor")
	AssertOK(test, envelope, code, "doctor") // doctor exits 0 with a report
	var data struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	envelope.dataInto(test, &data)
	if len(data.Checks) == 0 {
		test.Fatal("doctor produced no checks")
	}
}

func TestLogsEmptyReturnsCleanEnvelope(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "logs")
	AssertOK(test, envelope, code, "logs")
}

func TestLogsUnknownServiceExits2(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "logs", "--service", "bogus")
	AssertError(test, envelope, code, 2)
}

// The consolidated `ai doctor [name]` always runs to completion and exits 0 with
// a report (per-check status conveys health), even for an unknown name — a
// runtime/virtualization shortfall is folded into the report's checks rather than
// a non-zero exit.
func TestDoctorAlwaysReportsExit0(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "doctor", "ghost")
	AssertOK(test, envelope, code, "doctor")
	var data struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	envelope.dataInto(test, &data)
	// The platform-dependency checks are always present.
	names := map[string]bool{}
	for _, check := range data.Checks {
		names[check.Name] = true
	}
	for _, want := range []string{"container runtime", "nono runtime"} {
		if !names[want] {
			test.Errorf("doctor report missing dependency check %q", want)
		}
	}
}

// `ai doctor` lists every managed host service in its SERVICES section (sourced
// from setup.ServicesStatus). Off hardware the probes report stopped/not-installed;
// we assert the service NAMES appear, not that they are healthy. open-webui and
// odysseus are NO LONGER host services (open-webui is now a per-workspace in-VM app;
// odysseus was removed), so they are intentionally absent.
func TestDoctorListsAllServices(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "doctor")
	AssertOK(test, envelope, code, "doctor")
	var data struct {
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	envelope.dataInto(test, &data)
	names := map[string]bool{}
	for _, check := range data.Checks {
		names[check.Name] = true
	}
	for _, want := range []string{"omlx", "litellm", "headroom", "proxy", "dns"} {
		if !names[want] {
			test.Errorf("doctor SERVICES section missing %q (got %v)", want, names)
		}
	}
	for _, gone := range []string{"open-webui", "odysseus"} {
		if names[gone] {
			test.Errorf("doctor should NOT list the removed host service %q (got %v)", gone, names)
		}
	}
}

func TestStateRepairThenShow(test *testing.T) {
	harness := New(test)

	repair, code := harness.Run(test, "state", "repair")
	AssertOK(test, repair, code, "state.repair")

	show, code := harness.Run(test, "state", "show")
	AssertOK(test, show, code, "state.show")
	var data map[string]any
	show.dataInto(test, &data)
	for _, key := range []string{"projects", "config", "runtime"} {
		if _, ok := data[key]; !ok {
			test.Errorf("state show data missing %q", key)
		}
	}
}

func TestProjectCreateNoTTYExits2(test *testing.T) {
	harness := New(test)
	// harness.Run adds --json, which disables the wizard: an unknown --agents
	// value is rejected directly (CLI §3.1), with no prompt to fall back on.
	envelope, code := harness.Run(test, "create", "demo", "--agents", "not-a-real-cli")
	AssertError(test, envelope, code, 2)
}

func TestProjectLifecycle(test *testing.T) {
	harness := New(test)

	// Create non-interactively via flags (--json); the helper passes the
	// default agent CLI.
	created, code := harness.CreateProject(test, "lifecycle-test")
	AssertOK(test, created, code, "project.create")
	var createData struct {
		Name  string   `json:"name"`
		Tools []string `json:"tools"`
	}
	created.dataInto(test, &createData)
	if createData.Name != "lifecycle-test" {
		test.Fatalf("create data: %+v", createData)
	}

	// It appears in the list.
	list, code := harness.Run(test, "list")
	AssertOK(test, list, code, "project.list")
	var listData struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	list.dataInto(test, &listData)
	if len(listData.Projects) != 1 || listData.Projects[0].Name != "lifecycle-test" {
		test.Fatalf("list = %+v", listData)
	}

	// Delete requires --yes.
	noYes, code := harness.Run(test, "delete", "lifecycle-test")
	AssertError(test, noYes, code, 2)

	// Delete with --yes succeeds, and the list is empty again.
	deleted, code := harness.Run(test, "delete", "lifecycle-test", "--yes")
	AssertOK(test, deleted, code, "project.delete")

	after, code := harness.Run(test, "list")
	AssertOK(test, after, code, "project.list")
	after.dataInto(test, &listData)
	if len(listData.Projects) != 0 {
		test.Fatalf("expected empty list after delete, got %+v", listData)
	}
}

// TestContextOptimizationFlow exercises `ai context` end-to-end (CLI §9): a new
// project seeds the Caveman skill at the default level, status reports it, and
// strategy/caveman edits round-trip through the project config.
func TestContextOptimizationFlow(test *testing.T) {
	harness := New(test)

	created, code := harness.CreateProject(test, "context-test")
	AssertOK(test, created, code, "project.create")

	type statusData struct {
		Strategy         string `json:"strategy"`
		CavemanLevel     string `json:"caveman_level"`
		CavemanInstalled bool   `json:"caveman_installed"`
	}

	// Create seeds the Caveman LEVEL at the default; the skill itself is installed by
	// the real toolkit at workspace start, so it is not yet present after create.
	status, code := harness.Run(test, "context", "status", "context-test")
	AssertOK(test, status, code, "context.status")
	var initial statusData
	status.dataInto(test, &initial)
	if initial.CavemanInstalled || initial.CavemanLevel != "full" {
		test.Fatalf("expected default caveman level full and skill not yet installed, got %+v", initial)
	}

	// An invalid strategy is rejected with exit 2.
	bad, code := harness.Run(test, "context", "strategy", "context-test", "turbo")
	AssertError(test, bad, code, 2)

	// Valid edits round-trip.
	strat, code := harness.Run(test, "context", "strategy", "context-test", "aggressive")
	AssertOK(test, strat, code, "context.strategy")
	cave, code := harness.Run(test, "context", "caveman", "context-test", "ultra")
	AssertOK(test, cave, code, "context.caveman")

	after, code := harness.Run(test, "context", "status", "context-test")
	AssertOK(test, after, code, "context.status")
	var updated statusData
	after.dataInto(test, &updated)
	if updated.Strategy != "aggressive" || updated.CavemanLevel != "ultra" {
		test.Fatalf("context edits did not round-trip: %+v", updated)
	}
}

// --- service-dependent [S1] tests (hardware) --------------------------------

func TestSetupSucceedsOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + nono — runs on a provisioned host; set AIP_HARDWARE_TESTS=1")
	}
	harness := New(test)
	envelope, code := harness.Run(test, "setup")
	AssertOK(test, envelope, code, "setup")
}

// TestWorkspaceDoctorOnHardware covers AT §11.1 [S1]: a workspace with a live
// nono session. Off-hardware the command exits 3 (no nono); the session posture
// is asserted only on a provisioned host.
func TestWorkspaceDoctorOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + nono — runs on a provisioned host; set AIP_HARDWARE_TESTS=1")
	}
	harness := New(test)

	created, code := harness.CreateProject(test, "doctor-test")
	AssertOK(test, created, code, "project.create")

	// The consolidated `ai doctor [name]` adds the WORKSPACE-runtime section when a
	// name is given. On a provisioned host nono must be installed.
	envelope, code := harness.Run(test, "doctor", "doctor-test")
	AssertOK(test, envelope, code, "doctor")
	var data struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	envelope.dataInto(test, &data)
	status := map[string]string{}
	for _, check := range data.Checks {
		status[check.Name] = check.Status
	}
	if status["workspace runtime"] != "ok" {
		test.Fatalf("workspace doctor posture: workspace runtime = %q, want ok (checks: %+v)", status["workspace runtime"], data.Checks)
	}
}
