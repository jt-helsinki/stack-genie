package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/git"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/spf13/cobra"
)

// Selectable options for the create wizard. All four OS templates ship as of
// S5 (debian-trixie in S1; debian-bookworm, ubuntu, alma added in S5). The user
// always picks the OS — none is applied silently (arch §25).
var (
	supportedOSes      = []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"}
	supportedStacks    = []string{"go", "node", "python", "rust", "java", "maven", "deno"}
	supportedAgentCLIs = []string{"opencode", "claude-code", "codex", "gemini"}
)

func newProjectCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Create, list, and delete projects",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newProjectCreateCmd(emitter, exit),
		newProjectListCmd(emitter, exit),
		newProjectDeleteCmd(emitter, exit),
	)
	return cmd
}

func mapProjectErr(err error) error {
	switch {
	case errors.Is(err, project.ErrInvalidName),
		errors.Is(err, project.ErrAlreadyExists),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

func newProjectCreateCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var clone, dir string
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a project via the interactive setup wizard",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			defaultName := defaultProjectName(args, dir)
			spec, cancelled, err := runCreateWizard(defaultName)
			if err != nil {
				// No TTY (or wizard failure): the wizard cannot prompt (§3.1).
				*exit = emitter.Failure("project.create",
					output.Errorf(output.ExitInvalidInput, "interactive terminal required: %s", err))
				return nil
			}
			if cancelled {
				*exit = emitter.Success("project.create", map[string]any{"cancelled": true})
				return nil
			}
			spec.Clone = clone
			// Resolve the host source dir: --dir (any directory) or the default
			// ~/projects/<name>. Resolved once and threaded through create.
			root, err := project.ResolveRoot(spec.Name, dir)
			if err != nil {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			spec.Root = root

			if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
				*exit = emitter.Success("project.create", map[string]any{"dry_run": true, "plan": createPlan(spec, root)})
				return nil
			}

			if err := project.EnsureCreatable(spec.Name, root); err != nil {
				*exit = emitter.Failure("project.create", mapProjectErr(err))
				return nil
			}
			vcs := git.RealRunner()
			if spec.Clone != "" {
				if err := vcs.Clone(spec.Clone, root); err != nil {
					*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "clone: %s", err))
					return nil
				}
			} else if err := vcs.Init(root); err != nil {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "git init: %s", err))
				return nil
			}
			if _, err := project.Scaffold(spec, nowRFC3339()); err != nil {
				*exit = emitter.Failure("project.create", mapProjectErr(err))
				return nil
			}
			// Seed context-optimization defaults so the project config is
			// self-describing, and install the Caveman skill (arch §9, Slice 2).
			if err := contextopt.SetStrategy(root, contextopt.DefaultStrategy); err != nil {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "seed context strategy: %s", err))
				return nil
			}
			if err := contextopt.SetCavemanLevel(root, contextopt.DefaultCavemanLevel); err != nil {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "seed caveman skill: %s", err))
				return nil
			}
			*exit = emitter.Success("project.create", map[string]any{
				"name":   spec.Name,
				"root":   root,
				"os":     spec.OS,
				"tools":  spec.AgentCLIs,
				"stacks": spec.Stacks,
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&clone, "clone", "", "seed the project from an existing git repo")
	cmd.Flags().StringVar(&dir, "dir", "", "create the project in this directory (default ~/projects/<name>)")
	return cmd
}

// createPlan is the ordered, side-effect-free action list for --dry-run (§17.1).
func createPlan(spec project.Spec, root string) []string {
	gitStep := "git init " + root
	if spec.Clone != "" {
		gitStep = "git clone " + spec.Clone + " " + root
	}
	return []string{
		"create project directory " + root,
		gitStep,
		fmt.Sprintf("write .ai-platform/Dockerfile (os=%s, stacks=%v, agent CLIs=%v)", spec.OS, spec.Stacks, spec.AgentCLIs),
		"write config.yaml, profile.yaml, project.json, .gitignore",
		"register " + spec.Name + " in config/projects.json",
	}
}

func defaultProjectName(args []string, dir string) string {
	if len(args) == 1 {
		return args[0]
	}
	// With --dir but no name, default the name to the target directory's base.
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return sanitizeName(filepath.Base(abs))
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return sanitizeName(filepath.Base(cwd))
	}
	return ""
}

// sanitizeName lowercases and replaces disallowed characters so the wizard's
// default name is usually valid; the user can still edit it.
func sanitizeName(raw string) string {
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

// runCreateWizard collects a project.Spec interactively (CLI §3.1). It returns
// cancelled=true if the user aborts, or an error if no terminal is available.
func runCreateWizard(defaultName string) (project.Spec, bool, error) {
	name := defaultName
	osKey := "debian-trixie"
	agentCLIs := []string{"opencode"}
	defaultTool := "opencode"
	stacks := []string{}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Project name").Value(&name).Validate(wizardNameValidator),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Operating system").
				Options(huh.NewOptions(supportedOSes...)...).Value(&osKey),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Agent CLIs (space to toggle)").
				Options(huh.NewOptions(supportedAgentCLIs...)...).Value(&agentCLIs).
				Validate(wizardAtLeastOne),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Default agent CLI").
				OptionsFunc(func() []huh.Option[string] { return huh.NewOptions(agentCLIs...) }, &agentCLIs).
				Value(&defaultTool),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Software stacks (space to toggle)").
				Options(huh.NewOptions(supportedStacks...)...).Value(&stacks),
		),
	)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return project.Spec{}, true, nil
		}
		return project.Spec{}, false, err
	}

	return project.Spec{
		Name:        name,
		OS:          osKey,
		Stacks:      stacks,
		AgentCLIs:   agentCLIs,
		DefaultTool: defaultTool,
	}, false, nil
}

func wizardNameValidator(value string) error { return project.ValidateName(value) }

func wizardAtLeastOne(selected []string) error {
	if len(selected) == 0 {
		return errors.New("select at least one")
	}
	return nil
}

func newProjectListCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered projects",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			entries, err := project.List()
			if err != nil {
				*exit = emitter.Failure("project.list", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("project.list", map[string]any{"projects": entries})
			return nil
		},
	}
}

func newProjectDeleteCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:               "delete [project]",
		Short:             "Delete a project (host source kept unless --purge)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("project.delete", err)
				return nil
			}
			root, exists, err := project.Path(name)
			if err != nil {
				*exit = emitter.Failure("project.delete", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if !exists {
				*exit = emitter.Failure("project.delete",
					output.Errorf(output.ExitInvalidInput, "%s: %q", project.ErrUnknownProject, name))
				return nil
			}

			if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
				*exit = emitter.Success("project.delete", map[string]any{"dry_run": true, "plan": deletePlan(name, root, purge)})
				return nil
			}

			confirmed, _ := cmd.Flags().GetBool("yes") // global --yes (§20)
			if !confirmed {
				*exit = emitter.Failure("project.delete",
					output.Errorf(output.ExitInvalidInput, "destructive: pass --yes to confirm"))
				return nil
			}
			if err := project.Delete(name, purge); err != nil {
				*exit = emitter.Failure("project.delete", mapProjectErr(err))
				return nil
			}
			*exit = emitter.Success("project.delete", map[string]any{"name": name, "purged": purge})
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the host source at ~/projects/<project>")
	return cmd
}

// deletePlan is the side-effect-free action list for --dry-run (§17.1).
func deletePlan(name, root string, purge bool) []string {
	removal := "clear " + filepath.Join(root, ".ai-platform", "run") + " (host source kept)"
	if purge {
		removal = "remove host source " + root
	}
	return []string{
		"remove " + name + " from config/projects.json",
		removal,
	}
}
