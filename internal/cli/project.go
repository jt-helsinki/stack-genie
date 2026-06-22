package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// Selectable options for the create wizard. All four OS templates ship as of
// S5 (debian-trixie in S1; debian-bookworm, ubuntu, alma added in S5). The user
// always picks the OS — none is applied silently (arch §25).
var (
	supportedOSes      = []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"}
	supportedStacks    = []string{"go", "node", "python", "rust", "java", "maven", "deno"}
	supportedAgentCLIs = []string{"opencode", "pi", "claude-code", "codex", "gemini"}
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
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a project in the current directory (or attach if one exists here)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			// If the current directory is already a project, don't create a new
			// one — attach to its workspace instead (bubbling up like other
			// commands). This makes `ai project create` idempotent per directory.
			if existing, found, err := currentProjectName(); err != nil {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			} else if found {
				if dryRun {
					*exit = emitter.Success("project.create", map[string]any{"dry_run": true, "attach": existing})
					return nil
				}
				attachWorkspace(emitter, exit, existing)
				return nil
			}

			defaultName := defaultProjectName(args)
			// The wizard needs a real terminal. Check explicitly and exit 2 here,
			// rather than letting huh/bubbletea fall back to opening the controlling
			// terminal (/dev/tty) directly — which BLOCKS (hangs) when stdin is
			// redirected to a non-TTY but a controlling terminal still exists, e.g.
			// `go test` / `ai project create` run from an interactive shell (§3.1).
			if !term.IsTerminal(os.Stdin.Fd()) {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitInvalidInput,
					"interactive terminal required: `ai project create` runs a wizard — run it in a terminal"))
				return nil
			}
			spec, cancelled, err := runCreateWizard(defaultName)
			if err != nil {
				// Defensive: the wizard still failed despite a TTY (§3.1).
				*exit = emitter.Failure("project.create",
					output.Errorf(output.ExitInvalidInput, "interactive terminal required: %s", err))
				return nil
			}
			if cancelled {
				*exit = emitter.Success("project.create", map[string]any{"cancelled": true})
				return nil
			}
			// The project is created in the current working directory. This tool
			// manages only the reproducible AI dev environment (the .ai-platform/
			// definition) — it does not init or clone version control. Bring your
			// own git; existing files in the directory are left untouched.
			root, err := os.Getwd()
			if err != nil {
				*exit = emitter.Failure("project.create", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			spec.Root = root

			if dryRun {
				*exit = emitter.Success("project.create", map[string]any{"dry_run": true, "plan": createPlan(spec, root)})
				return nil
			}

			if err := project.EnsureCreatable(spec.Name, root); err != nil {
				*exit = emitter.Failure("project.create", mapProjectErr(err))
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
	return cmd
}

// attachWorkspace connects to an existing project's workspace VM: it starts the
// microVM (a no-op if already running) and opens an interactive login shell
// inside it. Used when `ai project create` runs in a directory that is already a
// project. The microVM start/exec are wired during hardware bring-up.
func attachWorkspace(emitter *output.Emitter, exit *int, name string) {
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	if _, err := manager.Start(name); err != nil {
		*exit = emitter.Failure("project.create", mapWorkspaceErr(err))
		return
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	result, err := manager.Exec(name, []string{shell, "-l"})
	if err != nil {
		*exit = emitter.Failure("project.create", mapWorkspaceErr(err))
		return
	}
	*exit = emitter.Success("project.create", result)
}

// createPlan is the ordered, side-effect-free action list for --dry-run (§17.1).
func createPlan(spec project.Spec, root string) []string {
	return []string{
		"use current directory " + root,
		fmt.Sprintf("write .ai-platform/Dockerfile (os=%s, stacks=%v, agent CLIs=%v)", spec.OS, spec.Stacks, spec.AgentCLIs),
		"write config.yaml, profile.yaml, project.yaml, .gitignore",
		"register " + spec.Name + " in config/projects.yaml",
	}
}

func defaultProjectName(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	// No name given: default to the current directory's base (the project is
	// created here).
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
	agentCLIs := []string{"opencode", "pi"} // both installed by default
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
		"remove " + name + " from config/projects.yaml",
		removal,
	}
}
