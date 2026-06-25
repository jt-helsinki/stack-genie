package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/spf13/cobra"
)

// ollamaClient is the constructor for the local-store client used by
// `ai models list|pull|rm|show`. It is a package var so tests can inject a fake;
// production wires ollama.RealClient.
var ollamaClient = ollama.RealClient

// litellmClient is the constructor for the gateway client used by
// `ai models status|test`. It is a package var so tests can inject a fake (no
// network); production wires litellm.RealClient.
var litellmClient = litellm.RealClient

// ollamaPopular reads the bundled popular-models snapshot (the embedded
// models.yaml) used by `ai models popular` and the `ai models pull` picker. It is a
// package var so tests can inject a fixture; production wires ollama.Popular (a pure
// embedded read, no network).
var ollamaPopular = func() ([]ollama.PopularModel, error) { return ollama.Popular() }

// newModelsCmd builds `ai models` (CLI §8).
func newModelsCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect model routing and test connectivity",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newModelsStatusCmd(em, exit),
		newModelsTestCmd(em, exit),
		newModelsListCmd(em, exit),
		newModelsPopularCmd(em, exit),
		newModelsPullCmd(em, exit),
		newModelsRmCmd(em, exit),
		newModelsShowCmd(em, exit),
	)
	return cmd
}

func newModelsStatusCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "LiteLLM health, providers, routing, Ollama connectivity",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var info litellm.StatusInfo
			var err error
			if ui.Enabled(em) {
				err = ui.RunWithSpinner(em.Err, "contacting the model gateway", func() error {
					var workErr error
					info, workErr = litellmClient().Status()
					return workErr
				})
			} else {
				info, err = litellmClient().Status()
			}
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
		Use:   "test [model]",
		Short: "Send a probe request to a model through LiteLLM",
		Long: "Send a probe request to a model through LiteLLM. On a terminal you are\n" +
			"prompted to pick one of the named model handles (pre-selected from any model\n" +
			"you pass); under --json/no TTY the model argument is used directly.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// On a terminal always prompt, PRE-SEEDED with any model given on the
			// command line (the user confirms/edits). Under --json / no TTY the
			// positional arg is used directly and is required (§21, §1.8).
			model := ""
			if len(args) == 1 {
				model = args[0]
			}
			if interactive(em) {
				picked, err := promptModel(model)
				if err != nil {
					*exit = em.Failure("models.test", err)
					return nil
				}
				model = picked
			} else if model == "" {
				*exit = em.Failure("models.test", output.Errorf(output.ExitInvalidInput,
					"specify a model to test (e.g. `ai models test gemma4`)"))
				return nil
			}
			var res litellm.TestResult
			var err error
			if ui.Enabled(em) {
				err = ui.RunWithSpinner(em.Err, "testing "+model, func() error {
					var workErr error
					res, workErr = litellmClient().Test(model)
					return workErr
				})
			} else {
				res, err = litellmClient().Test(model)
			}
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

// promptModel asks the user to pick one of the named (non-wildcard) model
// handles from the default routing so testing is pick-not-type. The prompt is
// PRE-SEEDED with seed when it names a known handle; otherwise it falls back to
// the first handle.
func promptModel(seed string) (string, error) {
	names := make([]string, 0)
	for name := range litellm.DefaultRouting().Aliases {
		if strings.ContainsAny(name, "/*") {
			continue // skip the per-provider wildcard handles
		}
		names = append(names, name)
	}
	sort.Strings(names)
	options := make([]huh.Option[string], 0, len(names))
	for _, name := range names {
		options = append(options, huh.NewOption(name, name))
	}
	initial := ""
	if len(names) > 0 {
		initial = names[0]
	}
	// A provided model pre-selects its option when it is one of the named handles;
	// an arbitrary string (e.g. a wildcard route) leaves the default selected.
	if seed != "" && slices.Contains(names, seed) {
		initial = seed
	}
	return promptChoice("Model", "send a probe request to this model through LiteLLM", options, initial)
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
