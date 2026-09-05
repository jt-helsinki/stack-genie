package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// litellmClient is the constructor for the gateway client used by
// `ai models status|test`. It is a package var so tests can inject a fake (no
// network); production wires litellm.RealClient.
var litellmClient = litellm.RealClient

// syncOmlxModelsFn is the injectable seam for `ai models refresh` (setup.SyncOmlxModels),
// so tests exercise it without a real omlx server or gateway.
var syncOmlxModelsFn = setup.SyncOmlxModels

// newModelsCmd builds `ai models` (CLI §8) — read-only gateway inspection. Local
// model management (pull/configure/enable/disable/rm) is gone: omlx, the sole
// local-inference backend, manages its own models entirely through its own admin
// panel (`ai services console omlx`) — this platform only installs/runs the
// server and keeps its live model list synced into the gateway (see
// internal/setup/omlx_host.go). Cloud models are managed via `ai keys`.
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
		newModelsRefreshCmd(em, exit),
	)
	return cmd
}

// newModelsRefreshCmd builds `ai models refresh`: rebuilds LiteLLM's omlx/*
// registrations from the omlx server's live GET /v1/models ON DEMAND — every
// CURRENTLY-registered omlx model is deleted and every model omlx currently
// reports is re-added fresh (even one whose name is unchanged), so a stale
// registration can never survive a refresh. Use it after adding/removing/renaming
// a model through omlx's own admin panel (`ai services console omlx`), without
// needing a full `ai services restart omlx`. This is the SAME rebuild that
// already runs automatically at `ai setup` and at `ai services start|restart
// omlx`; this command just triggers it standalone.
func newModelsRefreshCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Rebuild LiteLLM's registered models from omlx's live model list",
		Long: "Rebuild LiteLLM's omlx/* model registrations from the omlx server's live\n" +
			"GET /v1/models: every currently-registered omlx model is deleted and every\n" +
			"model omlx currently reports is re-added fresh, so a model added/removed/\n" +
			"renamed through omlx's own admin panel (`ai services console omlx`) shows up\n" +
			"in the gateway immediately, without a full `ai services restart omlx`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var result litellm.SyncResult
			var err error
			if ui.Enabled(em) {
				err = ui.RunWithSpinner(em.Err, "rebuilding omlx model registrations", func() error {
					var workErr error
					result, workErr = syncOmlxModelsFn()
					return workErr
				})
			} else {
				result, err = syncOmlxModelsFn()
			}
			if err != nil {
				*exit = em.Failure("models.refresh", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = em.Success("models.refresh", modelsRefreshResult(result))
			return nil
		},
	}
}

// modelsRefreshResult renders `ai models refresh`'s outcome.
type modelsRefreshResult litellm.SyncResult

func (result modelsRefreshResult) Human() string {
	if len(result.Added) == 0 && len(result.Deleted) == 0 {
		return ui.Success.Render(ui.IconOK) + " omlx currently serves no models — nothing to rebuild"
	}
	lines := []string{ui.Success.Render(ui.IconOK) + " rebuilt omlx model registrations"}
	for _, name := range result.Added {
		lines = append(lines, "  "+ui.Success.Render("+")+" "+ui.Value.Render(name))
	}
	for _, name := range result.Deleted {
		lines = append(lines, "  "+ui.Failure.Render("-")+" "+ui.Value.Render(name))
	}
	return strings.Join(lines, "\n")
}

func newModelsStatusCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "LiteLLM health, providers, routing, local (omlx) connectivity",
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
		if strings.HasPrefix(res.Model, "omlx/") {
			hint = " — manage it from omlx's own admin panel: `ai services console omlx`"
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
