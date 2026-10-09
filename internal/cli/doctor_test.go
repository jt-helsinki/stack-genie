package cli

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/doctor"
	"github.com/jt-helsinki/stack-genie/internal/setup"
)

func doctorServiceByName(services []doctor.Service, name string) doctor.Service {
	for _, service := range services {
		if service.Name == name {
			return service
		}
	}
	return doctor.Service{}
}

// mapDoctorServices lists the host-native omlx backend (carrying its own Detail from
// setup.omlxStatus).
func TestMapDoctorServicesHostInference(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "omlx", Mode: "host", Healthy: false, State: "stopped", Detail: "not installed — run `ai models install-omlx`"},
		{Name: "litellm", Mode: "container", Healthy: true, State: "running"},
	}
	services := mapDoctorServices(statuses, "darwin")

	omlx := doctorServiceByName(services, "omlx")
	if omlx.Name == "" {
		test.Fatal("host-native omlx must be listed")
	}
	if !strings.Contains(omlx.Detail, "install-omlx") {
		test.Fatalf("down omlx should carry its install detail, got Detail=%q", omlx.Detail)
	}
}

// A healthy host inference backend passes through unchanged.
func TestMapDoctorServicesHealthyPassthrough(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "omlx", Mode: "host", Healthy: true, State: "running"},
	}
	services := mapDoctorServices(statuses, "linux")
	if state := doctorServiceByName(services, "omlx").State; state != "running" {
		test.Fatalf("healthy omlx State should be untouched, got %q", state)
	}
}

func TestDoctorWorkspaceLiveChecksNoSessions(test *testing.T) {
	checks := doctorWorkspaceLiveChecks("nonexistent-project-xyz")
	if len(checks) != 1 {
		test.Fatalf("doctorWorkspaceLiveChecks() returned %d checks, want 1", len(checks))
	}
	if checks[0].Name != doctorWorkspaceSessionsCheck {
		test.Fatalf("check name = %q, want %q", checks[0].Name, doctorWorkspaceSessionsCheck)
	}
}
