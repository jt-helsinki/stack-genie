package acceptance

import (
	"strings"
	"testing"
)

// Project creation in acceptance is NON-INTERACTIVE, via flags. Under --json the
// interactive `project create` wizard is deliberately disabled (the programmatic
// contract — every input has a flag, no prompts; CLI §1.3/§3.1), and the harness
// always runs commands with --json, so it drives create with explicit flags
// rather than a pseudo-terminal.

// CreateProject creates a project with the default base OS (debian-trixie) and
// the default agent CLIs (opencode, pi).
func (harness *Harness) CreateProject(test *testing.T, name string) (Envelope, int) {
	return harness.CreateProjectWithOS(test, name, "debian-trixie")
}

// CreateProjectWithOS creates a project with a specific base OS via --os.
func (harness *Harness) CreateProjectWithOS(test *testing.T, name, osKey string) (Envelope, int) {
	test.Helper()
	return harness.Run(test, "create", name, "--os", osKey)
}

// CreateProjectFull creates a project pinning every input — base OS, agent CLIs
// (--agents), and software stacks (--stacks) — so the §6.1/§6.3/§6.4 file-probe
// tests can drive an exact selection non-interactively. Empty agents/stacks
// slices fall back to the create command's own defaults (opencode,pi / none).
func (harness *Harness) CreateProjectFull(test *testing.T, name, osKey string, agents, stacks []string) (Envelope, int) {
	test.Helper()
	args := []string{"create", name, "--os", osKey}
	if len(agents) > 0 {
		args = append(args, "--agents", strings.Join(agents, ","))
	}
	if len(stacks) > 0 {
		args = append(args, "--stacks", strings.Join(stacks, ","))
	}
	return harness.Run(test, args...)
}
