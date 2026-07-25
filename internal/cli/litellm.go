package cli

import (
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// litellmPasswordResult is the `ai litellm password` payload: confirmation that
// the LiteLLM admin UI was secured (the password itself is never echoed/returned).
type litellmPasswordResult struct {
	Secured bool   `json:"secured"`
	LoginAs string `json:"login_as"`
	URL     string `json:"url"`
}

// Human renders the secured-UI confirmation for an operator.
func (result litellmPasswordResult) Human() string {
	var builder strings.Builder
	builder.WriteString("LiteLLM admin UI " + ui.Success.Render("secured") + ".\n")
	builder.WriteString("log in as ")
	builder.WriteString(ui.Value.Render(result.LoginAs))
	builder.WriteString(" at ")
	builder.WriteString(ui.Value.Render(result.URL))
	return builder.String()
}

// newLiteLLMCmd builds `ai litellm` and its subcommands. The only subcommand is
// `password`, which sets/rotates the LiteLLM admin UI password later (after
// setup) — useful for a standalone user who opted out of a password at setup but
// now wants one.
func newLiteLLMCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "litellm",
		Short: "Manage the LiteLLM gateway (e.g. set its admin UI password)",
		Long: "Manage the shared LiteLLM gateway. Currently `ai litellm password` sets or\n" +
			"rotates the admin UI password, which also secures the gateway API — useful in\n" +
			"any role (e.g. a standalone host that skipped a password at setup but wants one\n" +
			"now). The master key is reused when the running gateway already has one, else a\n" +
			"new one is minted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help() // `ai litellm` with no subcommand prints help (§17.0)
		},
	}
	cmd.AddCommand(newLiteLLMPasswordCmd(emitter, exit))
	return cmd
}

// newLiteLLMPasswordCmd builds `ai litellm password`. It prompts (hidden) for a
// new admin-UI password, reuses the running gateway's master key (or mints one),
// relaunches LiteLLM with both set (via env passthrough, never disk/argv), and
// then offers to persist the secrets to ~/.ai-platform/.ai-platform.env. Exit codes: 3 when
// the platform/LiteLLM isn't set up, 2 on bad/empty input, 4 on relaunch failure.
func newLiteLLMPasswordCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "password",
		Short: "Set or rotate the LiteLLM admin UI password (secures the gateway)",
		Long: "Set or rotate the LiteLLM admin UI password. This secures the admin UI and the\n" +
			"gateway API in any deployment role. You are prompted (hidden) for the new\n" +
			"password; the master key is reused from the running gateway when present, else a\n" +
			"new one is minted. You are then offered to save the secrets to ~/.ai-platform/.ai-platform.env\n" +
			"(a 0600 file `ai` auto-loads) so they persist across restarts.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Fail fast (exit 3) when the platform isn't set up: no runtime.yaml, or
			// the LiteLLM gateway container isn't running (nothing to relaunch).
			info, err := runtime.Load()
			if err != nil {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if info == nil {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitMissingDep,
					"no platform config yet — run `ai setup` first"))
				return nil
			}
			if info.Role == runtime.RoleClient {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitMissingDep,
					"this host is a client (no local LiteLLM) — set the password on the server"))
				return nil
			}
			if !setup.LiteLLMRunning() {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitMissingDep,
					"the LiteLLM gateway is not running — run `ai setup` (or `ai services start litellm`) first"))
				return nil
			}

			// The password value must come from a hidden prompt: a credential can't
			// be pre-seeded into the TUI, and we never accept it as a flag/arg. Under
			// --json / no TTY there is no way to supply it → exit 2.
			if !interactive(emitter) {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitInvalidInput,
					"run on a terminal to set the password (it is a hidden prompt, not a flag)"))
				return nil
			}
			password, err := promptSecret("New LiteLLM admin UI password", "", nil)
			if err != nil {
				*exit = emitter.Failure("litellm.password", err) // exit 2 (cancelled / prompt error)
				return nil
			}
			if password == "" {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitInvalidInput,
					"the password must not be empty"))
				return nil
			}

			// Reuse the running gateway's master key, else mint one.
			masterKey := setup.CurrentLiteLLMMasterKey()
			if masterKey == "" {
				generated, genErr := generateMasterKey()
				if genErr != nil {
					*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitRuntimeFailure,
						"could not generate a LiteLLM master key: %s", genErr))
					return nil
				}
				masterKey = generated
			}
			// The relaunch (recreate the container against the new auth) takes a few
			// seconds; show a spinner so the user knows it is working and not hung. This
			// path is always interactive (guarded above), so a TTY spinner is safe.
			relaunchErr := ui.RunWithSpinner(emitter.Err, "securing the LiteLLM admin UI",
				func() error { return relaunchLiteLLM(password, masterKey) })
			if relaunchErr != nil {
				*exit = emitter.Failure("litellm.password", output.Errorf(output.ExitRuntimeFailure,
					"could not secure the LiteLLM UI: %s", relaunchErr))
				return nil
			}

			// Offer to persist the secrets to ~/.ai-platform/.ai-platform.env (Feature 2),
			// including the stable salt key the relaunch settled on (DB-credential
			// encryption key — must not change).
			offerPersistLiteLLMSecrets(emitter, true, password, masterKey, setup.CurrentLiteLLMSaltKey())

			domain := info.ResolveDomain()
			*exit = emitter.Success("litellm.password", litellmPasswordResult{
				Secured: true,
				LoginAs: "admin",
				URL:     "http://litellm." + domain + ":18787/ui",
			})
			return nil
		},
	}
}

// relaunchLiteLLM is the seam `ai litellm password` calls to relaunch the gateway
// with auth, indirected through a var so tests substitute a fake (the live
// relaunch is hardware bring-up). It defaults to setup.RelaunchLiteLLMWithAuth.
var relaunchLiteLLM = setup.RelaunchLiteLLMWithAuth
