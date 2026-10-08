package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/envimage"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// toolRow is one row of `ai tools list`: an AI tool's enabled state + description.
type toolRow struct {
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
}

// toolsListResult is the typed payload for `ai tools list`.
type toolsListResult struct {
	Project string    `json:"project"`
	Tools   []toolRow `json:"tools"`
}

// Human renders the TOOL / ENABLED / DESCRIPTION table.
func (result toolsListResult) Human() string {
	rows := make([][]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		state := ui.Muted.Render("disabled")
		if tool.Enabled {
			state = ui.Success.Render("enabled")
		}
		rows = append(rows, []string{ui.Value.Render(tool.Name), state, ui.Muted.Render(tool.Description)})
	}
	return ui.Table([]string{"TOOL", "ENABLED", "DESCRIPTION"}, rows)
}

// toolActionResult is the typed payload for `ai tools enable|disable`.
type toolActionResult struct {
	Project string `json:"project"`
	Tool    string `json:"tool"`
	Action  string `json:"action"`
}

// Human notes that applying the change needs an image rebuild, not just a restart —
// offerWorkspaceRestart (called right after) already runs the full rebuild+recreate.
func (result toolActionResult) Human() string {
	return fmt.Sprintf("%s AI tool %q in workspace %q (a rebuild is required to apply this — see below)",
		result.Action, result.Tool, result.Project)
}

// mapToolsErr maps tool-toggle errors to exit codes (§18), mirroring mapContextErr.
func mapToolsErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	if errors.Is(err, project.ErrUnknownProject) {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	return output.Errorf(output.ExitRuntimeFailure, "%s", err)
}

// setToolEnabled sets the given tool key's enabled bool pointer on a ContextConfig.
func setToolEnabled(settings *config.ContextConfig, tool string, enabled bool) {
	switch tool {
	case create.AIToolCaveman:
		settings.CavemanEnabled = &enabled
	case create.AIToolGraphify:
		settings.GraphifyEnabled = &enabled
	case create.AIToolCodeReviewGraph:
		settings.CodeReviewGraphEnabled = &enabled
	case create.AIToolCodebaseMemory:
		settings.CodebaseMemoryEnabled = &enabled
	case create.AIToolOpenViking:
		settings.OpenVikingEnabled = &enabled
	case create.AIToolTokenSave:
		settings.TokenSaveEnabled = &enabled
	case create.AIToolAgentMemory:
		settings.AgentMemoryEnabled = &enabled
	case create.AIToolPonytail:
		settings.PonytailEnabled = &enabled
	case create.AIToolAgentSkills:
		settings.AgentSkillsEnabled = &enabled
	}
}

// dockerfileToolsFor derives the Dockerfile tool-key list from a ContextConfig, sharing
// project.DockerfileTools's logic (caveman/ponytail are runtime-only — excluded).
func dockerfileToolsFor(settings config.ContextConfig) []string {
	return project.DockerfileTools(
		settings.GraphifyEnabledOrDefault(),
		settings.CodeReviewGraphEnabledOrDefault(),
		settings.CodebaseMemoryEnabledOrDefault(),
		settings.OpenVikingEnabledOrDefault(),
		settings.TokenSaveEnabledOrDefault(),
		settings.AgentMemoryEnabledOrDefault(),
	)
}

// newToolsCmd builds `ai tools <list|enable|disable> [tool] [name]`: inspect and
// toggle the opt-in AI tools (caveman, graphify, code-review-graph, codebase-memory-mcp,
// openviking, tokensave, agentmemory, ponytail) after `ai create` time. enable/disable
// persist config.yaml, regenerate the project Dockerfile so a rebuild stays in sync,
// and offer to restart a running workspace now (full rebuild+recreate — the only way an
// image-level change takes effect).
func newToolsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools <list|enable|disable> [tool] [name]",
		Short: "Inspect and toggle opt-in AI tools (caveman, graphify, tokensave, …)",
		Args:  cobra.MinimumNArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				return []string{"list", "enable", "disable"}, cobra.ShellCompDirectiveNoFileComp
			case 1:
				if args[0] == "list" {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
				return create.SupportedAITools(), cobra.ShellCompDirectiveNoFileComp
			default:
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]
			if action == "list" {
				name, err := resolveProjectName(cmd, firstArg(args[1:]))
				if err != nil {
					*exit = emitter.Failure("tools.list", err)
					return nil
				}
				runToolsList(emitter, exit, name)
				return nil
			}
			if action != "enable" && action != "disable" {
				*exit = emitter.Failure("tools."+action, output.Errorf(output.ExitInvalidInput,
					"usage: ai tools <list|enable|disable> [tool] [name]"))
				return nil
			}
			if len(args) < 2 {
				*exit = emitter.Failure("tools."+action, output.Errorf(output.ExitInvalidInput,
					"usage: ai tools %s <tool> [name] (tool one of: %s)", action, strings.Join(create.SupportedAITools(), ", ")))
				return nil
			}
			tool := args[1]
			if !slices.Contains(create.SupportedAITools(), tool) {
				*exit = emitter.Failure("tools."+action, output.Errorf(output.ExitInvalidInput,
					"unknown tool %q (one of: %s)", tool, strings.Join(create.SupportedAITools(), ", ")))
				return nil
			}
			name, err := resolveProjectName(cmd, thirdArg(args))
			if err != nil {
				*exit = emitter.Failure("tools."+action, err)
				return nil
			}
			runToolsToggle(emitter, exit, action, tool, name)
			return nil
		},
	}
	return cmd
}

func runToolsList(emitter *output.Emitter, exit *int, name string) {
	root, err := resolveProjectRoot(name)
	if err != nil {
		*exit = emitter.Failure("tools.list", mapToolsErr(err))
		return
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		*exit = emitter.Failure("tools.list", mapToolsErr(err))
		return
	}
	tools := create.SupportedAITools()
	rows := make([]toolRow, 0, len(tools))
	for _, tool := range tools {
		rows = append(rows, toolRow{
			Name:        tool,
			Enabled:     projectConfig.Context.ToolEnabled(tool),
			Description: create.AIToolDescription(tool),
		})
	}
	*exit = emitter.Success("tools.list", toolsListResult{Project: name, Tools: rows})
}

func runToolsToggle(emitter *output.Emitter, exit *int, action, tool, name string) {
	root, err := resolveProjectRoot(name)
	if err != nil {
		*exit = emitter.Failure("tools."+action, mapToolsErr(err))
		return
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		*exit = emitter.Failure("tools."+action, mapToolsErr(err))
		return
	}
	setToolEnabled(&projectConfig.Context, tool, action == "enable")
	if err := config.WriteProject(root, projectConfig); err != nil {
		*exit = emitter.Failure("tools."+action, mapToolsErr(err))
		return
	}
	// Regenerate the Dockerfile so a later rebuild picks up the change — stacks aren't
	// in config.yaml (profile.yaml is the one record of them outside the Dockerfile
	// itself), so they're read back separately rather than dropped.
	stacks, err := project.ReadStacks(root)
	if err != nil {
		*exit = emitter.Failure("tools."+action, mapToolsErr(err))
		return
	}
	dockerfileTools := dockerfileToolsFor(projectConfig.Context)
	if err := envimage.Write(root, projectConfig.OS, stacks, projectConfig.Agent.Tools, dockerfileTools); err != nil {
		*exit = emitter.Failure("tools."+action, mapToolsErr(err))
		return
	}
	*exit = emitter.Success("tools."+action, toolActionResult{Project: name, Tool: tool, Action: action})
	offerWorkspaceRestart(emitter, root)
}
