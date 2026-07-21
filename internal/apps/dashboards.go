package apps

import (
	"sort"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// agentDashboards maps an agent CLI that ships a web dashboard to the port that dashboard
// listens on inside the microVM (its documented default). The dashboard is a web server the
// agent CLI launches in-VM (e.g. `hermes dashboard`); the platform publishes the chosen
// host port so it is reachable from the host, exactly like an in-VM app. Host port ==
// guest port (the publish chain forwards host:<port> → VM:<port>); the user launches the
// dashboard bound to 0.0.0.0:<port> in-VM.
var agentDashboards = map[string]int{
	// Hermes: `hermes dashboard --host 0.0.0.0 --port <port>` (docs default 9119).
	"hermes": 9119,
}

// IsDashboardAgent reports whether an agent CLI ships a web dashboard the platform can
// publish a host port for.
func IsDashboardAgent(cli string) bool {
	_, ok := agentDashboards[cli]
	return ok
}

// DashboardAgents returns the agent CLIs that ship a web dashboard, sorted.
func DashboardAgents() []string {
	agents := make([]string, 0, len(agentDashboards))
	for cli := range agentDashboards {
		agents = append(agents, cli)
	}
	sort.Strings(agents)
	return agents
}

// SelectedDashboardAgents returns, in DashboardAgents order, the dashboard-capable CLIs
// present in the given selection — used to drive the create prompt + allocation for only
// the agent CLIs actually chosen.
func SelectedDashboardAgents(selected []string) []string {
	chosen := make(map[string]bool, len(selected))
	for _, cli := range selected {
		chosen[cli] = true
	}
	agents := make([]string, 0, len(agentDashboards))
	for _, cli := range DashboardAgents() {
		if chosen[cli] {
			agents = append(agents, cli)
		}
	}
	return agents
}

// SuggestedDashboardPort proposes a default host port to expose an agent CLI's dashboard on
// (for seeding the create prompt): its documented default port when free + unreserved, else
// the next auto-allocated free port. Returns 0 for a non-dashboard CLI.
func SuggestedDashboardPort(cli string, reserved map[int]bool, isFree portChecker) int {
	defaultPort, ok := agentDashboards[cli]
	if !ok {
		return 0
	}
	if isFree == nil {
		isFree = realPortFree
	}
	if reserved == nil {
		reserved = map[int]bool{}
	}
	if !reserved[defaultPort] && isFree(defaultPort) {
		return defaultPort
	}
	if port, err := allocatePort(reserved, isFree); err == nil {
		return port
	}
	return defaultPort
}

// AllocateDashboardEntries assigns each requested dashboard-capable agent CLI a host port,
// returning the config.AppEntry records to persist into config.AgentDashboards. Same
// honor/validate/auto-allocate rules as AllocateEntries; non-dashboard CLIs are skipped.
func AllocateDashboardEntries(clis []string, requested map[string]int, reserved map[int]bool, isFree portChecker) ([]config.AppEntry, error) {
	return allocatePortsForKeys(clis, requested, reserved, isFree, IsDashboardAgent)
}
