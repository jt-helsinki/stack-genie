package create

import "github.com/jt-helsinki/ideal-robot/internal/apps"

// The selectable create options — the SINGLE source of truth shared by the CLI's `ai
// create` wizard/flags and the `ai ui` in-TUI create wizard (the TUI cannot import cli,
// so these live here, a package both import).

// SupportedOSes are the base OS templates the user picks from (arch §25). All four
// ship; the user always picks one — none is applied silently.
func SupportedOSes() []string {
	return []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"}
}

// SupportedStacks are the extra software stacks (Python + Node + uv + Graphify are
// baked into every base by default, so they are NOT stacks).
func SupportedStacks() []string {
	return []string{"go", "rust", "java", "maven", "deno"}
}

// SupportedAgentCLIs are the agent CLIs installable into a workspace (opencode + pi are
// the defaults).
func SupportedAgentCLIs() []string {
	return []string{"opencode", "pi", "claude-code", "codex", "gemini"}
}

// SupportedApps are the opt-in in-VM AI applications (default none).
func SupportedApps() []string { return apps.Keys() }
