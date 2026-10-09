package acceptance

import (
	"strings"
	"testing"
)

// Project creation in acceptance is NON-INTERACTIVE, via flags. Under --json the
// interactive `create` wizard is deliberately disabled (the programmatic
// contract — every input has a flag, no prompts; CLI §1.3/§3.1), and the harness
// always runs commands with --json, so it drives create with explicit flags
// rather than a pseudo-terminal.

// CreateProject creates a project with the default agent CLI (opencode).
func (harness *Harness) CreateProject(test *testing.T, name string) (Envelope, int) {
	test.Helper()
	return harness.Run(test, "create", name)
}

// CreateProjectWithAgents creates a project pinning the agent CLIs (--agents) so
// the agent-selection file-probe tests can drive an exact selection
// non-interactively. An empty agents slice falls back to create's own default
// (opencode).
func (harness *Harness) CreateProjectWithAgents(test *testing.T, name string, agents []string) (Envelope, int) {
	test.Helper()
	args := []string{"create", name}
	if len(agents) > 0 {
		args = append(args, "--agents", strings.Join(agents, ","))
	}
	return harness.Run(test, args...)
}
