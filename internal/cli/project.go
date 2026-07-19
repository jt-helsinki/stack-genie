package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/sysinfo"
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
	OS         string   `json:"os"`
	Tools      []string `json:"tools"`
	Stacks     []string `json:"stacks"`
	Apps       []string `json:"apps"`
	ConfigYAML string   `json:"-"`
}

// Human prints the exact project config.yaml written to disk so the screen output
// mirrors the persisted workspace configuration, including agent.default_tool.
func (result createResult) Human() string {
	return strings.TrimRight(result.ConfigYAML, "\n")
}

// Human renders the workspaces as a table with every field: NAME, OS, AGENTS,
// STATUS, plus the microVM handle details ID, CREATED, LAST-STARTED (agents joined
// readably, "—" for an empty cell), or a friendly hint when there are none.
func (result projectsResult) Human() string {
	if len(result.Projects) == 0 {
		return ui.Muted.Render("No workspaces yet — create one with ") + ui.Primary.Render("`ai create`") + ui.Muted.Render(".")
	}
	rows := make([][]string, 0, len(result.Projects))
	for _, entry := range result.Projects {
		rows = append(rows, []string{
			ui.Value.Render(entry.Name),
			ui.Value.Render(orDash(entry.OS)),
			ui.Value.Render(orDash(formatAgentCLIs(entry.Agents))),
			styleWorkspaceStatus(entry.Status),
			ui.Value.Render(orDash(entry.ID)),
			ui.Value.Render(orDash(entry.Created)),
			ui.Value.Render(orDash(entry.LastStarted)),
		})
	}
	return ui.Table([]string{"NAME", "OS", "AGENTS", "STATUS", "ID", "CREATED", "LAST-STARTED"}, rows)
}

// styleWorkspaceStatus colours a workspace status word semantically: active
// states (running/created/started) green, inactive (stopped) orange, an error
// state red, and anything else (incl. the em-dash placeholder) muted.
func styleWorkspaceStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "started", "created", "active":
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

// Selectable options for the create wizard. All four OS templates ship as of
// S5 (debian-trixie in S1; debian-bookworm, ubuntu, alma added in S5). The user
// always picks the OS — none is applied silently (arch §25).
// The selectable create options are defined once in internal/create (shared with the
// in-TUI wizard, which cannot import cli). Python + Node + uv are baked into
// every base by default (Graphify is now a selectable AI tool), so they are NOT stacks.
var (
	supportedOSes      = create.SupportedOSes()
	supportedStacks    = create.SupportedStacks()
	supportedAgentCLIs = create.SupportedAgentCLIs()
	supportedApps      = create.SupportedApps()
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
	osKey         string
	agents        []string
	stacks        []string
	apps          []string
	idleTimeout   string
	cpus          int
	memory        string
	ports         []string
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
	osKey, _ := cmd.Flags().GetString("os")
	agents, _ := cmd.Flags().GetStringSlice("agents")
	stacks, _ := cmd.Flags().GetStringSlice("stacks")
	appsList, _ := cmd.Flags().GetStringSlice("apps")
	idleTimeout, _ := cmd.Flags().GetString("idle-timeout")
	cpus, _ := cmd.Flags().GetInt("cpus")
	memory, _ := cmd.Flags().GetString("memory")
	ports, _ := cmd.Flags().GetStringSlice("ports")
	location, _ := cmd.Flags().GetString("location")
	graphifyModel, _ := cmd.Flags().GetString("graphify-model")
	shell, _ := cmd.Flags().GetString("shell")
	authMode, _ := cmd.Flags().GetString("auth-mode")
	tools, _ := cmd.Flags().GetStringSlice("tools")
	return createFlags{
		name: name, osKey: osKey, agents: agents, stacks: stacks, apps: appsList,
		idleTimeout: idleTimeout, cpus: cpus, memory: memory, ports: ports,
		location: location, graphifyModel: graphifyModel, shell: shell, authMode: authMode,
		tools:       tools,
		toolsSet:    cmd.Flags().Changed("tools"),
		defaultName: defaultProjectName(args),
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

// parsePublishPorts parses `--ports` entries into host↔guest mappings. Each entry is
// either "PORT" (host == guest) or "HOST:GUEST" (Docker-style host-first).
func parsePublishPorts(values []string) ([]config.PortMapping, error) {
	var ports []config.PortMapping
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		hostText, guestText, hasGuest := strings.Cut(raw, ":")
		hostPort, err := strconv.Atoi(strings.TrimSpace(hostText))
		if err != nil || hostPort < 1 || hostPort > 65535 {
			return nil, output.Errorf(output.ExitInvalidInput, "invalid port %q (expected PORT or HOST:GUEST, 1-65535)", raw)
		}
		guestPort := hostPort
		if hasGuest {
			guestPort, err = strconv.Atoi(strings.TrimSpace(guestText))
			if err != nil || guestPort < 1 || guestPort > 65535 {
				return nil, output.Errorf(output.ExitInvalidInput, "invalid guest port in %q (1-65535)", raw)
			}
		}
		ports = append(ports, config.PortMapping{Host: hostPort, Guest: guestPort})
	}
	return ports, nil
}

// formatPublishPorts renders port mappings back to the `--ports` syntax (for the
// wizard seed + the dry-run plan).
func formatPublishPorts(ports []config.PortMapping) string {
	parts := make([]string, 0, len(ports))
	for _, mapping := range ports {
		if mapping.Host == mapping.Guest {
			parts = append(parts, strconv.Itoa(mapping.Host))
		} else {
			parts = append(parts, fmt.Sprintf("%d:%d", mapping.Host, mapping.Guest))
		}
	}
	return strings.Join(parts, ",")
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
			// programs can create it non-interactively with --json (§1.8, §3.1).
			// On a terminal (and not --json) the wizard always runs, PRE-SEEDED with
			// any flags the user passed — flags set the UI's defaults rather than
			// bypassing it. Under --json / no TTY the workspace is built straight from
			// flags with no prompt (the programmatic contract, §1.3/§3.1).
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
					// Defensive: the wizard still failed despite a TTY (§3.1).
					*exit = emitter.Failure(projectCreateCommand,
						output.Errorf(output.ExitInvalidInput, "a terminal is required for the workspace wizard (or pass --name/--os/--agents/--stacks with --json)"))
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

			// Dry-run: emit the side-effect-free plan, reflecting the effective
			// (host-capped) resources — validate + cap first, but do NOT scaffold.
			if dryRun {
				if err := create.ValidateResourcesWithinHost(spec.CPUs, spec.Memory); err != nil {
					*exit = emitter.Failure(projectCreateCommand, err)
					return nil
				}
				planSpec := spec
				planSpec.CPUs, planSpec.Memory = create.CappedDefaultResources(spec.CPUs, spec.Memory)
				*exit = emitter.Success(projectCreateCommand, map[string]any{"dry_run": true, "plan": createPlan(planSpec, root)})
				return nil
			}

			// Create in-process via the shared path (identical for `ai ui`'s wizard):
			// validate + cap resources, scaffold the tracked files, seed context-opt
			// defaults, and pull the Graphify model (best-effort → warnings).
			result, warnings, err := create.Execute(spec, nowRFC3339(), createProgressReporter(emitter))
			if err != nil {
				*exit = emitter.Failure(projectCreateCommand, err)
				return nil
			}
			*exit = emitter.Success(projectCreateCommand, createResult{
				Name:       result.Name,
				Root:       result.Root,
				OS:         result.OS,
				Tools:      result.Tools,
				Stacks:     result.Stacks,
				Apps:       result.Apps,
				ConfigYAML: result.ConfigYAML,
			}, warnings...)
			return nil
		},
	}
	// Non-interactive inputs — every value also has a flag so the command is fully
	// specifiable in one invocation (for --json / external callers). The [name]
	// positional remains a convenience equivalent to --name.
	cmd.Flags().String("name", "", "workspace name (default: the [name] argument or the current directory)")
	cmd.Flags().String("os", "", "base OS: "+strings.Join(supportedOSes, "|"))
	cmd.Flags().StringSlice("agents", nil, "agent CLIs to install (default: opencode): "+strings.Join(supportedAgentCLIs, ","))
	cmd.Flags().StringSlice("stacks", nil, "extra software stacks ("+strings.Join(supportedStacks, ",")+"); Python 3.x, uv, Node 24.x and Graphify are installed by default")
	cmd.Flags().StringSlice("apps", nil, "in-VM AI apps to install (default: none): "+strings.Join(supportedApps, ","))
	cmd.Flags().String("idle-timeout", "", "Microsandbox idle timeout (default: "+config.DefaultMicrosandboxIdleTimeout+", e.g. 30m, 24h)")
	cmd.Flags().Int("cpus", 0, fmt.Sprintf("workspace vCPUs (default: %d; max: host's %d)", config.Default().Workspace.CPULimit, sysinfo.CPUs()))
	cmd.Flags().String("memory", "", "workspace memory in GB, a plain number (default: "+config.Default().Workspace.MemoryLimit+"; capped below host RAM, reserving headroom for the host + service tier)")
	cmd.Flags().StringSlice("ports", nil, "ports to open into the workspace: PORT or HOST:GUEST (e.g. 8080,9000:3000)")
	cmd.Flags().String("location", "", "workspace directory (default: current directory; created if missing)")
	cmd.Flags().String("graphify-model", "", "Ollama model Graphify uses (e.g. qwen2.5-coder:7b); chosen in the wizard from the Ollama library and pulled if absent")
	cmd.Flags().String("shell", "bash", "default interactive shell for workspace sessions: "+strings.Join(supportedShells, "|"))
	cmd.Flags().String("auth-mode", "", "per-agent auth mode for claude-code/codex/gemini as cli=mode (api-key|oauth), comma-separated (e.g. claude-code=oauth,codex=api-key); default api-key")
	cmd.Flags().StringSlice("tools", nil, "AI tools to install (default: "+strings.Join(create.DefaultAITools(), ",")+"): "+strings.Join(create.SupportedAITools(), ",")+" — pass --tools=\"\" for none")
	_ = cmd.RegisterFlagCompletionFunc("os", fixedValues(supportedOSes...))
	_ = cmd.RegisterFlagCompletionFunc("shell", fixedValues(supportedShells...))
	_ = cmd.RegisterFlagCompletionFunc("agents", fixedValues(supportedAgentCLIs...))
	_ = cmd.RegisterFlagCompletionFunc("stacks", fixedValues(supportedStacks...))
	_ = cmd.RegisterFlagCompletionFunc("apps", fixedValues(supportedApps...))
	_ = cmd.RegisterFlagCompletionFunc("tools", fixedValues(supportedAITools...))
	_ = cmd.MarkFlagDirname("location")
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
		*exit = emitter.Failure(projectCreateCommand, mapWorkspaceErr(err))
		return
	}
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	// Without a terminal (e.g. --json) we can't open a shell — report the started
	// workspace instead.
	if !interactive(emitter) {
		*exit = emitter.Success(projectCreateCommand, map[string]any{"project": name, "attached": false})
		return
	}
	// Hand the terminal to a real interactive login shell in the microVM (a PTY
	// via msb exec -t); the inner shell exiting is a clean end, not a failure.
	if err := manager.Shell(name); err != nil {
		*exit = emitter.Failure(projectCreateCommand, mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// createPlan is the ordered, side-effect-free action list for --dry-run (§17.1).
// createProgressReporter returns a create.Execute progress callback that renders the
// flow on a TTY: each phase as a "→ step" line, and the Graphify-model download as a live
// ui.ProgressBar (the same meter as `ai models pull`). Returns nil under --json / no-TTY,
// so scripted/JSON runs stay quiet.
func createProgressReporter(emitter *output.Emitter) func(create.Progress) {
	if !ui.Enabled(emitter) {
		return nil
	}
	var bar *ui.ProgressBar
	var barStep string
	return func(progress create.Progress) {
		if progress.Total > 0 { // a model-download frame → live bar for this step
			if bar == nil || barStep != progress.Step {
				if bar != nil {
					bar.Finish(nil)
				}
				bar = ui.NewProgressBar(emitter.Err, progress.Step)
				barStep = progress.Step
			}
			bar.Update(progress.Completed, progress.Total, "")
			return
		}
		if bar != nil { // a new non-download step ends any active bar
			bar.Finish(nil)
			bar, barStep = nil, ""
		}
		_, _ = fmt.Fprintln(emitter.Err, ui.Muted.Render("→ "+progress.Step))
	}
}

func createPlan(spec project.Spec, root string) []string {
	return []string{
		"use location " + root + " (created if missing)",
		fmt.Sprintf("write .ai-platform/Dockerfile (os=%s, stacks=%s, agent CLIs=%s)", spec.OS, strings.Join(spec.Stacks, ", "), strings.Join(spec.AgentCLIs, ", ")),
		fmt.Sprintf("set resources: cpus=%s, memory=%s", orDefault(spec.CPUs), orDefaultStr(spec.Memory)),
		fmt.Sprintf("open ports: %s", orNone(formatPublishPorts(spec.PublishPorts))),
		fmt.Sprintf("write Microsandbox idle timeout: %s", spec.IdleTimeout),
		fmt.Sprintf("install in-VM apps: %s", orNone(strings.Join(spec.Apps, ", "))),
		"write config.yaml, profile.yaml, project.yaml, .gitignore",
		"register " + spec.Name + " in config/projects.yaml",
	}
	// NB: file/index names above are on-disk artifacts, intentionally unchanged.
}

// orNone renders "(none)" for an empty list cell so the plan reads clearly.
func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

// orDefault renders "(default)" for an unset (0) integer config value.
func orDefault(value int) string {
	if value <= 0 {
		return "(default)"
	}
	return strconv.Itoa(value)
}

// orDefaultStr renders "(default)" for an unset string config value.
func orDefaultStr(value string) string {
	if value == "" {
		return "(default)"
	}
	return value
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
	shell := seed.Shell
	if shell == "" {
		shell = "bash"
	}
	// The agent CLIs and the in-VM AI apps share ONE combined multi-select screen;
	// the selection is split back into the two sets after the form runs
	// (create.SplitAgentsAndApps), so selection order never matters.
	agentAppSelection := append(append([]string{}, seed.AgentCLIs...), seed.Apps...)
	defaultTool := seed.DefaultTool
	// Per-agent auth mode (only for the OAuth-capable CLIs, and only when selected). Seeded
	// from the flag; each defaults to api-key (gateway-routed, full guardrails).
	authClaude := seededAuthMode(seed.AuthModes, "claude-code")
	authCodex := seededAuthMode(seed.AuthModes, "codex")
	authGemini := seededAuthMode(seed.AuthModes, "gemini")
	stacks := seed.Stacks
	idleTimeout := seed.IdleTimeout
	location := seed.Root
	cpusText := ""
	if seed.CPUs > 0 {
		cpusText = strconv.Itoa(seed.CPUs)
	}
	memory := seed.Memory
	portsText := formatPublishPorts(seed.PublishPorts)

	// Graphify's LLM backend is an Ollama model chosen from the installable library
	// (like the Models page), pulled if absent (see pullGraphifyModelIfAbsent). The
	// picker is a model select + a tag select; blank/"(none)" leaves Graphify without
	// a configured model. The library is cache-first — if it can't be loaded (offline,
	// no cache) the group is omitted and only a --graphify-model flag can set it.
	// AI tools are ONE multi-select (like the agent CLIs), seeded from the spec's per-tool
	// bools. The graphify-model step is shown only when graphify is among the selection.
	toolsSelection := aiToolsFromSpec(seed)
	graphifyName, graphifyTag := splitModelRef(seed.GraphifyModel)
	// Cache-only: the wizard must never stall on a cold-cache network scrape. If no
	// cache exists yet (no prior `ai setup` / `ai models`), the group is omitted and
	// --graphify-model is the only way to set it (it is still pulled if absent).
	graphifyLibrary, _ := ollama.LoadLibrary()

	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Workspace name").Value(&name).Validate(wizardNameValidator),
			huh.NewInput().Title("Location (workspace directory)").
				Description("Created if missing; cannot be inside an existing workspace").
				Value(&location).
				SuggestionsFunc(func() []string { return dirSuggestions(location) }, &location).
				Validate(validateWizardLocation),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Operating system").
				Options(huh.NewOptions(supportedOSes...)...).Value(&osKey),
			huh.NewSelect[string]().Title("Default interactive shell").
				Options(huh.NewOptions(supportedShells...)...).Value(&shell),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Agent CLIs & AI apps (space to toggle)").
				Description("Agent CLIs first, then the opt-in in-VM AI apps — pick at least one agent CLI").
				Options(agentAndAppOptions()...).Value(&agentAppSelection).
				Validate(wizardAtLeastOneAgent),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Default agent CLI").
				OptionsFunc(func() []huh.Option[string] {
					selectedAgents, _ := create.SplitAgentsAndApps(agentAppSelection)
					return agentCLIOptions(selectedAgents)
				}, &agentAppSelection).
				Value(&defaultTool),
		),
		authModeGroup("claude-code", "Claude Code", &authClaude, &agentAppSelection),
		authModeGroup("codex", "Codex", &authCodex, &agentAppSelection),
		authModeGroup("gemini", "Gemini", &authGemini, &agentAppSelection),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Software stacks (space to toggle)").
				Description("Python 3.x, uv, Node 24.x and Graphify are installed by default").
				Options(huh.NewOptions(supportedStacks...)...).Value(&stacks),
		),
		huh.NewGroup(
			huh.NewInput().Title("Workspace vCPUs").
				Description(fmt.Sprintf("Blank uses the default (%d); host has %d", config.Default().Workspace.CPULimit, sysinfo.CPUs())).
				Value(&cpusText).Validate(wizardCPUsValidator),
			huh.NewInput().Title("Workspace memory (GB)").
				Description(fmt.Sprintf("A plain number in GB; blank uses the default (%s); usable max %d GB (host %d GB, minus headroom for the host + service tier)", config.Default().Workspace.MemoryLimit, create.UsableHostMemoryGB(), create.HostMemoryGB())).
				Value(&memory).Validate(wizardMemoryValidator),
			huh.NewInput().Title("Ports to open (comma-separated)").
				Description("PORT or HOST:GUEST, e.g. 8080,9000:3000").
				Value(&portsText).Validate(wizardPortsValidator),
		),
		huh.NewGroup(
			huh.NewInput().Title("Microsandbox idle timeout").
				Description("How long msb may leave the workspace idle before stopping it (default 24h)").
				Value(&idleTimeout).
				Validate(func(value string) error { return config.ValidateIdleTimeout(value) }),
		),
	}
	// AI tools — ONE multi-select (mirrors the agent-CLI list), instead of a screen each.
	groups = append(groups, huh.NewGroup(
		huh.NewMultiSelect[string]().Title("AI tools (space to toggle)").
			Description("Per-project code/context tooling installed at workspace start; caveman, graphify and code-review-graph are the defaults.").
			Options(aiToolOptions()...).Value(&toolsSelection),
	))
	// Graphify model — shown ONLY when graphify is selected above and a library is cached.
	if len(graphifyLibrary) > 0 {
		groups = append(groups, huh.NewGroup(
			huh.NewSelect[string]().Title("Graphify model (Ollama; optional)").
				Description("Routed through the gateway; pulled if not installed. Type to filter.").
				Options(graphifyModelOptions(graphifyLibrary)...).Value(&graphifyName),
			huh.NewSelect[string]().Title("Graphify model tag").
				OptionsFunc(func() []huh.Option[string] { return graphifyTagOptions(graphifyLibrary, graphifyName) }, &graphifyName).
				Value(&graphifyTag),
		).WithHideFunc(func() bool { return !slices.Contains(toolsSelection, create.AIToolGraphify) }))
	}

	form := huh.NewForm(groups...).WithTheme(ui.HuhTheme()).WithWidth(formWidth())

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return project.Spec{}, true, nil
		}
		return project.Spec{}, false, err
	}

	agentCLIs, selectedApps := create.SplitAgentsAndApps(agentAppSelection)
	defaultTool = normalizeDefaultAgentCLI(defaultTool, agentCLIs)
	cpus := 0
	if trimmed := strings.TrimSpace(cpusText); trimmed != "" {
		cpus, _ = strconv.Atoi(trimmed)
	}
	ports, _ := parsePublishPorts(splitCommaList(portsText))

	authModes := collectAuthModes(agentCLIs, map[string]string{
		"claude-code": authClaude, "codex": authCodex, "gemini": authGemini,
	})

	caveman, graphify, codeReviewGraph, codebaseMemory := create.SplitAITools(toolsSelection)
	// Only carry a graphify model when graphify is actually selected.
	graphifyModel := ""
	if graphify {
		graphifyModel = joinModelRef(graphifyName, graphifyTag)
	}
	return project.Spec{
		Name:                   name,
		OS:                     osKey,
		Shell:                  shell,
		Stacks:                 stacks,
		AgentCLIs:              agentCLIs,
		DefaultTool:            defaultTool,
		AuthModes:              authModes,
		Apps:                   selectedApps,
		IdleTimeout:            idleTimeout,
		CPUs:                   cpus,
		Memory:                 strings.TrimSpace(memory),
		PublishPorts:           ports,
		Root:                   location,
		GraphifyModel:          graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
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
	return selection
}

// aiToolOptions is the labeled option list for the AI-tools multi-select. Values are the
// raw tool keys (create.SupportedAITools) so the selection splits cleanly.
func aiToolOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("caveman — output-compression toolkit (skills/agents/commands + CLI-native plugins/hooks)", create.AIToolCaveman),
		huh.NewOption("graphify — knowledge-graph skill, baked into the image + registered with each agent CLI", create.AIToolGraphify),
		huh.NewOption("code-review-graph — code-review knowledge graph + MCP server (local, no API key)", create.AIToolCodeReviewGraph),
		huh.NewOption("codebase-memory-mcp — codebase-memory MCP server + optional 3D graph UI (local, no API key)", create.AIToolCodebaseMemory),
	}
}

// seededAuthMode returns the pre-seeded auth mode for a CLI, defaulting to "api-key".
func seededAuthMode(modes map[string]string, cli string) string {
	if mode := modes[cli]; mode == "oauth" {
		return "oauth"
	}
	return "api-key"
}

// authModeGroup builds the per-agent auth-mode select, shown ONLY when that OAuth-capable
// CLI is among the selected agents (a HideFunc keyed off the live combined agents+apps
// selection — app keys never collide with CLI names). The choice is api-key (gateway,
// full guardrails) vs oauth (direct to provider, bypasses the firewall). label is the
// human CLI name for the title.
func authModeGroup(cli, label string, value *string, agentAppSelection *[]string) *huh.Group {
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
	).WithHideFunc(func() bool { return !slices.Contains(*agentAppSelection, cli) })
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

// splitModelRef splits an Ollama reference "name:tag" into its name and tag at the
// LAST colon; a bare name yields an empty tag. A blank ref yields two empties.
func splitModelRef(ref string) (name, tag string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	if index := strings.LastIndex(ref, ":"); index >= 0 {
		return ref[:index], ref[index+1:]
	}
	return ref, ""
}

// joinModelRef reassembles a "name:tag" reference from the wizard's two selects. A
// blank name (the "(none)" option) yields "" — Graphify gets no model. A blank tag
// yields the bare name (Ollama resolves it to :latest).
func joinModelRef(name, tag string) string {
	name = strings.TrimSpace(name)
	tag = strings.TrimSpace(tag)
	if name == "" {
		return ""
	}
	if tag == "" {
		return name
	}
	return name + ":" + tag
}

// graphifyModelOptions builds the Graphify model select: a leading "(none)" (empty
// value) followed by every library model name, so the choice is optional.
func graphifyModelOptions(library []ollama.LibraryModel) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(library)+1)
	options = append(options, huh.NewOption("(none)", ""))
	for _, model := range library {
		options = append(options, huh.NewOption(model.Name, model.Name))
	}
	return options
}

// graphifyTagOptions builds the tag select for the currently-selected Graphify
// model. With no model selected it offers only "(none)"; otherwise it lists the
// model's scraped tags (falling back to "latest" when the tag table is empty).
func graphifyTagOptions(library []ollama.LibraryModel, modelName string) []huh.Option[string] {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return []huh.Option[string]{huh.NewOption("(none)", "")}
	}
	for _, model := range library {
		if model.Name != modelName {
			continue
		}
		tags := model.TagNames()
		if len(tags) == 0 {
			return []huh.Option[string]{huh.NewOption("latest", "latest")}
		}
		options := make([]huh.Option[string], 0, len(tags))
		for _, tag := range tags {
			options = append(options, huh.NewOption(tag, tag))
		}
		return options
	}
	return []huh.Option[string]{huh.NewOption("latest", "latest")}
}

// splitCommaList splits a comma-separated input into trimmed, non-empty items.
func splitCommaList(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
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

// wizardCPUsValidator validates the vCPU field (blank = default).
func wizardCPUsValidator(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	cpus, err := strconv.Atoi(trimmed)
	if err != nil {
		return fmt.Errorf("enter a whole number of vCPUs")
	}
	return create.ValidateResourcesWithinHost(cpus, "")
}

// wizardMemoryValidator validates the memory field (blank = default).
func wizardMemoryValidator(value string) error {
	return create.ValidateResourcesWithinHost(0, strings.TrimSpace(value))
}

// wizardPortsValidator validates the comma-separated ports field.
func wizardPortsValidator(value string) error {
	_, err := parsePublishPorts(splitCommaList(value))
	return err
}

// agentAndAppOptions renders the combined "Agent CLIs & AI apps" multi-select: the
// agent CLIs first (label == value), then the opt-in in-VM apps labeled
// "<Name> (app)" with the stable app key as the value (so the wizard returns keys,
// matching --apps).
func agentAndAppOptions() []huh.Option[string] {
	options := huh.NewOptions(supportedAgentCLIs...)
	for _, manifest := range apps.All() {
		options = append(options, huh.NewOption(manifest.Name+" (app)", manifest.Key))
	}
	return options
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

// wizardAtLeastOneAgent validates the combined agents+apps multi-select: at least one
// AGENT CLI must be checked (apps alone do not satisfy it — they are opt-in extras).
func wizardAtLeastOneAgent(selected []string) error {
	agentCLIs, _ := create.SplitAgentsAndApps(selected)
	if len(agentCLIs) == 0 {
		return errors.New("select at least one agent CLI")
	}
	return nil
}

// validateProvidedCreateFlags rejects any create flag whose value is not a known
// option or is out of range (a typo'd --os/--agents/--stacks, an over-host
// --cpus/--memory, a malformed --ports → exit 2). Empty flags are left for defaults.
// Shared by the interactive (seed) and non-interactive paths.
func validateProvidedCreateFlags(flags createFlags) error {
	if flags.osKey != "" && !slices.Contains(supportedOSes, flags.osKey) {
		return output.Errorf(output.ExitInvalidInput,
			"unknown --os %q (one of: %s)", flags.osKey, strings.Join(supportedOSes, ", "))
	}
	for _, agent := range flags.agents {
		if !slices.Contains(supportedAgentCLIs, agent) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --agents value %q (one of: %s)", agent, strings.Join(supportedAgentCLIs, ", "))
		}
	}
	for _, stack := range flags.stacks {
		if !slices.Contains(supportedStacks, stack) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --stacks value %q (one of: %s)", stack, strings.Join(supportedStacks, ", "))
		}
	}
	for _, app := range flags.apps {
		if !slices.Contains(supportedApps, app) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --apps value %q (one of: %s)", app, strings.Join(supportedApps, ", "))
		}
	}
	for _, tool := range flags.tools {
		if !slices.Contains(supportedAITools, tool) {
			return output.Errorf(output.ExitInvalidInput,
				"unknown --tools value %q (one of: %s)", tool, strings.Join(supportedAITools, ", "))
		}
	}
	if err := config.ValidateIdleTimeout(flags.idleTimeout); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if err := config.ValidateShell(flags.shell); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if err := create.ValidateResourcesWithinHost(flags.cpus, flags.memory); err != nil {
		return err
	}
	if _, err := parsePublishPorts(flags.ports); err != nil {
		return err
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
// that is ALSO among the selected agents; each value must be api-key|oauth. A FORCED-oauth
// CLI (copilot) accepts only "oauth" — it has no api-key/gateway mode, so "copilot=api-key"
// is rejected (Scaffold records it as "oauth" automatically regardless). An empty flag
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
			// Forced-oauth: it can ONLY be oauth (no api-key/gateway mode exists).
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
// wizard supplies the rest (name → cwd basename, OS → debian-trixie, agents →
// opencode). The user can still change anything in the wizard.
func seedSpec(flags createFlags) project.Spec {
	name := flags.name
	if name == "" {
		name = flags.defaultName
	}
	osKey := flags.osKey
	if osKey == "" {
		osKey = "debian-trixie"
	}
	agents := flags.agents
	if len(agents) == 0 {
		agents = []string{"opencode"}
	}
	idleTimeout := flags.idleTimeout
	if idleTimeout == "" {
		idleTimeout = config.DefaultMicrosandboxIdleTimeout
	}
	ports, _ := parsePublishPorts(flags.ports)
	// Auth modes were validated by validateProvidedCreateFlags (called before seeding), so
	// a parse error here is impossible — ignore it and seed the wizard's per-agent selects.
	authModes, _ := parseAuthModes(flags.authMode, agents)
	// Apps are opt-in: an unset --apps seeds the wizard with NOTHING selected.
	caveman, graphify, codeReviewGraph, codebaseMemory := create.SplitAITools(effectiveAITools(flags))
	return project.Spec{
		Name:                   name,
		OS:                     osKey,
		Shell:                  flags.shell,
		Stacks:                 flags.stacks,
		AgentCLIs:              agents,
		DefaultTool:            normalizeDefaultAgentCLI(agents[0], agents),
		AuthModes:              authModes,
		Apps:                   flags.apps,
		IdleTimeout:            idleTimeout,
		CPUs:                   flags.cpus,
		Memory:                 flags.memory,
		PublishPorts:           ports,
		GraphifyModel:          flags.graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
	}
}

// specFromFlags builds and validates a project.Spec from the non-interactive
// create flags (the path external programs use with --json). --os is required;
// agents default to opencode; stacks are optional. The default agent CLI is
// the first one listed. Unknown / out-of-range values map to exit 2.
func specFromFlags(flags createFlags) (project.Spec, error) {
	name := flags.name
	if name == "" {
		name = flags.defaultName
	}
	if err := project.ValidateName(name); err != nil {
		return project.Spec{}, output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if flags.osKey == "" {
		return project.Spec{}, output.Errorf(output.ExitInvalidInput,
			"--os is required (one of: %s)", strings.Join(supportedOSes, ", "))
	}
	if err := validateProvidedCreateFlags(flags); err != nil {
		return project.Spec{}, err
	}
	agents := flags.agents
	if len(agents) == 0 {
		agents = []string{"opencode"}
	}
	idleTimeout := flags.idleTimeout
	if idleTimeout == "" {
		idleTimeout = config.DefaultMicrosandboxIdleTimeout
	}
	ports, _ := parsePublishPorts(flags.ports)
	authModes, err := parseAuthModes(flags.authMode, agents)
	if err != nil {
		return project.Spec{}, err
	}
	caveman, graphify, codeReviewGraph, codebaseMemory := create.SplitAITools(effectiveAITools(flags))
	return project.Spec{
		Name:                   name,
		OS:                     flags.osKey,
		Shell:                  flags.shell,
		Stacks:                 flags.stacks,
		AgentCLIs:              agents,
		DefaultTool:            normalizeDefaultAgentCLI(agents[0], agents),
		AuthModes:              authModes,
		Apps:                   flags.apps,
		IdleTimeout:            idleTimeout,
		CPUs:                   flags.cpus,
		Memory:                 flags.memory,
		PublishPorts:           ports,
		GraphifyModel:          flags.graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
	}, nil
}

// newListCmd builds the canonical top-level `ai list` (the flat surface — there is
// no `ai project list` group). It lists registered workspaces with their OS,
// workspace status, and agents (the richer project.List-backed renderer).
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
// there is no `ai project delete` group): remove the whole workspace — definition,
// index entry, and microVM — keeping the host source unless --purge.
func newDeleteCmd(emitter *output.Emitter, exit *int, use string) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:               use,
		Aliases:           []string{"destroy"},
		Short:             "Delete a workspace: tear down its microVM + remove its .ai-platform state (your other files kept unless --purge)",
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
			confirmed, _ := cmd.Flags().GetBool("yes") // global --yes (§20)
			if !confirmed {
				// On a terminal (and not --json) show themed confirm dialogs before this
				// destructive action; declining the first cancels with a success envelope
				// (exit 0). Under --json / no TTY the contract is unchanged: --yes is
				// required (absence is exit 2) and --purge / --keep-agent-config drive the
				// choices directly.
				if !interactive(emitter) {
					*exit = emitter.Failure(projectDeleteCommand,
						output.Errorf(output.ExitInvalidInput, "destructive: pass --yes to confirm"))
					return nil
				}
				ok, promptErr := promptConfirm(
					fmt.Sprintf("Delete workspace %q? This removes its microVM and platform state.", name),
					"Tears down the workspace microVM and removes its .ai-platform directory (config + state) and platform registration.")
				if promptErr != nil {
					*exit = emitter.Failure(projectDeleteCommand, promptErr)
					return nil
				}
				if !ok {
					*exit = emitter.Success(projectDeleteCommand, map[string]any{"cancelled": true})
					return nil
				}
				// Purge — the whole project directory (all the user's files), not just
				// the platform state. Seeded by --purge.
				purge, promptErr = promptConfirmDefault(
					"Purge — also delete the ENTIRE project directory (all your files in it)?",
					"Decline to keep your other files and remove only the platform's .ai-platform state.",
					purge)
				if promptErr != nil {
					*exit = emitter.Failure(projectDeleteCommand, promptErr)
					return nil
				}
				// The per-CLI agent config folders + venv (only meaningful when NOT
				// purging — purge removes the whole directory anyway). Seeded by
				// --keep-agent-config.
				if !purge {
					removeAgentDirs, promptErr = promptConfirmDefault(
						"Also delete the agent config folders (.opencode, .claude, .codex, .gemini, .openclaw, .hermes, .venv-msb)?",
						"If kept, their symlinked skills/agents/prompts are converted to real files first (the platform's shared copy is being removed).",
						removeAgentDirs)
					if promptErr != nil {
						*exit = emitter.Failure(projectDeleteCommand, promptErr)
						return nil
					}
				}
			}
			// Tear down the workspace microVM first so deleting the project never
			// leaves a running/created microVM (and its run-state) orphaned (CLI
			// §3.4). Idempotent: a project that was never started is a no-op, so a
			// project with no workspace still deletes. A missing Microsandbox
			// runtime is tolerated — there is nothing running to tear down — but any
			// other teardown failure is surfaced rather than silently leaking a VM.
			manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
			var warnings []string
			if err := manager.DestroyIfPresent(name); err != nil && !errors.Is(err, workspace.ErrMsbMissing) {
				// The microVM could not be torn down (e.g. an msb error or an odd
				// VM state). The user asked to delete the workspace, so removing
				// the platform state is still the right outcome — DON'T abort and
				// leave .ai-platform behind. Warn so any orphaned microVM can be
				// cleaned up manually.
				warnings = append(warnings, fmt.Sprintf(
					"workspace microVM teardown failed (%s); removed platform state anyway — if a microVM lingers, run: msb remove -f %s",
					err, workspace.Name(name)))
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
	cmd.Flags().Bool("keep-agent-config", false, "keep the per-CLI agent config folders (.opencode/.claude/.codex/.gemini/.openclaw/.hermes/.venv-msb); their symlinked content is materialized")
	return cmd
}

// deletePlan is the side-effect-free action list for --dry-run (§17.1).
func deletePlan(name, root string, purge bool) []string {
	removal := "remove " + filepath.Join(root, ".ai-platform") + " (your other files kept)"
	if purge {
		removal = "remove the whole directory " + root
	}
	plan := []string{
		"destroy workspace microVM " + workspace.Name(name) + " (if running)",
		"remove " + name + " from config/projects.yaml",
		removal,
	}
	if !purge {
		plan = append(plan, "remove the agent config folders (.opencode/.claude/.codex/.gemini/.openclaw/.hermes/.venv-msb) unless kept with --keep-agent-config")
	}
	return plan
}
