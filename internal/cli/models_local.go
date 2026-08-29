package cli

import (
	"errors"
	"fmt"
	"io"
	goruntime "runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
	"github.com/spf13/cobra"
)

// This file adds the LOCAL model-store management subcommands to `ai models`:
// list / pull / rm / show. vLLM is the SOLE local-inference runtime; model
// weights live in the shared vLLM store (~/.ai-platform/volumes/models/
// vllm) and are managed through the Hugging Face CLI (`hf`, internal/hf). Unlike
// `ai models status`/`test` (which describe/probe LiteLLM ROUTING), these act on the
// local weight store: `pull` downloads a HF repo and serves+registers it, `rm` stops
// its server + deletes the weights + unregisters it. Registering a model in the gateway
// is NOT the same as installing it; that is what `pull` does.

// The custom-model-entry sentinel value used by the pull select.
const customModelOption = "\x00custom"

// vLLM host-side seams — package vars so tests inject fakes without touching the host.
// The real Detect/InstallGuidance/Pull live in internal/vllm; the manager (server
// lifecycle) is built lazily.
var (
	vllmDetectFn          = vllm.Detect
	vllmInstallGuidanceFn = vllm.InstallGuidance
	vllmPullFn            = vllm.Pull
	// vllmManagerFactory builds the per-model vLLM server manager. It takes the recorded
	// alias→port seed (from config/model-runtimes.yaml) so a known model REUSES its port
	// and a new model never steals a recorded one — cross-invocation port truth for a
	// daemonless CLI.
	vllmManagerFactory = func(reserved map[string]int) vllmServer {
		return vllm.NewManager(vllm.Config{
			Runner:   vllm.RealRunner{},
			Probe:    vllm.RealHealthProbe(),
			Reserved: reserved,
		})
	}
	// vllmStopByPortFn stops a detached `vllm serve` by its recorded port — the
	// cross-invocation stop path used on `ai models rm`, since a fresh Manager holds no
	// handle. A package var so tests observe it without pkill-ing a real process.
	vllmStopByPortFn = vllm.StopByPort
	// vllmInstallFn is the one-shot vLLM installer (`ai models install-vllm`): it ensures
	// the platform venv and pip-installs vLLM into it. A package var so tests exercise the
	// command without a real (large) network install.
	vllmInstallFn = vllm.Install
)

// vllmServer is the host-side vLLM server manager slice used by `ai models pull`: it
// starts/locates a per-model `vllm serve` endpoint, returning the loopback port +
// endpoint. Production binds *vllm.Manager; tests a fake. (Removal stops the detached
// server by recorded port via vllmStopByPortFn, not through this handle-less fresh
// manager.)
type vllmServer interface {
	EnsureServed(alias, model string) (port int, endpoint string, err error)
}

// recordedVLLMPorts builds the alias→port seed for the vLLM Manager from the persisted
// model-runtimes store: for each recorded runtime=vllm choice it parses the loopback port
// out of the stored Endpoint. An unreadable store or a portless endpoint contributes
// nothing (best-effort — this only optimizes port stability, never correctness).
func recordedVLLMPorts() map[string]int {
	reserved := map[string]int{}
	choices, err := config.LoadModelRuntimes()
	if err != nil {
		return reserved
	}
	for _, choice := range choices {
		if choice.Runtime != config.RuntimeVLLM {
			continue
		}
		if port, ok := vllm.PortOf(choice.Endpoint); ok {
			reserved[choice.Alias] = port
		}
	}
	return reserved
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

// vllmNotInstalledError is the exit-3 error shown when no vLLM install is present. It
// carries the per-OS install guidance so the user can act.
func vllmNotInstalledError() error {
	guidance := vllmInstallGuidanceFn(goruntime.GOOS)
	return output.Errorf(output.ExitMissingDep,
		"vLLM is not installed — required to serve local models. To install it:\n  %s",
		strings.Join(guidance, "\n  "))
}

// vllmActionable wraps a `hardware bring-up` seam error (ErrNotWired) so the user sees
// why a real vLLM run/download failed rather than a bare internal error. Any other
// error passes through.
func vllmActionable(err error) error {
	if errors.Is(err, vllm.ErrNotWired) {
		return errors.New("vLLM serving is not yet available on this host (hardware bring-up): " + err.Error())
	}
	return err
}

// pullVLLM downloads one or more Hugging Face repos into the vLLM store (via the `hf`
// CLI), starts/locates each model's per-model `vllm serve` endpoint, registers it in the
// gateway as vllm/<alias>, and records the runtime choice with the observed endpoint so a
// later run / `rm` finds it. It gates on a real vLLM install (exit 3 with guidance when
// missing). The live download + serve are `hardware bring-up`. Returns the process exit
// code.
func pullVLLM(emitter *output.Emitter, names []string, alias string) int {
	if installed, _ := vllmDetectFn(); !installed {
		return emitter.Failure("models.pull", vllmNotInstalledError())
	}
	if alias != "" && len(names) > 1 {
		return emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput,
			"--alias applies to a single model (got %d)", len(names)))
	}
	manager := vllmManagerFactory(recordedVLLMPorts())
	registrar := modelRegistrarFactory()
	store := hfClient()

	result := modelsPullResult{Pulled: make([]modelPullOutcome, 0, len(names))}
	failures := 0
	for _, model := range names {
		modelAlias := alias
		if modelAlias == "" {
			modelAlias = vllmDefaultAlias(model)
		}
		outcome := modelPullOutcome{Model: model}
		// Ensure the vLLM store exists (best-effort) then download the weights via `hf`.
		if err := vllmPullFn(model); err != nil {
			outcome.Error = vllmActionable(err).Error()
			failures++
			result.Pulled = append(result.Pulled, outcome)
			continue
		}
		// Stream `hf download`'s LIVE progress bars to stderr (not a silent spinner and
		// not discarded) so the user sees download progress; stdout stays clean for the
		// --json envelope. In the TUI this runs in the embedded terminal overlay (a PTY),
		// so hf renders its native in-place progress bars there too.
		_, _ = fmt.Fprintln(emitter.Err, "Downloading "+model+" …")
		downloadErr := store.Download(model, emitter.Err)
		if downloadErr != nil {
			outcome.Error = downloadErr.Error()
			failures++
			result.Pulled = append(result.Pulled, outcome)
			continue
		}
		port, endpoint, err := manager.EnsureServed(modelAlias, model)
		if err != nil {
			outcome.Error = vllmActionable(err).Error()
			failures++
			result.Pulled = append(result.Pulled, outcome)
			continue
		}
		outcome.OK = true
		// vLLM tool support is model-dependent and not probed here; unknown defaults to
		// capable, matching the gateway's default treatment.
		status := "registered"
		if regErr := registrar.RegisterVLLMModel(modelAlias, model, vllm.ContainerEndpoint(port), true); regErr != nil {
			outcome.RegisterError = regErr.Error()
			status = "registration-failed"
		} else {
			outcome.Registered = true
		}
		// Record the runtime choice with the LOOPBACK endpoint (host-side truth) so a
		// later `rm` can find and stop the running server. The LiteLLM CONTAINER reaches
		// the server via host.docker.internal, so the api_base REGISTERED in the gateway
		// is the CONTAINER endpoint, not the loopback. Status reflects the ACTUAL
		// registration outcome above, not an assumed success.
		recordModelRuntimeChoice(modelAlias, model, config.RuntimeVLLM, endpoint, status)
		result.Pulled = append(result.Pulled, outcome)
	}
	if failures > 0 {
		return emitter.Failure("models.pull", output.Errorf(output.ExitRuntimeFailure,
			"%d of %d model(s) failed to install", failures, len(names)).WithDetails(result))
	}
	return emitter.Success("models.pull", result)
}

// recordModelRuntimeChoice persists the serving-runtime selection for a pulled model
// (best-effort — a store-write failure must never fail the pull). The store is keyed by
// the gateway alias (Alias); Model carries the underlying HF repo id so a later `rm` can
// find the record by model name. status reflects the caller's ACTUAL outcome (e.g.
// "registered" or "registration-failed") — it must never be assumed successful.
func recordModelRuntimeChoice(alias, model string, runtime config.ModelRuntime, endpoint, status string) {
	_ = config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias:    alias,
		Model:    model,
		Runtime:  runtime,
		Endpoint: endpoint,
		Status:   status,
	})
}

// runtimeChoicesForModel returns the recorded runtime choices whose underlying Model
// matches ref (best-effort — an unreadable store yields none). It is keyed on Model
// (not the store's Alias key) so `rm <ref>` finds a choice stored under the repo id.
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
// by the HF repo id). Returns false when ref is not a recorded vLLM model.
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

// removeVLLM de-registers a vLLM-served model: it un-registers it from the gateway, stops
// its per-model `vllm serve` process, deletes the downloaded weights (`hf cache rm`), and
// clears the recorded runtime choice. All steps are best-effort; the gateway un-register
// error is surfaced in the result. Returns the process exit code.
func removeVLLM(emitter *output.Emitter, ref string, choice config.ModelRuntimeChoice) int {
	result := modelsRmResult{Model: ref}
	registrar := modelRegistrarFactory()

	repo := choice.Model
	if repo == "" {
		repo = ref
	}
	// A repo can be pulled more than once under different --alias values; every
	// recorded choice for the same underlying repo shares the same downloaded weights,
	// so ALL of them must be unregistered/stopped before the weights are deleted —
	// otherwise another alias's gateway route and vllm serve process outlive the
	// weights on disk. Fall back to just the resolved choice if none matched (should
	// not happen — choice itself always has Model == repo).
	relatedChoices := runtimeChoicesForModel(repo)
	if len(relatedChoices) == 0 {
		relatedChoices = []config.ModelRuntimeChoice{choice}
	}
	var unregisterErrs []string
	for _, recorded := range relatedChoices {
		if regErr := registrar.UnregisterVLLMModel(recorded.Alias); regErr != nil {
			unregisterErrs = append(unregisterErrs, recorded.Alias+": "+regErr.Error())
		}
		// The detached `vllm serve` process outlives every CLI invocation, so a fresh
		// Manager holds no handle to it — stop it by the recorded loopback port instead.
		if port, ok := vllm.PortOf(recorded.Endpoint); ok {
			_ = vllmStopByPortFn(port)
		}
	}
	if len(unregisterErrs) > 0 {
		result.UnregisterError = strings.Join(unregisterErrs, "; ")
	}

	// Delete the downloaded weights from the HF cache. Unlike the best-effort steps
	// above, a failure here must surface as a real error (exit 4) and leave every
	// runtime record in place — so the removal can be retried and `ai models show`
	// doesn't report the model as gone while its weights are still on disk.
	if err := hfClient().CacheRemove(repo); err != nil {
		return emitter.Failure("models.rm", hfErr(err))
	}
	for _, recorded := range relatedChoices {
		_ = config.DeleteModelRuntime(recorded.Alias)
	}
	return emitter.Success("models.rm", result)
}

// hfErr maps an hf.Client error to a runtime failure (exit 4) with the underlying
// message (the `hf` CLI surfaces its own detail).
func hfErr(err error) error {
	return output.Errorf(output.ExitRuntimeFailure, "%s", err)
}

// localModelEntry is one row of `ai models list`: a repo downloaded into the local vLLM
// store (from `hf cache ls`).
type localModelEntry struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Size      string `json:"size,omitempty"`
}

// modelsListResult is the `ai models list` payload: the locally-downloaded repos.
type modelsListResult struct {
	Models []localModelEntry `json:"models"`
}

// Human renders the installed list as a NAME / SIZE table.
func (result modelsListResult) Human() string {
	if len(result.Models) == 0 {
		return ui.Muted.Render("no models in the local store — pull one with ") +
			ui.Primary.Render("ai models pull") +
			ui.Muted.Render(" (") + ui.Primary.Render("ai models popular") +
			ui.Muted.Render(" lists installable models)")
	}
	var builder strings.Builder
	builder.WriteString(ui.Label.Render(padRight("NAME", 44)) + "  " + ui.Label.Render("SIZE") + "\n")
	for _, entry := range result.Models {
		size := entry.Size
		if size == "" {
			size = "-"
		}
		builder.WriteString(ui.Value.Render(padRight(entry.Name, 44)) + "  " + ui.Value.Render(size) + "\n")
	}
	builder.WriteString("\n" + ui.Muted.Render("installed = downloaded in the local vLLM store · ") +
		ui.Primary.Render("ai models popular") + ui.Muted.Render(" lists installable models"))
	return strings.TrimRight(builder.String(), "\n")
}

// padRight pads value with spaces to at least width runes (fixed-width table columns).
func padRight(value string, width int) string {
	if len([]rune(value)) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len([]rune(value)))
}

// installedEntries maps the locally-cached repos to list rows.
func installedEntries(cached []hf.CachedModel) []localModelEntry {
	entries := make([]localModelEntry, 0, len(cached))
	for _, model := range cached {
		entries = append(entries, localModelEntry{Name: model.Repo, Installed: true, Size: model.Size})
	}
	return entries
}

func newModelsListCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List locally-downloaded models (vLLM store, via `hf cache ls`)",
		Long: "List the model repos downloaded into the local vLLM store (`hf cache ls`).\n" +
			"The human output is a NAME / SIZE table; --json returns the list. This manages\n" +
			"the LOCAL weight store; `ai models popular` lists installable models and\n" +
			"`ai models status` describes LiteLLM routing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			store := hfClient()
			var cached []hf.CachedModel
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "listing local models", func() error {
					var workErr error
					cached, workErr = store.CacheList()
					return workErr
				})
			} else {
				cached, err = store.CacheList()
			}
			if err != nil {
				*exit = emitter.Failure("models.list", hfErr(err))
				return nil
			}
			result := modelsListResult{Models: installedEntries(cached)}
			*exit = emitter.Success("models.list", result)
			return nil
		},
	}
}

// popularModelEntry is one row of `ai models popular`: one curated, vLLM-servable
// Hugging Face repo.
type popularModelEntry struct {
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	Description string `json:"description,omitempty"`
	Size        string `json:"size,omitempty"`
}

// modelsPopularResult is the `ai models popular` payload.
type modelsPopularResult struct {
	Models []popularModelEntry `json:"models"`
}

// Human renders the curated list as a REPO / SIZE / DESCRIPTION table.
func (result modelsPopularResult) Human() string {
	if len(result.Models) == 0 {
		return ui.Muted.Render("no curated models available")
	}
	var builder strings.Builder
	builder.WriteString(ui.Label.Render(padRight("REPO", 46)) + "  " +
		ui.Label.Render(padRight("SIZE", 9)) + "  " + ui.Label.Render("DESCRIPTION") + "\n")
	for _, entry := range result.Models {
		size := entry.Size
		if size == "" {
			size = "—"
		}
		builder.WriteString(ui.Value.Render(padRight(entry.Repo, 46)) + "  " +
			ui.Value.Render(padRight(size, 9)) + "  " + ui.Value.Render(entry.Description) + "\n")
	}
	builder.WriteString("\n" + ui.Muted.Render("pull any of these with ") +
		ui.Primary.Render("ai models pull <repo>") +
		ui.Muted.Render(" (any other Hugging Face repo id also works)"))
	return strings.TrimRight(builder.String(), "\n")
}

// curatedModelsProvider is a seam over hf.CuratedModels so `ai models popular`
// tests inject a fake list without touching the network.
var curatedModelsProvider = hf.CuratedModels

// curatedEntries maps the curated hf list to popular rows.
func curatedEntries(models []hf.CuratedModel) []popularModelEntry {
	entries := make([]popularModelEntry, 0, len(models))
	for _, model := range models {
		entries = append(entries, popularModelEntry{
			Name:        model.Name,
			Repo:        model.Repo,
			Description: model.Description,
			Size:        model.Size,
		})
	}
	return entries
}

func newModelsPopularCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "popular",
		Short: "List curated installable models (vLLM-servable HF repos)",
		Long: "List the curated set of vLLM-servable models — mlx-community/* repos on\n" +
			"Apple Silicon, plain Hugging Face safetensors repos on Linux — with their\n" +
			"repo id, size, and a one-line description. This is a static curated set,\n" +
			"not a live search. Pull one with `ai models pull <repo>` (any other\n" +
			"Hugging Face repo id also works).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			models := curatedModelsProvider(goruntime.GOOS)
			result := modelsPopularResult{Models: curatedEntries(models)}
			*exit = emitter.Success("models.popular", result)
			return nil
		},
	}
	return cmd
}

// modelPullOutcome is the per-model result of a (multi-)pull.
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
				builder.WriteString("\n    " + ui.Muted.Render(
					"the model is downloaded and served locally; start the gateway with `ai setup`, then re-run this pull to register it"))
			}
		} else {
			builder.WriteString(ui.Failure.Render(ui.IconFail) + " " + ui.Value.Render(outcome.Model) + ": " + outcome.Error)
		}
	}
	return builder.String()
}

func newModelsPullCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var aliasFlag string
	cmd := &cobra.Command{
		Use:   "pull [repo...]",
		Short: "Download one or more models into the local vLLM store",
		Long: "Download one or more Hugging Face model repos into the local vLLM store (via the\n" +
			"`hf` CLI), then start and register each model's per-model vLLM server so it is\n" +
			"served through the gateway as vllm/<alias>. With repo arguments (or under\n" +
			"--json / no TTY) each given repo is pulled in turn (e.g.\n" +
			"`ai models pull mlx-community/Qwen2.5-7B-Instruct-4bit`). On a terminal with no\n" +
			"arguments you check off any number of curated models (a bundled snapshot), and\n" +
			"may also tick \"enter custom repo(s)…\" to type extra Hugging Face repo ids.\n" +
			"vLLM is the sole local runtime; --alias sets the gateway alias for a single\n" +
			"model (default: the repo id's base name).",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			names := dedupeModelNames(args)
			if len(names) == 0 {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput,
						"specify one or more models to pull (e.g. `ai models pull mlx-community/Qwen2.5-7B-Instruct-4bit`)"))
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
				*exit = emitter.Failure("models.pull", output.Errorf(output.ExitInvalidInput, "cancelled"))
				return nil
			}
			*exit = pullVLLM(emitter, names, strings.TrimSpace(aliasFlag))
			return nil
		},
	}
	cmd.Flags().StringVar(&aliasFlag, "alias", "",
		"gateway alias for the served model (single model only; default: the repo id's base name)")
	return cmd
}

// modelsInstallVLLMResult is the envelope payload for `ai models install-vllm`.
type modelsInstallVLLMResult struct {
	Specs     []string `json:"specs"`
	Installed bool     `json:"installed"`
	Format    string   `json:"format,omitempty"`
}

// newModelsInstallVLLMCmd builds `ai models install-vllm` — the one-shot installer that
// provisions the platform-managed host venv (~/.ai-platform/venv) and pip-installs vLLM
// into it. With no --spec it uses the per-OS default (vllm-metal on Apple Silicon, plain
// vllm on Linux); --spec (repeatable) overrides that entirely. The install is a large
// network download, so it runs behind a spinner and is never triggered by `ai setup`
// (setup's best-effort auto-install is separate).
func newModelsInstallVLLMCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var specFlags []string
	cmd := &cobra.Command{
		Use:   "install-vllm",
		Short: "Install the vLLM backend into the platform-managed host venv",
		Long: "Install vLLM into the platform-managed host Python venv (~/.ai-platform/venv)\n" +
			"so `ai models pull` can serve local models. With no --spec the per-OS default\n" +
			"is used (the vLLM-Metal plugin on Apple Silicon, the plain `vllm` package on\n" +
			"Linux); pass --spec (repeatable) to override the pip requirement(s) exactly.\n" +
			"This is a large network download.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			specs := specFlags
			if len(specs) == 0 {
				specs = vllm.InstallSpecs(goruntime.GOOS)
			}
			install := func() error { return vllmInstallFn(specs...) }
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "installing vLLM into the platform venv (this can take a while)", install)
			} else {
				err = install()
			}
			if err != nil {
				*exit = emitter.Failure("models.install-vllm", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			installed, kind := vllmDetectFn()
			result := modelsInstallVLLMResult{Specs: specs, Installed: installed, Format: kind}
			*exit = emitter.Success("models.install-vllm", result)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&specFlags, "spec", nil,
		"pip requirement to install instead of the per-OS default (repeatable)")
	return cmd
}

// hfNotInstalledError reports that the `hf` CLI is unavailable, with guidance: `hf`
// installs into the platform-managed venv at `ai setup`. Exit 3 (missing dependency).
func hfNotInstalledError() error {
	return output.Errorf(output.ExitMissingDep,
		"the Hugging Face CLI (`hf`) is not installed — it is required to authenticate for\n"+
			"gated repos. Run `ai setup` to install it into the platform-managed venv\n"+
			"(~/.ai-platform/venv), or `ai models install-vllm` provisions the same venv.")
}

// modelsLoginResult is the `ai models login` payload.
type modelsLoginResult struct {
	LoggedIn bool   `json:"logged_in"`
	User     string `json:"user,omitempty"`
}

func (result modelsLoginResult) Human() string {
	if result.User != "" {
		return ui.Success.Render(ui.IconOK) + " logged in to Hugging Face as " + ui.Value.Render(result.User)
	}
	return ui.Success.Render(ui.IconOK) + " logged in to Hugging Face"
}

// newModelsLoginCmd builds `ai models login` — authenticate the `hf` CLI with a Hugging
// Face token so gated repos (meta-llama/*, google/gemma-*, mistralai/*) can be pulled.
// The token is a HIDDEN CREDENTIAL: --token/--stdin are used directly (a hidden field
// can't pre-seed), otherwise it is prompted hidden on a TTY. The token is handed only to
// `hf`, which owns its own store — the platform never persists it.
func newModelsLoginCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var token string
	var fromStdin bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate the Hugging Face CLI to download gated repos",
		Long: "Authenticate the Hugging Face CLI (`hf`) with an access token so gated repos\n" +
			"(meta-llama/*, google/gemma-*, mistralai/*) can be pulled. On a terminal you are\n" +
			"prompted for the token (hidden); under --json/no TTY pass it via --token/--stdin.\n" +
			"The token is stored by `hf` itself (~/.cache/huggingface), never on platform disk.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !hfDetectFn() {
				*exit = emitter.Failure("models.login", hfNotInstalledError())
				return nil
			}
			tok, ok := resolveHFToken(cmd, emitter, token, fromStdin, exit)
			if !ok {
				return nil
			}
			if err := hfClient().Login(tok); err != nil {
				*exit = emitter.Failure("models.login", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			// Best-effort whoami for the confirmation line — a whoami error must not fail
			// a login that just succeeded.
			result := modelsLoginResult{LoggedIn: true}
			if user, err := hfClient().Whoami(); err == nil {
				result.User = strings.TrimSpace(user)
			}
			*exit = emitter.Success("models.login", result)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "Hugging Face access token (discouraged — leaks to shell history; prefer --stdin)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the Hugging Face token from stdin")
	return cmd
}

// resolveHFToken reads the HF token from --token/--stdin, else prompts hidden on a TTY.
// It returns ok=false and sets *exit (a failure envelope already emitted) when no token
// can be obtained. The token is trimmed and never echoed.
func resolveHFToken(cmd *cobra.Command, emitter *output.Emitter, token string, fromStdin bool, exit *int) (string, bool) {
	switch {
	case fromStdin:
		read, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			*exit = emitter.Failure("models.login", output.Errorf(output.ExitRuntimeFailure, "read stdin: %s", err))
			return "", false
		}
		if trimmed := strings.TrimSpace(string(read)); trimmed != "" {
			return trimmed, true
		}
		*exit = emitter.Failure("models.login", output.Errorf(output.ExitInvalidInput, "the Hugging Face token is empty"))
		return "", false
	case strings.TrimSpace(token) != "":
		return strings.TrimSpace(token), true
	}
	if interactive(emitter) {
		entered, err := promptSecret(
			"Hugging Face token",
			"hidden — stored by `hf` (~/.cache/huggingface), never on platform disk",
			func(candidate string) error {
				if strings.TrimSpace(candidate) == "" {
					return errors.New("a Hugging Face token is required")
				}
				return nil
			})
		if err != nil {
			*exit = emitter.Failure("models.login", err)
			return "", false
		}
		return strings.TrimSpace(entered), true
	}
	*exit = emitter.Failure("models.login", output.Errorf(output.ExitInvalidInput,
		"provide the Hugging Face token with --token or --stdin"))
	return "", false
}

// modelsLogoutResult is the `ai models logout` payload.
type modelsLogoutResult struct {
	LoggedOut bool `json:"logged_out"`
}

func (modelsLogoutResult) Human() string {
	return ui.Success.Render(ui.IconOK) + " logged out of Hugging Face"
}

// newModelsLogoutCmd builds `ai models logout` — clear the `hf` CLI's stored Hugging
// Face credentials.
func newModelsLogoutCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Clear the Hugging Face CLI's stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !hfDetectFn() {
				*exit = emitter.Failure("models.logout", hfNotInstalledError())
				return nil
			}
			if err := hfClient().Logout(); err != nil {
				*exit = emitter.Failure("models.logout", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("models.logout", modelsLogoutResult{LoggedOut: true})
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

// promptModelsToPull presents the curated model list — one checkbox per repo — plus a
// final "enter custom repo(s)…" checkbox; ticking the latter prompts for free-text repo
// ids which are added to the selection. Returns the de-duplicated set of repo ids
// (possibly empty → cancelled). Only call on an interactive terminal.
func promptModelsToPull() ([]string, error) {
	curated := hf.CuratedModels(goruntime.GOOS)
	if len(curated) == 0 {
		return promptCustomModels()
	}
	options := make([]huh.Option[string], 0, len(curated)+1)
	for _, model := range curated {
		label := model.Repo
		if model.Size != "" {
			label += "  (" + model.Size + ")"
		}
		options = append(options, huh.NewOption(label, model.Repo))
	}
	options = append(options, huh.NewOption("✎ enter custom repo(s)…", customModelOption))

	selected, err := promptMultiChoice("Models to pull",
		"check any number; tick ✎ to also type custom Hugging Face repo ids",
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

// promptCustomModels asks for one or more free-text Hugging Face repo ids (the custom-
// entry path), space- or comma-separated. Returns the parsed, de-duplicated repo ids.
func promptCustomModels() ([]string, error) {
	custom, err := promptText("Custom model repo(s)",
		"space- or comma-separated Hugging Face repo ids to pull (e.g. mlx-community/Qwen2.5-7B-Instruct-4bit)", "",
		func(value string) error {
			if len(parseModelRefs(value)) == 0 {
				return errors.New("enter at least one repo id")
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return parseModelRefs(custom), nil
}

// parseModelRefs splits a free-text entry into repo ids on whitespace and commas.
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
		Use:     "rm [repo]",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a model from the local vLLM store",
		Long: "Remove a model from the local vLLM store: stop its per-model vLLM server,\n" +
			"de-register it from the gateway, and delete its downloaded weights\n" +
			"(`hf cache rm`). On a terminal with no argument you pick from the recorded\n" +
			"models; with a repo argument (or under --json) that model is removed. On a\n" +
			"terminal you are asked to confirm before deleting.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			if name == "" {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.rm", output.Errorf(output.ExitInvalidInput,
						"specify a model to remove (e.g. `ai models rm Qwen2.5-7B-Instruct-4bit`)"))
					return nil
				}
				picked, err := promptRecordedModel()
				if err != nil {
					*exit = emitter.Failure("models.rm", err)
					return nil
				}
				name = picked
			}
			choice, isVLLM := vllmChoiceForRef(name)
			if interactive(emitter) {
				confirmed, err := promptConfirm("Remove "+name+"?",
					"this stops the vLLM server, de-registers the model from the gateway, and deletes its weights")
				if err != nil {
					*exit = emitter.Failure("models.rm", err)
					return nil
				}
				if !confirmed {
					*exit = emitter.Failure("models.rm", output.Errorf(output.ExitInvalidInput, "cancelled"))
					return nil
				}
			}
			if isVLLM {
				*exit = removeVLLM(emitter, name, choice)
				return nil
			}
			// Not a recorded vLLM model: best-effort remove the HF cache entry + unregister
			// by alias/base name so a stray download/registration is still cleanable.
			result := modelsRmResult{Model: name}
			registrar := modelRegistrarFactory()
			if regErr := registrar.UnregisterVLLMModel(vllmDefaultAlias(name)); regErr != nil {
				result.UnregisterError = regErr.Error()
			}
			var err error
			if ui.Enabled(emitter) {
				err = ui.RunWithSpinner(emitter.Err, "removing "+name, func() error { return hfClient().CacheRemove(name) })
			} else {
				err = hfClient().CacheRemove(name)
			}
			if err != nil {
				*exit = emitter.Failure("models.rm", hfErr(err))
				return nil
			}
			*exit = emitter.Success("models.rm", result)
			return nil
		},
	}
}

// promptRecordedModel asks the user to pick one of the recorded local models (by gateway
// alias). Only call on an interactive terminal.
func promptRecordedModel() (string, error) {
	choices, err := config.LoadModelRuntimes()
	if err != nil || len(choices) == 0 {
		return "", output.Errorf(output.ExitInvalidInput,
			"no models in the local store — pull one with `ai models pull`")
	}
	options := make([]huh.Option[string], 0, len(choices))
	initial := ""
	for _, choice := range choices {
		if initial == "" {
			initial = choice.Alias
		}
		label := choice.Alias
		if choice.Model != "" && choice.Model != choice.Alias {
			label += "  (" + choice.Model + ")"
		}
		options = append(options, huh.NewOption(label, choice.Alias))
	}
	return promptChoice("Model to remove", "remove this model from the local vLLM store", options, initial)
}

// modelsShowResult is the `ai models show` payload: the recorded runtime choice + any
// curated metadata for the model.
type modelsShowResult struct {
	Alias       string `json:"alias,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
	Size        string `json:"size,omitempty"`
	Found       bool   `json:"found"`
}

func (result modelsShowResult) Human() string {
	if !result.Found {
		return ui.Muted.Render("no local record for that model — pull one with ") + ui.Primary.Render("ai models pull")
	}
	name := result.Alias
	if name == "" {
		name = result.Repo
	}
	lines := []string{ui.Heading.Render(name)}
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, "  "+ui.Label.Render(padRight(label, 12))+" "+ui.Value.Render(value))
		}
	}
	add("repo", result.Repo)
	add("runtime", result.Runtime)
	add("endpoint", result.Endpoint)
	add("status", result.Status)
	add("size", result.Size)
	add("about", result.Description)
	return strings.Join(lines, "\n")
}

// curatedByRepo returns the curated metadata for a repo id (or short name), if any.
func curatedByRepo(repo string) (hf.CuratedModel, bool) {
	for _, model := range hf.CuratedModels(goruntime.GOOS) {
		if model.Repo == repo || model.Name == repo {
			return model, true
		}
	}
	return hf.CuratedModel{}, false
}

func newModelsShowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show metadata for a local model",
		Long: "Show what the platform knows about a local model: its recorded runtime choice\n" +
			"(gateway alias, repo id, serving endpoint, status) plus any curated description\n" +
			"and size. Accepts the gateway alias or the Hugging Face repo id.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			if name == "" {
				if !interactive(emitter) {
					*exit = emitter.Failure("models.show", output.Errorf(output.ExitInvalidInput,
						"specify a model to show (e.g. `ai models show Qwen2.5-7B-Instruct-4bit`)"))
					return nil
				}
				picked, err := promptRecordedModel()
				if err != nil {
					*exit = emitter.Failure("models.show", err)
					return nil
				}
				name = picked
			}
			result := modelsShowResult{}
			if choice, ok := vllmChoiceForRef(name); ok {
				result.Found = true
				result.Alias = choice.Alias
				result.Repo = choice.Model
				result.Runtime = string(choice.Runtime)
				result.Endpoint = choice.Endpoint
				result.Status = choice.Status
			}
			lookupRepo := result.Repo
			if lookupRepo == "" {
				lookupRepo = name
			}
			if curated, ok := curatedByRepo(lookupRepo); ok {
				result.Found = true
				if result.Repo == "" {
					result.Repo = curated.Repo
				}
				result.Description = curated.Description
				result.Size = curated.Size
			}
			*exit = emitter.Success("models.show", result)
			return nil
		},
	}
}
