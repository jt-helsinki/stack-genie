package cli

import (
	"fmt"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/spf13/cobra"
)

// newModelsCmd builds `ai models` (CLI §8).
func newModelsCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect model routing and test connectivity",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newModelsStatusCmd(em, exit), newModelsTestCmd(em, exit))
	return cmd
}

func newModelsStatusCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "LiteLLM health, providers, routing, Ollama connectivity",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info, err := litellm.RealClient().Status()
			if err != nil {
				*exit = em.Failure("models.status", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = em.Success("models.status", info)
			return nil
		},
	}
}

func newModelsTestCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "test <model>",
		Short: "Send a probe request to a model through LiteLLM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			res, err := litellm.RealClient().Test(args[0])
			if err != nil {
				// Transport-level failure: the gateway itself was unreachable.
				*exit = em.Failure("models.test", output.Errorf(output.ExitRuntimeFailure,
					"could not reach LiteLLM (is it running? run `ai doctor`): %s", err))
				return nil
			}
			if !res.OK {
				// The gateway responded but the call failed (bad model, missing
				// credential, provider error). Surface why, with a hint.
				*exit = em.Failure("models.test", modelTestError(res))
				return nil
			}
			*exit = em.Success("models.test", res)
			return nil
		},
	}
}

// modelTestError turns a failed model probe into an actionable error: it reports
// the provider's message, picks an exit code (5 for auth/credential issues, else
// 4), and adds a hint pointing at the likely fix.
func modelTestError(res litellm.TestResult) error {
	lower := strings.ToLower(res.Error)
	code := output.ExitRuntimeFailure
	hint := ""
	switch {
	case res.Status == 401 || res.Status == 403 ||
		strings.Contains(lower, "api key") || strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "credential"):
		code = output.ExitPermission
		hint = " — add the provider credential with `ai secrets set <PROVIDER>_API_KEY`"
	case res.Status == 404 || strings.Contains(lower, "not found") ||
		strings.Contains(lower, "does not exist") || strings.Contains(lower, "no such model") ||
		strings.Contains(lower, "not a valid model"):
		if strings.HasPrefix(res.Model, "ollama/") {
			hint = " — pull it first: `ollama pull " + strings.TrimPrefix(res.Model, "ollama/") + "`"
		} else {
			hint = " — check the model name (the gateway exposes <provider>/<model>, e.g. openai/gpt-5.5)"
		}
	}

	detail := res.Error
	if detail == "" {
		detail = fmt.Sprintf("gateway returned HTTP %d", res.Status)
	}
	return output.Errorf(code, "model %q failed: %s%s", res.Model, detail, hint)
}
