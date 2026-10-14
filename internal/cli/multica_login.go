package cli

import (
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/spf13/cobra"
)

type multicaLoginResult struct {
	URL string `json:"url"`
}

// Human renders the sign-in confirmation.
func (result multicaLoginResult) Human() string {
	return "Multica signed in at " + result.URL + " — daemon started with sandboxed agents."
}

// newMulticaCmd builds `ai multica` with `login`, which runs the documented
// `multica setup self-host` (SELF_HOSTING_AI.md) on demand — the same step `ai setup`
// offers — with the sandboxing shims and shared gateway key prepared first.
func newMulticaCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "multica",
		Short: "Manage the Multica host CLI + daemon (e.g. sign in)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Sign in to the self-hosted Multica and start the sandboxed daemon",
		Long: "Runs `multica setup self-host --server-url <url> --app-url <url>`: opens your browser to sign in\n" +
			"(create your account there first — the verification code is in `ai logs --service multica`),\n" +
			"discovers workspaces and starts the daemon with every agent CLI wrapped in the nono sandbox.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info, err := runtime.Load()
			if err != nil || info == nil || info.Role == runtime.RoleClient {
				*exit = emitter.Failure("multica.login", output.Errorf(output.ExitMissingDep,
					"no local Multica server — run `ai setup` (standalone or server) first"))
				return nil
			}
			if !interactive(emitter) {
				*exit = emitter.Failure("multica.login", output.Errorf(output.ExitInvalidInput,
					"run on a terminal — sign-in opens a browser and prompts"))
				return nil
			}
			if !setup.MulticaInstalled() {
				*exit = emitter.Failure("multica.login", output.Errorf(output.ExitMissingDep,
					"the multica CLI is not installed — run `ai setup` first"))
				return nil
			}
			domain := info.ResolveDomain()
			if err := setup.MulticaSignIn(domain); err != nil {
				*exit = emitter.Failure("multica.login", output.Errorf(output.ExitRuntimeFailure,
					"multica setup self-host did not complete: %s", err))
				return nil
			}
			*exit = emitter.Success("multica.login", multicaLoginResult{URL: setup.MulticaURL(domain)})
			return nil
		},
	})
	return cmd
}
