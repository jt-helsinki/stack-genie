package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

// projectsResult is the typed payload of `ai list`. The `projects` field keeps
// the JSON envelope shape (and the `project.list` command key) stable while
// Human() renders a table.
type projectsResult struct {
	Projects []project.Entry `json:"projects"`
}

type createResult struct {
	Name       string   `json:"name"`
	Root       string   `json:"root"`
	Tools      []string `json:"tools"`
	ConfigYAML string   `json:"-"`
}

// Human prints the exact project config.yaml written to disk so the screen output
// mirrors the persisted workspace configuration, including agent.default_tool.
func (result createResult) Human() string {
	return strings.TrimRight(result.ConfigYAML, "\n")
}

// Human renders the workspaces as a table: NAME, AGENTS, STATUS, plus the live
// nono session count ("—" for an empty cell), or a friendly hint when there are none.
func (result projectsResult) Human() string {
	if len(result.Projects) == 0 {
		return ui.Muted.Render("No workspaces yet — create one with ") + ui.Primary.Render("`ai create`") + ui.Muted.Render(".")
	}
	rows := make([][]string, 0, len(result.Projects))
	for _, entry := range result.Projects {
		rows = append(rows, []string{
			ui.Value.Render(entry.Name),
			ui.Value.Render(orDash(formatAgentCLIs(entry.Agents))),
			styleWorkspaceStatus(entry.Status),
		})
	}
	return ui.Table([]string{"NAME", "AGENTS", "STATUS"}, rows)
}

// styleWorkspaceStatus colours a workspace status word semantically: active
// states (running/started) green, inactive (stopped) orange, an error
// state red, and anything else (incl. the em-dash placeholder) muted.
func styleWorkspaceStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "started", "active":
		return ui.Success.Render(status)
	case "stopped", "paused":
		return ui.Warn.Render(status)
	case "failed", "error", "unavailable":
		return ui.Failure.Render(status)
	case "", "—":
		return ui.Muted.Render(orDash(status))
	default:
		return ui.Value.Render(status)
	}
}

// orDash renders an em dash for an empty cell so blank fields read clearly.
func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func formatAgentCLIs(agents []string) string {
	return strings.Join(agents, ", ")
}

// Selectable options for the create wizard. Defined once in internal/create
// (shared with the in-TUI wizard, which cannot import cli).
var (
	supportedAgentCLIs = create.SupportedAgentCLIs()
	supportedShells    = create.SupportedShells()
	supportedAITools   = create.SupportedAITools()
)

const (
	projectCreateCommand = "project.create"
	projectDeleteCommand = "project.delete"
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

// createFlags bundles every `ai create` input (each has a flag so the command is
// fully specifiable in one invocation for --json / external callers). defaultName is
// the fallback workspace name (the [name] arg or the cwd basename).
type createFlags struct {
	name          string
	agents        []string
	location      string
	graphifyModel string
	shell         string
	authMode      string
	tools         []string
	toolsSet      bool
	defaultName   string
}

// readCreateFlags reads every create flag (and the optional [name] positional).
func readCreateFlags(cmd *cobra.Command, args []string) createFlags {
	name, _ := cmd.Flags().GetString("name")
	agents, _ := cmd.Flags().GetStringSlice("agents")
	location, _ := cmd.Flags().GetString("location")
	graphifyModel, _ := cmd.Flags().GetString("graphify-model")
	shell, _ := cmd.Flags().GetString("shell")
	authMode, _ := cmd.Flags().GetString("auth-mode")
	tools, _ := cmd.Flags().GetStringSlice("tools")
	return createFlags{
		name: name, agents: agents,
		location: location, graphifyModel: graphifyModel, shell: shell, authMode: authMode,
		tools:       tools,
		toolsSet:    cmd.Flags().Changed("tools"),
		defaultName: defaultProjectName(args, location),
	}
}

// resolveLocation turns a location flag into an absolute path (expanding a leading
// ~/). An empty location means the current working directory.
func resolveLocation(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return os.Getwd()
	}
	expanded := input
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[2:])
		}
	}
	return filepath.Abs(expanded)
}

// dirSuggestions lists directories matching the typed path prefix, for the create
// wizard's location autocompletion.
func dirSuggestions(path string) []string {
	expanded := path
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[2:])
		}
	}
	dir, partial := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	suggestions := make([]string, 0, 32)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(entry.Name(), partial) {
			continue
		}
		suggestions = append(suggestions, filepath.Join(dir, entry.Name()))
		if len(suggestions) >= 32 {
			break
		}
	}
	return suggestions
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
			flags := readCreateFlags(cmd, args)

			// Resolve the target location (--location, default cwd; created if missing).
			location, err := resolveLocation(flags.location)
			if err != nil {
				*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}

			// With no explicit --location, if the cwd is already a workspace, attach to
			// it instead of creating another (idempotent per directory). An explicit
			// --location is always a create target (validated below; never auto-attaches).
			if flags.location == "" {
				if existing, found, err := currentProjectName(); err != nil {
					*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "%s", err))
					return nil
				} else if found {
					if dryRun {
						*exit = emitter.Success(projectCreateCommand, map[string]any{"dry_run": true, "attach": existing})
						return nil
					}
					attachWorkspace(emitter, exit, existing)
					return nil
				}
			}

			// A workspace is fully specifiable in one command via flags, so external
			// programs can create it non-interactively with --json. On a terminal (and
			// not --json) the wizard always runs, PRE-SEEDED with any flags the user
			// passed. Under --json / no TTY the workspace is built straight from flags
			// with no prompt.
			interactiveTTY := !emitter.JSON && term.IsTerminal(os.Stdin.Fd())

			var spec project.Spec
			if interactiveTTY {
				if err := validateProvidedCreateFlags(flags); err != nil {
					*exit = emitter.Failure(projectCreateCommand, err)
					return nil
				}
				seed := seedSpec(flags)
				seed.Root = location
				built, cancelled, err := runCreateWizard(seed)
				if err != nil {
					*exit = emitter.Failure(projectCreateCommand,
						output.Errorf(output.ExitInvalidInput, "a terminal is required for the workspace wizard (or pass --name/--agents with --json)"))
					return nil
				}
				if cancelled {
					*exit = emitter.Success(projectCreateCommand, map[string]any{"cancelled": true})
					return nil
				}
				spec = built
			} else {
				built, err := specFromFlags(flags)
				if err != nil {
					*exit = emitter.Failure(projectCreateCommand, err)
					return nil
				}
				spec = built
				spec.Root = location
			}

			// The wizard may have changed the location; re-resolve to an absolute path.
			// This tool manages only the .ai-platform/ definition — it does not init or
			// clone version control. Existing files in the directory are left untouched.
			root, err := resolveLocation(spec.Root)
			if err != nil {
				*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			spec.Root = root

			if dryRun {
				*exit = emitter.Success(projectCreateCommand, map[string]any{"dry_run": true, "plan": createPlan(spec, root)})
				return nil
			}

			// Create in-process via the shared path (identical for `ai ui`'s wizard):
			// scaffold the tracked files + seed context-opt defaults. There is no image
			// to build and nothing to download — the nono profile + per-CLI gateway
			// configs are generated at `ai agent`/`ai start` time, not here.
			result, warnings, err := create.Execute(spec, nowRFC3339(), createProgressReporter(emitter))
			if err != nil {
				*exit = emitter.Failure(projectCreateCommand, err)
				return nil
			}
			*exit = emitter.Success(projectCreateCommand, createResult{
				Name:       result.Name,
				Root:       result.Root,
				Tools:      result.Tools,
				ConfigYAML: result.ConfigYAML,
			}, warnings...)
			return nil
		},
	}
	// Non-interactive inputs — every value also has a flag so the command is fully
	// specifiable in one invocation (for --json / external callers). The [name]
	// positional remains a convenience equivalent to --name.
	cmd.Flags().String("name", "", "workspace name (default: the [name] argument or the current directory)")
	cmd.Flags().StringSlice("agents", nil, "agent CLIs to install (default: opencode): "+strings.Join(supportedAgentCLIs, ","))
	cmd.Flags().String("location", "", "workspace directory (default: current directory; created if missing)")
	cmd.Flags().String("graphify-model", "", "name of a model omlx is ALREADY serving (manage models from its own admin panel — `ai services console omlx`); routed through the gateway as omlx/<name>, no download")
	cmd.Flags().String("shell", "bash", "default interactive shell for `ai shell`: "+strings.Join(supportedShells, "|"))
	cmd.Flags().String("auth-mode", "", "per-agent auth mode for claude-code/codex/gemini as cli=mode (api-key|oauth), comma-separated (e.g. claude-code=oauth,codex=api-key); default api-key")
	cmd.Flags().StringSlice("tools", nil, "AI tools to install (default: none): "+strings.Join(create.SupportedAITools(), ","))
	_ = cmd.RegisterFlagCompletionFunc("shell", fixedValues(supportedShells...))
	_ = cmd.RegisterFlagCompletionFunc("agents", fixedValues(supportedAgentCLIs...))
	_ = cmd.RegisterFlagCompletionFunc("tools", fixedValues(supportedAITools...))
	_ = cmd.MarkFlagDirname("location")
	return cmd
}

// attachWorkspace opens an interactive sandboxed shell in an existing
// workspace's project directory. Used when `ai create` runs in a directory
// that is already a workspace. Unlike the Microsandbox era there is nothing to
// "boot" first — nono sandboxes the shell process directly, per invocation.
func attachWorkspace(emitter *output.Emitter, exit *int, name string) {
	manager := workspace.RealManager(nowRFC3339)
	if !interactive(emitter) {
		*exit = emitter.Success(projectCreateCommand, map[string]any{"project": name, "attached": false})
		return
	}
	if err := manager.Shell(name); err != nil {
		*exit = emitter.Failure(projectCreateCommand, mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// createProgressReporter returns a create.Execute progress callback that renders the
// flow on a TTY: each phase as a "→ step" line. Returns nil under --json / no-TTY,
// so scripted/JSON runs stay quiet.
func createProgressReporter(emitter *output.Emitter) func(create.Progress) {
	if !ui.Enabled(emitter) {
		return nil
	}
	return func(progress create.Progress) {
		_, _ = fmt.Fprintln(emitter.Err, ui.Muted.Render("→ "+progress.Step))
	}
}

// createPlan is the side-effect-free action list for --dry-run.
func createPlan(spec project.Spec, root string) []string {
	return []string{
		"use location " + root + " (created if missing)",
		fmt.Sprintf("install agent CLIs: %s", strings.Join(spec.AgentCLIs, ", ")),
		"write config.yaml, project.yaml, .gitignore",
		"register " + spec.Name + " in config/projects.yaml",
	}
}

// defaultProjectName defaults the workspace name to the WORKSPACE's own directory: an
// explicit --location's basename when given, else the current directory's base (the
// project is created here when --location is unset).
func defaultProjectName(args []string, location string) string {
	if len(args) == 1 {
		return args[0]
	}
	if name := create.DefaultDirName(location); name != "" {
		return name
	}
	if cwd, err := os.Getwd(); err == nil {
		return create.DefaultDirName(cwd)
	}
	return ""
}

// runCreateWizard collects a workspace project.Spec interactively. It returns
// cancelled=true if the user aborts, or an error if no terminal is available.
// graphifyModelNone is the picker's "leave Graphify unconfigured" option.
const graphifyModelNone = "(none — leave Graphify unconfigured)"

// omlxLiveModelNamesFn is the injectable seam for the Graphify-model picker's
// options (queries omlx's live GET /v1/models directly — the CLI runs on the
// SAME host as omlx, so no gateway round trip is needed). Degrades gracefully to
// an empty slice, never an error, when omlx isn't installed/running or is
// currently serving nothing.
var omlxLiveModelNamesFn = func() []string {
	models, err := omlx.ListModels()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.ID)
	}
	slices.Sort(names)
	return names
}

// graphifyModelOptions builds the Graphify-model picker's option list: the "none"
// sentinel always first, then every model omlx currently reports. A pre-seeded
// value (e.g. from --graphify-model) that omlx does not currently report is
// APPENDED rather than dropped, so the wizard still shows and lets the user
// confirm/edit whatever was passed in, even though it won't resolve until omlx
// actually serves it.
func graphifyModelOptions(seeded string) []string {
	options := append([]string{graphifyModelNone}, omlxLiveModelNamesFn()...)
	if seeded != "" && !slices.Contains(options, seeded) {
		options = append(options, seeded)
	}
	return options
}

func runCreateWizard(seed project.Spec) (project.Spec, bool, error) {
	// Pre-seed every field from the caller (flags become the wizard's defaults).
	name := seed.Name
	originalName, originalLocation := seed.Name, seed.Root
	shell := seed.Shell
	if shell == "" {
		shell = "bash"
	}
	agentSelection := append([]string{}, seed.AgentCLIs...)
	defaultTool := seed.DefaultTool
	authClaude := seededAuthMode(seed.AuthModes, "claude-code")
	authCodex := seededAuthMode(seed.AuthModes, "codex")
	authGemini := seededAuthMode(seed.AuthModes, "gemini")
	location := seed.Root

	toolsSelection := aiToolsFromSpec(seed)
	graphifyModelSelection := seed.GraphifyModel
	if graphifyModelSelection == "" {
		graphifyModelSelection = graphifyModelNone
	}

	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Workspace name").Value(&name).Validate(wizardNameValidator),
			huh.NewInput().Title("Location (workspace directory)").
				Description("Created if missing; cannot be inside an existing workspace").
				Value(&location).
				SuggestionsFunc(func() []string { return dirSuggestions(location) }, &location).
				Validate(validateWizardLocation),
			huh.NewSelect[string]().Title("Default interactive shell").
				Options(huh.NewOptions(supportedShells...)...).Value(&shell),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Agent CLIs (space to toggle)").
				Description("Installed directly on this host, wired to the LiteLLM gateway — pick at least one").
				Options(huh.NewOptions(supportedAgentCLIs...)...).Value(&agentSelection).
				Validate(wizardAtLeastOneAgent),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Default agent CLI").
				OptionsFunc(func() []huh.Option[string] { return agentCLIOptions(agentSelection) }, &agentSelection).
				Value(&defaultTool),
		),
		authModeGroup("claude-code", "Claude Code", &authClaude, &agentSelection),
		authModeGroup("codex", "Codex", &authCodex, &agentSelection),
		authModeGroup("gemini", "Gemini", &authGemini, &agentSelection),
	}
	// AI tools — ONE multi-select (mirrors the agent-CLI list), instead of a screen each.
	groups = append(groups, huh.NewGroup(
		huh.NewMultiSelect[string]().Title("AI tools (space to toggle)").
			Description("Per-project code/context tooling installed on this host — none selected by default.").
			Options(aiToolOptions()...).Value(&toolsSelection),
	))
	// Graphify model — a PICKER over omlx's live model list (no free-text entry, no
	// download), shown ONLY when graphify is selected above.
	groups = append(groups, huh.NewGroup(
		huh.NewSelect[string]().Title("Graphify model").
			Description("A model omlx is ALREADY serving (manage the list itself from its own admin panel — `ai services console omlx`); routed through the gateway as omlx/<name>. No download happens here.").
			Options(huh.NewOptions(graphifyModelOptions(seed.GraphifyModel)...)...).Value(&graphifyModelSelection),
	).WithHideFunc(func() bool { return !slices.Contains(toolsSelection, create.AIToolGraphify) }))

	form := huh.NewForm(groups...).WithTheme(ui.HuhTheme()).WithWidth(formWidth())

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return project.Spec{}, true, nil
		}
		return project.Spec{}, false, err
	}
	// The name field is typed before location in this form, so it can't react live to
	// a location the user picks afterward — re-derive it here instead: if the user
	// left name at its pre-seeded default while changing location, follow location's
	// new directory rather than the stale seed.
	if name == originalName && location != originalLocation {
		name = defaultProjectName(nil, location)
	}

	defaultTool = normalizeDefaultAgentCLI(defaultTool, agentSelection)
	authModes := collectAuthModes(agentSelection, map[string]string{
		"claude-code": authClaude, "codex": authCodex, "gemini": authGemini,
	})

	caveman, graphify, codeReviewGraph, codebaseMemory, openViking, tokenSave, agentMemory, ponytail, agentSkills, serena := create.SplitAITools(toolsSelection)
	// Only carry a graphify model when graphify is actually selected.
	graphifyModel := ""
	if graphify && graphifyModelSelection != graphifyModelNone {
		graphifyModel = graphifyModelSelection
	}
	return project.Spec{
		Name:                   name,
		Shell:                  shell,
		AgentCLIs:              agentSelection,
		DefaultTool:            defaultTool,
		AuthModes:              authModes,
		Root:                   location,
		GraphifyModel:          graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
		OpenVikingEnabled:      openViking,
		TokenSaveEnabled:       tokenSave,
		AgentMemoryEnabled:     agentMemory,
		PonytailEnabled:        ponytail,
		AgentSkillsEnabled:     agentSkills,
		SerenaEnabled:          serena,
	}, false, nil
}

// aiToolsFromSpec builds the wizard's initial AI-tools selection from a seeded spec's
// per-tool bool flags, in SupportedAITools display order.
func aiToolsFromSpec(seed project.Spec) []string {
	var selection []string
	if seed.CavemanEnabled {
		selection = append(selection, create.AIToolCaveman)
	}
	if seed.GraphifyEnabled {
		selection = append(selection, create.AIToolGraphify)
	}
	if seed.CodeReviewGraphEnabled {
		selection = append(selection, create.AIToolCodeReviewGraph)
	}
	if seed.CodebaseMemoryEnabled {
		selection = append(selection, create.AIToolCodebaseMemory)
	}
	if seed.OpenVikingEnabled {
		selection = append(selection, create.AIToolOpenViking)
	}
	if seed.TokenSaveEnabled {
		selection = append(selection, create.AIToolTokenSave)
	}
	if seed.AgentMemoryEnabled {
		selection = append(selection, create.AIToolAgentMemory)
	}
	if seed.PonytailEnabled {
		selection = append(selection, create.AIToolPonytail)
	}
	if seed.AgentSkillsEnabled {
		selection = append(selection, create.AIToolAgentSkills)
	}
	if seed.SerenaEnabled {
		selection = append(selection, create.AIToolSerena)
	}
	return selection
}

// aiToolOptions is the labeled option list for the AI-tools multi-select. Values are the
// raw tool keys (create.SupportedAITools) so the selection splits cleanly. Each label is
// "<tool> — <description>" (create.AIToolDescription), so the picker doubles as
// documentation — the same convention `ai tools list` reuses.
func aiToolOptions() []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(create.SupportedAITools()))
	for _, tool := range create.SupportedAITools() {
		options = append(options, huh.NewOption(tool+" — "+create.AIToolDescription(tool), tool))
	}
	return options
}

// seededAuthMode returns the pre-seeded auth mode for a CLI, defaulting to "api-key".
func seededAuthMode(modes map[string]string, cli string) string {
	if mode := modes[cli]; mode == "oauth" {
		return "oauth"
	}
	return "api-key"
}

// authModeGroup builds the per-agent auth-mode select, shown ONLY when that OAuth-capable
// CLI is among the selected agents. The choice is api-key (gateway, full guardrails) vs
// oauth (direct to provider, bypasses the firewall). label is the human CLI name.
func authModeGroup(cli, label string, value *string, agentSelection *[]string) *huh.Group {
	options := []huh.Option[string]{
		huh.NewOption("API keys (routed through the gateway — keeps the tool firewall & secret masking)", "api-key"),
		huh.NewOption("Plan / OAuth login (direct to the provider — bypasses the firewall)", "oauth"),
	}
	return huh.NewGroup(
		huh.NewSelect[string]().
			Title(label + " authentication").
			Description("OAuth/plan mode talks DIRECTLY to the provider, bypassing the gateway — the tool firewall, secret masking, and content-level egress audit do NOT apply.").
			Options(options...).
			Value(value),
	).WithHideFunc(func() bool { return !slices.Contains(*agentSelection, cli) })
}

// collectAuthModes assembles the per-agent auth-mode map from the wizard vars, keeping
// only the OAuth-capable CLIs that were actually selected. Returns nil when none apply.
func collectAuthModes(agentCLIs []string, chosen map[string]string) map[string]string {
	modes := map[string]string{}
	for _, cli := range config.OAuthCapableCLIs() {
		if !slices.Contains(agentCLIs, cli) {
			continue
		}
		mode := chosen[cli]
		if mode == "" {
			mode = "api-key"
		}
		modes[cli] = mode
	}
	if len(modes) == 0 {
		return nil
	}
	return modes
}

// validateWizardLocation validates the wizard's location field (blank = cwd).
func validateWizardLocation(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	resolved, err := resolveLocation(value)
	if err != nil {
		return err
	}
	return project.ValidateNewLocation(resolved)
}

func agentCLIOptions(agentCLIs []string) []huh.Option[string] {
	return huh.NewOptions(agentCLIs...)
}

func normalizeDefaultAgentCLI(defaultTool string, agentCLIs []string) string {
	if len(agentCLIs) == 0 {
		return ""
	}
	if slices.Contains(agentCLIs, defaultTool) {
		return defaultTool
	}
	return agentCLIs[0]
}

func wizardNameValidator(value string) error { return project.ValidateName(value) }

// wizardAtLeastOneAgent validates the agent multi-select: at least one must be checked.
func wizardAtLeastOneAgent(selected []string) error {
	if len(selected) == 0 {
		return errors.New("select at least one agent CLI")
	}
	return nil
}

// validateProvidedCreateFlags rejects any create flag whose value is not a known
// option (a typo'd --agents/--shell/--tools → exit 2). Empty flags are left for
// defaults. Shared by the interactive (seed) and non-interactive paths.
func validateProvidedCreateFlags(flags createFlags) error {
	for _, agent := range flags.agents {
		if !slices.Contains(supportedAgentCLIs, agent) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --agents value %q (one of: %s)", agent, strings.Join(supportedAgentCLIs, ", "))
		}
	}
	for _, tool := range flags.tools {
		if !slices.Contains(supportedAITools, tool) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --tools value %q (one of: %s)", tool, strings.Join(supportedAITools, ", "))
		}
	}
	if err := config.ValidateShell(flags.shell); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if _, err := parseAuthModes(flags.authMode, effectiveAgents(flags)); err != nil {
		return err
	}
	return nil
}

// effectiveAITools resolves the create flags' AI tools, applying the default set
// (create.DefaultAITools) when --tools was not provided. An explicit --tools="" (provided
// but empty) selects NO tools.
func effectiveAITools(flags createFlags) []string {
	if !flags.toolsSet {
		return create.DefaultAITools()
	}
	return flags.tools
}

// effectiveAgents resolves the create flags' agent CLIs, applying the opencode default
// when --agents is unset (matching seedSpec/specFromFlags). Used to validate --auth-mode
// keys against the actually-selected agents.
func effectiveAgents(flags createFlags) []string {
	if len(flags.agents) == 0 {
		return []string{"opencode"}
	}
	return flags.agents
}

// parseAuthModes parses the --auth-mode flag — a comma list of cli=mode (e.g.
// "claude-code=oauth,codex=api-key") — into a map. Each key must be an OAuth-eligible CLI
// that is ALSO among the selected agents; each value must be api-key|oauth. An empty flag
// yields a nil map. All errors are exit 2 (invalid input).
func parseAuthModes(raw string, agents []string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	modes := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		cli, mode, ok := strings.Cut(entry, "=")
		cli = strings.TrimSpace(cli)
		mode = strings.TrimSpace(mode)
		if !ok || cli == "" || mode == "" {
			return nil, output.Errorf(output.ExitInvalidInput,
				"invalid --auth-mode %q (expected cli=mode, e.g. claude-code=oauth)", entry)
		}
		if slices.Contains(config.ForcedOAuthCLIs(), cli) {
			if mode != "oauth" {
				return nil, output.Errorf(output.ExitInvalidInput,
					"--auth-mode: %q only supports oauth/plan login (it authenticates natively and cannot route through the gateway)", cli)
			}
			if !slices.Contains(agents, cli) {
				return nil, output.Errorf(output.ExitInvalidInput,
					"--auth-mode: %q is not among the selected agents (%s)", cli, strings.Join(agents, ", "))
			}
			modes[cli] = mode
			continue
		}
		if !slices.Contains(config.OAuthCapableCLIs(), cli) {
			return nil, output.Errorf(output.ExitInvalidInput,
				"--auth-mode: %q has no subscription login — only %s can use oauth", cli, strings.Join(config.OAuthCapableCLIs(), ", "))
		}
		if !slices.Contains(agents, cli) {
			return nil, output.Errorf(output.ExitInvalidInput,
				"--auth-mode: %q is not among the selected agents (%s)", cli, strings.Join(agents, ", "))
		}
		if err := config.ValidateAuthMode(mode); err != nil {
			return nil, output.Errorf(output.ExitInvalidInput, "--auth-mode %q: %s", entry, err)
		}
		modes[cli] = mode
	}
	if len(modes) == 0 {
		return nil, nil
	}
	return modes, nil
}

// seedSpec applies defaults to the (already-validated) create flags to produce the
// wizard's pre-seeded starting point on a terminal: flags fill the defaults, the
// wizard supplies the rest (name → cwd basename, agents → opencode). The user can
// still change anything in the wizard.
func seedSpec(flags createFlags) project.Spec {
	name := flags.name
	if name == "" {
		name = flags.defaultName
	}
	agents := flags.agents
	if len(agents) == 0 {
		agents = []string{"opencode"}
	}
	// Auth modes were validated by validateProvidedCreateFlags (called before seeding), so
	// a parse error here is impossible — ignore it and seed the wizard's per-agent selects.
	authModes, _ := parseAuthModes(flags.authMode, agents)
	caveman, graphify, codeReviewGraph, codebaseMemory, openViking, tokenSave, agentMemory, ponytail, agentSkills, serena := create.SplitAITools(effectiveAITools(flags))
	return project.Spec{
		Name:                   name,
		Shell:                  flags.shell,
		AgentCLIs:              agents,
		DefaultTool:            normalizeDefaultAgentCLI(agents[0], agents),
		AuthModes:              authModes,
		GraphifyModel:          flags.graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
		OpenVikingEnabled:      openViking,
		TokenSaveEnabled:       tokenSave,
		AgentMemoryEnabled:     agentMemory,
		PonytailEnabled:        ponytail,
		AgentSkillsEnabled:     agentSkills,
		SerenaEnabled:          serena,
	}
}

// specFromFlags builds and validates a project.Spec from the non-interactive
// create flags (the path external programs use with --json). Agents default to
// opencode. The default agent CLI is the first one listed. Unknown values map
// to exit 2.
func specFromFlags(flags createFlags) (project.Spec, error) {
	name := flags.name
	if name == "" {
		name = flags.defaultName
	}
	if err := project.ValidateName(name); err != nil {
		return project.Spec{}, output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if err := validateProvidedCreateFlags(flags); err != nil {
		return project.Spec{}, err
	}
	agents := flags.agents
	if len(agents) == 0 {
		agents = []string{"opencode"}
	}
	authModes, err := parseAuthModes(flags.authMode, agents)
	if err != nil {
		return project.Spec{}, err
	}
	caveman, graphify, codeReviewGraph, codebaseMemory, openViking, tokenSave, agentMemory, ponytail, agentSkills, serena := create.SplitAITools(effectiveAITools(flags))
	return project.Spec{
		Name:                   name,
		Shell:                  flags.shell,
		AgentCLIs:              agents,
		DefaultTool:            normalizeDefaultAgentCLI(agents[0], agents),
		AuthModes:              authModes,
		GraphifyModel:          flags.graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
		OpenVikingEnabled:      openViking,
		TokenSaveEnabled:       tokenSave,
		AgentMemoryEnabled:     agentMemory,
		PonytailEnabled:        ponytail,
		AgentSkillsEnabled:     agentSkills,
		SerenaEnabled:          serena,
	}, nil
}

// newListCmd builds the canonical top-level `ai list` (the flat surface — there is
// no `ai project list` group). It lists registered workspaces with their agents
// and workspace status.
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

// newDeleteCmd builds the canonical top-level `ai delete [name]` (the flat surface —
// there is no `ai project delete` group): stop any live sessions, then remove the
// whole workspace — definition + index entry — keeping the host source unless --purge.
func newDeleteCmd(emitter *output.Emitter, exit *int, use string) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:               use,
		Aliases:           []string{"destroy"},
		Short:             "Delete a workspace: stop any live sessions + remove its .ai-platform state (your other files kept unless --purge)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure(projectDeleteCommand, err)
				return nil
			}
			root, exists, err := project.Path(name)
			if err != nil {
				*exit = emitter.Failure(projectDeleteCommand, output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if !exists {
				*exit = emitter.Failure(projectDeleteCommand,
					output.Errorf(output.ExitInvalidInput, "%s: %q", project.ErrUnknownProject, name))
				return nil
			}

			if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
				*exit = emitter.Success(projectDeleteCommand, map[string]any{"dry_run": true, "plan": deletePlan(name, root, purge)})
				return nil
			}

			keepAgentConfig, _ := cmd.Flags().GetBool("keep-agent-config")
			removeAgentDirs := !keepAgentConfig
			confirmed, _ := cmd.Flags().GetBool("yes") // global --yes
			if !confirmed {
				if !interactive(emitter) {
					*exit = emitter.Failure(projectDeleteCommand,
						output.Errorf(output.ExitInvalidInput, "destructive: pass --yes to confirm"))
					return nil
				}
				ok, promptErr := promptConfirm(
					fmt.Sprintf("Delete workspace %q? This stops any live sessions and removes its platform state.", name),
					"Stops any live nono sessions and removes its .ai-platform directory (config + state) and platform registration.")
				if promptErr != nil {
					*exit = emitter.Failure(projectDeleteCommand, promptErr)
					return nil
				}
				if !ok {
					*exit = emitter.Success(projectDeleteCommand, map[string]any{"cancelled": true})
					return nil
				}
				purge, promptErr = promptConfirmDefault(
					"Purge — also delete the ENTIRE project directory (all your files in it)?",
					"Decline to keep your other files and remove only the platform's .ai-platform state.",
					purge)
				if promptErr != nil {
					*exit = emitter.Failure(projectDeleteCommand, promptErr)
					return nil
				}
				if !purge {
					removeAgentDirs, promptErr = promptConfirmDefault(
						"Also delete the agent config folders (.opencode, .claude, .codex, .gemini, .hermes)?",
						"If kept, their symlinked skills/agents/prompts are converted to real files first (the platform's shared copy is being removed).",
						removeAgentDirs)
					if promptErr != nil {
						*exit = emitter.Failure(projectDeleteCommand, promptErr)
						return nil
					}
				}
			}
			// Stop any live sessions first so deleting the project never leaves one
			// running, then revoke + remove its scoped key from the OS keystore.
			// Best-effort — the user asked to delete the workspace, so removing the
			// platform state proceeds even if a teardown step fails.
			manager := workspace.RealManager(nowRFC3339)
			var warnings []string
			if err := manager.StopAll(name); err != nil {
				warnings = append(warnings, fmt.Sprintf("stopping live sessions failed (%s) — check `nono ps` manually", err))
			}
			if err := manager.DeleteCredentials(name); err != nil {
				warnings = append(warnings, fmt.Sprintf("removing the gateway credential failed (%s)", err))
			}
			if err := project.Delete(name, purge, removeAgentDirs); err != nil {
				*exit = emitter.Failure(projectDeleteCommand, mapProjectErr(err))
				return nil
			}
			*exit = emitter.Success(projectDeleteCommand, map[string]any{"name": name, "purged": purge}, warnings...)
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the whole project directory (all your files, not just platform state)")
	cmd.Flags().Bool("keep-agent-config", false, "keep the per-CLI agent config folders (.opencode/.claude/.codex/.gemini/.hermes); their symlinked content is materialized")
	return cmd
}

// deletePlan is the side-effect-free action list for --dry-run.
func deletePlan(name, root string, purge bool) []string {
	removal := "remove " + filepath.Join(root, ".ai-platform") + " (your other files kept)"
	if purge {
		removal = "remove the whole directory " + root
	}
	plan := []string{
		"stop any live nono sessions for " + name,
		"remove its gateway credential from the OS keystore",
		"remove " + name + " from config/projects.yaml",
		removal,
	}
	if !purge {
		plan = append(plan, "remove the agent config folders (.opencode/.claude/.codex/.gemini/.hermes) unless kept with --keep-agent-config")
	}
	return plan
}
