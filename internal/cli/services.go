package cli

import (
	goruntime "runtime"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/spf13/cobra"
)

// newServicesCmd builds `ai services` and its subcommands (CLI §10.2). In
// Slice 1 only `status` is implemented; start/stop/restart land in Slice 6.
func newServicesCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Inspect and manage host services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newServicesStatusCmd(em, exit))
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
