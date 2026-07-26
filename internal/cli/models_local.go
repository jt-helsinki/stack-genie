package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
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

// dmrAvailable reports whether Docker Model Runner (DMR) is reachable from this
// host, gating the `--runtime docker-model-runner` install path. It is a package
// var so tests inject a deterministic result (no network); production probes the
// local DMR endpoint. Ollama remains the authoritative DOWNLOADER for both runtimes,
// so this only decides whether the DMR SERVING backend may be selected — never a
// silent fallback: an explicit DMR request against an unavailable runtime is an
// error, not a downgrade to Ollama.
//
// Phase 3 (`ai setup`) owns first-class DMR detection (a runtime.yaml flag). Until
// that lands, and to stay resilient if it is not merged yet, this probes the DMR
// endpoint directly rather than referencing a runtime field that may not exist.
//
// hardware bring-up: the LIVE probe only succeeds against a running DMR engine —
// verify on a provisioned host.
var dmrAvailable = probeDockerModelRunner

// probeDockerModelRunner does a short GET against the host-local DMR OpenAI-compatible
// endpoint (:12434). DockerModelRunnerAPIBase is the CONTAINER-facing address
// (host.docker.internal); from the host CLI the same engine is reachable on localhost,
// so we probe there. Any HTTP response (even an error status) means the engine is up.
func probeDockerModelRunner() bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get("http://localhost:12434/engines/v1/models")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// resolveModelRuntime decides the serving runtime for an install. flagValue is the
// (possibly empty) --runtime value; empty resolves to the default (Ollama), so the
// behaviour is UNCHANGED without --runtime. The flag value is validated (invalid →
// exit 2). On a terminal, when DMR is a real option, the user is PROMPTED (pre-seeded
// with flagValue) per the repo's TTY-prompt convention; under --json / no TTY the
// flag value is used directly. An explicit (or picked) DMR choice against an
// unavailable runtime is REJECTED (exit 3) — never a silent fallback to Ollama.
func resolveModelRuntime(emitter *output.Emitter, flagValue string) (config.ModelRuntime, error) {
	value := strings.TrimSpace(flagValue)
	if value == "" {
		value = string(config.RuntimeOllama)
	}
	if !config.ValidModelRuntime(value) {
		return "", output.Errorf(output.ExitInvalidInput,
			"invalid --runtime %q (expected one of: ollama, docker-model-runner)", flagValue)
	}
	chosen := config.ModelRuntime(value)
	// Probe DMR availability only when it matters: to offer the choice on a terminal,
	// or to gate an explicit DMR request. The default non-interactive Ollama path never
	// probes (no network, no behaviour change).
	dmrOK := false
	if interactive(emitter) || chosen == config.RuntimeDockerModelRunner {
		dmrOK = dmrAvailable()
	}
	// Only prompt when DMR is a genuine alternative; when it is unavailable the
	// single Ollama option is used silently (no needless prompt).
	if interactive(emitter) && dmrOK {
		options := make([]huh.Option[string], 0, len(config.ModelRuntimes()))
		for _, runtime := range config.ModelRuntimes() {
			options = append(options, huh.NewOption(modelRuntimeLabel(runtime), string(runtime)))
		}
		picked, err := promptChoice("Serving runtime",
			"how the model is served through the gateway (Ollama downloads it either way)",
			options, string(chosen))
		if err != nil {
			return "", err
		}
		chosen = config.ModelRuntime(picked)
	}
	if chosen == config.RuntimeDockerModelRunner && !dmrOK {
		return "", output.Errorf(output.ExitMissingDep,
			"Docker Model Runner is not available on this host — enable it (Docker Desktop → Model Runner, or `docker desktop enable model-runner`) and re-run, or use `--runtime ollama`")
	}
	return chosen, nil
}

// modelRuntimeLabel is the human label for a runtime option in the picker.
func modelRuntimeLabel(runtime config.ModelRuntime) string {
	switch runtime {
	case config.RuntimeDockerModelRunner:
		return "Docker Model Runner"
	default:
		return "Ollama (default)"
	}
}

// dmrDefaultAlias derives the gateway alias for a DMR model from its pull reference:
// the last path segment (so "hf.co/user/model:tag" → "model:tag", "qwen2.5:7b" →
// "qwen2.5:7b"). Callers may override it with an explicit --alias.
func dmrDefaultAlias(ref string) string {
	if index := strings.LastIndex(ref, "/"); index >= 0 && index+1 < len(ref) {
		return ref[index+1:]
	}
	return ref
}

// recordModelRuntimeChoice persists the serving-runtime selection for a pulled model
// (best-effort — a store-write failure must never fail the pull). The store is keyed
// by the gateway alias (Alias): for Ollama that is the pull ref, for DMR the derived
// or explicit alias; Model always carries the underlying pull ref so a later `rm`
// can find the record by model name.
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
// (not the store's Alias key) so `rm <ref>` finds a DMR choice stored under a derived
// or custom alias as well as an Ollama choice stored under the ref itself.
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
		Short: "Download one or more models into the local (Ollama) store",
		Long: "Download one or more models into the local Ollama store. With name arguments\n" +
			"(or under --json / no TTY) each given reference is pulled in turn — this is\n" +
			"the custom-reference path (e.g. `llama3.2:3b qwen2.5:7b`, or `hf.co/user/model`).\n" +
			"On a terminal with no arguments you check off any number of popular models (a\n" +
			"bundled snapshot), and may also tick \"enter custom model(s)…\" to type extra\n" +
			"references. Every selected model is pulled; the run continues past a failure\n" +
			"and reports a per-model summary. Re-pulling an installed model updates it\n" +
			"(there is no separate update command).\n\n" +
			"--runtime selects how the model is SERVED through the gateway: `ollama`\n" +
			"(default — unchanged behaviour) or `docker-model-runner`. Ollama downloads the\n" +
			"model either way; Docker Model Runner is offered only when it is available on\n" +
			"this host (never a silent fallback). --alias sets the DMR gateway alias for a\n" +
			"single model (default: the model's base name).",
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
			// no behaviour change without --runtime). An explicit DMR request against an
			// unavailable runtime is rejected here (exit 3) — never a silent fallback.
			chosenRuntime, runtimeErr := resolveModelRuntime(emitter, runtimeFlag)
			if runtimeErr != nil {
				*exit = emitter.Failure("models.pull", runtimeErr)
				return nil
			}
			// A DMR --alias identifies a single model; reject it for a multi-model pull.
			if strings.TrimSpace(aliasFlag) != "" && len(names) > 1 {
				*exit = emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput,
					"--alias applies to a single model, but %d were requested", len(names)))
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
				// The registration backend follows the chosen serving runtime; the choice
				// is recorded (best-effort) so a later `rm` de-registers the right backend.
				outcome := modelPullOutcome{Model: name, OK: true}
				supportsTools := ollamaModelSupportsTools(client, name)
				var regErr error
				if chosenRuntime == config.RuntimeDockerModelRunner {
					alias := strings.TrimSpace(aliasFlag)
					if alias == "" {
						alias = dmrDefaultAlias(name)
					}
					regErr = registrar.RegisterDockerModelRunnerModel(alias, name, supportsTools)
					if regErr == nil {
						recordModelRuntimeChoice(alias, name, config.RuntimeDockerModelRunner, litellm.DockerModelRunnerAPIBase)
					}
				} else {
					regErr = registrar.RegisterOllamaModel(name, supportsTools)
					if regErr == nil {
						recordModelRuntimeChoice(name, name, config.RuntimeOllama, "")
					}
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
		"serving runtime: ollama (default) or docker-model-runner")
	cmd.Flags().StringVar(&aliasFlag, "alias", "",
		"gateway alias for a docker-model-runner model (single model; default: the model's base name)")
	return cmd
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
			if interactive(emitter) {
				confirmed, err := promptConfirm("Remove "+name+"?",
					"this deletes the model from the local Ollama store")
				if err != nil {
					*exit = emitter.Failure("models.rm", err)
					return nil
				}
				if !confirmed {
					*exit = emitter.Failure("models.rm", output.Errorf(output.ExitInvalidInput, "cancelled"))
					return nil
				}
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
			// model, de-registering the backend that WAS recorded at install (DMR vs
			// Ollama) and clearing the runtime choice. A gateway that is down or has no
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
					var regErr error
					if choice.Runtime == config.RuntimeDockerModelRunner {
						regErr = registrar.UnregisterDockerModelRunnerModel(choice.Alias)
					} else {
						regErr = registrar.UnregisterOllamaModel(name)
					}
					if regErr != nil && result.UnregisterError == "" {
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
