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

	"github.com/jt-helsinki/ideal-robot/internal/templates"
)

// Compose builds the full Dockerfile content for a project from the installed
// templates (~/.ai-platform/templates). An unknown OS key, stack, or agent CLI
// is an error.
func Compose(osKey string, stacks, agentCLIs []string) (string, error) {
	base, err := templates.BaseDockerfile(osKey)
	if err != nil {
		return "", fmt.Errorf("os %q: %w", osKey, err)
	}

	var builder strings.Builder
	builder.WriteString(strings.TrimRight(base, "\n"))
	builder.WriteString("\n")

	// Headroom (input compression) is installed in every workspace — it is core
	// context optimization, not an optional stack (arch §8–10).
	headroom, err := templates.HeadroomSnippet()
	if err != nil {
		return "", fmt.Errorf("headroom: %w", err)
	}
	appendSection(&builder, headroom)

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
	return builder.String(), nil
}

func appendSection(builder *strings.Builder, snippet string) {
	builder.WriteString("\n")
	builder.WriteString(strings.TrimRight(snippet, "\n"))
	builder.WriteString("\n")
}

// Write composes and writes <projectRoot>/.ai-platform/Dockerfile.
func Write(projectRoot, osKey string, stacks, agentCLIs []string) error {
	content, err := Compose(osKey, stacks, agentCLIs)
	if err != nil {
		return err
	}
	destination := filepath.Join(projectRoot, ".ai-platform", "Dockerfile")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destination, []byte(content), 0o644)
}
