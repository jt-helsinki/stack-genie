package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
	"github.com/spf13/cobra"
)

// This file adds the LOCAL Ollama store management subcommands to `ai models`:
// list / pull / rm / show. Unlike `ai models status`/`test` (which describe and
// probe LiteLLM ROUTING), these act directly on the host Ollama service's local
// model store via its HTTP API (ollama.Client). Pulling a tag here makes it
// available immediately through LiteLLM's `ollama/*` wildcard route — registering
// a model in the gateway is NOT the same as installing it; that is what `pull`
// does. There is no separate "update" verb: re-pulling a name updates it.
//
// The custom-model-entry sentinel value used by the pull select.
const customModelOption = "\x00custom"

// resolveModelRuntime decides the serving runtime for an install. flagValue is the
// (possibly empty) --runtime value; empty resolves to the default (Ollama). The value
// is validated (invalid → exit 2). Both host-native runtimes are accepted — `ollama`
// (default) and `vllm` — but an unrecognised value is rejected rather than silently
// downgraded to Ollama.
func resolveModelRuntime(emitter *output.Emitter, flagValue string) (config.ModelRuntime, error) {
	value := strings.TrimSpace(flagValue)
	if value == "" {
		value = string(config.RuntimeOllama)
	}
	if !config.ValidModelRuntime(value) {
		return "", output.Errorf(output.ExitInvalidInput,
			"invalid --runtime %q (expected: ollama or vllm)", flagValue)
	}
	return config.ModelRuntime(value), nil
}

// vLLM host-side seams — package vars so tests inject fakes without touching the host.
// The real Detect/InstallGuidance/Pull live in internal/vllm; the manager (server
// lifecycle) is built lazily so a plain Ollama pull never constructs one.
var (
	vllmDetectFn          = vllm.Detect
	vllmInstallGuidanceFn = vllm.InstallGuidance
	vllmPullFn            = vllm.Pull
	vllmManagerFactory    = func() vllmServer {
		return vllm.NewManager(vllm.Config{Runner: vllm.RealRunner{}, Probe: vllm.RealHealthProbe()})
	}
)

// vllmServer is the host-side vLLM server manager slice used by `ai models pull|rm
// --runtime vllm`: it starts/locates a per-model `vllm serve` endpoint and stops it on
// removal. Production binds *vllm.Manager; tests a fake.
type vllmServer interface {
	EnsureServed(alias, model string) (endpoint string, err error)
	Stop(alias string) error
}

// vllmDefaultAlias derives the gateway alias for a vLLM model id when --alias is
// omitted: the id's last path segment (e.g. "mlx-community/Qwen2.5-7B-Instruct-4bit" →
// "Qwen2.5-7B-Instruct-4bit").
func vllmDefaultAlias(model string) string {
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 && slash < len(model)-1 {
		return model[slash+1:]
	}
	return model
}

// vllmNotInstalledError is the exit-3 error shown when --runtime vllm is chosen but no
// vLLM install is present. It NEVER falls back to Ollama — it carries the per-OS
// install guidance so the user can act.
func vllmNotInstalledError() error {
	guidance := vllmInstallGuidanceFn(goruntime.GOOS)
	return output.Errorf(output.ExitMissingDep,
		"vLLM is not installed — required for `--runtime vllm`. To install it:\n  %s",
		strings.Join(guidance, "\n  "))
}

// vllmActionable wraps a `hardware bring-up` seam error (ErrNotWired) so the user sees
// why a real vLLM run/download failed rather than a bare internal error. Any other
// error passes through.
func vllmActionable(err error) error {
	if errors.Is(err, vllm.ErrNotWired) {
		return fmt.Errorf("vLLM serving is not yet available on this host (hardware bring-up): %w", err)
	}
	return err
}

// pullVLLM installs one or more models through the host-native vLLM backend: it gates on
// a real vLLM install (exit 3 with guidance when missing — NEVER a silent Ollama
// fallback), downloads each model's weights (vllm.Pull), starts/locates its per-model
// endpoint (EnsureServed), registers it in the gateway under its alias, and records the
// runtime choice with the observed endpoint so a later run / `rm` finds it. The live
// download + serve are `hardware bring-up` (ErrNotWired on a dev host) — surfaced as an
// actionable per-model error, never a crash. Returns the process exit code.
func pullVLLM(emitter *output.Emitter, names []string, alias string) int {
	if installed, _ := vllmDetectFn(); !installed {
		return emitter.Failure("models.pull", vllmNotInstalledError())
	}
	if alias != "" && len(names) > 1 {
		return emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput,
			"--alias applies to a single model (got %d)", len(names)))
	}
	manager := vllmManagerFactory()
	registrar := modelRegistrarFactory()

	result := modelsPullResult{Pulled: make([]modelPullOutcome, 0, len(names))}
	failures := 0
	for _, model := range names {
		modelAlias := alias
		if modelAlias == "" {
			modelAlias = vllmDefaultAlias(model)
		}
		outcome := modelPullOutcome{Model: model}
		if err := vllmPullFn(model); err != nil {
			outcome.Error = vllmActionable(err).Error()
			failures++
			result.Pulled = append(result.Pulled, outcome)
			continue
		}
		endpoint, err := manager.EnsureServed(modelAlias, model)
		if err != nil {
			outcome.Error = vllmActionable(err).Error()
			failures++
			result.Pulled = append(result.Pulled, outcome)
			continue
		}
		outcome.OK = true
		// vLLM tool support is model-dependent and not probed here; unknown defaults to
		// capable, matching the gateway's default treatment.
		if regErr := registrar.RegisterVLLMModel(modelAlias, model, endpoint, true); regErr != nil {
			outcome.RegisterError = regErr.Error()
		} else {
			outcome.Registered = true
			recordModelRuntimeChoice(modelAlias, model, config.RuntimeVLLM, endpoint)
		}
		result.Pulled = append(result.Pulled, outcome)
	}
	if failures > 0 {
		return emitter.Failure("models.pull", output.Errorf(output.ExitRuntimeFailure,
			"%d of %d vLLM model(s) failed to install", failures, len(names)).WithDetails(result))
	}
	return emitter.Success("models.pull", result)
}

// recordModelRuntimeChoice persists the serving-runtime selection for a pulled model
// (best-effort — a store-write failure must never fail the pull). The store is keyed
// by the gateway alias (Alias) — for Ollama that is the pull ref; Model always
// carries the underlying pull ref so a later `rm` can find the record by model name.
func recordModelRuntimeChoice(alias, model string, runtime config.ModelRuntime, endpoint string) {
	_ = config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias:    alias,
		Model:    model,
		Runtime:  runtime,
		Endpoint: endpoint,
		Status:   "registered",
	})
}

// runtimeChoicesForModel returns the recorded runtime choices whose underlying Model
// matches ref (best-effort — an unreadable store yields none). It is keyed on Model
// (not the store's Alias key) so `rm <ref>` finds an Ollama choice stored under the
// ref itself.
func runtimeChoicesForModel(ref string) []config.ModelRuntimeChoice {
	choices, err := config.LoadModelRuntimes()
	if err != nil {
		return nil
	}
	matched := make([]config.ModelRuntimeChoice, 0, 1)
	for _, choice := range choices {
		if choice.Model == ref {
			matched = append(matched, choice)
		}
	}
	return matched
}

// vllmChoiceForRef finds a recorded vLLM runtime choice for ref, matching either the
// store's Alias key (a user removing by gateway alias) or the underlying Model (removing
// by the vLLM model id). Returns false when ref is not a recorded vLLM model — in which
// case `rm` falls through to the Ollama path.
func vllmChoiceForRef(ref string) (config.ModelRuntimeChoice, bool) {
	if choice, ok := config.ModelRuntimeFor(ref); ok && choice.Runtime == config.RuntimeVLLM {
		return choice, true
	}
	for _, choice := range runtimeChoicesForModel(ref) {
		if choice.Runtime == config.RuntimeVLLM {
			return choice, true
		}
	}
	return config.ModelRuntimeChoice{}, false
}

// removeVLLM de-registers a vLLM-served model: it un-registers it from the gateway,
// stops its per-model `vllm serve` process, and clears the recorded runtime choice. All
// steps are best-effort (a gateway/host hiccup must not fail the removal); the gateway
// un-register error is surfaced in the result. Returns the process exit code.
func removeVLLM(emitter *output.Emitter, ref string, choice config.ModelRuntimeChoice) int {
	result := modelsRmResult{Model: ref}
	registrar := modelRegistrarFactory()
	if regErr := registrar.UnregisterVLLMModel(choice.Alias); regErr != nil {
		result.UnregisterError = regErr.Error()
	}
	_ = vllmManagerFactory().Stop(choice.Alias)
	_ = config.DeleteModelRuntime(choice.Alias)
	return emitter.Success("models.rm", result)
}

// ollamaErr maps an ollama.Client error to the platform exit codes: an unreachable
// server is exit 3 (a missing service dependency — point at how to start it); a
// not-found model is exit 2 (bad input); anything else is exit 4 (runtime).
func ollamaErr(command string, err error) error {
	if ollama.IsUnreachable(err) {
		return output.Errorf(output.ExitMissingDep,
			"could not reach Ollama: %s — start it with `ai services start ollama` (or run `ai setup`)", err)
	}
	var notFound *ollama.NotFoundError
	if errors.As(err, &notFound) {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	return output.Errorf(output.ExitRuntimeFailure, "%s", err)
}

// localModelEntry is one row of `ai models list`: a model in the local Ollama
// store. The list is INSTALLED-only now (live from GET /api/tags) — the hardcoded
// catalog was dropped; installable suggestions live behind `ai models popular`.
type localModelEntry struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Size      int64  `json:"size,omitempty"`
	Params    string `json:"params,omitempty"`
}

// modelsListResult is the `ai models list` payload: the INSTALLED models in the
// LOCAL Ollama store.
type modelsListResult struct {
	Models []localModelEntry `json:"models"`
}

// Human renders the installed list as a NAME / SIZE / PARAMS table.
func (result modelsListResult) Human() string {
	if len(result.Models) == 0 {
		return ui.Muted.Render("no models in the local store — pull one with ") +
			ui.Primary.Render("ai models pull") +
			ui.Muted.Render(" (") + ui.Primary.Render("ai models popular") +
			ui.Muted.Render(" lists installable models)")
	}
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "%s  %s  %s\n",
		ui.Label.Render(fmt.Sprintf("%-28s", "NAME")),
		ui.Label.Render(fmt.Sprintf("%-9s", "SIZE")),
		ui.Label.Render("PARAMS"))
	for _, entry := range result.Models {
		size := "-"
		if entry.Size > 0 {
			size = ollama.HumanByteSize(entry.Size)
		}
		params := entry.Params
		if params == "" {
			params = "-"
		}
		_, _ = fmt.Fprintf(&builder, "%s  %s  %s\n",
			ui.Value.Render(fmt.Sprintf("%-28s", entry.Name)),
			ui.Value.Render(fmt.Sprintf("%-9s", size)),
			ui.Value.Render(params))
	}
	builder.WriteString("\n" + ui.Muted.Render("installed = in the local Ollama store · ") +
		ui.Primary.Render("ai models popular") + ui.Muted.Render(" lists installable models"))
	return strings.TrimRight(builder.String(), "\n")
}

// installedEntries maps the installed local-store models to list rows.
func installedEntries(installed []ollama.Model) []localModelEntry {
	entries := make([]localModelEntry, 0, len(installed))
	for _, model := range installed {
		entries = append(entries, localModelEntry{
			Name:      model.Name,
			Installed: true,
			Size:      model.Size,
			Params:    model.ParameterSize,
		})
	}
	return entries
}

func newModelsListCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed local (Ollama) models",
		Long: "List the models in the local Ollama store (GET /api/tags). The human output\n" +
			"is a NAME / SIZE / PARAMS table; --json returns the list. This manages the\n" +
			"LOCAL model store; `ai models popular` lists installable models (live from\n" +
			"ollama.com) and `ai models status` describes LiteLLM routing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			client := ollamaClient()
			var installed []ollama.Model
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "listing local models", func() error {
					var workErr error
					installed, workErr = client.List()
					return workErr
				})
			} else {
				installed, err = client.List()
			}
			if err != nil {
				*exit = emitter.Failure("models.list", ollamaErr("models.list", err))
				return nil
			}
			result := modelsListResult{Models: installedEntries(installed)}
			*exit = emitter.Success("models.list", result)
			return nil
		},
	}
}

// popularModelEntry is one row of `ai models popular`: one installable model from
// the live ollama.com library. Name is the base model name (e.g. qwen2.5); Tags are
// its pullable size tags (e.g. 7b, 72b) — pull a specific variant with
// `ai models pull <name>:<tag>`. The library carries no per-tag download size, so
// SIZE is shown as "—". RepoURL is the model's ollama.com/library page.
type popularModelEntry struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Tags        []ollama.LibraryTag `json:"tags,omitempty"`
	RepoURL     string              `json:"repo_url"`
}

// representativeTag returns the tag whose size/context/input best represents the
// model in a one-row overview: the "latest" tag when present, else the first.
func (entry popularModelEntry) representativeTag() (ollama.LibraryTag, bool) {
	for _, tag := range entry.Tags {
		if tag.Name == "latest" {
			return tag, true
		}
	}
	if len(entry.Tags) > 0 {
		return entry.Tags[0], true
	}
	return ollama.LibraryTag{}, false
}

// modelsPopularResult is the `ai models popular` payload.
type modelsPopularResult struct {
	Models []popularModelEntry `json:"models"`
}

// Human renders the library list as a NAME / SIZE / CONTEXT / INPUT / REPO table.
// SIZE/CONTEXT/INPUT are the "latest" (or first) tag's values scraped from the
// model's ollama.com /tags table; a dash marks a column the table omits.
func (result modelsPopularResult) Human() string {
	if len(result.Models) == 0 {
		return ui.Muted.Render("no popular models returned")
	}
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "%s  %s  %s  %s  %s\n",
		ui.Label.Render(fmt.Sprintf("%-24s", "NAME")),
		ui.Label.Render(fmt.Sprintf("%-9s", "SIZE")),
		ui.Label.Render(fmt.Sprintf("%-8s", "CONTEXT")),
		ui.Label.Render(fmt.Sprintf("%-13s", "INPUT")),
		ui.Label.Render("REPO"))
	for _, entry := range result.Models {
		size, context, input := "—", "—", "—"
		if tag, ok := entry.representativeTag(); ok {
			size = valueOrDash(tag.Size)
			context = valueOrDash(tag.Context)
			input = valueOrDash(tag.Input)
		}
		_, _ = fmt.Fprintf(&builder, "%s  %s  %s  %s  %s\n",
			ui.Value.Render(fmt.Sprintf("%-24s", entry.Name)),
			ui.Value.Render(fmt.Sprintf("%-9s", size)),
			ui.Value.Render(fmt.Sprintf("%-8s", context)),
			ui.Value.Render(fmt.Sprintf("%-13s", truncateCell(input, 13))),
			ui.Value.Render(entry.RepoURL))
	}
	builder.WriteString("\n" + ui.Muted.Render("pull any of these with ") +
		ui.Primary.Render("ai models pull <name>:<tag>") +
		ui.Muted.Render(" (size/context/input = the model's default tag · live from ollama.com)"))
	return strings.TrimRight(builder.String(), "\n")
}

// valueOrDash returns value, or "—" when it is empty.
func valueOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// truncateCell clips a cell value to width runes with a trailing ellipsis so the
// fixed-width column lines stay aligned.
func truncateCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}

func toPopularEntries(models []ollama.LibraryModel) []popularModelEntry {
	entries := make([]popularModelEntry, 0, len(models))
	for _, model := range models {
		entries = append(entries, popularModelEntry{
			Name:        model.Name,
			Description: model.Description,
			Tags:        model.Tags,
			RepoURL:     model.RepoURL,
		})
	}
	return entries
}

func newModelsPopularCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "popular",
		Short: "List popular installable models (live Ollama library)",
		Long: "List installable models from the live ollama.com library, with their pullable\n" +
			"size tags and ollama.com/library link. The list is fetched from the Ollama\n" +
			"library endpoint and cached locally; when the endpoint is unreachable the\n" +
			"cached copy is used. The library reports no per-tag download size, so SIZE\n" +
			"shows \"—\". Pull a specific variant — or any other reference — with\n" +
			"`ai models pull <name>:<tag>`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			models, _, err := ollamaLibrary()
			if err != nil && len(models) == 0 {
				*exit = emitter.Failure("models.popular", output.Errorf(output.ExitRuntimeFailure,
					"could not reach the Ollama library and no cached copy: %s", err))
				return nil
			}
			*exit = emitter.Success("models.popular", modelsPopularResult{Models: toPopularEntries(models)})
			return nil
		},
	}
}

// modelPullOutcome is the per-model result of a (multi-)pull: the exact reference
// and either success or the error message. Registered records whether the model was
// also registered in the LiteLLM gateway (best-effort, post-pull); RegisterError
// carries the registration failure message when registration was attempted and failed
// (it never fails the pull itself).
type modelPullOutcome struct {
	Model         string `json:"model"`
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	Registered    bool   `json:"registered,omitempty"`
	RegisterError string `json:"register_error,omitempty"`
}

// modelsPullResult is the `ai models pull` payload: one outcome per requested model.
type modelsPullResult struct {
	Pulled []modelPullOutcome `json:"pulled"`
}

func (result modelsPullResult) Human() string {
	var builder strings.Builder
	for index, outcome := range result.Pulled {
		if index > 0 {
			builder.WriteString("\n")
		}
		if outcome.OK {
			builder.WriteString(ui.Success.Render(ui.IconOK) + " pulled " + ui.Value.Render(outcome.Model))
			if outcome.Registered {
				builder.WriteString(ui.Muted.Render(" · registered in the gateway"))
			} else if outcome.RegisterError != "" {
				builder.WriteString("\n  " + ui.Warn.Render(ui.IconDot) +
					" gateway registration skipped: " + ui.Muted.Render(outcome.RegisterError))
			}
		} else {
			builder.WriteString(ui.Failure.Render(ui.IconFail) + " " + ui.Value.Render(outcome.Model) + ": " + outcome.Error)
		}
	}
	return builder.String()
}

func newModelsPullCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var runtimeFlag string
	var aliasFlag string
	cmd := &cobra.Command{
		Use:   "pull [name...]",
		Short: "Download one or more models into the local store (Ollama or vLLM)",
		Long: "Download one or more models into the local store. With name arguments\n" +
			"(or under --json / no TTY) each given reference is pulled in turn — this is\n" +
			"the custom-reference path (e.g. `llama3.2:3b qwen2.5:7b`, or `hf.co/user/model`).\n" +
			"On a terminal with no arguments you check off any number of popular models (a\n" +
			"bundled snapshot), and may also tick \"enter custom model(s)…\" to type extra\n" +
			"references. Every selected model is pulled; the run continues past a failure\n" +
			"and reports a per-model summary. Re-pulling an installed model updates it\n" +
			"(there is no separate update command).\n\n" +
			"--runtime selects how the model is SERVED through the gateway: `ollama` (default,\n" +
			"GGUF via host-native Ollama) or `vllm` (host-native vLLM — mlx-community ids on\n" +
			"macOS, Hugging Face safetensors ids on Linux). With --runtime vllm, --alias sets\n" +
			"the gateway alias (default: the model id's base name).",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			names := dedupeModelNames(args)
			if len(names) == 0 {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput,
						"specify one or more models to pull (e.g. `ai models pull llama3.2 qwen2.5:7b`)"))
					return nil
				}
				picked, err := promptModelsToPull()
				if err != nil {
					*exit = emitter.Failure("models.pull", err)
					return nil
				}
				names = picked
			}
			if len(names) == 0 {
				// Interactive: nothing checked / entered → a clean cancellation.
				*exit = emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput, "cancelled"))
				return nil
			}

			// Resolve the SERVING runtime once for the whole pull (default Ollama, so
			// no behaviour change without --runtime).
			chosenRuntime, runtimeErr := resolveModelRuntime(emitter, runtimeFlag)
			if runtimeErr != nil {
				*exit = emitter.Failure("models.pull", runtimeErr)
				return nil
			}
			// On a TTY, offer the runtime as a pre-seeded choice — but only when vLLM is
			// actually installed (otherwise Ollama is the sole real option and prompting
			// adds nothing). Under --json / no TTY the flag value stands.
			if interactive(emitter) {
				if installed, _ := vllmDetectFn(); installed {
					picked, promptErr := promptModelRuntime(chosenRuntime)
					if promptErr != nil {
						*exit = emitter.Failure("models.pull", promptErr)
						return nil
					}
					chosenRuntime = picked
				}
			}
			if chosenRuntime == config.RuntimeVLLM {
				*exit = pullVLLM(emitter, names, strings.TrimSpace(aliasFlag))
				return nil
			}
			client := ollamaClient()
			registrar := modelRegistrarFactory()
			outcomes := make([]modelPullOutcome, 0, len(names))
			anyFailed := false
			var lastErr error
			for index, name := range names {
				label := fmt.Sprintf("pulling %s (%d/%d)", name, index+1, len(names))
				var err error
				if ui.Enabled(emitter) {
					// Render a live download progress bar from Ollama's streamed
					// total/completed frames (the meter overwrites in place, and also
					// renders in the ai ui embedded terminal via the \r LogView normalize).
					bar := ui.NewProgressBar(emitter.Err, label)
					err = client.Pull(name, func(progress ollama.PullProgress) {
						bar.Update(progress.Completed, progress.Total, progress.Status)
					})
					bar.Finish(err)
				} else {
					err = client.Pull(name, func(ollama.PullProgress) {})
				}
				if err != nil {
					anyFailed = true
					lastErr = err
					outcomes = append(outcomes, modelPullOutcome{Model: name, OK: false, Error: err.Error()})
					continue
				}
				// Best-effort: register the freshly-pulled model in the gateway so it
				// gains a stable id and shows in the live catalogue. A gateway that is
				// down or has no master key must NOT fail the pull — warn and continue.
				// The choice is recorded (best-effort) so a later `rm` de-registers the
				// right backend (this is the Ollama path; vLLM returns earlier).
				outcome := modelPullOutcome{Model: name, OK: true}
				supportsTools := ollamaModelSupportsTools(client, name)
				regErr := registrar.RegisterOllamaModel(name, supportsTools)
				if regErr == nil {
					recordModelRuntimeChoice(name, name, config.RuntimeOllama, "")
				}
				if regErr != nil {
					outcome.RegisterError = regErr.Error()
				} else {
					outcome.Registered = true
				}
				// Best-effort: bake a memory-safe num_ctx into the model so it uses its
				// trained context window (LiteLLM does not forward num_ctx for the
				// ollama_chat provider). A Show/SetNumCtx failure must NOT fail the pull.
				if info, showErr := client.Show(name); showErr == nil {
					if numCtx := ollama.RecommendedNumCtx(info.ContextLength); numCtx > 0 {
						_ = client.SetNumCtx(name, numCtx)
					}
				}
				outcomes = append(outcomes, outcome)
			}

			result := modelsPullResult{Pulled: outcomes}
			if anyFailed {
				// Report the per-model summary but exit non-zero. The exit code is
				// mapped from the last failure (e.g. unreachable Ollama → exit 3);
				// the per-model outcomes ride along in error.details (JSON) and the
				// human message lists each failure.
				failed := make([]string, 0, len(outcomes))
				for _, outcome := range outcomes {
					if !outcome.OK {
						failed = append(failed, outcome.Model+": "+outcome.Error)
					}
				}
				mapped := ollamaErr("models.pull", lastErr).(*output.Error)
				summary := output.Errorf(mapped.Code, "%d of %d models failed to pull:\n  %s",
					len(failed), len(outcomes), strings.Join(failed, "\n  ")).WithDetails(result)
				*exit = emitter.Failure("models.pull", summary)
				return nil
			}
			*exit = emitter.Success("models.pull", result)
			return nil
		},
	}
	cmd.Flags().StringVar(&runtimeFlag, "runtime", string(config.RuntimeOllama),
		"serving runtime: ollama (default) or vllm")
	cmd.Flags().StringVar(&aliasFlag, "alias", "",
		"gateway alias for the served model (--runtime vllm only; default: the model id's base name)")
	return cmd
}

// promptModelRuntime shows the serving-runtime picker on a TTY, pre-seeded with seed
// (the --runtime value / default). It is only invoked when vLLM is actually installed,
// so both options are meaningful. Returns the chosen runtime.
func promptModelRuntime(seed config.ModelRuntime) (config.ModelRuntime, error) {
	options := make([]huh.Option[string], 0, len(config.ModelRuntimes()))
	for _, runtime := range config.ModelRuntimes() {
		options = append(options, huh.NewOption(modelRuntimeLabel(runtime), string(runtime)))
	}
	chosen, err := promptChoice("Serving runtime",
		"How the model is served through the gateway.", options, string(seed))
	if err != nil {
		return "", err
	}
	return config.ModelRuntime(chosen), nil
}

// modelRuntimeLabel is the human label for a serving runtime in the CLI picker.
func modelRuntimeLabel(runtime config.ModelRuntime) string {
	switch runtime {
	case config.RuntimeOllama:
		return "Ollama (GGUF, host-native)"
	case config.RuntimeVLLM:
		return "vLLM (MLX / safetensors, host-native)"
	default:
		return string(runtime)
	}
}

// dedupeModelNames trims, drops empties, and removes duplicate references while
// preserving first-seen order.
func dedupeModelNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// promptModelsToPull presents the installable library models — one checkbox per
// model:tag reference (built from each library model's Name + its size Tags) — plus a
// final "enter custom model(s)…" checkbox; ticking the latter prompts for free-text
// references (space- or comma-separated) which are added to the selection. If the
// library is unavailable it falls back to the free-text custom-entry prompt (still
// multiple). Returns the de-duplicated set of references (possibly empty → cancelled).
// Only call on an interactive terminal.
func promptModelsToPull() ([]string, error) {
	library, _, libraryErr := ollamaLibrary()
	if libraryErr != nil && len(library) == 0 {
		// The library is unavailable; don't block pulling — go straight to the
		// free-text custom-entry prompt (still allows multiple, space/comma separated).
		return promptCustomModels()
	}
	refs := libraryPullRefs(library)
	if len(refs) == 0 {
		return promptCustomModels()
	}
	options := make([]huh.Option[string], 0, len(refs)+1)
	for _, ref := range refs {
		options = append(options, huh.NewOption(ref, ref))
	}
	options = append(options, huh.NewOption("✎ enter custom model(s)…", customModelOption))

	selected, err := promptMultiChoice("Models to pull",
		"check any number; tick ✎ to also type custom references (e.g. llama3.2:3b, hf.co/user/model)",
		options)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(selected))
	wantCustom := false
	for _, value := range selected {
		if value == customModelOption {
			wantCustom = true
			continue
		}
		names = append(names, value)
	}
	if wantCustom {
		custom, customErr := promptCustomModels()
		if customErr != nil {
			return nil, customErr
		}
		names = append(names, custom...)
	}
	return dedupeModelNames(names), nil
}

// libraryPullRefs expands the library models into pullable references: one
// "name:tag" per tag, or the bare name for a model with no tags. Order follows the
// library (already sorted by name), tags in their listed order.
func libraryPullRefs(library []ollama.LibraryModel) []string {
	refs := make([]string, 0, len(library))
	for _, model := range library {
		if len(model.Tags) == 0 {
			refs = append(refs, model.Name)
			continue
		}
		for _, tag := range model.Tags {
			refs = append(refs, model.Name+":"+tag.Name)
		}
	}
	return refs
}

// promptCustomModels asks for one or more free-text model references (the custom-
// entry path), space- or comma-separated (e.g. "llama3.2:1b qwen2.5:7b"). Returns
// the parsed, de-duplicated references.
func promptCustomModels() ([]string, error) {
	custom, err := promptText("Custom model reference(s)",
		"space- or comma-separated Ollama models to pull (e.g. llama3.2:3b qwen2.5:7b, hf.co/user/model)", "",
		func(value string) error {
			if len(parseModelRefs(value)) == 0 {
				return errors.New("enter at least one model reference")
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return parseModelRefs(custom), nil
}

// parseModelRefs splits a free-text entry into model references on whitespace and
// commas, dropping empties and duplicates.
func parseModelRefs(value string) []string {
	fields := strings.FieldsFunc(value, func(runeValue rune) bool {
		return runeValue == ',' || runeValue == ' ' || runeValue == '\t' || runeValue == '\n'
	})
	return dedupeModelNames(fields)
}

// modelsRmResult is the `ai models rm` payload. UnregisterError carries a best-effort
// gateway-unregistration failure message (never fails the removal itself).
type modelsRmResult struct {
	Model           string `json:"model"`
	UnregisterError string `json:"unregister_error,omitempty"`
}

func (result modelsRmResult) Human() string {
	out := ui.Success.Render(ui.IconOK) + " removed " + ui.Value.Render(result.Model)
	if result.UnregisterError != "" {
		out += "\n  " + ui.Warn.Render(ui.IconDot) +
			" gateway unregistration skipped: " + ui.Muted.Render(result.UnregisterError)
	}
	return out
}

func newModelsRmCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:     "rm [name]",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a model from the local (Ollama) store",
		Long: "Remove a model from the local Ollama store. On a terminal with no argument you\n" +
			"pick from the installed models; with a name argument (or under --json) that\n" +
			"model is removed. On a terminal you are asked to confirm before deleting.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			client := ollamaClient()
			if name == "" {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.rm", output.Errorf(output.ExitInvalidInput,
						"specify a model to remove (e.g. `ai models rm llama3.2`)"))
					return nil
				}
				picked, err := promptInstalledModel(client)
				if err != nil {
					*exit = emitter.Failure("models.rm", err)
					return nil
				}
				name = picked
			}
			// Resolve the recorded serving runtime up front so the confirm wording and
			// the de-registration target match the backend the model was installed on.
			vllmChoice, isVLLM := vllmChoiceForRef(name)
			if interactive(emitter) {
				detail := "this deletes the model from the local Ollama store"
				if isVLLM {
					detail = "this stops the vLLM server and de-registers the model from the gateway"
				}
				confirmed, err := promptConfirm("Remove "+name+"?", detail)
				if err != nil {
					*exit = emitter.Failure("models.rm", err)
					return nil
				}
				if !confirmed {
					*exit = emitter.Failure("models.rm", output.Errorf(output.ExitInvalidInput, "cancelled"))
					return nil
				}
			}
			// vLLM backend: there is no Ollama store entry to delete — stop the per-model
			// server, de-register it, and clear the recorded choice (all best-effort).
			if isVLLM {
				*exit = removeVLLM(emitter, name, vllmChoice)
				return nil
			}
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "removing "+name, func() error { return client.Remove(name) })
			} else {
				err = client.Remove(name)
			}
			if err != nil {
				*exit = emitter.Failure("models.rm", ollamaErr("models.rm", err))
				return nil
			}
			// Best-effort: drop the gateway's DB-backed registration for the removed
			// model, de-registering the Ollama backend that WAS recorded at install and
			// clearing the runtime choice. A gateway that is down or has no
			// master key must NOT fail the removal — surface the warning in the result.
			result := modelsRmResult{Model: name}
			registrar := modelRegistrarFactory()
			choices := runtimeChoicesForModel(name)
			if len(choices) == 0 {
				// No recorded choice → the default (Ollama) backend, as before.
				if regErr := registrar.UnregisterOllamaModel(name); regErr != nil {
					result.UnregisterError = regErr.Error()
				}
			} else {
				for _, choice := range choices {
					if regErr := registrar.UnregisterOllamaModel(name); regErr != nil && result.UnregisterError == "" {
						result.UnregisterError = regErr.Error()
					}
					_ = config.DeleteModelRuntime(choice.Alias)
				}
			}
			*exit = emitter.Success("models.rm", result)
			return nil
		},
	}
}

// ollamaModelSupportsTools best-effort reports whether a freshly-pulled Ollama model
// advertises tool/function-calling support (its /api/show capabilities include "tools").
// On any probe error it returns true (unknown → assume capable), matching the gateway's
// default so a probe hiccup never wrongly disables tools for a model.
func ollamaModelSupportsTools(client ollama.Client, name string) bool {
	info, err := client.Show(name)
	if err != nil {
		return true
	}
	for _, capability := range info.Capabilities {
		if capability == "tools" {
			return true
		}
	}
	return false
}

// promptInstalledModel asks the user to pick one of the currently-installed models.
// Only call on an interactive terminal.
func promptInstalledModel(client ollama.Client) (string, error) {
	installed, err := client.List()
	if err != nil {
		return "", ollamaErr("models.rm", err)
	}
	if len(installed) == 0 {
		return "", output.Errorf(output.ExitInvalidInput,
			"no models in the local store — pull one with `ai models pull`")
	}
	options := make([]huh.Option[string], 0, len(installed))
	for _, model := range installed {
		options = append(options, huh.NewOption(model.Name, model.Name))
	}
	return promptChoice("Model to remove", "delete this model from the local Ollama store",
		options, installed[0].Name)
}

// modelsShowResult is the `ai models show` payload.
type modelsShowResult struct {
	Info ollama.ModelInfo `json:"info"`
}

func (result modelsShowResult) Human() string {
	info := result.Info
	lines := []string{ui.Heading.Render(info.Name)}
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, "  "+ui.Label.Render(fmt.Sprintf("%-12s", label))+" "+ui.Value.Render(value))
		}
	}
	add("params", info.ParameterSize)
	add("quant", info.QuantizationLevel)
	add("family", info.Family)
	add("format", info.Format)
	if len(info.Capabilities) > 0 {
		add("caps", strings.Join(info.Capabilities, ", "))
	}
	return strings.Join(lines, "\n")
}

func newModelsShowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show metadata for a local (Ollama) model",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			client := ollamaClient()
			if name == "" {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.show", output.Errorf(output.ExitInvalidInput,
						"specify a model to show (e.g. `ai models show llama3.2`)"))
					return nil
				}
				picked, err := promptInstalledModel(client)
				if err != nil {
					*exit = emitter.Failure("models.show", err)
					return nil
				}
				name = picked
			}
			var info ollama.ModelInfo
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "fetching "+name, func() error {
					var workErr error
					info, workErr = client.Show(name)
					return workErr
				})
			} else {
				info, err = client.Show(name)
			}
			if err != nil {
				*exit = emitter.Failure("models.show", ollamaErr("models.show", err))
				return nil
			}
			*exit = emitter.Success("models.show", modelsShowResult{Info: info})
			return nil
		},
	}
}
