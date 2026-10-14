package cli

import (
	goruntime "runtime"

	"github.com/jt-helsinki/stack-genie/internal/colibri"
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// localBackend describes a host-native local-inference runtime whose API key / admin
// password `ai <name> password` can rotate. omlx and colibri differ only in these
// fields, so one command builder serves both.
type localBackend struct {
	name      string // command + service name
	installed func() bool
	setKey    func(key string)
}

var (
	omlxBackend    = localBackend{"omlx", omlx.Detect, func(key string) { setup.SetOmlxAPIKey(key) }}
	colibriBackend = localBackend{"colibri", colibri.Detect, setup.SetColibriAPIKey}
)

type localPasswordResult struct {
	Backend string `json:"backend"`
	Updated bool   `json:"updated"`
}

// Human renders the confirmation (the password itself is never echoed).
func (result localPasswordResult) Human() string {
	return result.Backend + " password " + ui.Success.Render("updated") + " (server restarted)."
}

// newLocalBackendCmd builds `ai omlx` / `ai colibri` with their `password` subcommand: a
// hidden prompt for a new password, stored in the OS keychain, then a restart so the
// running server adopts it. Exit codes: 3 not installed, 2 no TTY/empty, 4 restart failed.
func newLocalBackendCmd(emitter *output.Emitter, exit *int, backend localBackend) *cobra.Command {
	command := backend.name + ".password"
	cmd := &cobra.Command{
		Use:   backend.name,
		Short: "Manage the " + backend.name + " local-inference server (e.g. set its password)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "password",
		Short: "Set or rotate the " + backend.name + " password / API key",
		Long: "Set or rotate the " + backend.name + " password (its API key). You are prompted (hidden) for the\n" +
			"new value; it is stored in the OS keychain and " + backend.name + " is restarted to adopt it.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !backend.installed() {
				*exit = emitter.Failure(command, output.Errorf(output.ExitMissingDep,
					"%s is not installed — run `ai setup` first", backend.name))
				return nil
			}
			if !interactive(emitter) {
				*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput,
					"run on a terminal to set the password (it is a hidden prompt, not a flag)"))
				return nil
			}
			password, err := promptSecret("New "+backend.name+" password", "", nil)
			if err != nil {
				*exit = emitter.Failure(command, err)
				return nil
			}
			if password == "" {
				*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput, "the password must not be empty"))
				return nil
			}
			backend.setKey(password)
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			restartErr := ui.RunWithSpinner(emitter.Err, "restarting "+backend.name, func() error {
				_, err := setup.ControlService(deps, "restart", backend.name)
				return err
			})
			if restartErr != nil {
				*exit = emitter.Failure(command, output.Errorf(output.ExitRuntimeFailure,
					"password saved, but %s did not restart: %s", backend.name, restartErr))
				return nil
			}
			*exit = emitter.Success(command, localPasswordResult{Backend: backend.name, Updated: true})
			return nil
		},
	})
	return cmd
}
