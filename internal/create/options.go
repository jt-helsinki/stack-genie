package create

import (
	"slices"

	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/config"
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

// SupportedAgentCLIs are the agent CLIs installable into a workspace (opencode is the
// default). hermes is a gateway/api-key agent like opencode/omp (always routes
// through the gateway, never OAuth).
func SupportedAgentCLIs() []string {
	return []string{"opencode", "omp", "claude-code", "codex", "gemini", "hermes"}
}

// SupportedApps are the opt-in in-VM AI applications (default none).
func SupportedApps() []string { return apps.Keys() }

// SplitAgentsAndApps partitions the combined "Agent CLIs & AI apps" wizard selection
// into agent CLIs vs in-VM app keys using the known option sets (SupportedAgentCLIs /
// SupportedApps) — both create wizards present agents and apps on ONE multi-select
// screen, so the split must not depend on selection order. Values in neither set are
// dropped; each side keeps the order the values were selected in.
func SplitAgentsAndApps(selected []string) (agentCLIs, appKeys []string) {
	knownAgents := SupportedAgentCLIs()
	knownApps := SupportedApps()
	for _, value := range selected {
		switch {
		case slices.Contains(knownAgents, value):
			agentCLIs = append(agentCLIs, value)
		case slices.Contains(knownApps, value):
			appKeys = append(appKeys, value)
		}
	}
	return agentCLIs, appKeys
}

// AI-tool keys for the "AI tools" multi-select (like the agent-CLI list): the
// per-project code/context tooling installed at workspace start. Values match the
// config.yaml context flags and, for the image-baked ones, the tools/<key> Dockerfile
// snippet name.
const (
	AIToolCaveman         = "caveman"
	AIToolGraphify        = "graphify"
	AIToolCodeReviewGraph = "code-review-graph"
	AIToolCodebaseMemory  = "codebase-memory-mcp"
)

// SupportedAITools are the per-project AI tools the user picks from one multi-select
// (mirroring the agent-CLI list) at `ai create`, instead of a screen/flag each. Order is
// the display order.
func SupportedAITools() []string {
	return []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory}
}

// DefaultAITools are the tools pre-selected when the user gives no --tools flag (and the
// wizard's initial checkboxes): caveman + graphify + code-review-graph on, codebase-memory
// off.
func DefaultAITools() []string {
	return []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph}
}

// SplitAITools reports, from a selected AI-tools list, whether each tool is enabled. Used
// by both create wizards to set the project.Spec bool flags from one selection.
func SplitAITools(tools []string) (caveman, graphify, codeReviewGraph, codebaseMemory bool) {
	return slices.Contains(tools, AIToolCaveman),
		slices.Contains(tools, AIToolGraphify),
		slices.Contains(tools, AIToolCodeReviewGraph),
		slices.Contains(tools, AIToolCodebaseMemory)
}

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
