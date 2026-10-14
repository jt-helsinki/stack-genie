package create

import (
	"path/filepath"
	"slices"
	"strings"

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

// SupportedAgentCLIs are the agent CLIs installable on the host (opencode is the
// default). hermes is a gateway/api-key agent like opencode/omp (always routes
// through the gateway, never OAuth).
func SupportedAgentCLIs() []string {
	return []string{"opencode", "omp", "claude-code", "codex", "gemini", "hermes"}
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
	AIToolTokenSave       = "tokensave"
	AIToolAgentMemory     = "agentmemory"
	AIToolPonytail        = "ponytail"
	AIToolAgentSkills     = "agent-skills"
	AIToolSerena          = "serena"
)

// SupportedAITools are the per-project AI tools the user picks from one multi-select
// (mirroring the agent-CLI list) at `ai create`, instead of a screen/flag each. Order is
// the display order.
func SupportedAITools() []string {
	return []string{
		AIToolCaveman, AIToolGraphify, AIToolCodeReviewGraph, AIToolCodebaseMemory, AIToolOpenViking,
		AIToolTokenSave, AIToolAgentMemory, AIToolPonytail, AIToolAgentSkills, AIToolSerena,
	}
}

// DefaultAITools are the tools pre-selected when the user gives no --tools flag (and the
// wizard's initial checkboxes). All AI tools are opt-in — none selected by default.
func DefaultAITools() []string {
	return nil
}

// AIToolDescription returns a one-line, user-facing description of what an AI tool does,
// shown next to its name in the create wizard and `ai tools list`. An unknown key returns "".
func AIToolDescription(tool string) string {
	switch tool {
	case AIToolCaveman:
		return "Compresses agent output into a terser style to cut tokens (lite/full/ultra levels)."
	case AIToolGraphify:
		return "Builds a knowledge graph of the codebase for faster, cheaper code navigation and queries."
	case AIToolCodeReviewGraph:
		return "Code-review knowledge-graph MCP server; generates a visual D3 dependency graph."
	case AIToolCodebaseMemory:
		return "Persistent codebase-memory MCP server, with an optional 3D graph UI."
	case AIToolOpenViking:
		return "Local context database server for agents (openviking.ai)."
	case AIToolTokenSave:
		return "Code-graph MCP server for 34 languages — semantic queries instead of grepping/reading files, cutting token use."
	case AIToolAgentMemory:
		return "Persistent cross-session memory — captures and recalls prior work so agents stop re-learning the codebase."
	case AIToolPonytail:
		return "Nags the agent to check YAGNI/stdlib/existing code before writing anything new — an anti-over-engineering skill."
	case AIToolAgentSkills:
		return "Curated senior-engineer skills (spec, TDD, review, security, shipping, …) — github.com/addyosmani/agent-skills."
	case AIToolSerena:
		return "LSP-backed semantic code navigation and editing (symbols, references, refactors) — installed into the project's Python environment."
	default:
		return ""
	}
}

// SplitAITools reports, from a selected AI-tools list, whether each tool is enabled. Used
// by both create wizards to set the project.Spec bool flags from one selection.
func SplitAITools(tools []string) (caveman, graphify, codeReviewGraph, codebaseMemory, openViking, tokenSave, agentMemory, ponytail, agentSkills, serena bool) {
	return slices.Contains(tools, AIToolCaveman),
		slices.Contains(tools, AIToolGraphify),
		slices.Contains(tools, AIToolCodeReviewGraph),
		slices.Contains(tools, AIToolCodebaseMemory),
		slices.Contains(tools, AIToolOpenViking),
		slices.Contains(tools, AIToolTokenSave),
		slices.Contains(tools, AIToolAgentMemory),
		slices.Contains(tools, AIToolPonytail),
		slices.Contains(tools, AIToolAgentSkills),
		slices.Contains(tools, AIToolSerena)
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
