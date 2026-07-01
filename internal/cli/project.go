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
	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/sysinfo"
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
var (
	supportedOSes = []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"}
	// Python is NOT offered here — Python 3.x, uv, and Graphify (via `uv tool install`)
	// are baked into every OS base by default (see the OS Dockerfiles), and each
	// selected agent CLI registers Graphify with itself. The stack machinery still
	// supports a "python" snippet for backward compatibility with older projects.
	supportedStacks    = []string{"go", "node", "rust", "java", "maven", "deno"}
	supportedAgentCLIs = []string{"opencode", "pi", "claude-code", "codex", "gemini"}
	// supportedApps are the opt-in in-VM AI applications (apps.Keys()). Default OFF.
	supportedApps = apps.Keys()
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
	name        string
	osKey       string
	agents      []string
	stacks      []string
	apps        []string
	idleTimeout string
	cpus        int
	memory      string
	ports       []string
	location    string
	defaultName string
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
	return createFlags{
		name: name, osKey: osKey, agents: agents, stacks: stacks, apps: appsList,
		idleTimeout: idleTimeout, cpus: cpus, memory: memory, ports: ports,
		location: location, defaultName: defaultProjectName(args),
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

// validateResourcesWithinHost rejects a CPU/memory request that exceeds the host's
// actual resources (so a workspace can never be configured larger than the machine).
func validateResourcesWithinHost(cpus int, memory string) error {
	if err := config.ValidateCPUs(cpus); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if hostCPUs := sysinfo.CPUs(); cpus > hostCPUs {
		return output.Errorf(output.ExitInvalidInput, "--cpus %d exceeds the host's %d logical CPUs", cpus, hostCPUs)
	}
	if err := config.ValidateMemory(memory); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if memory != "" {
		if requested, err := config.ParseMemoryMiB(memory); err == nil {
			if hostMiB, ok := sysinfo.MemoryMiB(); ok && requested > hostMiB {
				return output.Errorf(output.ExitInvalidInput, "--memory %s exceeds the host's %d MiB of RAM", memory, hostMiB)
			}
		}
	}
	return nil
}

// cappedDefaultResources resolves an UNSET cpu/memory to the platform default and
// then caps it at the host — so a host smaller than the default (e.g. 4 GiB RAM vs the
// 8G default) never yields a workspace configured larger than the machine. Explicit
// over-host values are rejected earlier by validateResourcesWithinHost; this handles
// only the unset case, which must not error.
func cappedDefaultResources(cpus int, memory string) (int, string) {
	hostMiB, ok := sysinfo.MemoryMiB()
	return cappedResources(cpus, memory, sysinfo.CPUs(), hostMiB, ok)
}

// cappedResources is the host-agnostic core (host values injected so it is testable):
// an unset cpu/memory becomes the platform default capped at the host.
func cappedResources(cpus int, memory string, hostCPUs int, hostMiB uint64, hostMiBKnown bool) (int, string) {
	if cpus <= 0 {
		cpus = config.Default().Workspace.CPULimit
		if hostCPUs > 0 && cpus > hostCPUs {
			cpus = hostCPUs
		}
	}
	if memory == "" {
		memory = config.Default().Workspace.MemoryLimit
		if hostMiBKnown {
			if defaultMiB, err := config.ParseMemoryMiB(memory); err == nil && defaultMiB > hostMiB {
				memory = fmt.Sprintf("%dM", hostMiB)
			}
		}
	}
	return cpus, memory
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

			// Reject an EXPLICIT over-host CPU/memory request, then resolve an UNSET
			// value to the platform default capped at the host — so the persisted config
			// is never larger than the machine.
			if err := validateResourcesWithinHost(spec.CPUs, spec.Memory); err != nil {
				*exit = emitter.Failure(projectCreateCommand, err)
				return nil
			}
			spec.CPUs, spec.Memory = cappedDefaultResources(spec.CPUs, spec.Memory)

			if dryRun {
				*exit = emitter.Success(projectCreateCommand, map[string]any{"dry_run": true, "plan": createPlan(spec, root)})
				return nil
			}

			if err := project.EnsureCreatable(spec.Name, root); err != nil {
				*exit = emitter.Failure(projectCreateCommand, mapProjectErr(err))
				return nil
			}
			// Create the location if it does not exist (Scaffold also MkdirAll's, but be
			// explicit so a brand-new path is clearly the workspace root).
			if err := os.MkdirAll(root, 0o755); err != nil {
				*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "create location %s: %s", root, err))
				return nil
			}
			if _, err := project.Scaffold(spec, nowRFC3339()); err != nil {
				*exit = emitter.Failure(projectCreateCommand, mapProjectErr(err))
				return nil
			}
			// Seed context-optimization defaults so the project config is
			// self-describing, and install the Caveman skill (arch §9, Slice 2).
			if err := contextopt.SetStrategy(root, contextopt.DefaultStrategy); err != nil {
				*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "seed context strategy: %s", err))
				return nil
			}
			if err := contextopt.SetCavemanLevel(root, contextopt.DefaultCavemanLevel); err != nil {
				*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "seed caveman skill: %s", err))
				return nil
			}
			configYAML, err := os.ReadFile(config.ProjectPath(root))
			if err != nil {
				*exit = emitter.Failure(projectCreateCommand, output.Errorf(output.ExitRuntimeFailure, "read config.yaml: %s", err))
				return nil
			}
			*exit = emitter.Success(projectCreateCommand, createResult{
				Name:       spec.Name,
				Root:       root,
				OS:         spec.OS,
				Tools:      spec.AgentCLIs,
				Stacks:     spec.Stacks,
				Apps:       spec.Apps,
				ConfigYAML: string(configYAML),
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
	cmd.Flags().StringSlice("stacks", nil, "extra software stacks ("+strings.Join(supportedStacks, ",")+"); Python 3.x, uv, and Graphify are installed by default")
	cmd.Flags().StringSlice("apps", nil, "in-VM AI apps to install (default: none): "+strings.Join(supportedApps, ","))
	cmd.Flags().String("idle-timeout", "", "Microsandbox idle timeout (default: "+config.DefaultMicrosandboxIdleTimeout+", e.g. 30m, 24h)")
	cmd.Flags().Int("cpus", 0, fmt.Sprintf("workspace vCPUs (default: %d; max: host's %d)", config.Default().Workspace.CPULimit, sysinfo.CPUs()))
	cmd.Flags().String("memory", "", "workspace memory limit (default: "+config.Default().Workspace.MemoryLimit+", e.g. 2G, 4096; max: host RAM)")
	cmd.Flags().StringSlice("ports", nil, "ports to open into the workspace: PORT or HOST:GUEST (e.g. 8080,9000:3000)")
	cmd.Flags().String("location", "", "workspace directory (default: current directory; created if missing)")
	_ = cmd.RegisterFlagCompletionFunc("os", fixedValues(supportedOSes...))
	_ = cmd.RegisterFlagCompletionFunc("agents", fixedValues(supportedAgentCLIs...))
	_ = cmd.RegisterFlagCompletionFunc("stacks", fixedValues(supportedStacks...))
	_ = cmd.RegisterFlagCompletionFunc("apps", fixedValues(supportedApps...))
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
	agentCLIs := seed.AgentCLIs
	defaultTool := seed.DefaultTool
	stacks := seed.Stacks
	selectedApps := seed.Apps
	idleTimeout := seed.IdleTimeout
	location := seed.Root
	cpusText := ""
	if seed.CPUs > 0 {
		cpusText = strconv.Itoa(seed.CPUs)
	}
	memory := seed.Memory
	portsText := formatPublishPorts(seed.PublishPorts)

	form := huh.NewForm(
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
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Agent CLIs (space to toggle)").
				Options(huh.NewOptions(supportedAgentCLIs...)...).Value(&agentCLIs).
				Validate(wizardAtLeastOne),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Default agent CLI").
				OptionsFunc(func() []huh.Option[string] { return agentCLIOptions(agentCLIs) }, &agentCLIs).
				Value(&defaultTool),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Software stacks (space to toggle)").
				Description("Python 3.x, uv, and Graphify are installed by default").
				Options(huh.NewOptions(supportedStacks...)...).Value(&stacks),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("AI apps to run in the workspace (space to toggle; default none)").
				Options(appOptions()...).Value(&selectedApps),
		),
		huh.NewGroup(
			huh.NewInput().Title("Workspace vCPUs").
				Description(fmt.Sprintf("Blank uses the default (%d); host has %d", config.Default().Workspace.CPULimit, sysinfo.CPUs())).
				Value(&cpusText).Validate(wizardCPUsValidator),
			huh.NewInput().Title("Workspace memory").
				Description("e.g. 2G, 4096 (MiB); blank uses the default ("+config.Default().Workspace.MemoryLimit+")").
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
	).WithTheme(ui.HuhTheme()).WithWidth(formWidth())

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return project.Spec{}, true, nil
		}
		return project.Spec{}, false, err
	}

	defaultTool = normalizeDefaultAgentCLI(defaultTool, agentCLIs)
	cpus := 0
	if trimmed := strings.TrimSpace(cpusText); trimmed != "" {
		cpus, _ = strconv.Atoi(trimmed)
	}
	ports, _ := parsePublishPorts(splitCommaList(portsText))

	return project.Spec{
		Name:         name,
		OS:           osKey,
		Stacks:       stacks,
		AgentCLIs:    agentCLIs,
		DefaultTool:  defaultTool,
		Apps:         selectedApps,
		IdleTimeout:  idleTimeout,
		CPUs:         cpus,
		Memory:       strings.TrimSpace(memory),
		PublishPorts: ports,
		Root:         location,
	}, false, nil
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
	return validateResourcesWithinHost(cpus, "")
}

// wizardMemoryValidator validates the memory field (blank = default).
func wizardMemoryValidator(value string) error {
	return validateResourcesWithinHost(0, strings.TrimSpace(value))
}

// wizardPortsValidator validates the comma-separated ports field.
func wizardPortsValidator(value string) error {
	_, err := parsePublishPorts(splitCommaList(value))
	return err
}

// appOptions renders the app multi-select options with the human label but the
// stable key as the value (so the wizard returns keys, matching --apps).
func appOptions() []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(apps.All()))
	for _, manifest := range apps.All() {
		options = append(options, huh.NewOption(manifest.Name, manifest.Key))
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

func wizardAtLeastOne(selected []string) error {
	if len(selected) == 0 {
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
	if err := config.ValidateIdleTimeout(flags.idleTimeout); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if err := validateResourcesWithinHost(flags.cpus, flags.memory); err != nil {
		return err
	}
	if _, err := parsePublishPorts(flags.ports); err != nil {
		return err
	}
	return nil
}

// seedSpec applies defaults to the (already-validated) create flags to produce the
// wizard's pre-seeded starting point on a terminal: flags fill the defaults, the
// wizard supplies the rest (name → cwd basename, OS → debian-trixie, agents →
// opencode+pi). The user can still change anything in the wizard.
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
		agents = []string{"opencode", "pi"}
	}
	idleTimeout := flags.idleTimeout
	if idleTimeout == "" {
		idleTimeout = config.DefaultMicrosandboxIdleTimeout
	}
	ports, _ := parsePublishPorts(flags.ports)
	// Apps are opt-in: an unset --apps seeds the wizard with NOTHING selected.
	return project.Spec{
		Name:         name,
		OS:           osKey,
		Stacks:       flags.stacks,
		AgentCLIs:    agents,
		DefaultTool:  normalizeDefaultAgentCLI(agents[0], agents),
		Apps:         flags.apps,
		IdleTimeout:  idleTimeout,
		CPUs:         flags.cpus,
		Memory:       flags.memory,
		PublishPorts: ports,
	}
}

// specFromFlags builds and validates a project.Spec from the non-interactive
// create flags (the path external programs use with --json). --os is required;
// agents default to opencode+pi; stacks are optional. The default agent CLI is
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
		agents = []string{"opencode", "pi"}
	}
	idleTimeout := flags.idleTimeout
	if idleTimeout == "" {
		idleTimeout = config.DefaultMicrosandboxIdleTimeout
	}
	ports, _ := parsePublishPorts(flags.ports)
	return project.Spec{
		Name:         name,
		OS:           flags.osKey,
		Stacks:       flags.stacks,
		AgentCLIs:    agents,
		DefaultTool:  normalizeDefaultAgentCLI(agents[0], agents),
		Apps:         flags.apps,
		IdleTimeout:  idleTimeout,
		CPUs:         flags.cpus,
		Memory:       flags.memory,
		PublishPorts: ports,
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

			confirmed, _ := cmd.Flags().GetBool("yes") // global --yes (§20)
			if !confirmed {
				// On a terminal (and not --json) show a themed confirm dialog
				// before this destructive action; declining cancels with a success
				// envelope (exit 0). Under --json / no TTY the contract is unchanged:
				// --yes is required, and its absence is exit 2.
				if interactive(emitter) {
					ok, promptErr := promptConfirm(
						fmt.Sprintf("Delete workspace %q? This removes its microVM and platform state.", name),
						"Tears down the workspace microVM and removes its .ai-platform directory (config + state) and platform registration. Your OTHER files in the directory are kept unless --purge.")
					if promptErr != nil {
						*exit = emitter.Failure(projectDeleteCommand, promptErr)
						return nil
					}
					if !ok {
						*exit = emitter.Success(projectDeleteCommand, map[string]any{"cancelled": true})
						return nil
					}
				} else {
					*exit = emitter.Failure(projectDeleteCommand,
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
			if err := project.Delete(name, purge); err != nil {
				*exit = emitter.Failure(projectDeleteCommand, mapProjectErr(err))
				return nil
			}
			*exit = emitter.Success(projectDeleteCommand, map[string]any{"name": name, "purged": purge}, warnings...)
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the host source directory")
	return cmd
}

// deletePlan is the side-effect-free action list for --dry-run (§17.1).
func deletePlan(name, root string, purge bool) []string {
	removal := "remove " + filepath.Join(root, ".ai-platform") + " (your other files kept)"
	if purge {
		removal = "remove the whole directory " + root
	}
	return []string{
		"destroy workspace microVM " + workspace.Name(name) + " (if running)",
		"remove " + name + " from config/projects.yaml",
		removal,
	}
}
