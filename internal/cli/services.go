package cli

import (
	goruntime "runtime"

	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/spf13/cobra"
)

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

// newServicesControlCmd builds `ai services start|stop|restart [service]`. With
// no service (or "all") it acts on every platform-owned container; a single
// service name (ollama, presidio, litellm, headroom) targets just that one.
func newServicesControlCmd(action string, em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               action + " [service]",
		Short:             action + " host services (all, or one named service)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeServiceNames,
		RunE: func(_ *cobra.Command, args []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			statuses, err := setup.ControlService(deps, action, firstArg(args))
			if err != nil {
				*exit = em.Failure("services."+action, err)
				return nil
			}
			*exit = em.Success("services."+action, map[string]any{"services": statuses})
			return nil
		},
	}
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
			*exit = em.Success("services.status", map[string]any{"services": statuses})
			return nil
		},
	}
}
