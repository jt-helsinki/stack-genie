package cli

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// servicesResult.Human() renders a table with the column headers and each
// service's host-reachable address + admin-console URL where present — so `ai
// services status` shows the user where each UI lives.
func TestServicesResultHumanShowsAddressAndConsole(test *testing.T) {
	result := servicesResult{Services: []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true,
			Address: "http://localhost:14000", Console: "http://localhost:14000/ui"},
		{Name: "omlx", Mode: "container", State: "running", Healthy: true,
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
		test.Errorf("omlx address missing from services status output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "presidio") || !strings.Contains(rendered, "microsandbox") {
		test.Errorf("all services should appear in the table:\n%s", rendered)
	}
}

// controllableServices drops the workspace runtime (microsandbox, Mode ==
// "runtime") so it never becomes a start/stop/restart target, while keeping
// every container service.
func TestControllableServicesExcludesRuntime(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "omlx", Mode: "container"},
		{Name: "presidio", Mode: "container"},
		{Name: "litellm", Mode: "container"},
		{Name: "headroom", Mode: "container"},
		{Name: "proxy", Mode: "container"},
		{Name: "dns", Mode: "container"},
		{Name: "microsandbox", Mode: "runtime"},
	}
	controllable := controllableServices(statuses)

	for _, service := range controllable {
		if service.Mode == "runtime" {
			test.Errorf("runtime service %q must not be controllable", service.Name)
		}
		if service.Name == "microsandbox" {
			test.Errorf("microsandbox must be excluded from the control checkbox")
		}
	}
	if len(controllable) != len(statuses)-1 {
		test.Errorf("expected exactly the runtime dropped: got %d of %d", len(controllable), len(statuses))
	}
	kept := make(map[string]bool, len(controllable))
	for _, service := range controllable {
		kept[service.Name] = true
	}
	for _, name := range []string{"omlx", "presidio", "litellm", "headroom", "proxy", "dns"} {
		if !kept[name] {
			test.Errorf("container service %q should be kept", name)
		}
	}
}

// controllableServices is a no-op when there is no runtime entry.
func TestControllableServicesKeepsAllContainers(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "omlx", Mode: "container"},
		{Name: "litellm", Mode: "container"},
	}
	if got := len(controllableServices(statuses)); got != len(statuses) {
		test.Errorf("expected all %d container services kept, got %d", len(statuses), got)
	}
}

// expandServiceSelection collapses to ["all"] whenever the sentinel is present
// (alone or mixed with names), leaves plain selections untouched, and keeps an
// empty selection empty.
func TestExpandServiceSelection(test *testing.T) {
	cases := []struct {
		name     string
		selected []string
		want     []string
	}{
		{"all sentinel alone", []string{"all"}, []string{"all"}},
		{"all sentinel mixed", []string{"omlx", "all", "litellm"}, []string{"all"}},
		{"plain names unchanged", []string{"omlx", "litellm"}, []string{"omlx", "litellm"}},
		{"empty stays empty", []string{}, []string{}},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			got := expandServiceSelection(testCase.selected)
			if len(got) != len(testCase.want) {
				test.Fatalf("got %v, want %v", got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					test.Errorf("got %v, want %v", got, testCase.want)
				}
			}
		})
	}
}
