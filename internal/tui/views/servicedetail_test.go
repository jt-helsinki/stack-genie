package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/setup"
)

// newDetailForTest builds a ServiceDetail pointed at a single fake service whose
// container log returns a fixed line and whose stats fetch returns one container.
// follow toggles the f/enter follow message.
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
	statsFn := func(string) ([]setup.ContainerStats, error) {
		return []setup.ContainerStats{{
			Container: status.Name, ID: "abc123def456", State: status.State,
			Found: status.State == "running", CPUPercent: "2.00%", MemUsage: "10MiB / 20MiB",
		}}, nil
	}
	detail := NewServiceDetail(
		func(string) (setup.ServiceStatus, bool) { return status, true },
		statsFn, noControl, noOpen, log,
	)
	detail.SetService(status.Name)
	detail.SetActive(true)
	detail.SetSize(80, 20)
	return detail
}

// primeInfo starts the Service sub-tab's poll and feeds one status+stats refresh so the
// summary + metrics render.
func primeInfo(detail *ServiceDetail) {
	_ = detail.Init()
	_ = detail.Update(detail.refreshCmd(detail.generation)())
}

// showLogs switches to the Logs sub-tab and feeds one log load so the container log
// renders.
func showLogs(detail *ServiceDetail) {
	_ = detail.Update(tea.KeyMsg{Type: tea.KeyTab})
	_ = detail.Update(detail.log.loadCmd(detail.log.generation)())
}

// TestServiceDetailServiceTabShowsSummaryAndMetrics: the Service sub-tab shows the
// summary + a live per-container metrics block (no log — that is on the Logs tab).
func TestServiceDetailServiceTabShowsSummaryAndMetrics(test *testing.T) {
	detail := newDetailForTest(
		setup.ServiceStatus{Name: "litellm", Mode: "container", State: "running", Healthy: true, Console: "http://x/ui"},
		"container boot line\n", true)
	primeInfo(detail)

	rendered := detail.View()
	for _, want := range []string{"litellm", "running", "abc123def456", "2.00%", "Containers"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("Service sub-tab should contain %q, got:\n%s", want, rendered)
		}
	}
	// The log lives on the OTHER sub-tab — not here.
	if strings.Contains(rendered, "container boot line") {
		test.Errorf("the log must not render on the Service sub-tab:\n%s", rendered)
	}
}

// TestServiceDetailLogsTabShowsLog: switching to the Logs sub-tab shows the scrollable
// container log.
func TestServiceDetailLogsTabShowsLog(test *testing.T) {
	detail := newDetailForTest(
		setup.ServiceStatus{Name: "litellm", State: "running"}, "container boot line\n", true)
	primeInfo(detail)
	showLogs(detail)

	if !strings.Contains(detail.View(), "container boot line") {
		test.Errorf("the Logs sub-tab should show the container log, got:\n%s", detail.View())
	}
}

// TestServiceDetailLifecycleStaysPut: a lifecycle key on the Service tab shows an
// in-place spinner (pending set) and lands without leaving the detail.
func TestServiceDetailLifecycleStaysPut(test *testing.T) {
	var controlled string
	log := NewLogView(func() (string, error) { return "", nil }, func() bool { return true },
		func() string { return "litellm" }, LogViewLabels{Loading: "…"}, nil)
	detail := NewServiceDetail(
		func(string) (setup.ServiceStatus, bool) {
			return setup.ServiceStatus{Name: "litellm", State: "running"}, true
		},
		func(string) ([]setup.ContainerStats, error) { return nil, nil },
		func(action, service string) error { controlled = action + ":" + service; return nil },
		noOpen, log,
	)
	detail.SetService("litellm")
	detail.SetActive(true)
	detail.SetSize(80, 20)
	primeInfo(detail)

	cmd := detail.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		test.Fatal("s must return a command")
	}
	if detail.pending != "start" {
		test.Fatalf("pending should be 'start' (the in-place spinner), got %q", detail.pending)
	}
	if !strings.Contains(detail.View(), "starting…") {
		test.Errorf("the state line should show the in-flight spinner, got:\n%s", detail.View())
	}
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

// TestServiceDetailUpdateEmitsRequest: `p` on the Service tab emits the update request.
func TestServiceDetailUpdateEmitsRequest(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "vllm", State: "running"}, "", false)
	primeInfo(detail)

	cmd := detail.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd == nil {
		test.Fatal("p must return a command")
	}
	if request, ok := cmd().(ServiceUpdateRequestedMsg); !ok || request.Service != "vllm" {
		test.Fatalf("p must emit ServiceUpdateRequestedMsg{vllm}, got %#v", cmd())
	}
}

// TestServiceDetailFollowEmitsRequest: `f` on the Logs sub-tab asks the parent to
// follow the container log live in the real terminal.
func TestServiceDetailFollowEmitsRequest(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "litellm", State: "running"}, "x\n", true)
	primeInfo(detail)
	showLogs(detail)

	cmd := detail.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if cmd == nil {
		test.Fatal("f must request a live follow")
	}
	if request, ok := cmd().(ServiceLogFollowRequestedMsg); !ok || request.Service != "litellm" {
		test.Fatalf("f must emit ServiceLogFollowRequestedMsg{litellm}, got %#v", cmd())
	}
}

// TestServiceDetailStoppedShowsNotRunning: a stopped service's Logs sub-tab shows the
// "not running" hint, not stale output.
func TestServiceDetailStoppedShowsNotRunning(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "vllm", State: "stopped"}, "old output\n", false)
	primeInfo(detail)
	showLogs(detail)

	rendered := detail.View()
	if strings.Contains(rendered, "old output") {
		test.Errorf("a stopped service must not show stale log output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "not running") {
		test.Errorf("a stopped service should show the 'not running' hint:\n%s", rendered)
	}
}

// TestServiceDetailTabSwitchesSubTabs verifies Tab cycles Service ↔ Logs.
func TestServiceDetailTabSwitchesSubTabs(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "vllm", State: "running"}, "line\n", false)
	primeInfo(detail)
	if detail.subIndex != serviceTabInfo {
		test.Fatalf("should open on the Service sub-tab, got %d", detail.subIndex)
	}
	_ = detail.Update(tea.KeyMsg{Type: tea.KeyTab})
	if detail.subIndex != serviceTabLogs {
		test.Errorf("Tab should switch to the Logs sub-tab, got %d", detail.subIndex)
	}
	_ = detail.Update(tea.KeyMsg{Type: tea.KeyTab})
	if detail.subIndex != serviceTabInfo {
		test.Errorf("Tab should switch back to the Service sub-tab, got %d", detail.subIndex)
	}
}
