// Package envimage composes a project's .ai-platform/Dockerfile (arch §25, §12):
// the selected OS base template, then each selected software stack, then each
// selected agent CLI. The Dockerfile is the project's owned, git-tracked source
// of truth for the workspace image; the OCI build + microVM boot consume it.
package envimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/templates"
)

// Compose builds the full Dockerfile content for a project from the installed
// templates (~/.ai-platform/templates): the OS base, then each selected software
// stack, each selected agent CLI, and finally each selected OPT-IN dev tool
// (code-review-graph / codebase-memory-mcp) — the tools are appended only when the
// project selected them, so an unselected tool never bloats the image. An unknown
// OS key, stack, agent CLI, or tool is an error.
func Compose(osKey string, stacks, agentCLIs, tools []string) (string, error) {
	base, err := templates.BaseDockerfile(osKey)
	if err != nil {
		return "", fmt.Errorf("os %q: %w", osKey, err)
	}

	var builder strings.Builder
	builder.WriteString(strings.TrimRight(base, "\n"))
	builder.WriteString("\n")

	// Headroom (input compression) is NOT baked into the workspace image: it runs
	// as a shared host container in front of LiteLLM (arch §8–10, §15). Agents
	// reach it at AI_PLATFORM_HOST:18787 and carry the project's compression knobs
	// per request (internal/contextopt.HeadroomParams). The workspace image only
	// needs the OS base, the selected stacks, and the agent CLIs.
	for _, stack := range stacks {
		snippet, err := templates.StackSnippet(stack)
		if err != nil {
			return "", fmt.Errorf("stack %q: %w", stack, err)
		}
		appendSection(&builder, snippet)
	}
	for _, agentCLI := range agentCLIs {
		snippet, err := templates.AgentCLISnippet(agentCLI)
		if err != nil {
			return "", fmt.Errorf("agent cli %q: %w", agentCLI, err)
		}
		appendSection(&builder, snippet)
	}
	// Opt-in dev tools LAST — appended only for the tools the project selected, so an
	// unselected tool's installer never runs at build time (arch §12/§25).
	for _, tool := range tools {
		snippet, err := templates.ToolSnippet(tool)
		if err != nil {
			return "", fmt.Errorf("tool %q: %w", tool, err)
		}
		appendSection(&builder, snippet)
	}
	return builder.String(), nil
}

func appendSection(builder *strings.Builder, snippet string) {
	builder.WriteString("\n")
	builder.WriteString(strings.TrimRight(snippet, "\n"))
	builder.WriteString("\n")
}

// Write composes and writes <projectRoot>/.ai-platform/Dockerfile.
func Write(projectRoot, osKey string, stacks, agentCLIs, tools []string) error {
	content, err := Compose(osKey, stacks, agentCLIs, tools)
	if err != nil {
		return err
	}
	destination := filepath.Join(projectRoot, ".ai-platform", "Dockerfile")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destination, []byte(content), 0o644)
}
