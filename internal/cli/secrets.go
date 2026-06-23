package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/spf13/cobra"
)

// secretsResult is the typed payload of `ai secrets list`. The `secrets` field
// keeps the JSON envelope shape stable while Human() renders a table. Like
// secrets.Entry, it carries names/metadata only — never values.
type secretsResult struct {
	Secrets []secrets.Entry `json:"secrets"`
}

// Human renders the credentials as a table NAME, ENV-VAR (the workspace
// placeholder the gateway swaps; "—" when unbound), or a friendly hint when
// none are stored. Never renders secret values — secrets.Entry has no value.
func (result secretsResult) Human() string {
	if len(result.Secrets) == 0 {
		return "No credentials yet — store one with `ai secrets set`."
	}
	rows := make([][]string, 0, len(result.Secrets))
	for _, entry := range result.Secrets {
		rows = append(rows, []string{entry.Name, orDash(entry.EnvVar)})
	}
	return ui.Table([]string{"NAME", "ENV-VAR"}, rows)
}

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
		Long: "Store a credential in the LiteLLM gateway. On a terminal you are prompted for\n" +
			"the name (pre-seeded with any name you pass) and, when not supplied, the\n" +
			"hidden credential value; under --json/no TTY pass the name as an argument and\n" +
			"the value via --stdin/--value. A value given via --stdin/--value is always\n" +
			"used directly (the hidden field can't display a seed).",
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

			// On a TTY prompt in ONE form so the user can navigate back. The name
			// field always shows, PRE-SEEDED with any provided name (§1.8). The
			// credential value is the exception: a hidden field can't display a seed
			// and --value/--stdin are explicit non-interactive inputs, so when a value
			// was provided it is used directly — the hidden field is only prompted when
			// the value is missing. It is never echoed.
			if interactive(em) {
				promptedName := name
				var promptedValue string
				fields := make([]huh.Field, 0, 2)
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
			*exit = em.Success("secrets.list", secretsResult{Secrets: entries})
			return nil
		},
	}
}

func newSecretsRmCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "rm [name]",
		Short: "Remove a credential",
		Long: "Remove a credential. On a terminal you are prompted to pick one of the stored\n" +
			"credentials (pre-selected from any name you pass); under --json/no TTY the\n" +
			"name argument is used directly.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			// On a terminal always prompt, PRE-SEEDED with any name given on the
			// command line; under --json / no TTY the arg is used directly and is
			// required (§21, §1.8).
			if interactive(em) {
				picked, err := promptSecretName("Remove which credential", name)
				if err != nil {
					*exit = em.Failure("secrets.rm", err)
					return nil
				}
				name = picked
			} else if name == "" {
				*exit = em.Failure("secrets.rm", output.Errorf(output.ExitInvalidInput, "provide a credential name to remove"))
				return nil
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
		Long: "Bind a credential to a workspace placeholder env var. On a terminal you are\n" +
			"prompted for the name (picked from the stored credentials, pre-selected from\n" +
			"any name you pass) and the env var (pre-seeded with any --env you pass); under\n" +
			"--json/no TTY pass the name as an argument and --env.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			// On a TTY prompt for both {name, env var} in ONE form so the user can
			// navigate back. Both fields always show, PRE-SEEDED with any provided
			// values (a matching stored credential is pre-selected); under --json / no
			// TTY the provided values are used directly and required (§21, §1.8).
			if interactive(em) {
				fields := make([]huh.Field, 0, 2)
				promptedName := name
				promptedEnv := env
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
// falling back to a free-text input when the list is unavailable or empty. The
// prompt is PRE-SEEDED with seed: the matching option is pre-selected (or the
// first), and the free-text fallback is pre-filled.
func promptSecretName(title, seed string) (string, error) {
	options, err := secretNameOptions()
	if err == nil && len(options) > 0 {
		initial := options[0].Value
		for _, option := range options {
			if option.Value == seed {
				initial = seed
				break
			}
		}
		return promptChoice(title, "", options, initial)
	}
	return promptText(title, "", seed, func(candidate string) error {
		if candidate == "" {
			return fmt.Errorf("name is required")
		}
		return nil
	})
}
