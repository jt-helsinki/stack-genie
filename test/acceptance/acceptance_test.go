package acceptance

import (
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

func hardwareAvailable() bool {
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

// --- service-dependent [S1] tests (hardware) --------------------------------

func TestSetupSucceedsOnHardware(test *testing.T) {
	if !hardwareAvailable() {
		test.Skip("requires Docker + Microsandbox (msb) — runs on a provisioned Apple Silicon host")
	}
	harness := New(test)
	envelope, code := harness.Run(test, "setup")
	AssertOK(test, envelope, code, "setup")
}
