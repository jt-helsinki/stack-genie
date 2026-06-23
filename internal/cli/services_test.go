package cli

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

// servicesResult.Human() renders a table with the column headers and each
// service's host-reachable address + admin-console URL where present — so `ai
// services status` shows the user where each UI lives.
func TestServicesResultHumanShowsAddressAndConsole(test *testing.T) {
	result := servicesResult{Services: []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true,
			Address: "http://localhost:14000", Console: "http://localhost:14000/ui"},
		{Name: "ollama", Mode: "container", State: "running", Healthy: true,
			Address: "http://localhost:11434"},
		{Name: "presidio", Mode: "container", State: "running", Healthy: true},
		{Name: "microsandbox", Mode: "runtime", State: "ready", Healthy: true},
	}}
	rendered := result.Human()

	for _, header := range []string{"SERVICE", "MODE", "STATE", "ADDRESS", "CONSOLE"} {
		if !strings.Contains(rendered, header) {
			test.Errorf("services status table missing header %q:\n%s", header, rendered)
		}
	}
	if !strings.Contains(rendered, "http://localhost:14000/ui") {
		test.Errorf("litellm console URL missing from services status output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "http://localhost:11434") {
		test.Errorf("ollama address missing from services status output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "presidio") || !strings.Contains(rendered, "microsandbox") {
		test.Errorf("all services should appear in the table:\n%s", rendered)
	}
}
