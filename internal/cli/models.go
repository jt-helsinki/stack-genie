package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// hfClient is the constructor for the Hugging Face model-store client used by
// `ai models list|pull|rm` (locally-downloaded weights in the vLLM store). It is a
// package var so tests can inject a fake; production wires hf.RealClient.
var hfClient = hf.RealClient

// litellmClient is the constructor for the gateway client used by
// `ai models status|test`. It is a package var so tests can inject a fake (no
// network); production wires litellm.RealClient.
var litellmClient = litellm.RealClient

// modelRegistrar is the slice of litellm.KeyManager that `ai models pull|rm` use to
// keep the gateway's DB-backed model list in step with the local vLLM store (the sole
// local runtime now that Ollama is removed): a freshly-pulled model is registered under
// its gateway alias pointing at the per-model `vllm serve` endpoint, and removed on rm.
// It is an interface so tests inject a fake (no network); registration is BEST-EFFORT —
// a gateway that is down or has no master key must never fail a pull/rm.
type modelRegistrar interface {
	RegisterVLLMModel(alias, model, apiBase string, supportsTools bool) error
	UnregisterVLLMModel(alias string) error
}

// modelRegistrarFactory builds the registrar. A package var so tests inject a fake;
// production binds a KeyManager over the real container prober.
var modelRegistrarFactory = func() modelRegistrar {
	return litellm.NewKeyManager(runtime.RealProber())
}

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
		newModelsInstallVLLMCmd(em, exit),
		newModelsRmCmd(em, exit),
		newModelsShowCmd(em, exit),
	)
	return cmd
}

func newModelsStatusCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "LiteLLM health, providers, routing, local (vLLM) connectivity",
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
				// Transport-level failure: the gateway was unreachable OR the request
				// exceeded the (generous) chat-test timeout. A timeout usually means a
				// cold model is still loading, not that the gateway is down.
				hint := "is it running? run `ai doctor`"
				if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
					hint = "the model may still be loading — try again once it is warm, or pick a smaller model"
				}
				*exit = em.Failure("models.test", output.Errorf(output.ExitRuntimeFailure,
					"could not reach LiteLLM (%s): %s", hint, err))
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

// promptModel asks the user to pick one of the models the gateway currently
// serves (the live DB-backed model list) so testing is pick-not-type. The named
// aliases were removed (catalog-driven, DB-backed models), so the candidate set is
// the LIVE served-model list; a gateway that is down or serving nothing yields an
// empty option set and the prompt falls back to the seed. The prompt is PRE-SEEDED
// with seed when it names a served model; otherwise it falls back to the first one.
func promptModel(seed string) (string, error) {
	names := servedModelNames()
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

// servedModelNames returns the live served-model names worth offering in the
// `ai models test` picker (sorted, wildcards excluded). The named aliases were
// removed (catalog-driven, DB-backed models), so the source is the gateway's live
// model list; a gateway that is down or empty yields an empty slice (the prompt
// then falls back to the seed). Extracted so it is unit-testable without a TTY.
func servedModelNames() []string {
	names := make([]string, 0)
	if models, err := litellmClient().Models(); err == nil {
		for _, model := range models {
			if strings.Contains(model.Name, "*") {
				continue // skip any wildcard handle (none expected, but be safe)
			}
			names = append(names, model.Name)
		}
	}
	sort.Strings(names)
	return names
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
		hint = " — add the provider key with `ai keys add <provider>`"
	case res.Status == 404 || strings.Contains(lower, "not found") ||
		strings.Contains(lower, "does not exist") || strings.Contains(lower, "no such model") ||
		strings.Contains(lower, "not a valid model"):
		if strings.HasPrefix(res.Model, "vllm/") {
			hint = " — pull it first: `ai models pull " + strings.TrimPrefix(res.Model, "vllm/") + "`"
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
