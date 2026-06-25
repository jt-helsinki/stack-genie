package cli

import (
	"errors"
	"fmt"
	"sort"
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

// localModelEntry is one row of `ai models list`: a model in the local store, a
// catalog model not yet installed, or both. Installed reports the local-store
// presence; Size/Params are populated only for installed models.
type localModelEntry struct {
	Name        string `json:"name"`
	Installed   bool   `json:"installed"`
	Size        int64  `json:"size,omitempty"`
	Params      string `json:"params,omitempty"`
	Description string `json:"description,omitempty"`
}

// modelsListResult is the `ai models list` payload: the merged installed + catalog
// view of the LOCAL Ollama store.
type modelsListResult struct {
	Models []localModelEntry `json:"models"`
}

// Human renders the merged list as a NAME / STATUS / SIZE / PARAMS table.
func (result modelsListResult) Human() string {
	if len(result.Models) == 0 {
		return "no local models and an empty catalog"
	}
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "%-28s  %-10s  %-9s  %s\n", "NAME", "STATUS", "SIZE", "PARAMS")
	for _, entry := range result.Models {
		status := "available"
		if entry.Installed {
			status = "installed"
		}
		size := "-"
		if entry.Installed && entry.Size > 0 {
			size = humanByteSize(entry.Size)
		}
		params := entry.Params
		if params == "" {
			params = "-"
		}
		_, _ = fmt.Fprintf(&builder, "%-28s  %-10s  %-9s  %s\n", entry.Name, status, size, params)
	}
	builder.WriteString("\ninstalled = in the local Ollama store · available = installable with `ai models pull`")
	return strings.TrimRight(builder.String(), "\n")
}

// mergeModels merges the installed local-store models with the curated catalog
// into one sorted list: catalog models that are not installed appear as
// "available"; installed models always appear (installed-but-not-in-catalog ones
// included). It matches catalog entries to installed models by base name (the part
// before ':'), so a catalog "gemma4" is considered installed when "gemma4:31b" is.
func mergeModels(installed []ollama.Model, catalog []ollama.CatalogModel) []localModelEntry {
	byBase := make(map[string]bool, len(installed))
	entries := make([]localModelEntry, 0, len(installed)+len(catalog))
	for _, model := range installed {
		byBase[baseName(model.Name)] = true
		entries = append(entries, localModelEntry{
			Name:      model.Name,
			Installed: true,
			Size:      model.Size,
			Params:    model.ParameterSize,
		})
	}
	for _, candidate := range catalog {
		if byBase[baseName(candidate.Name)] {
			continue // already represented by an installed tag
		}
		entries = append(entries, localModelEntry{
			Name:        candidate.Name,
			Installed:   false,
			Description: candidate.Description,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Installed != entries[j].Installed {
			return entries[i].Installed // installed first
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func baseName(name string) string {
	if idx := strings.IndexByte(name, ':'); idx >= 0 {
		return name[:idx]
	}
	return name
}

func newModelsListCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed and installable local (Ollama) models",
		Long: "List models in the local Ollama store merged with the platform's curated\n" +
			"catalog of installable models. Each row is marked installed (in the store) or\n" +
			"available (installable with `ai models pull`). This manages the LOCAL model\n" +
			"store; `ai models status` describes LiteLLM routing.",
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
			result := modelsListResult{Models: mergeModels(installed, ollama.Catalog())}
			*exit = emitter.Success("models.list", result)
			return nil
		},
	}
}

// modelsPullResult is the `ai models pull` payload.
type modelsPullResult struct {
	Model string `json:"model"`
}

func (result modelsPullResult) Human() string {
	return ui.Success.Render(ui.IconOK) + " pulled " + result.Model
}

func newModelsPullCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "pull [name]",
		Short: "Download a model into the local (Ollama) store",
		Long: "Download a model into the local Ollama store. With a name argument (or under\n" +
			"--json / no TTY) the given reference is pulled directly — this is the custom-\n" +
			"reference path (e.g. `llama3.2:3b`, or `hf.co/user/model`). On a terminal with\n" +
			"no argument you pick from the curated catalog of not-yet-installed models, or\n" +
			"choose \"enter a custom model…\" to type any reference. Re-pulling an installed\n" +
			"model updates it (there is no separate update command).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			if name == "" {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput,
						"specify a model to pull (e.g. `ai models pull llama3.2`)"))
					return nil
				}
				picked, err := promptModelToPull()
				if err != nil {
					*exit = emitter.Failure("models.pull", err)
					return nil
				}
				name = picked
			}

			client := ollamaClient()
			pull := func() error {
				return client.Pull(name, func(progress ollama.PullProgress) {
					// hardware bring-up: a future revision can render a live byte-
					// progress bar; the spinner already reflects ongoing work.
					_ = progress
				})
			}
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "pulling "+name, pull)
			} else {
				err = pull()
			}
			if err != nil {
				*exit = emitter.Failure("models.pull", ollamaErr("models.pull", err))
				return nil
			}
			*exit = emitter.Success("models.pull", modelsPullResult{Model: name})
			return nil
		},
	}
}

// promptModelToPull presents the curated catalog (filtered to models not already
// installed) plus a final "enter a custom model…" option; choosing the latter
// prompts for a free-text reference. Returns the chosen/typed model name. Only call
// on an interactive terminal.
func promptModelToPull() (string, error) {
	installed, listErr := ollamaClient().List()
	// A list failure is non-fatal here: we can still offer the full catalog and the
	// custom-entry path. If Ollama is truly down, the subsequent pull will surface
	// the unreachable error with the right exit code.
	byBase := make(map[string]bool)
	if listErr == nil {
		for _, model := range installed {
			byBase[baseName(model.Name)] = true
		}
	}
	options := make([]huh.Option[string], 0)
	for _, candidate := range ollama.Catalog() {
		if byBase[baseName(candidate.Name)] {
			continue
		}
		label := candidate.Name + " — " + candidate.Description
		options = append(options, huh.NewOption(label, candidate.Name))
	}
	options = append(options, huh.NewOption("✎ enter a custom model…", customModelOption))

	choice, err := promptChoice("Model to pull",
		"pick a catalog model, or enter a custom reference (e.g. llama3.2:3b, hf.co/user/model)",
		options, options[0].Value)
	if err != nil {
		return "", err
	}
	if choice != customModelOption {
		return choice, nil
	}
	custom, err := promptText("Custom model reference",
		"the Ollama model to pull (e.g. llama3.2:3b, qwen2.5:7b, hf.co/user/model)", "",
		func(value string) error {
			if strings.TrimSpace(value) == "" {
				return errors.New("enter a model reference")
			}
			return nil
		})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(custom), nil
}

// modelsRmResult is the `ai models rm` payload.
type modelsRmResult struct {
	Model string `json:"model"`
}

func (result modelsRmResult) Human() string {
	return ui.Success.Render(ui.IconOK) + " removed " + result.Model
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
			*exit = emitter.Success("models.rm", modelsRmResult{Model: name})
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
			lines = append(lines, fmt.Sprintf("  %-12s %s", label, value))
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

// humanByteSize formats a byte count as a compact binary-unit string (e.g. 1.5 GB).
func humanByteSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
