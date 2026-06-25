package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// projectsResult is the typed payload of `ai list`. The `projects` field keeps
// the JSON envelope shape (and the `project.list` command key) stable while
// Human() renders a table.
type projectsResult struct {
	Projects []project.Entry `json:"projects"`
}

// Human renders the workspaces as a table with every field: NAME, OS, AGENTS,
// STATUS, plus the microVM handle details ID, CREATED, LAST-STARTED (agents joined
// with ",", "—" for an empty cell), or a friendly hint when there are none.
func (result projectsResult) Human() string {
	if len(result.Projects) == 0 {
		return "No workspaces yet — create one with `ai create`."
	}
	rows := make([][]string, 0, len(result.Projects))
	for _, entry := range result.Projects {
		rows = append(rows, []string{
			entry.Name,
			orDash(entry.OS),
			orDash(strings.Join(entry.Agents, ",")),
			entry.Status,
			orDash(entry.ID),
			orDash(entry.Created),
			orDash(entry.LastStarted),
		})
	}
	return ui.Table([]string{"NAME", "OS", "AGENTS", "STATUS", "ID", "CREATED", "LAST-STARTED"}, rows)
}

// orDash renders an em dash for an empty cell so blank fields read clearly.
func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// Selectable options for the create wizard. All four OS templates ship as of
// S5 (debian-trixie in S1; debian-bookworm, ubuntu, alma added in S5). The user
// always picks the OS — none is applied silently (arch §25).
var (
	supportedOSes      = []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"}
	supportedStacks    = []string{"go", "node", "python", "rust", "java", "maven", "deno"}
	supportedAgentCLIs = []string{"opencode", "pi", "claude-code", "codex", "gemini"}
)

func mapProjectErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	switch {
	case errors.Is(err, project.ErrInvalidName),
		errors.Is(err, project.ErrAlreadyExists),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// newCreateCmd builds the top-level `ai create [name]` — the only create verb
// (the surface is flat; there is no `ai project create`). The emitted envelope
// `command` key stays "project.create" for stability. use sets the Use line.
func newCreateCmd(emitter *output.Emitter, exit *int, use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Create a workspace in the current directory (or attach if one exists here)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			// If the current directory is already a workspace, don't create a new
			// one — attach to it instead (bubbling up like other commands). This
			// makes `ai create` idempotent per directory.
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
			nameFlag, _ := cmd.Flags().GetString("name")
			osFlag, _ := cmd.Flags().GetString("os")
			agentsFlag, _ := cmd.Flags().GetStringSlice("agents")
			stacksFlag, _ := cmd.Flags().GetStringSlice("stacks")

			// A workspace is fully specifiable in one command via flags, so external
			// programs can create it non-interactively with --json (§1.8, §3.1).
			// On a terminal (and not --json) the wizard always runs, PRE-SEEDED with
			// any flags the user passed — flags set the UI's defaults rather than
			// bypassing it. Under --json / no TTY the workspace is built straight from
			// flags with no prompt (the programmatic contract, §1.3/§3.1).
			interactiveTTY := !emitter.JSON && term.IsTerminal(os.Stdin.Fd())

			var spec project.Spec
			if interactiveTTY {
				if err := validateProvidedCreateFlags(osFlag, agentsFlag, stacksFlag); err != nil {
					*exit = emitter.Failure("project.create", err)
					return nil
				}
				built, cancelled, err := runCreateWizard(seedSpec(nameFlag, osFlag, agentsFlag, stacksFlag, defaultName))
				if err != nil {
					// Defensive: the wizard still failed despite a TTY (§3.1).
					*exit = emitter.Failure("project.create",
						output.Errorf(output.ExitInvalidInput, "a terminal is required for the workspace wizard (or pass --name/--os/--agents/--stacks with --json)"))
					return nil
				}
				if cancelled {
					*exit = emitter.Success("project.create", map[string]any{"cancelled": true})
					return nil
				}
				spec = built
			} else {
				built, err := specFromFlags(nameFlag, osFlag, agentsFlag, stacksFlag, defaultName)
				if err != nil {
					*exit = emitter.Failure("project.create", err)
					return nil
				}
				spec = built
			}
			// The workspace is created in the current working directory. This tool
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
	// Non-interactive inputs — every value also has a flag so the command is fully
	// specifiable in one invocation (for --json / external callers). The [name]
	// positional remains a convenience equivalent to --name.
	cmd.Flags().String("name", "", "workspace name (default: the [name] argument or the current directory)")
	cmd.Flags().String("os", "", "base OS: "+strings.Join(supportedOSes, "|"))
	cmd.Flags().StringSlice("agents", nil, "agent CLIs to install (default: opencode,pi): "+strings.Join(supportedAgentCLIs, ","))
	cmd.Flags().StringSlice("stacks", nil, "software stacks to install: "+strings.Join(supportedStacks, ","))
	_ = cmd.RegisterFlagCompletionFunc("os", fixedValues(supportedOSes...))
	_ = cmd.RegisterFlagCompletionFunc("agents", fixedValues(supportedAgentCLIs...))
	_ = cmd.RegisterFlagCompletionFunc("stacks", fixedValues(supportedStacks...))
	return cmd
}

// attachWorkspace connects to an existing workspace's microVM: it starts the
// microVM (a no-op if already running) and opens an interactive login shell
// inside it. Used when `ai create` runs in a directory that is already a
// workspace. The microVM start/exec run against the real Microsandbox runtime.
func attachWorkspace(emitter *output.Emitter, exit *int, name string) {
	// Boot the microVM (slow — spinner-wrapped on a TTY via startWorkspace), then
	// exec an interactive login shell. The Exec is NOT spinner-wrapped: it takes
	// over the terminal.
	if _, err := startWorkspace(emitter, name); err != nil {
		*exit = emitter.Failure("project.create", mapWorkspaceErr(err))
		return
	}
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	// Without a terminal (e.g. --json) we can't open a shell — report the started
	// workspace instead.
	if !interactive(emitter) {
		*exit = emitter.Success("project.create", map[string]any{"project": name, "attached": false})
		return
	}
	// Hand the terminal to a real interactive login shell in the microVM (a PTY
	// via msb exec -t); the inner shell exiting is a clean end, not a failure.
	if err := manager.Shell(name); err != nil {
		*exit = emitter.Failure("project.create", mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// createPlan is the ordered, side-effect-free action list for --dry-run (§17.1).
func createPlan(spec project.Spec, root string) []string {
	return []string{
		"use current directory " + root,
		fmt.Sprintf("write .ai-platform/Dockerfile (os=%s, stacks=%s, agent CLIs=%s)", spec.OS, strings.Join(spec.Stacks, ", "), strings.Join(spec.AgentCLIs, ", ")),
		"write config.yaml, profile.yaml, project.yaml, .gitignore",
		"register " + spec.Name + " in config/projects.yaml",
	}
	// NB: file/index names above are on-disk artifacts, intentionally unchanged.
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

// runCreateWizard collects a workspace project.Spec interactively (CLI §3.1). It
// returns cancelled=true if the user aborts, or an error if no terminal is available.
func runCreateWizard(seed project.Spec) (project.Spec, bool, error) {
	// Pre-seed every field from the caller (flags become the wizard's defaults).
	name := seed.Name
	osKey := seed.OS
	agentCLIs := seed.AgentCLIs
	defaultTool := seed.DefaultTool
	stacks := seed.Stacks

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Workspace name").Value(&name).Validate(wizardNameValidator),
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
		return errors.New("select at least one agent CLI")
	}
	return nil
}

// validateProvidedCreateFlags rejects any non-empty create flag whose value is
// not a known option (a typo'd --os/--agents/--stacks → exit 2). Empty flags are
// left for defaults. Shared by the interactive (seed) and non-interactive paths.
func validateProvidedCreateFlags(osKey string, agents, stacks []string) error {
	if osKey != "" && !slices.Contains(supportedOSes, osKey) {
		return output.Errorf(output.ExitInvalidInput,
			"unknown --os %q (one of: %s)", osKey, strings.Join(supportedOSes, ", "))
	}
	for _, agent := range agents {
		if !slices.Contains(supportedAgentCLIs, agent) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --agents value %q (one of: %s)", agent, strings.Join(supportedAgentCLIs, ", "))
		}
	}
	for _, stack := range stacks {
		if !slices.Contains(supportedStacks, stack) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --stacks value %q (one of: %s)", stack, strings.Join(supportedStacks, ", "))
		}
	}
	return nil
}

// seedSpec applies defaults to the (already-validated) create flags to produce the
// wizard's pre-seeded starting point on a terminal: flags fill the defaults, the
// wizard supplies the rest (name → cwd basename, OS → debian-trixie, agents →
// opencode+pi). The user can still change anything in the wizard.
func seedSpec(name, osKey string, agents, stacks []string, defaultName string) project.Spec {
	if name == "" {
		name = defaultName
	}
	if osKey == "" {
		osKey = "debian-trixie"
	}
	if len(agents) == 0 {
		agents = []string{"opencode", "pi"}
	}
	return project.Spec{Name: name, OS: osKey, Stacks: stacks, AgentCLIs: agents, DefaultTool: agents[0]}
}

// specFromFlags builds and validates a project.Spec from the non-interactive
// create flags (the path external programs use with --json). --os is required;
// agents default to opencode+pi; stacks are optional. The default agent CLI is
// the first one listed. Unknown values map to exit 2.
func specFromFlags(name, osKey string, agents, stacks []string, defaultName string) (project.Spec, error) {
	if name == "" {
		name = defaultName
	}
	if err := project.ValidateName(name); err != nil {
		return project.Spec{}, output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if osKey == "" {
		return project.Spec{}, output.Errorf(output.ExitInvalidInput,
			"--os is required (one of: %s)", strings.Join(supportedOSes, ", "))
	}
	if err := validateProvidedCreateFlags(osKey, agents, stacks); err != nil {
		return project.Spec{}, err
	}
	if len(agents) == 0 {
		agents = []string{"opencode", "pi"}
	}
	return project.Spec{
		Name:        name,
		OS:          osKey,
		Stacks:      stacks,
		AgentCLIs:   agents,
		DefaultTool: agents[0],
	}, nil
}

// newListCmd builds the canonical top-level `ai list` (also the body of the hidden
// `ai project list` alias). It lists registered workspaces with their OS, workspace
// status, and agents (the richer project.List-backed renderer).
func newListCmd(emitter *output.Emitter, exit *int, use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "List workspaces",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			entries, err := project.List()
			if err != nil {
				*exit = emitter.Failure("project.list", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("project.list", projectsResult{Projects: entries})
			return nil
		},
	}
}

// newDeleteCmd builds the canonical top-level `ai delete [name]` (also the body of
// the hidden `ai project delete` alias): remove the whole workspace — definition,
// index entry, and microVM — keeping the host source unless --purge.
func newDeleteCmd(emitter *output.Emitter, exit *int, use string) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:               use,
		Short:             "Delete a workspace (host source kept unless --purge)",
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
				// On a terminal (and not --json) show a themed confirm dialog
				// before this destructive action; declining cancels with a success
				// envelope (exit 0). Under --json / no TTY the contract is unchanged:
				// --yes is required, and its absence is exit 2.
				if interactive(emitter) {
					ok, promptErr := promptConfirm(
						fmt.Sprintf("Delete workspace %q? This removes its microVM and platform state.", name),
						"This destroys the workspace microVM and removes the workspace from the platform. Your source directory is kept unless --purge.")
					if promptErr != nil {
						*exit = emitter.Failure("project.delete", promptErr)
						return nil
					}
					if !ok {
						*exit = emitter.Success("project.delete", map[string]any{"cancelled": true})
						return nil
					}
				} else {
					*exit = emitter.Failure("project.delete",
						output.Errorf(output.ExitInvalidInput, "destructive: pass --yes to confirm"))
					return nil
				}
			}
			// Tear down the workspace microVM first so deleting the project never
			// leaves a running/created microVM (and its run-state) orphaned (CLI
			// §3.4). Idempotent: a project that was never started is a no-op, so a
			// project with no workspace still deletes. A missing Microsandbox
			// runtime is tolerated — there is nothing running to tear down — but any
			// other teardown failure is surfaced rather than silently leaking a VM.
			manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
			if err := manager.DestroyIfPresent(name); err != nil && !errors.Is(err, workspace.ErrMsbMissing) {
				*exit = emitter.Failure("project.delete", mapWorkspaceErr(err))
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
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the host source directory")
	return cmd
}

// deletePlan is the side-effect-free action list for --dry-run (§17.1).
func deletePlan(name, root string, purge bool) []string {
	removal := "clear " + filepath.Join(root, ".ai-platform", "run") + " (host source kept)"
	if purge {
		removal = "remove host source " + root
	}
	return []string{
		"destroy workspace microVM " + workspace.Name(name) + " (if running)",
		"remove " + name + " from config/projects.yaml",
		removal,
	}
}
