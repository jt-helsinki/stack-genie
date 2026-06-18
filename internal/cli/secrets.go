package cli

import (
	"errors"
	"io"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
	"github.com/spf13/cobra"
)

// newSecretsCmd builds `ai secrets` (CLI §16.1). Credentials live only in
// ClawPatrol's store; the platform never writes values to its own disk.
func newSecretsCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage credentials (stored only in ClawPatrol, never on platform disk)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newSecretsSetCmd(em, exit),
		newSecretsListCmd(em, exit),
		newSecretsRmCmd(em, exit),
		newSecretsMapCmd(em, exit),
	)
	return cmd
}

// mapSecretErr maps broker errors to exit codes (§18).
func mapSecretErr(err error) error {
	if errors.Is(err, secrets.ErrClawPatrolMissing) {
		return output.Errorf(output.ExitMissingDep, "%s", err)
	}
	return output.Errorf(output.ExitRuntimeFailure, "%s", err)
}

func newSecretsSetCmd(em *output.Emitter, exit *int) *cobra.Command {
	var value string
	var fromStdin bool
	command := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a credential in ClawPatrol",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var credential []byte
			switch {
			case fromStdin:
				read, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					*exit = em.Failure("secrets.set", output.Errorf(output.ExitRuntimeFailure, "read stdin: %s", err))
					return nil
				}
				credential = read
			case value != "":
				credential = []byte(value)
			default:
				*exit = em.Failure("secrets.set", output.Errorf(output.ExitInvalidInput, "provide --stdin or --value"))
				return nil
			}
			if err := secrets.RealBroker().Set(args[0], credential); err != nil {
				*exit = em.Failure("secrets.set", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.set", map[string]any{"name": args[0]})
			return nil
		},
	}
	command.Flags().StringVar(&value, "value", "", "credential value (discouraged — leaks to shell history; prefer --stdin)")
	command.Flags().BoolVar(&fromStdin, "stdin", false, "read the credential value from stdin")
	return command
}

func newSecretsListCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List credential names and metadata (never values)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			entries, err := secrets.RealBroker().List()
			if err != nil {
				*exit = em.Failure("secrets.list", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.list", map[string]any{"secrets": entries})
			return nil
		},
	}
}

func newSecretsRmCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := secrets.RealBroker().Remove(args[0]); err != nil {
				*exit = em.Failure("secrets.rm", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.rm", map[string]any{"name": args[0]})
			return nil
		},
	}
}

func newSecretsMapCmd(em *output.Emitter, exit *int) *cobra.Command {
	var env string
	command := &cobra.Command{
		Use:   "map <name> --env <ENV_VAR>",
		Short: "Bind a credential to a workspace placeholder env var",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if env == "" {
				*exit = em.Failure("secrets.map", output.Errorf(output.ExitInvalidInput, "--env is required"))
				return nil
			}
			if err := secrets.RealBroker().Map(args[0], env); err != nil {
				*exit = em.Failure("secrets.map", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.map", map[string]any{"name": args[0], "env_var": env})
			return nil
		},
	}
	command.Flags().StringVar(&env, "env", "", "workspace placeholder env var the gateway swaps")
	return command
}
