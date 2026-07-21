package apps

import (
	"errors"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

func TestIsDashboardAgentAndList(test *testing.T) {
	if !IsDashboardAgent("hermes") {
		test.Error("hermes must be a dashboard agent")
	}
	if IsDashboardAgent("opencode") || IsDashboardAgent("openclaw") {
		test.Error("opencode/openclaw are not dashboard agents")
	}
	if agents := DashboardAgents(); len(agents) != 1 || agents[0] != "hermes" {
		test.Errorf("DashboardAgents() = %v, want [hermes]", agents)
	}
}

func TestSelectedDashboardAgents(test *testing.T) {
	got := SelectedDashboardAgents([]string{"opencode", "hermes", "codex"})
	if len(got) != 1 || got[0] != "hermes" {
		test.Errorf("SelectedDashboardAgents = %v, want [hermes]", got)
	}
	if got := SelectedDashboardAgents([]string{"opencode", "codex"}); len(got) != 0 {
		test.Errorf("no dashboard agent selected → %v, want empty", got)
	}
}

func TestSuggestedDashboardPort(test *testing.T) {
	// Free + unreserved → hermes' documented default 9119.
	if port := SuggestedDashboardPort("hermes", nil, func(int) bool { return true }); port != 9119 {
		test.Errorf("suggested = %d, want the default 9119", port)
	}
	// Default reserved → falls back to an auto-allocated window port.
	port := SuggestedDashboardPort("hermes", map[int]bool{9119: true}, func(int) bool { return true })
	if port < portRangeStart || port > portRangeEnd {
		test.Errorf("fallback = %d, want a window port", port)
	}
	if got := SuggestedDashboardPort("opencode", nil, func(int) bool { return true }); got != 0 {
		test.Errorf("non-dashboard CLI → %d, want 0", got)
	}
}

func TestAllocateDashboardEntries(test *testing.T) {
	// Honors a requested port; skips non-dashboard CLIs.
	entries, err := AllocateDashboardEntries([]string{"hermes", "opencode"},
		map[string]int{"hermes": 9119}, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != "hermes" || entries[0].Port != 9119 {
		test.Fatalf("entries = %+v, want [hermes:9119]", entries)
	}
	// A reserved requested port is rejected.
	_, err = AllocateDashboardEntries([]string{"hermes"},
		map[string]int{"hermes": 9119}, map[int]bool{9119: true}, func(int) bool { return true })
	if !errors.Is(err, ErrPortUnavailable) {
		test.Errorf("err = %v, want ErrPortUnavailable for a reserved dashboard port", err)
	}
	// No request → auto-allocated from the window.
	auto, err := AllocateDashboardEntries([]string{"hermes"}, nil, nil, func(int) bool { return true })
	if err != nil {
		test.Fatal(err)
	}
	if auto[0].Port < portRangeStart || auto[0].Port > portRangeEnd {
		test.Errorf("auto port = %d, want a window port", auto[0].Port)
	}
}

// PublishedPorts must publish agent-dashboard ports (host==guest) alongside app ports.
func TestPublishedPortsIncludesDashboards(test *testing.T) {
	projectConfig := &config.Config{
		Apps:            []config.AppEntry{{Key: "openwebui", Port: 21000}},
		AgentDashboards: []config.AppEntry{{Key: "hermes", Port: 9119}},
	}
	published := PublishedPorts(projectConfig)
	seen := map[int]bool{}
	for _, mapping := range published {
		if mapping.Host != mapping.Guest {
			test.Errorf("mapping %+v must use host==guest", mapping)
		}
		seen[mapping.Host] = true
	}
	if !seen[21000] || !seen[9119] {
		test.Errorf("published %v must include the app (21000) AND the hermes dashboard (9119)", published)
	}
}
