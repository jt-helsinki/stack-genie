package apps

import (
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

// ReservedPortsAcrossWorkspaces returns the set of host ports already allocated to
// in-VM apps across EVERY registered workspace on this host. It is the real
// machine-wide reserved-port view a Manager uses so a freshly-installed app gets a
// port no other running workspace publishes. A workspace whose config cannot be
// read is skipped (best-effort — a missing/half-written config must not block an
// allocation elsewhere).
func ReservedPortsAcrossWorkspaces() (map[int]bool, error) {
	index, err := state.LoadIndex()
	if err != nil {
		return nil, err
	}
	reserved := map[int]bool{}
	for _, entry := range index.Projects {
		projectConfig, err := config.LoadProjectConfig(entry.Path)
		if err != nil {
			continue
		}
		for _, app := range projectConfig.Apps {
			reserved[app.Port] = true
		}
		for _, dashboard := range projectConfig.AgentDashboards {
			reserved[dashboard.Port] = true
		}
	}
	return reserved, nil
}
