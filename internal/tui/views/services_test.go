package views

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

func TestServicesPopulatesTableOnRefresh(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true, Address: "127.0.0.1:14000"},
		{Name: "ollama", Mode: "container", State: "stopped", Healthy: false},
	}
	view := NewServices(func() ([]setup.ServiceStatus, error) { return statuses, nil })

	// Run the fetch command Init returns, then feed its message back in.
	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	if got := len(view.table.Rows()); got != 2 {
		test.Fatalf("table rows = %d, want 2", got)
	}
	if !strings.Contains(view.View(), "litellm") {
		test.Error("rendered view is missing a service name")
	}
}

func TestServicesSurfacesFetchError(test *testing.T) {
	view := NewServices(func() ([]setup.ServiceStatus, error) {
		return nil, errors.New("docker is not running")
	})

	_ = view.Update(view.Init()())

	if view.err == nil {
		test.Fatal("a fetch error must be recorded")
	}
	if !strings.Contains(view.View(), "docker is not running") {
		test.Error("rendered view must surface the fetch error")
	}
}
