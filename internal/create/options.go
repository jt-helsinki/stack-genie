package create

import (
	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/config"
)

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
	return []string{"opencode", "pi", "omp", "claude-code", "codex", "gemini"}
}

// SupportedApps are the opt-in in-VM AI applications (default none).
func SupportedApps() []string { return apps.Keys() }

// SupportedShells are the interactive shells a workspace can default to. bash is the
// platform default (today's behavior); zsh is the alternative.
func SupportedShells() []string { return []string{"bash", "zsh"} }

// SupportedAuthModes are the per-agent authentication modes: "api-key" (route through
// the gateway with the scoped virtual key — keeps the tool firewall + secret masking)
// and "oauth" (the CLI's own subscription login, direct to the provider — bypasses the
// gateway guardrails). Only the OAuth-capable CLIs (OAuthCapableCLIs) can be "oauth".
func SupportedAuthModes() []string { return []string{"api-key", "oauth"} }

// OAuthCapableCLIs are the agent CLIs that can be set to "oauth" (they have a first-party
// subscription login). Re-exported from config so the CLI/TUI create wizards share one
// source of truth without importing config's other surface.
func OAuthCapableCLIs() []string { return config.OAuthCapableCLIs() }
