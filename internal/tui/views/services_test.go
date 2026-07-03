package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

// noControl / noOpen are stubs for tests that don't exercise those actions.
func noControl(string, string) error { return nil }
func noOpen(string) error            { return nil }

// newServicesForTest builds a Services list wired to a per-service detail whose
// container log + status fetcher are derived from the SAME status set, so the tests
// can drive both levels without re-wiring. control backs both the list toggle and
// the detail's in-place lifecycle.
func newServicesForTest(fetch ServiceFetcher, control ServiceController) *Services {
	statusByName := func(service string) (setup.ServiceStatus, bool) {
		statuses, err := fetch()
		if err != nil {
			return setup.ServiceStatus{}, false
		}
		for _, status := range statuses {
			if status.Name == service {
				return status, true
			}
		}
		return setup.ServiceStatus{}, false
	}
	log := NewLogView(
		func() (string, error) { return "container log line\n", nil },
		func() bool { return true },
		func() string { return "" }, // the detail re-points the subject via SetService
		LogViewLabels{Loading: "loading container log…"},
		nil,
	)
	detail := NewServiceDetail(statusByName,
		func(string) ([]setup.ContainerStats, error) { return nil, nil },
		control, noOpen, log)
	return NewServices(fetch, control, detail)
}

func TestServicesPopulatesTableOnRefresh(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true, Address: "127.0.0.1:14000"},
		{Name: "ollama", Mode: "container", State: "stopped", Healthy: false},
	}
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) { return statuses, nil }, noControl)

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

// A listTable view must FILL the content height it is sized to (the table pads
// with blank rows) and keep a CONSTANT total height whether or not a flash is shown.
func TestServicesTableFillsConstantHeight(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "litellm", State: "running", Healthy: true},
		{Name: "ollama", State: "running", Healthy: true},
	}
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) { return statuses, nil }, noControl)
	_ = view.Update(view.Init()())

	const contentHeight = 20
	view.SetSize(100, contentHeight)

	noFlash := view.View()
	if got := renderedHeight(noFlash); got != contentHeight {
		test.Fatalf("table view height = %d, want it to FILL the content height %d:\n%s", got, contentHeight, noFlash)
	}

	view.flash = "did a thing"
	withFlash := view.View()
	if got := renderedHeight(withFlash); got != contentHeight {
		test.Fatalf("with a flash the view height = %d, want constant %d:\n%s", got, contentHeight, withFlash)
	}
	if !strings.Contains(withFlash, "did a thing") {
		test.Errorf("the flash must be rendered in the slot:\n%s", withFlash)
	}
}

func TestServicesSurfacesFetchError(test *testing.T) {
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) {
		return nil, errors.New("docker is not running")
	}, noControl)

	_ = view.Update(view.Init()())

	if view.err == nil {
		test.Fatal("a fetch error must be recorded")
	}
	if !strings.Contains(view.View(), "docker is not running") {
		test.Error("rendered view must surface the fetch error")
	}
}

// TestServicesDrillsIntoDetailAndEscBacksOut: enter/d opens the per-service detail
// (summary + embedded container log); esc backs out to the list. This is the
// workspace-style drill-down behavior.
func TestServicesDrillsIntoDetailAndEscBacksOut(test *testing.T) {
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) {
		return []setup.ServiceStatus{{Name: "litellm", State: "running", Console: "http://localhost:14000/ui"}}, nil
	}, noControl)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	// enter drills in; the detail is now shown (the list table is hidden).
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !view.drilled || !view.CapturesNav() {
		test.Fatal("enter must drill into the service detail (drilled + CapturesNav)")
	}
	// Feed the detail's status refresh so its summary renders.
	_ = view.Update(view.detail.refreshCmd(view.detail.generation)())
	rendered := view.View()
	// The detail opens on the Service sub-tab (summary + metrics), not the list.
	if !strings.Contains(rendered, "Containers") {
		test.Errorf("the detail should show the Service sub-tab summary, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "SERVICE") {
		test.Error("the list table must be hidden while the detail is open")
	}
	// esc backs out to the list.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.drilled || view.CapturesNav() {
		test.Fatal("esc must back out of the detail to the list")
	}
}

// TestServicesDetailLifecycleStaysInDetail is the regression for the bounce-back bug:
// s/x/r in the detail act IN PLACE (controller called, spinner shown) and do NOT pop
// back to the list.
func TestServicesDetailLifecycleStaysInDetail(test *testing.T) {
	var calls []string
	view := newServicesForTest(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "running"}}, nil
		},
		func(action, service string) error { calls = append(calls, action+":"+service); return nil },
	)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // drill in
	_ = view.Update(view.detail.refreshCmd(view.detail.generation)())

	// Press "r" (restart) in the detail.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		test.Fatal("restart in the detail must return a command")
	}
	if !view.drilled {
		test.Fatal("a lifecycle key in the detail must NOT pop back to the list")
	}
	if view.detail.pending != "restart" {
		test.Fatalf("the detail should show an in-flight restart spinner, got pending=%q", view.detail.pending)
	}
	// Running the batch yields a lifecycle-done + a spinner tick; run the done message.
	done := cmd()
	if batch, ok := done.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				if msg := sub(); msg != nil {
					_ = view.Update(msg)
				}
			}
		}
	} else {
		_ = view.Update(done)
	}
	if len(calls) != 1 || calls[0] != "restart:litellm" {
		test.Fatalf("controller calls = %v, want [restart:litellm]", calls)
	}
	if !view.drilled {
		test.Fatal("after the action lands the view must STILL be in the detail (no bounce to list)")
	}
	if view.detail.pending != "" {
		test.Errorf("the spinner should clear once the action lands, got pending=%q", view.detail.pending)
	}
}

// TestServicesDetailUpdateEmitsRequest: `p` in the detail emits a
// ServiceUpdateRequestedMsg (routed by the parent to the terminal overlay so the CLI
// pull spinner shows) and STAYS in the detail.
func TestServicesDetailUpdateEmitsRequest(test *testing.T) {
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) {
		return []setup.ServiceStatus{{Name: "litellm", State: "running"}}, nil
	}, noControl)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = view.Update(view.detail.refreshCmd(view.detail.generation)())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd == nil {
		test.Fatal("p in the detail must return an update-request command")
	}
	msg := cmd()
	request, ok := msg.(ServiceUpdateRequestedMsg)
	if !ok {
		test.Fatalf("p must emit a ServiceUpdateRequestedMsg, got %T", msg)
	}
	if request.Service != "litellm" {
		test.Fatalf("update request service = %q, want litellm", request.Service)
	}
	if !view.drilled {
		test.Fatal("p must NOT pop back to the list")
	}
}

// TestServicesEnableKeyTogglesOptional: `e` on the LIST enables a disabled optional
// service (and would disable an enabled one), driving the control func.
func TestServicesEnableKeyTogglesOptional(test *testing.T) {
	var controlled string
	view := newServicesForTest(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "open-webui", State: "disabled", Optional: true}}, nil
		},
		func(action, service string) error { controlled = action + ":" + service; return nil },
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		test.Fatal("e on a disabled optional must return an enable command")
	}
	cmd()
	if controlled != "enable:open-webui" {
		test.Fatalf("e should enable a disabled optional, got %q", controlled)
	}
}

// TestServicesEnableKeyRejectsCore: `e` on a core service is a no-op with a hint.
func TestServicesEnableKeyRejectsCore(test *testing.T) {
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) {
		return []setup.ServiceStatus{{Name: "litellm", State: "running"}}, nil
	}, noControl)
	_ = view.Update(view.Init()())

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		test.Fatal("e on a core service must not control anything")
	}
	if !strings.Contains(view.View(), "core service") {
		test.Errorf("expected a core-service hint, got:\n%s", view.View())
	}
}

// TestServicesListRestartKeys verifies the list-level keys: `r` restarts the selected
// service, `a` restarts them all (control with an empty target), neither drilling in.
func TestServicesListRestartKeys(test *testing.T) {
	var calls []string
	view := newServicesForTest(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "running"}, {Name: "ollama", State: "running"}}, nil
		},
		func(action, service string) error { calls = append(calls, action+":"+service); return nil },
	)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd != nil {
		_ = view.Update(cmd())
	}
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd != nil {
		_ = view.Update(cmd())
	}
	if len(calls) != 2 || calls[0] != "restart:litellm" || calls[1] != "restart:" {
		test.Fatalf("want [restart:litellm restart:], got %v", calls)
	}
	if view.drilled {
		test.Error("the restart keys must not drill into the detail")
	}
}

// TestServicesSetActiveOnlyAffectsOpenDetail: SetActive is a no-op on the list and
// toggles the detail's log poll while drilled — so the container-log poll only runs
// while the Services tab is visible AND a detail is open.
func TestServicesSetActiveOnlyAffectsOpenDetail(test *testing.T) {
	view := newServicesForTest(func() ([]setup.ServiceStatus, error) {
		return []setup.ServiceStatus{{Name: "litellm", State: "running"}}, nil
	}, noControl)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	// On the list (not drilled): SetActive is a no-op — it must not touch the detail.
	view.SetActive(false)

	// Drill in — opens on the Service sub-tab, where the container-log poll is PAUSED
	// (the log runs only on the Logs sub-tab now).
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !view.detail.log.paused {
		test.Fatal("the container-log poll must be paused on the Service sub-tab")
	}
	// Switch to the Logs sub-tab: the poll resumes while the tab is visible.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyTab})
	if view.detail.log.paused {
		test.Fatal("switching to the Logs sub-tab must resume the container-log poll")
	}
	// Switching the top-level tab away pauses it; returning resumes it.
	view.SetActive(false)
	if !view.detail.log.paused {
		test.Fatal("SetActive(false) must pause the open detail's container-log poll")
	}
	view.SetActive(true)
	if view.detail.log.paused {
		test.Fatal("SetActive(true) must resume the open detail's container-log poll")
	}
}
