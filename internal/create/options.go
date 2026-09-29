package create

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/config"
)

// SanitizeName lowercases raw and keeps only [a-z0-9-] (everything else becomes "-",
// with leading/trailing "-" trimmed), for a wizard's auto-derived default workspace
// name — the user can still edit the result.
func SanitizeName(raw string) string {
	lowered := strings.ToLower(raw)
	var builder strings.Builder
	for _, char := range lowered {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			builder.WriteRune(char)
		} else {
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

// DefaultDirName derives a default workspace name from a directory path's basename
// (SanitizeName'd) — the create wizard's "name defaults to the workspace's directory"
// behavior, shared by the CLI and TUI wizards. An empty/root-only path returns "".
func DefaultDirName(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return ""
	}
	return SanitizeName(filepath.Base(trimmed))
}

// The selectable create options — the SINGLE source of truth shared by the CLI's `ai
// create` wizard/flags and the `ai ui` in-TUI create wizard (the TUI cannot import cli,
// so these live here, a package both import).

// SupportedOSes are the base OS templates the user picks from (arch §25). All three
// ship; the user always picks one — none is applied silently.
func SupportedOSes() []string {
	return []string{"debian-trixie", "ubuntu", "alma"}
}

// osDisplayNames maps each SupportedOSes() key to a human-readable "name + version"
// label, kept in sync with the pinned base image in each
// internal/templates/files/dockerfiles/<key>/Dockerfile FROM line.
var osDisplayNames = map[string]string{
	"debian-trixie": "Debian 13 (Trixie)",
	"ubuntu":        "Ubuntu 24.04 LTS",
	"alma":          "AlmaLinux 10.2",
}

// OSDisplayName returns the human-readable "name + version" label for a SupportedOSes()
// key (e.g. "debian-trixie" -> "Debian 13 (Trixie)"), for the create wizard's OS picker.
// An unrecognized key is returned unchanged.
func OSDisplayName(key string) string {
	if label, ok := osDisplayNames[key]; ok {
		return label
	}
	return key
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
	AIToolOpenViking      = "openviking"
)

// SupportedAITools are the per-project AI tools the user picks from one multi-select
// (mirroring the agent-CLI list) at `ai create`, instead of a screen/flag each. Order is
// the display order.
func SupportedAITools() []string {
	return []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory, AIToolOpenViking}
}

// DefaultAITools are the tools pre-selected when the user gives no --tools flag (and the
// wizard's initial checkboxes): caveman + graphify + code-review-graph on,
// codebase-memory + openviking off.
func DefaultAITools() []string {
	return []string{AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph}
}

// SplitAITools reports, from a selected AI-tools list, whether each tool is enabled. Used
// by both create wizards to set the project.Spec bool flags from one selection.
func SplitAITools(tools []string) (caveman, graphify, codeReviewGraph, codebaseMemory, openViking bool) {
	return slices.Contains(tools, AIToolCaveman),
		slices.Contains(tools, AIToolGraphify),
		slices.Contains(tools, AIToolCodeReviewGraph),
		slices.Contains(tools, AIToolCodebaseMemory),
		slices.Contains(tools, AIToolOpenViking)
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
