package acceptance

import (
	"os"
	"os/exec"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/templates"
)

// installTemplates lays down ~/.ai-platform/templates into the harness HOME.
// On a provisioned host `ai setup` does this; here it is a fixture so the
// create wizard (which composes the Dockerfile from templates) can run without
// the hardware-bound `ai setup`.
func (harness *Harness) installTemplates(test *testing.T) {
	test.Helper()
	test.Setenv("HOME", harness.Home) // make the in-process Install target the same HOME
	if err := templates.Install(); err != nil {
		test.Fatal(err)
	}
}

func requireGit(test *testing.T) {
	test.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		test.Skip("git not installed")
	}
}

// hardwareAvailable gates the full-stack [S1] tests. They require not just the
// tools on PATH but the real service/microVM impls wired (currently deferred —
// ErrPending), so detecting docker+msb is not enough: a dev machine can have the
// tools while the launch paths are still stubs. Require an explicit opt-in
// (AIP_HARDWARE_TESTS=1), set on the provisioned host where the impls are live,
// so `go test ./...` stays green on a tooled-but-not-provisioned machine.
func hardwareAvailable() bool {
	if os.Getenv("AIP_HARDWARE_TESTS") != "1" {
		return false
	}
	_, docker := exec.LookPath("docker")
	_, msb := exec.LookPath("msb")
	return docker == nil && msb == nil
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

func TestWorkspaceDoctorUnknownProjectExits2(test *testing.T) {
	harness := New(test)
	envelope, code := harness.Run(test, "workspace", "doctor", "ghost")
	AssertError(test, envelope, code, 2)
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
	// Run (not PTY) → no terminal → the wizard cannot prompt (§3.1).
	envelope, code := harness.Run(test, "project", "create", "demo")
	AssertError(test, envelope, code, 2)
}

func TestProjectLifecycleViaWizard(test *testing.T) {
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	// Create via the PTY wizard, accepting defaults.
	created, code := harness.CreateProject(test, "lifecycle-test")
	AssertOK(test, created, code, "project.create")
	var createData struct {
		Name string   `json:"name"`
		OS   string   `json:"os"`
		Tool []string `json:"tools"`
	}
	created.dataInto(test, &createData)
	if createData.Name != "lifecycle-test" || createData.OS != "debian-trixie" {
		test.Fatalf("create data: %+v", createData)
	}

	// It appears in the list.
	list, code := harness.Run(test, "project", "list")
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
	noYes, code := harness.Run(test, "project", "delete", "lifecycle-test")
	AssertError(test, noYes, code, 2)

	// Delete with --yes succeeds, and the list is empty again.
	deleted, code := harness.Run(test, "project", "delete", "lifecycle-test", "--yes")
	AssertOK(test, deleted, code, "project.delete")

	after, code := harness.Run(test, "project", "list")
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
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProject(test, "context-test")
	AssertOK(test, created, code, "project.create")

	type statusData struct {
		Strategy         string `json:"strategy"`
		CavemanLevel     string `json:"caveman_level"`
		CavemanInstalled bool   `json:"caveman_installed"`
	}

	// Create seeds the Caveman skill at the default level.
	status, code := harness.Run(test, "context", "status", "context-test")
	AssertOK(test, status, code, "context.status")
	var initial statusData
	status.dataInto(test, &initial)
	if !initial.CavemanInstalled || initial.CavemanLevel != "full" {
		test.Fatalf("expected seeded full caveman skill, got %+v", initial)
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
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host")
	}
	harness := New(test)
	envelope, code := harness.Run(test, "setup")
	AssertOK(test, envelope, code, "setup")
}

// TestWorkspaceDoctorOnHardware covers AT §11.1 [S1]: a rootless service tier and
// a microVM workspace. Off-hardware the command exits 3 (no msb) or 4 (no
// virtualization); the rootless/privileged/microvm envelope is asserted only on a
// provisioned host.
func TestWorkspaceDoctorOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host")
	}
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProject(test, "doctor-test")
	AssertOK(test, created, code, "project.create")

	envelope, code := harness.Run(test, "workspace", "doctor", "doctor-test")
	AssertOK(test, envelope, code, "workspace.doctor")
	var data struct {
		Runtime struct {
			Rootless   bool `json:"rootless"`
			Privileged bool `json:"privileged"`
		} `json:"runtime"`
		Workspace struct {
			Kind string `json:"kind"`
		} `json:"workspace"`
	}
	envelope.dataInto(test, &data)
	if !data.Runtime.Rootless || data.Runtime.Privileged || data.Workspace.Kind != "microvm" {
		test.Fatalf("workspace doctor posture: %+v", data)
	}
}

// TestOverlayPersistsAcrossRecreation covers the [S4] persistence criterion
// (arch §26): a program installed inside the workspace, plus agent state written
// outside the project mount, must survive `ai workspace destroy` + `start`. This
// needs a real microVM, so it runs only on a provisioned host.
func TestOverlayPersistsAcrossRecreation(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host")
	}
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	created, code := harness.CreateProject(test, "overlay-test")
	AssertOK(test, created, code, "project.create")

	start, code := harness.Run(test, "workspace", "start", "--project", "overlay-test")
	AssertOK(test, start, code, "workspace.start")

	// Write a marker outside the project mount; it must land in the overlay.
	mark, code := harness.Exec(test, "overlay-test", "sh", "-c", "echo persisted > /root/marker")
	AssertOK(test, mark, code, "workspace.exec")

	destroy, code := harness.Run(test, "workspace", "destroy", "--project", "overlay-test")
	AssertOK(test, destroy, code, "workspace.destroy")
	restart, code := harness.Run(test, "workspace", "start", "--project", "overlay-test")
	AssertOK(test, restart, code, "workspace.start")

	check, code := harness.Exec(test, "overlay-test", "cat", "/root/marker")
	AssertOK(test, check, code, "workspace.exec")
}

// TestOSEquivalenceOnHardware covers the [S5] OS-equivalence criterion (spec
// §12.4): build a workspace from each of the four OS templates with the same
// agent-CLI selection and confirm an identical base tooling surface (git, gh).
// The composition-side equivalence is unit-tested in internal/envimage; this
// end-to-end build-and-smoke check needs real images, so it runs only on a
// provisioned host (and drives the wizard's OS picker per OS).
func TestOSEquivalenceOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host")
	}
	requireGit(test)
	harness := New(test)
	harness.installTemplates(test)

	for _, osKey := range []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"} {
		created, code := harness.CreateProjectWithOS(test, "os-"+osKey, osKey)
		AssertOK(test, created, code, "project.create")

		start, code := harness.Run(test, "workspace", "start", "--project", "os-"+osKey)
		AssertOK(test, start, code, "workspace.start")

		// Same base tooling surface across every OS.
		for _, tool := range []string{"git", "gh"} {
			smoke, code := harness.Exec(test, "os-"+osKey, tool, "--version")
			AssertOK(test, smoke, code, "workspace.exec")
		}
	}
}
