package acceptance

import "testing"

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
	return harness.Run(test, "project", "create", name, "--os", osKey)
}
