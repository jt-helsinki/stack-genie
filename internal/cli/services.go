package cli

import (
	"fmt"
	goruntime "runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/spf13/cobra"
)

// servicesResult is the typed payload of the services subcommands. It carries
// the per-service statuses and renders them — including each service's
// host-reachable address and admin-console URL — for non-JSON output, while the
// `services` field keeps the JSON envelope shape stable.
type servicesResult struct {
	Services []setup.ServiceStatus `json:"services"`
}

// Human renders the services as a table: SERVICE, MODE, STATE, then the
// host-reachable ADDRESS and admin-console URL where present (blank cells where
// the service publishes nothing to the host) — so the user can find each
// service's address + UI.
func (result servicesResult) Human() string {
	rows := make([][]string, 0, len(result.Services))
	for _, service := range result.Services {
		rows = append(rows, []string{
			service.Name, service.Mode, service.State, service.Address, service.Console,
		})
	}
	return ui.Table([]string{"SERVICE", "MODE", "STATE", "ADDRESS", "CONSOLE"}, rows)
}

// newServicesCmd builds `ai services` and its subcommands (CLI §10.2).
// `start`/`stop`/`restart` control the platform-owned containers (Ollama,
// Presidio, LiteLLM, Headroom).
func newServicesCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Inspect and manage host services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newServicesStatusCmd(em, exit),
		newServicesControlCmd("start", em, exit),
		newServicesControlCmd("stop", em, exit),
		newServicesControlCmd("restart", em, exit),
		newServicesToggleCmd("enable", em, exit),
		newServicesToggleCmd("disable", em, exit),
		newServicesConsoleCmd(em, exit),
	)
	return cmd
}

// newServicesToggleCmd builds `ai services enable|disable <service>`: it toggles
// an OPTIONAL service (open-webui, odysseus) in the persisted set and brings it
// up/down. Core services are always on, so only the optional ones are valid.
// With no argument on a terminal it shows a single-select of the optional
// services; under --json / no TTY a name is required (exit 2).
func newServicesToggleCmd(action string, em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               action + " <service>",
		Short:             action + " an optional service (" + strings.Join(setup.OptionalServiceNames(), ", ") + ")",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeOptionalServiceNames,
		RunE: func(_ *cobra.Command, args []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			service := ""
			if len(args) == 1 {
				service = args[0]
			}
			if service == "" {
				if !interactive(em) {
					*exit = em.Failure("services."+action, output.Errorf(output.ExitInvalidInput,
						"%s needs an optional service name (one of %v)", action, setup.OptionalServiceNames()))
					return nil
				}
				selected, err := selectOptionalService(action)
				if err != nil {
					*exit = em.Failure("services."+action, err)
					return nil
				}
				service = selected
			}
			var statuses []setup.ServiceStatus
			var err error
			work := func() error {
				var workErr error
				statuses, workErr = setup.ControlService(deps, action, service)
				return workErr
			}
			if ui.Enabled(em) {
				err = ui.RunWithSpinner(em.Err, action+" "+service, work)
			} else {
				err = work()
			}
			if err != nil {
				*exit = em.Failure("services."+action, err)
				return nil
			}
			*exit = em.Success("services."+action, servicesResult{Services: statuses})
			return nil
		},
	}
}

// selectOptionalService prompts for one optional service to enable/disable (a
// single-select, since the action takes exactly one name).
func selectOptionalService(action string) (string, error) {
	names := setup.OptionalServiceNames()
	options := make([]huh.Option[string], 0, len(names))
	for _, name := range names {
		label := name
		if hint := setup.OptionalServiceLabel(name); hint != "" {
			label = fmt.Sprintf("%s — %s", name, hint)
		}
		options = append(options, huh.NewOption(label, name))
	}
	return promptChoice("Which optional service to "+action+"?", "", options, "")
}

// newServicesControlCmd builds `ai services start|stop|restart [service]`.
//   - a named logical service (ollama, presidio, litellm, headroom, proxy,
//     open-webui, odysseus, dns) — or the literal "all" — targets it directly,
//     no prompt; a service may own several containers (odysseus → chromadb /
//     searxng / ntfy), which are acted on as a unit;
//   - with no argument on a terminal, it shows a CHECKBOX list of every service
//     and its current state and acts on the one(s) the user selects (minimal
//     typing);
//   - with no argument and no terminal (automation/--json), it acts on every
//     service, preserving the scriptable "do everything" default.
func newServicesControlCmd(action string, em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               action + " [service]",
		Short:             action + " host services (named, \"all\", or pick from a checkbox on a terminal)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeServiceNames,
		RunE: func(_ *cobra.Command, args []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			// An explicit service (or "all") acts directly; bare invocation prompts on
			// a terminal and falls back to "all" for non-interactive use.
			targets := args
			if len(args) == 0 {
				if interactive(em) {
					selected, err := selectServices(deps, action)
					if err != nil {
						*exit = em.Failure("services."+action, err)
						return nil
					}
					targets = selected
				} else {
					targets = []string{""} // "" == all platform services
				}
			}
			var statuses []setup.ServiceStatus
			var err error
			if ui.Enabled(em) {
				err = ui.RunWithSpinner(em.Err, action+" services", func() error {
					var workErr error
					statuses, workErr = controlServices(deps, action, targets)
					return workErr
				})
			} else {
				statuses, err = controlServices(deps, action, targets)
			}
			if err != nil {
				*exit = em.Failure("services."+action, err)
				return nil
			}
			*exit = em.Success("services."+action, servicesResult{Services: statuses})
			return nil
		},
	}
}

// allServicesSentinel is the checkbox option value that targets every managed
// service at once. It matches the empty/"all" target that setup.ControlService
// already expands to every container service.
const allServicesSentinel = "all"

// controllableServices drops the workspace runtime (Mode == "runtime", i.e.
// microsandbox) from the status list so only the platform-owned containers — the
// services start/stop/restart can actually act on — are offered in the checkbox.
// The full list (runtime included) is still shown by `ai services status`.
func controllableServices(statuses []setup.ServiceStatus) []setup.ServiceStatus {
	controllable := make([]setup.ServiceStatus, 0, len(statuses))
	for _, service := range statuses {
		if service.Mode == "runtime" {
			continue
		}
		controllable = append(controllable, service)
	}
	return controllable
}

// expandServiceSelection collapses the checkbox result to ["all"] when the
// "All services" sentinel is present (it takes precedence over any individually
// selected names); otherwise it returns the selection unchanged.
func expandServiceSelection(selected []string) []string {
	for _, name := range selected {
		if name == allServicesSentinel {
			return []string{allServicesSentinel}
		}
	}
	return selected
}

// selectServices fetches the current per-service status and presents a checkbox
// list — an "All services" option first, then each controllable service labeled
// with its live state — for the user to choose which services to act on. The
// workspace runtime (microsandbox) is excluded since it isn't a control target.
// Returns an exit-2 error if nothing is selected.
func selectServices(deps setup.Deps, action string) ([]string, error) {
	statuses, err := setup.ServicesStatus(deps)
	if err != nil {
		return nil, err
	}
	controllable := controllableServices(statuses)
	options := make([]huh.Option[string], 0, len(controllable)+1)
	options = append(options, huh.NewOption("All services", allServicesSentinel))
	for _, service := range controllable {
		options = append(options, huh.NewOption(fmt.Sprintf("%s (%s)", service.Name, service.State), service.Name))
	}
	selected, err := promptMultiChoice(
		"Which services to "+action+"?",
		"space to toggle, enter to confirm",
		options,
	)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, output.Errorf(output.ExitInvalidInput, "no services selected")
	}
	return expandServiceSelection(selected), nil
}

// controlServices applies action to each target service in turn, returning the
// final per-service status. An empty target name means every platform service.
func controlServices(deps setup.Deps, action string, targets []string) ([]setup.ServiceStatus, error) {
	var statuses []setup.ServiceStatus
	for _, name := range targets {
		applied, err := setup.ControlService(deps, action, name)
		if err != nil {
			return nil, err
		}
		statuses = applied
	}
	return statuses, nil
}

// newServicesConsoleCmd builds `ai services console [service]`: open a service's
// admin console in the browser (the LiteLLM UI). With no argument it lists the
// services that have a console. --print shows the URL instead of opening it
// (also the default with --json, for headless use).
func newServicesConsoleCmd(em *output.Emitter, exit *int) *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:               "console [service]",
		Short:             "Open a host service's admin console in the browser",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeConsoleServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				*exit = em.Success("services.console", map[string]any{"consoles": console.WithConsoles()})
				return nil
			}
			name := args[0]
			if !console.Known(name) {
				*exit = em.Failure("services.console",
					output.Errorf(output.ExitInvalidInput, "unknown service %q", name))
				return nil
			}
			url, hasConsole := console.URL(name)
			if !hasConsole {
				*exit = em.Failure("services.console",
					output.Errorf(output.ExitInvalidInput, "%s has no admin console", name))
				return nil
			}
			jsonMode, _ := cmd.Flags().GetBool("json")
			if printOnly || jsonMode {
				*exit = em.Success("services.console", map[string]any{"service": name, "url": url})
				return nil
			}
			if err := console.RealOpener(goruntime.GOOS).Open(url); err != nil {
				*exit = em.Failure("services.console",
					output.Errorf(output.ExitRuntimeFailure, "open %s: %s", url, err))
				return nil
			}
			*exit = em.Success("services.console", map[string]any{"service": name, "url": url, "opened": true})
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the console URL instead of opening it")
	return cmd
}

func newServicesStatusCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report the health and run mode of every host service",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			statuses, err := setup.ServicesStatus(deps)
			if err != nil {
				*exit = em.Failure("services.status", err)
				return nil
			}
			*exit = em.Success("services.status", servicesResult{Services: statuses})
			return nil
		},
	}
}
