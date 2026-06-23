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

// Human renders one line per service: name, mode, state, then (when present) the
// host-reachable address and a `UI <url>` hint for services with an admin
// console — so the user can find each service's address + UI.
func (result servicesResult) Human() string {
	var builder strings.Builder
	for _, service := range result.Services {
		line := fmt.Sprintf("%-13s %-9s %-9s", service.Name, service.Mode, service.State)
		line += service.EndpointSuffix()
		_, _ = fmt.Fprintln(&builder, strings.TrimRight(line, " "))
	}
	return strings.TrimRight(builder.String(), "\n")
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
		newServicesConsoleCmd(em, exit),
	)
	return cmd
}

// newServicesControlCmd builds `ai services start|stop|restart [service]`.
//   - a named service (ollama, presidio, llm-guard, litellm, headroom, proxy,
//     open-webui, dns) — or the literal "all" — targets it directly, no prompt;
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

// selectServices fetches the current per-service status and presents a checkbox
// list (each labeled with its live state) for the user to choose which services
// to act on. Returns an exit-2 error if nothing is selected.
func selectServices(deps setup.Deps, action string) ([]string, error) {
	statuses, err := setup.ServicesStatus(deps)
	if err != nil {
		return nil, err
	}
	options := make([]huh.Option[string], 0, len(statuses))
	for _, service := range statuses {
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
	return selected, nil
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
