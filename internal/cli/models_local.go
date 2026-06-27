package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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

// popularModelEntry is one row of `ai models popular`: one parameter-size VARIANT of
// a popular model from ollama.com/library. Name is the exact pullable ref incl. the
// size tag (e.g. qwen2.5:7b); Params is that single size (e.g. "7b", "" for a model
// with no variants); DownloadSize is that tag's download size (bytes; 0 = unknown);
// RepoURL is the model's ollama.com/library page.
type popularModelEntry struct {
	Name         string `json:"name"`
	Params       string `json:"params,omitempty"`
	DownloadSize int64  `json:"download_size,omitempty"`
	RepoURL      string `json:"repo_url"`
}

// modelsPopularResult is the `ai models popular` payload.
type modelsPopularResult struct {
	Models []popularModelEntry `json:"models"`
}

// Human renders the popular list as a NAME / PARAMS / SIZE / REPO table.
func (result modelsPopularResult) Human() string {
	if len(result.Models) == 0 {
		return ui.Muted.Render("no popular models returned")
	}
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "%s  %s  %s  %s\n",
		ui.Label.Render(fmt.Sprintf("%-24s", "NAME")),
		ui.Label.Render(fmt.Sprintf("%-22s", "PARAMS")),
		ui.Label.Render(fmt.Sprintf("%-9s", "SIZE")),
		ui.Label.Render("REPO"))
	for _, entry := range result.Models {
		params := entry.Params
		if params == "" {
			params = "-"
		}
		size := "—"
		if entry.DownloadSize > 0 {
			size = ollama.HumanByteSize(entry.DownloadSize)
		}
		_, _ = fmt.Fprintf(&builder, "%s  %s  %s  %s\n",
			ui.Value.Render(fmt.Sprintf("%-24s", entry.Name)),
			ui.Value.Render(fmt.Sprintf("%-22s", params)),
			ui.Value.Render(fmt.Sprintf("%-9s", size)),
			ui.Value.Render(entry.RepoURL))
	}
	builder.WriteString("\n" + ui.Muted.Render("pull any of these with ") +
		ui.Primary.Render("ai models pull <name>") +
		ui.Muted.Render(" (size — = unknown · bundled snapshot of ollama.com/library)"))
	return strings.TrimRight(builder.String(), "\n")
}

func toPopularEntries(models []ollama.PopularModel) []popularModelEntry {
	entries := make([]popularModelEntry, 0, len(models))
	for _, model := range models {
		entries = append(entries, popularModelEntry{
			Name:         model.Name,
			Params:       model.Parameters,
			DownloadSize: model.DownloadSize,
			RepoURL:      model.RepoURL,
		})
	}
	return entries
}

func newModelsPopularCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "popular",
		Short: "List popular installable models (bundled snapshot)",
		Long: "List popular installable models from a BUNDLED snapshot of ollama.com/library,\n" +
			"with their parameter-size variants, default-tag download size, and\n" +
			"ollama.com/library link. The list is embedded in the binary — it reads\n" +
			"instantly and OFFLINE; maintainers refresh it with `make models-refresh`.\n" +
			"Pull any of them — or any other reference — with `ai models pull`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			models, err := ollamaPopular()
			if err != nil {
				*exit = emitter.Failure("models.popular", output.Errorf(output.ExitRuntimeFailure,
					"could not read the bundled popular-models snapshot: %s", err))
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
	return &cobra.Command{
		Use:   "pull [name...]",
		Short: "Download one or more models into the local (Ollama) store",
		Long: "Download one or more models into the local Ollama store. With name arguments\n" +
			"(or under --json / no TTY) each given reference is pulled in turn — this is\n" +
			"the custom-reference path (e.g. `llama3.2:3b qwen2.5:7b`, or `hf.co/user/model`).\n" +
			"On a terminal with no arguments you check off any number of popular models (a\n" +
			"bundled snapshot), and may also tick \"enter custom model(s)…\" to type extra\n" +
			"references. Every selected model is pulled; the run continues past a failure\n" +
			"and reports a per-model summary. Re-pulling an installed model updates it\n" +
			"(there is no separate update command).",
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

			client := ollamaClient()
			registrar := modelRegistrarFactory()
			outcomes := make([]modelPullOutcome, 0, len(names))
			anyFailed := false
			var lastErr error
			for index, name := range names {
				label := fmt.Sprintf("pulling %s… (%d/%d)", name, index+1, len(names))
				pull := func() error {
					return client.Pull(name, func(progress ollama.PullProgress) {
						// hardware bring-up: a future revision can render a live byte-
						// progress bar; the spinner already reflects ongoing work.
						_ = progress
					})
				}
				var err error
				if ui.Enabled(emitter) {
					err = ui.RunWithSpinner(emitter.Err, label, pull)
				} else {
					err = pull()
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
				outcome := modelPullOutcome{Model: name, OK: true}
				if regErr := registrar.RegisterOllamaModel(name); regErr != nil {
					outcome.RegisterError = regErr.Error()
				} else {
					outcome.Registered = true
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

// promptModelsToPull presents the popular models (from the bundled snapshot) as a
// CHECKBOX multi-select plus a final "enter custom model(s)…" checkbox; ticking the
// latter prompts for free-text references (space- or comma-separated) which are added
// to the selection. If the snapshot is unavailable it falls back to the free-text
// custom-entry prompt (still multiple). Returns the de-duplicated set of references
// (possibly empty → cancelled). Only call on an interactive terminal.
func promptModelsToPull() ([]string, error) {
	popular, popularErr := ollamaPopular()
	if popularErr != nil || len(popular) == 0 {
		// The popular list is unavailable; don't block pulling — go straight to the
		// free-text custom-entry prompt (still allows multiple, space/comma separated).
		return promptCustomModels()
	}
	options := make([]huh.Option[string], 0, len(popular)+1)
	for _, candidate := range popular {
		options = append(options, huh.NewOption(popularPickerLabel(candidate), candidate.Name))
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

// popularPickerLabel formats a popular model variant as "name — size" for the pull
// picker (Name already carries the size tag, e.g. qwen2.5:7b); unknown sizes show
// "—".
func popularPickerLabel(model ollama.PopularModel) string {
	size := "—"
	if model.DownloadSize > 0 {
		size = ollama.HumanByteSize(model.DownloadSize)
	}
	return model.Name + " — " + size
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
			// model. A gateway that is down or has no master key must NOT fail the
			// removal — surface the warning in the result.
			result := modelsRmResult{Model: name}
			if regErr := modelRegistrarFactory().UnregisterOllamaModel(name); regErr != nil {
				result.UnregisterError = regErr.Error()
			}
			*exit = emitter.Success("models.rm", result)
			return nil
		},
	}
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
