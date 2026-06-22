package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
	"github.com/spf13/cobra"
)

// newSecretsCmd builds `ai secrets` (CLI §16.1). Credentials live only in the
// LiteLLM gateway's credential store (keys-in-LiteLLM); the platform never writes
// values to its own disk.
func newSecretsCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage credentials (stored only in the LiteLLM gateway, never on platform disk)",
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

// mapSecretErr maps broker errors to exit codes (§18). Brokers that already carry
// a specific code (*output.Error — e.g. the LiteLLM credentials API surfacing an
// invalid request) are passed through unchanged.
func mapSecretErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	if errors.Is(err, secrets.ErrGatewayMissing) {
		return output.Errorf(output.ExitMissingDep, "%s", err)
	}
	return output.Errorf(output.ExitRuntimeFailure, "%s", err)
}

func newSecretsSetCmd(em *output.Emitter, exit *int) *cobra.Command {
	var value string
	var fromStdin bool
	command := &cobra.Command{
		Use:   "set [name]",
		Short: "Store a credential in the LiteLLM gateway",
		Long: "Store a credential in the LiteLLM gateway. Run with no name (and no value)\n" +
			"on a terminal to be prompted for the name and the credential (hidden); pass\n" +
			"the name as an argument and the value via --stdin/--value for\n" +
			"non-interactive/scripted use.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			// Resolve the value from the non-interactive sources first.
			var credential []byte
			haveValue := false
			switch {
			case fromStdin:
				read, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					*exit = em.Failure("secrets.set", output.Errorf(output.ExitRuntimeFailure, "read stdin: %s", err))
					return nil
				}
				credential = read
				haveValue = true
			case value != "":
				credential = []byte(value)
				haveValue = true
			}

			// On a TTY prompt for whichever of {name, value} we are still missing,
			// in ONE form so the user can navigate back. The credential field is
			// hidden and never echoed.
			if (name == "" || !haveValue) && interactive(em) {
				promptedName := name
				var promptedValue string
				fields := make([]huh.Field, 0, 2)
				if name == "" {
					nameInput := huh.NewInput().
						Title("Credential name").
						Description("e.g. OPENAI_API_KEY").
						Value(&promptedName).
						Validate(func(candidate string) error {
							if candidate == "" {
								return fmt.Errorf("name is required")
							}
							return nil
						})
					fields = append(fields, nameInput)
				}
				if !haveValue {
					valueInput := huh.NewInput().
						Title("Credential value").
						Description("hidden — stored only in the LiteLLM gateway, never on platform disk").
						EchoMode(huh.EchoModePassword).
						Value(&promptedValue).
						Validate(func(candidate string) error {
							if candidate == "" {
								return fmt.Errorf("value is required")
							}
							return nil
						})
					fields = append(fields, valueInput)
				}
				if err := runForm(huh.NewGroup(fields...)); err != nil {
					*exit = em.Failure("secrets.set", err)
					return nil
				}
				name = promptedName
				if !haveValue {
					credential = []byte(promptedValue)
					haveValue = true
				}
			}

			if name == "" {
				*exit = em.Failure("secrets.set", output.Errorf(output.ExitInvalidInput, "provide a credential name"))
				return nil
			}
			if !haveValue {
				*exit = em.Failure("secrets.set", output.Errorf(output.ExitInvalidInput, "provide --stdin or --value"))
				return nil
			}
			if err := secrets.RealBroker().Set(name, credential); err != nil {
				*exit = em.Failure("secrets.set", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.set", map[string]any{"name": name})
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
		Use:   "rm [name]",
		Short: "Remove a credential",
		Long: "Remove a credential. Run with no name on a terminal to be prompted to pick\n" +
			"one of the stored credentials; pass the name as an argument for\n" +
			"non-interactive/scripted use.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" {
				if !interactive(em) {
					*exit = em.Failure("secrets.rm", output.Errorf(output.ExitInvalidInput, "provide a credential name to remove"))
					return nil
				}
				picked, err := promptSecretName("Remove which credential")
				if err != nil {
					*exit = em.Failure("secrets.rm", err)
					return nil
				}
				name = picked
			}
			if err := secrets.RealBroker().Remove(name); err != nil {
				*exit = em.Failure("secrets.rm", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.rm", map[string]any{"name": name})
			return nil
		},
	}
}

func newSecretsMapCmd(em *output.Emitter, exit *int) *cobra.Command {
	var env string
	command := &cobra.Command{
		Use:   "map [name] --env <ENV_VAR>",
		Short: "Bind a credential to a workspace placeholder env var",
		Long: "Bind a credential to a workspace placeholder env var. Run with the name or\n" +
			"--env omitted on a terminal to be prompted for them (name picked from the\n" +
			"stored credentials); pass the name as an argument and --env for\n" +
			"non-interactive/scripted use.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			// On a TTY prompt for whichever of {name, env var} are missing, in ONE
			// form so the user can navigate back.
			if (name == "" || env == "") && interactive(em) {
				fields := make([]huh.Field, 0, 2)
				promptedName := name
				promptedEnv := env
				if name == "" {
					options, listErr := secretNameOptions()
					if listErr == nil && len(options) > 0 {
						fields = append(fields, huh.NewSelect[string]().
							Title("Credential").
							Description("bind which stored credential").
							Options(options...).
							Value(&promptedName))
					} else {
						fields = append(fields, huh.NewInput().
							Title("Credential name").
							Value(&promptedName).
							Validate(func(candidate string) error {
								if candidate == "" {
									return fmt.Errorf("name is required")
								}
								return nil
							}))
					}
				}
				if env == "" {
					fields = append(fields, huh.NewInput().
						Title("Workspace env var").
						Description("the placeholder env var the gateway swaps, e.g. OPENAI_API_KEY").
						Value(&promptedEnv).
						Validate(func(candidate string) error {
							if candidate == "" {
								return fmt.Errorf("env var is required")
							}
							return nil
						}))
				}
				if err := runForm(huh.NewGroup(fields...)); err != nil {
					*exit = em.Failure("secrets.map", err)
					return nil
				}
				name = promptedName
				env = promptedEnv
			}

			if name == "" {
				*exit = em.Failure("secrets.map", output.Errorf(output.ExitInvalidInput, "provide a credential name"))
				return nil
			}
			if env == "" {
				*exit = em.Failure("secrets.map", output.Errorf(output.ExitInvalidInput, "--env is required"))
				return nil
			}
			if err := secrets.RealBroker().Map(name, env); err != nil {
				*exit = em.Failure("secrets.map", mapSecretErr(err))
				return nil
			}
			*exit = em.Success("secrets.map", map[string]any{"name": name, "env_var": env})
			return nil
		},
	}
	command.Flags().StringVar(&env, "env", "", "workspace placeholder env var the gateway swaps")
	return command
}

// secretNameOptions lists the stored credential names as select options so the
// user can pick rather than retype (reducing entry errors). It returns the
// broker's error so callers can fall back to a free-text prompt.
func secretNameOptions() ([]huh.Option[string], error) {
	entries, err := secrets.RealBroker().List()
	if err != nil {
		return nil, err
	}
	options := make([]huh.Option[string], 0, len(entries))
	for _, entry := range entries {
		options = append(options, huh.NewOption(entry.Name, entry.Name))
	}
	return options, nil
}

// promptSecretName prompts the user to pick one of the stored credential names,
// falling back to a free-text input when the list is unavailable or empty.
func promptSecretName(title string) (string, error) {
	options, err := secretNameOptions()
	if err == nil && len(options) > 0 {
		return promptChoice(title, "", options, options[0].Value)
	}
	return promptText(title, "", "", func(candidate string) error {
		if candidate == "" {
			return fmt.Errorf("name is required")
		}
		return nil
	})
}
