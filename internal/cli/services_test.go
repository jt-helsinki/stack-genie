package cli

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

// servicesResult.Human() renders each service's host-reachable address and, where
// it has one, an admin-console UI hint — so `ai services status` shows the user
// where each UI lives.
func TestServicesResultHumanShowsAddressAndConsole(test *testing.T) {
	result := servicesResult{Services: []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true,
			Address: "http://localhost:4000", Console: "http://localhost:4000/ui"},
		{Name: "ollama", Mode: "container", State: "running", Healthy: true,
			Address: "http://localhost:11434"},
		{Name: "presidio", Mode: "container", State: "running", Healthy: true},
		{Name: "microsandbox", Mode: "runtime", State: "ready", Healthy: true},
	}}
	rendered := result.Human()

	if !strings.Contains(rendered, "http://localhost:4000 · UI http://localhost:4000/ui") {
		test.Errorf("litellm UI URL missing from services status output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "http://localhost:11434") {
		test.Errorf("ollama address missing from services status output:\n%s", rendered)
	}
	// Services without a host endpoint show no address/UI.
	for _, line := range strings.Split(rendered, "\n") {
		if (strings.Contains(line, "presidio") || strings.Contains(line, "microsandbox")) &&
			strings.Contains(line, "http") {
			test.Errorf("service with no host endpoint should show no address/UI: %q", line)
		}
	}
}
