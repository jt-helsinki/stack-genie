package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

// newDetailForTest builds a ServiceDetail pointed at a single fake service whose
// container log returns a fixed line. follow toggles the f/enter follow message.
func newDetailForTest(status setup.ServiceStatus, logLine string, follow bool) *ServiceDetail {
	var followFn func(string) tea.Msg
	if follow {
		followFn = func(svc string) tea.Msg { return ServiceLogFollowRequestedMsg{Service: svc} }
	}
	log := NewLogView(
		func() (string, error) { return logLine, nil },
		func() bool { return status.State == "running" },
		func() string { return status.Name },
		LogViewLabels{
			NotRunning: "service not running",
			Loading:    "loading container log…",
			Empty:      "no container output captured yet",
		},
		followFn,
	)
	detail := NewServiceDetail(
		func(string) (setup.ServiceStatus, bool) { return status, true },
		noControl, noOpen, log,
	)
	detail.SetService(status.Name)
	detail.SetActive(true)
	return detail
}

// TestServiceDetailRendersSummaryAndEmbeddedLog: the detail shows the service summary
// on top with the container log embedded beneath (the workspace-detail shape).
func TestServiceDetailRendersSummaryAndEmbeddedLog(test *testing.T) {
	detail := newDetailForTest(
		setup.ServiceStatus{Name: "litellm", Mode: "container", State: "running", Healthy: true, Console: "http://x/ui"},
		"container boot line\n", true)
	detail.SetSize(80, 20)

	// Feed the status refresh + a log load so both render.
	_ = detail.Update(detail.refreshCmd()())
	detail.log.generation = 1
	_ = detail.Update(detail.log.loadCmd(1)())

	rendered := detail.View()
	for _, want := range []string{"litellm", "Container log", "container boot line"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("detail view should contain %q, got:\n%s", want, rendered)
		}
	}
}

// TestServiceDetailLifecycleSpinnerAndStaysPut: a lifecycle key shows an in-place
// spinner (pending set) and the action lands without leaving the detail.
func TestServiceDetailLifecycleStaysPut(test *testing.T) {
	var controlled string
	log := NewLogView(func() (string, error) { return "", nil }, func() bool { return true },
		func() string { return "litellm" }, LogViewLabels{Loading: "…"}, nil)
	detail := NewServiceDetail(
		func(string) (setup.ServiceStatus, bool) {
			return setup.ServiceStatus{Name: "litellm", State: "running"}, true
		},
		func(action, service string) error { controlled = action + ":" + service; return nil },
		noOpen, log,
	)
	detail.SetService("litellm")
	detail.SetSize(80, 20)
	_ = detail.Update(detail.refreshCmd()())

	cmd := detail.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		test.Fatal("s must return a command")
	}
	if detail.pending != "start" {
		test.Fatalf("pending should be 'start' (the in-place spinner), got %q", detail.pending)
	}
	// The detail view must show the spinner-ed state line, not bounce away.
	if !strings.Contains(detail.View(), "starting…") {
		test.Errorf("the state line should show the in-flight spinner, got:\n%s", detail.View())
	}
	// Run the batch's lifecycle-done sub-command.
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, sub := range batch {
			if msg := sub(); msg != nil {
				if _, isDone := msg.(serviceLifecycleDoneMsg); isDone {
					_ = detail.Update(msg)
				}
			}
		}
	}
	if controlled != "start:litellm" {
		test.Fatalf("controller calls = %q, want start:litellm", controlled)
	}
	if detail.pending != "" {
		test.Errorf("the spinner should clear once the action lands, got %q", detail.pending)
	}
}

// TestServiceDetailUpdateEmitsRequest: `p` emits the update request (handled by the
// parent's terminal overlay), not an in-view controller call.
func TestServiceDetailUpdateEmitsRequest(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "ollama", State: "running"}, "", false)
	detail.SetSize(80, 20)
	_ = detail.Update(detail.refreshCmd()())

	cmd := detail.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd == nil {
		test.Fatal("p must return a command")
	}
	if request, ok := cmd().(ServiceUpdateRequestedMsg); !ok || request.Service != "ollama" {
		test.Fatalf("p must emit ServiceUpdateRequestedMsg{ollama}, got %#v", cmd())
	}
}

// TestServiceDetailFollowEmitsRequest: `f` asks the parent to follow the container
// log live in the real terminal.
func TestServiceDetailFollowEmitsRequest(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "litellm", State: "running"}, "x\n", true)
	detail.SetSize(80, 20)
	_ = detail.Update(detail.refreshCmd()())
	detail.log.generation = 1
	_ = detail.Update(detail.log.loadCmd(1)())

	cmd := detail.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if cmd == nil {
		test.Fatal("f must request a live follow")
	}
	if request, ok := cmd().(ServiceLogFollowRequestedMsg); !ok || request.Service != "litellm" {
		test.Fatalf("f must emit ServiceLogFollowRequestedMsg{litellm}, got %#v", cmd())
	}
}

// TestServiceDetailStoppedShowsNotRunning: a stopped service shows the "not running"
// hint in the embedded log, not stale output.
func TestServiceDetailStoppedShowsNotRunning(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "ollama", State: "stopped"}, "old output\n", false)
	detail.SetSize(80, 20)
	_ = detail.Update(detail.refreshCmd()())
	detail.log.generation = 1
	_ = detail.Update(detail.log.loadCmd(1)())

	rendered := detail.View()
	if strings.Contains(rendered, "old output") {
		test.Errorf("a stopped service must not show stale log output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "not running") {
		test.Errorf("a stopped service should show the 'not running' hint:\n%s", rendered)
	}
}
