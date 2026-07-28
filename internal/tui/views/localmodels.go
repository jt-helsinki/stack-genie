package views

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/mattn/go-runewidth"
)

// LocalModelLister returns the models in the local Ollama store. Injected; the
// parent wires ollama.RealClient().List.
type LocalModelLister func() ([]ollama.Model, error)

// LibraryLister returns the installable Ollama library (live, cache-backed) plus the
// data source so the view can message availability. Injected; the parent wires
// ollama.Library.
type LibraryLister func() ([]ollama.LibraryModel, ollama.Source, error)

// ModelShowFetcher returns the full /api/show detail for one local model. Injected;
// the parent wires ollama.RealClient().Show. Nil is tolerated (the describe pane then
// reports detail is unavailable).
type ModelShowFetcher func(model string) (ollama.ModelInfo, error)

// localModel is one model row of the Local Models view: a library model (or a
// synthesized row for an installed custom not in the library) plus the set of its
// tags that are installed locally. The list shows NAME · DESCRIPTION (the tags are
// shown only in the per-model drill-down); enter drills into the per-tag picker.
type localModel struct {
	name        string                       // base model name (e.g. "qwen2.5")
	description string                       // library description ("" for a synthesized custom)
	tags        []string                     // all known tags (library tags ∪ installed tags)
	installed   map[string]bool              // which of tags are installed locally
	sizes       map[string]int64             // on-disk byte size of installed tags
	tagInfo     map[string]ollama.LibraryTag // library size/context/input by short tag
	// runtime marks a row's serving backend. Empty = the default Ollama row (a library
	// model with tags); RuntimeVLLM marks a curated vLLM starter row (a single model id,
	// no ollama-style tags) that pulls directly via `enter` rather than drilling.
	runtime config.ModelRuntime
}

// anyInstalled reports whether at least one of the model's tags is installed.
func (model localModel) anyInstalled() bool {
	for _, tag := range model.tags {
		if model.installed[tag] {
			return true
		}
	}
	return false
}

// localModelsRefreshedMsg carries the installed store + the library load result.
type localModelsRefreshedMsg struct {
	installed  []ollama.Model
	library    []ollama.LibraryModel
	source     ollama.Source
	listErr    error
	libraryErr error
}

// LocalModelSyncer registers the installed Ollama models with the LiteLLM gateway
// (idempotent, ADD-only), returning the model_names it newly registered. Injected; the
// parent wires it over litellm.KeyManager.RegisterOllamaModels(installed). It is how the
// Local Models `r` refresh makes installed models available in the gateway — the
// service-tier analogue of what `ai models pull` does at pull time. A nil syncer skips
// the step (tests that don't exercise it).
type LocalModelSyncer func() ([]string, error)

// localModelsSyncedMsg reports the outcome of the register-installed-into-LiteLLM step
// that runs alongside the `r` refresh.
type localModelsSyncedMsg struct {
	added []string
	err   error
}

// runtimePicker is the lightweight in-view sub-state shown AFTER the user has ticked
// tags to pull: a single-choice list of install ENGINES (Ollama), mirroring the CLI
// `ai models pull --runtime`. enter emits the pull with the chosen runtime; esc
// reopens the tag drill-down so the tick selection isn't lost.
type runtimePicker struct {
	drill   *tagPicker // the drill to restore on esc (keeps the tick selection)
	refs    []string   // the name:tag refs to pull
	options []config.ModelRuntime
	cursor  int // index into options (0 = Ollama, the default)
}

// currentRuntime returns the runtime under the picker cursor.
func (picker *runtimePicker) currentRuntime() config.ModelRuntime {
	if picker.cursor < 0 || picker.cursor >= len(picker.options) {
		return config.RuntimeOllama
	}
	return picker.options[picker.cursor]
}

// tagPicker is the in-view drill-down sub-state: the selected model's tags, each
// installed or not, with a moving cursor and a multi-select of NOT-installed tags to
// pull. esc backs out to the list.
type tagPicker struct {
	model    localModel
	tags     []string        // the model's tags, in display order
	cursor   int             // index into tags
	scroll   int             // first visible tag row (the list scrolls to keep cursor visible)
	selected map[string]bool // not-installed tags ticked for pull
}

// Column widths for the NAME · DESCRIPTION list. NAME is fixed; DESCRIPTION takes
// everything left over (the widest column). There is no TAGS column — tags are shown
// in the per-model drill-down (Enter). The row sums to the full pane width so the
// highlight bar fills the row with no wrap (see contentLine).
const localNameWidth = 26

// LocalModels is the Local Models tab: the local Ollama store (installed) + the
// installable ollama.com library, grouped into an "Installed" section (library models
// with ≥1 pulled tag, plus installed customs) and an "Installable" section (the rest).
// The two sections are rendered as a custom viewport-windowed list (NOT a bubbles
// table) so each section header can be bold + accent-coloured with blank-line padding
// above and below — a styled header cannot live in a single-line, ANSI-unaware table
// cell. One cursor spans both sections over MODEL ROWS ONLY (it never lands on a
// header or padding line); the highlighted model row is rendered with the SAME style
// as ui.TableStyles().Selected, stretched to the full pane width so it looks identical
// to the Cloud Models highlight. enter drills into a per-model tag picker to
// pull/remove/test individual tags. Keys: enter manage, r refresh, t test, d remove.
type LocalModels struct {
	test     ModelTester
	list     LocalModelLister
	library  LibraryLister // CACHE-FIRST load (normal open); no network when cached
	refresh  LibraryLister // FORCE re-scrape (the `r` key); nil falls back to library
	show     ModelShowFetcher
	syncGway LocalModelSyncer // registers installed models into LiteLLM on `r` (nilable)

	describe describePane
	drill    *tagPicker
	runtime  *runtimePicker // the post-selection install-engine choice (nil when inactive)

	models         []localModel // every model row (installed, then Ollama installable, then vLLM)
	installedCount int          // how many of models are in the Installed section
	vllmStart      int          // index in models where the vLLM installable section begins
	window         listWindow   // the reusable windowed-list layout + anchor scroll
	installed      map[string]bool

	// vllmGoos, when non-empty, turns on the curated "Installable (vLLM)" section for
	// that host platform (MLX ids on darwin, HF safetensors ids on linux). Off by default
	// so views built without a platform — unit tests focused on the Ollama sections — show
	// no vLLM rows; production wires it via EnableVLLMSection.
	vllmGoos string

	source     ollama.Source
	libraryErr error
	listErr    error

	width  int
	height int
	flash  string
	loaded bool
	// syncFlash marks that the flash holds the gateway-register result (from the `r`
	// sync), so the concurrent display-refresh handler does not clear it — whichever of
	// the two batched commands lands last, the sync result survives.
	syncFlash bool
}

// NewLocalModels builds the Local Models view over the injected installed-store
// lister, the CACHE-FIRST library lister (normal open), the FORCE-refresh library
// lister (the `r` key; may be nil to reuse library), the per-model /api/show
// fetcher, and the gateway tester.
func NewLocalModels(list LocalModelLister, library, refresh LibraryLister, show ModelShowFetcher, test ModelTester, syncGway LocalModelSyncer) *LocalModels {
	return &LocalModels{list: list, library: library, refresh: refresh, show: show, test: test, syncGway: syncGway, describe: newDescribePane()}
}

// EnableVLLMSection turns on the curated "Installable (vLLM)" section for the given host
// platform (goos): mlx-community ids on darwin, Hugging Face safetensors ids on linux.
// Called once by the production wiring; unit tests that focus on the Ollama sections
// leave it off so their row counts are unaffected. Takes effect on the next build.
func (view *LocalModels) EnableVLLMSection(goos string) { view.vllmGoos = goos }

func (view *LocalModels) Title() string { return "Local Models" }

func (view *LocalModels) Hints() string {
	return "↑/↓ select · enter manage tags · t test · d remove · r refresh"
}

// SetSize records the pane dimensions and fits the describe pane.
func (view *LocalModels) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.syncWindow()
	view.describe.setSize(width, height)
}

// windowLines flattens the two sections into the full sequence of rendered lines for
// the reusable listWindow: each present section's header (blank · header · blank)
// followed by its model rows (marked Selectable). The cursor highlight + windowing is
// handled by the listWindow; this only produces the styled line text.
// sectionHeadingStyle is the bold, secondary-coloured style for the "Installed" /
// "Installable" section headers — distinct from ui.Heading (the accent colour) so the
// section labels read as the secondary colour.
func sectionHeadingStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(ui.Secondary())
}

func (view *LocalModels) windowLines() []ListLine {
	lines := make([]ListLine, 0, len(view.models)+6)
	appendSection := func(label string, from, to int) {
		if from >= to {
			return
		}
		lines = append(lines,
			ListLine{Text: ""},
			ListLine{Text: sectionHeadingStyle().Render(label)},
			ListLine{Text: ""},
		)
		for index := from; index < to; index++ {
			lines = append(lines, ListLine{Text: view.contentLine(view.models[index]), Selectable: true})
		}
	}
	appendSection("Installed", 0, view.installedCount)
	appendSection("Installable", view.installedCount, view.vllmStart)
	appendSection("Installable (vLLM)", view.vllmStart, len(view.models))
	return lines
}

// syncWindow pushes the current rendered lines + pane geometry into the listWindow,
// re-clamping the cursor + scroll position. Called before any move/clamp/render so the
// window always reflects the latest data + size (header line count can change with
// errors/empties, so the list height is recomputed here each time).
func (view *LocalModels) syncWindow() {
	view.window.SetContent(view.windowLines(), view.width, view.listHeight())
}

// Init kicks off the first install-store list + library load (CACHE-FIRST — a
// cached library is used as-is, so opening the tab never blocks on a live scrape).
func (view *LocalModels) Init() tea.Cmd { return view.listCmd(false) }

// listCmd lists the installed store and loads the library. When force is false the
// library load is CACHE-FIRST (no network when cached); when true (the `r` key) it
// FORCE re-scrapes via the refresh lister. Each side degrades independently.
func (view *LocalModels) listCmd(force bool) tea.Cmd {
	list := view.list
	library := view.library
	if force && view.refresh != nil {
		library = view.refresh
	}
	return func() tea.Msg {
		var msg localModelsRefreshedMsg
		if list != nil {
			msg.installed, msg.listErr = list()
		}
		if library != nil {
			msg.library, msg.source, msg.libraryErr = library()
		}
		return msg
	}
}

// syncCmd registers the installed Ollama models with the gateway (ADD-only). A nil
// syncer (tests) yields no command, so no sync flash appears.
func (view *LocalModels) syncCmd() tea.Cmd {
	syncGway := view.syncGway
	if syncGway == nil {
		return nil
	}
	return func() tea.Msg {
		added, err := syncGway()
		return localModelsSyncedMsg{added: added, err: err}
	}
}

// localSyncFlash renders the outcome of the gateway-register step.
func localSyncFlash(msg localModelsSyncedMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " register local models with the gateway: " + msg.err.Error())
	}
	if len(msg.added) == 0 {
		return ui.Muted.Render("local models already registered with the gateway")
	}
	return ui.Success.Render(ui.IconOK + fmt.Sprintf(" registered %d local model(s) with the gateway", len(msg.added)))
}

func (view *LocalModels) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// Update advances the view: a refresh rebuilds the two sections; the drill-down
// owns keys while open; otherwise the action keys + list navigation apply.
func (view *LocalModels) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case localModelsRefreshedMsg:
		view.loaded = true
		view.listErr = message.listErr
		view.libraryErr = message.libraryErr
		view.source = message.source
		view.buildModels(message.installed, message.library)
		// Clear the transient "refreshing…" notice; libraryFlash re-sets a
		// source-availability warning only when the live library fetch failed. But do NOT
		// clobber the gateway-register result (the `r` sync runs concurrently).
		if !view.syncFlash {
			view.flash = ""
			view.libraryFlash()
		}
		view.syncWindow()
		return nil
	case localModelsSyncedMsg:
		view.syncFlash = true
		view.flash = localSyncFlash(message)
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return nil
	case tea.KeyMsg:
		return view.handleKey(message)
	}
	return nil
}

// handleKey routes a key: the drill-down first (when open), then the describe pane,
// then the list actions / navigation.
func (view *LocalModels) handleKey(key tea.KeyMsg) tea.Cmd {
	if view.runtime != nil {
		return view.handleRuntimeKey(key)
	}
	if view.drill != nil {
		return view.handleDrillKey(key)
	}
	if view.describe.active() {
		return view.describe.update(key)
	}
	switch key.String() {
	case "enter":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("no model selected")
			return nil
		}
		// A curated vLLM row has no ollama-style tags to drill into — enter pulls it
		// directly under the vLLM runtime (`ai models pull --runtime vllm <id>`).
		if model.runtime == config.RuntimeVLLM {
			ref := model.name
			return func() tea.Msg {
				return ModelsPullRequestedMsg{Refs: []string{ref}, Runtime: string(config.RuntimeVLLM)}
			}
		}
		view.openDrill(model)
		return nil
	case "t":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to test")
			return nil
		}
		ref := view.firstTestRef(model)
		if ref == "" {
			view.flash = ui.Muted.Render(model.name + " has no installed tag to test (enter to pull one)")
			return nil
		}
		view.flash = ui.Muted.Render("testing " + ref + "…")
		return view.testCmd(ref)
	case "d":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to remove")
			return nil
		}
		ref := view.firstInstalledRef(model)
		if ref == "" {
			view.flash = ui.Muted.Render(model.name + " is not installed (enter to pull a tag)")
			return nil
		}
		return func() tea.Msg { return ModelRemoveRequestedMsg{Name: ref} }
	case "r":
		// Refresh the display AND register the installed Ollama models with the gateway
		// (ADD-only) so `r` makes local models available in LiteLLM — recovering any that
		// drifted out. Both run concurrently; the sync result shows as a flash.
		view.syncFlash = false
		view.flash = ui.Muted.Render("refreshing + registering local models with the gateway…")
		return tea.Batch(view.listCmd(true), view.syncCmd())
	case "up", "k":
		view.moveCursor(-1)
		return nil
	case "down", "j":
		view.moveCursor(1)
		return nil
	}
	return nil
}

// firstTestRef returns the first installed tag's full ref (name:tag) for a quick `t`
// test from the list; "" when none is installed.
func (view *LocalModels) firstTestRef(model localModel) string {
	for _, tag := range model.tags {
		if model.installed[tag] {
			return model.name + ":" + tag
		}
	}
	return ""
}

// firstInstalledRef is firstTestRef under another name (the first installed ref to
// remove from the list `d`).
func (view *LocalModels) firstInstalledRef(model localModel) string { return view.firstTestRef(model) }

// --- drill-down -------------------------------------------------------------

// openDrill enters the per-model tag picker for model.
func (view *LocalModels) openDrill(model localModel) {
	view.drill = &tagPicker{
		model:    model,
		tags:     model.tags,
		cursor:   0,
		selected: map[string]bool{},
	}
	view.describe.close()
}

// handleDrillKey routes keys while the tag picker is open.
func (view *LocalModels) handleDrillKey(key tea.KeyMsg) tea.Cmd {
	drill := view.drill
	switch key.String() {
	case "esc":
		view.drill = nil
		return nil
	case "up", "k":
		if drill.cursor > 0 {
			drill.cursor--
		}
		return nil
	case "down", "j":
		if drill.cursor < len(drill.tags)-1 {
			drill.cursor++
		}
		return nil
	case " ":
		tag := drill.currentTag()
		if tag == "" {
			return nil
		}
		if drill.model.installed[tag] {
			view.flash = ui.Muted.Render(tag + " is already installed (d removes it)")
			return nil
		}
		drill.selected[tag] = !drill.selected[tag]
		return nil
	case "enter", "p":
		refs := drill.selectedRefs()
		if len(refs) == 0 {
			// No tags ticked: if the cursor sits on an INSTALLED tag, open its
			// /api/show detail; otherwise prompt to select tags to pull.
			tag := drill.currentTag()
			if tag != "" && drill.model.installed[tag] {
				view.describe.show(view.describeTag(drill.model.name + ":" + tag))
				view.drill = nil
				return nil
			}
			view.flash = ui.Muted.Render("select 1+ tags (space) to pull")
			return nil
		}
		// Tags are ticked: choose the install ENGINE (Ollama) before emitting the
		// pull, mirroring the CLI `--runtime`.
		view.openRuntimePicker(drill, refs)
		return nil
	case "d":
		tag := drill.currentTag()
		if tag == "" || !drill.model.installed[tag] {
			view.flash = ui.Muted.Render("only an installed tag (●) can be removed")
			return nil
		}
		ref := drill.model.name + ":" + tag
		view.drill = nil
		return func() tea.Msg { return ModelRemoveRequestedMsg{Name: ref} }
	case "t":
		tag := drill.currentTag()
		if tag == "" || !drill.model.installed[tag] {
			view.flash = ui.Muted.Render("only an installed tag (●) can be tested")
			return nil
		}
		ref := drill.model.name + ":" + tag
		view.flash = ui.Muted.Render("testing " + ref + "…")
		return view.testCmd(ref)
	}
	return nil
}

// --- runtime picker ---------------------------------------------------------

// openRuntimePicker leaves the drill-down and opens the install-engine choice for the
// ticked refs (Ollama seated as the default). The drill is stashed so esc restores it
// with the tick selection intact.
func (view *LocalModels) openRuntimePicker(drill *tagPicker, refs []string) {
	view.drill = nil
	view.runtime = &runtimePicker{
		drill:   drill,
		refs:    refs,
		options: config.ModelRuntimes(),
		cursor:  0,
	}
}

// handleRuntimeKey routes keys while the install-engine picker is open: up/down move,
// enter emits the pull with the chosen runtime, esc reopens the tag drill-down.
func (view *LocalModels) handleRuntimeKey(key tea.KeyMsg) tea.Cmd {
	picker := view.runtime
	switch key.String() {
	case "esc":
		view.runtime = nil
		view.drill = picker.drill // restore the drill (tick selection preserved)
		return nil
	case "up", "k":
		if picker.cursor > 0 {
			picker.cursor--
		}
		return nil
	case "down", "j":
		if picker.cursor < len(picker.options)-1 {
			picker.cursor++
		}
		return nil
	case "enter", "p":
		refs := picker.refs
		runtime := string(picker.currentRuntime())
		view.runtime = nil
		return func() tea.Msg { return ModelsPullRequestedMsg{Refs: refs, Runtime: runtime} }
	}
	return nil
}

// runtimePickerLabel is the human label for an install-engine option.
func runtimePickerLabel(runtime config.ModelRuntime) string {
	switch runtime {
	case config.RuntimeOllama:
		return "Ollama"
	case config.RuntimeVLLM:
		return "vLLM"
	default:
		return string(runtime)
	}
}

// runtimeView renders the install-engine picker: one row per runtime, the cursor row
// marked with the same "› " caret idiom as the tag drill-down.
func (view *LocalModels) runtimeView() string {
	picker := view.runtime
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Install engine for "+strings.Join(picker.refs, ", ")) + "\n")
	body.WriteString(ui.Muted.Render("↑/↓ select · enter pull · esc back") + "\n\n")
	for index, runtime := range picker.options {
		line := runtimePickerLabel(runtime)
		if index == picker.cursor {
			line = ui.Primary.Bold(true).Render("› ") + line
		} else {
			line = "  " + line
		}
		body.WriteString(line + "\n")
	}
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// currentTag returns the tag under the picker cursor ("" when empty).
func (picker *tagPicker) currentTag() string {
	if picker.cursor < 0 || picker.cursor >= len(picker.tags) {
		return ""
	}
	return picker.tags[picker.cursor]
}

// selectedRefs returns the ticked not-installed tags as full name:tag references.
func (picker *tagPicker) selectedRefs() []string {
	refs := make([]string, 0, len(picker.selected))
	for _, tag := range picker.tags {
		if picker.selected[tag] {
			refs = append(refs, picker.model.name+":"+tag)
		}
	}
	return refs
}

// --- data build -------------------------------------------------------------

// buildModels groups the installed store + the library into Installed-first then
// Installable rows and seats the cursor on the first model row.
func (view *LocalModels) buildModels(installed []ollama.Model, library []ollama.LibraryModel) {
	// Index installed tags by base name.
	installedTags := make(map[string]map[string]bool)
	installedSizes := make(map[string]map[string]int64)
	view.installed = make(map[string]bool, len(installed))
	for _, model := range installed {
		view.installed[model.Name] = true
		base, tag := splitRef(model.Name)
		if installedTags[base] == nil {
			installedTags[base] = map[string]bool{}
			installedSizes[base] = map[string]int64{}
		}
		installedTags[base][tag] = true
		installedSizes[base][tag] = model.Size
	}

	seen := make(map[string]bool, len(library))
	rows := make([]localModel, 0, len(library)+len(installedTags))
	for _, libModel := range library {
		seen[libModel.Name] = true
		tags := libModel.TagNames()
		tagInfo := make(map[string]ollama.LibraryTag, len(libModel.Tags))
		for _, tag := range libModel.Tags {
			tagInfo[tag.Name] = tag
		}
		instTags := installedTags[libModel.Name]
		// Fold in any installed tag the library doesn't list (a custom tag of a
		// library model), so installed tags always show in the drill-down.
		for tag := range instTags {
			if !containsString(tags, tag) {
				tags = append(tags, tag)
			}
		}
		rows = append(rows, localModel{
			name:        libModel.Name,
			description: libModel.Description,
			tags:        sortTags(tags),
			installed:   copyBoolMap(instTags),
			sizes:       installedSizes[libModel.Name],
			tagInfo:     tagInfo,
		})
	}
	// Synthesize rows for installed customs not present in the library.
	for base, instTags := range installedTags {
		if seen[base] {
			continue
		}
		tags := make([]string, 0, len(instTags))
		for tag := range instTags {
			tags = append(tags, tag)
		}
		rows = append(rows, localModel{
			name:        base,
			description: "",
			tags:        sortTags(tags),
			installed:   copyBoolMap(instTags),
			sizes:       installedSizes[base],
		})
	}

	// Partition Installed-first then Installable, each alpha by name.
	var installedRows, installableRows []localModel
	for _, row := range rows {
		if row.anyInstalled() {
			installedRows = append(installedRows, row)
		} else {
			installableRows = append(installableRows, row)
		}
	}
	sort.Slice(installedRows, func(left, right int) bool { return installedRows[left].name < installedRows[right].name })
	sort.Slice(installableRows, func(left, right int) bool { return installableRows[left].name < installableRows[right].name })

	view.models = append(installedRows, installableRows...)
	view.installedCount = len(installedRows)
	// The curated vLLM starter rows form their own trailing section (only on a platform
	// with a curated set). They carry no ollama-style tags — `enter` pulls them directly
	// under the vLLM runtime.
	view.vllmStart = len(view.models)
	for _, id := range vllmStarterRows(view.vllmGoos) {
		view.models = append(view.models, localModel{
			name:        id,
			description: "vLLM starter model",
			runtime:     config.RuntimeVLLM,
		})
	}
	view.window.Reset() // reseat the cursor on the first model + scroll to the top
}

// vllmStarterModelsFn is the seam that yields the curated per-platform vLLM starter
// list; a package var so tests can pin a platform without touching runtime.GOOS.
var vllmStarterModelsFn = vllmStarterModels

// vllmStarterRows returns the curated vLLM starter ids for goos (empty when goos is
// unset or unsupported).
func vllmStarterRows(goos string) []string {
	if goos == "" {
		return nil
	}
	return vllmStarterModelsFn(goos)
}

// vllmStarterModels is a SMALL CURATED per-platform starter set of vLLM-servable model
// ids: mlx-community/* ids on darwin (served via the vLLM-Metal plugin) and Hugging Face
// safetensors ids on linux (CUDA). It is a deliberate STARTER SET — a follow-up
// (`hardware bring-up`) will replace it with a LIVE Hugging Face hub scrape filtered by
// the platform weight format. Returns nil on an unsupported GOOS.
func vllmStarterModels(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"mlx-community/Qwen2.5-7B-Instruct-4bit",
			"mlx-community/Llama-3.2-3B-Instruct-4bit",
			"mlx-community/Mistral-7B-Instruct-v0.3-4bit",
		}
	case "linux":
		return []string{
			"Qwen/Qwen2.5-7B-Instruct",
			"meta-llama/Llama-3.2-3B-Instruct",
			"mistralai/Mistral-7B-Instruct-v0.3",
		}
	default:
		return nil
	}
}

// moveCursor steps the cursor by step (±1) over MODEL rows only (the listWindow never
// lands the cursor on a section header or padding line), keeping it inside the scroll
// window.
func (view *LocalModels) moveCursor(step int) {
	if len(view.models) == 0 {
		return
	}
	view.syncWindow() // ensure the window reflects the current content/size first
	view.window.Move(step)
}

// selectedModel returns the model under the cursor.
func (view *LocalModels) selectedModel() (localModel, bool) {
	cursor := view.window.Cursor()
	if cursor < 0 || cursor >= len(view.models) {
		return localModel{}, false
	}
	return view.models[cursor], true
}

// libraryFlash sets the source-availability warning when the library fetch failed:
// "showing the cached copy" when a cached copy is in use (library rows present),
// otherwise "no cached copy". A successful fetch clears no flash here (the action
// flashes already cleared it on the keystroke).
func (view *LocalModels) libraryFlash() {
	if view.libraryErr == nil {
		return
	}
	view.flash = sourceFlash("Ollama library", view.hasLibraryRows())
}

// hasLibraryRows reports whether any installable (library-sourced) rows are shown —
// the signal that a cached copy is in use after a failed live fetch.
func (view *LocalModels) hasLibraryRows() bool {
	for _, model := range view.models {
		if model.runtime == config.RuntimeVLLM {
			// Curated vLLM rows are not sourced from the ollama.com library, so they
			// must not stand in for a live library fetch having succeeded.
			continue
		}
		if model.description != "" || !model.anyInstalled() {
			return true
		}
	}
	return false
}

// --- rendering --------------------------------------------------------------

// headerLines counts the rendered top-header lines (used to size the scroll window).
func (view *LocalModels) headerLines() int { return strings.Count(view.header(), "\n") }

// header renders everything above the two-section list.
func (view *LocalModels) header() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Local models — Ollama store + installable library") + "\n")
	if view.listErr != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" Ollama unreachable: "+view.listErr.Error()) +
			"\n" + ui.Muted.Render("installed status is unavailable — start it with `ai services start ollama`") + "\n")
	}
	if len(view.models) == 0 {
		body.WriteString(ui.Muted.Render("no models to show (enter a model to pull tags)") + "\n")
	}
	return body.String()
}

// drillMaxVisible is how many tag rows the drill-down window shows: the content
// height minus the drill header (name + optional description + hint + blank) and a
// reserve for the flash slot and the ↑/↓ overflow markers. When the height is
// unknown it shows every tag (no windowing).
func (view *LocalModels) drillMaxVisible() int {
	if view.height <= 0 {
		return len(view.drill.tags)
	}
	header := 3 // name + hint + blank-after-hint
	if view.drill.model.description != "" {
		header++
	}
	visible := view.height - header - 3 // flash slot (1) + ↑/↓ markers (2)
	if visible < 3 {
		visible = 3
	}
	return visible
}

// listHeight is the FIXED number of rendered lines the two-section list block
// occupies: the content height minus the top header and the one always-rendered flash
// slot. The list is padded to this height so its bottom never moves with scroll. At
// least one line is always shown.
func (view *LocalModels) listHeight() int {
	if view.height <= 0 {
		return len(view.models)
	}
	height := view.height - view.headerLines() - 1 // -1 for the flash slot
	if height < 1 {
		height = 1
	}
	return height
}

// contentLine renders one model row's "NAME  DESCRIPTION" content as a single plain
// (un-highlighted) line padded to the full pane width, so a later highlight bar fills
// the whole row. DESCRIPTION takes everything left over after the fixed NAME column.
// Tags are NOT shown here — they live in the per-model drill-down.
func (view *LocalModels) contentLine(model localModel) string {
	descWidth := view.descriptionWidth()
	// Both columns are clipped to their DISPLAY width (cells) so the description
	// always starts at the same column (the name never overruns localNameWidth) and
	// the row never overflows the pane (so the right border stays aligned), even when
	// a value contains wide runes. Emoji are stripped from the description first.
	name := padCell(truncateRunes(model.name, localNameWidth), localNameWidth)
	desc := truncateRunes(stripEmoji(model.description), descWidth)
	line := " " + name + "  " + desc
	return padToWidth(line, view.width)
}

// stripEmoji removes pictographic emoji (and their joiners / variation selectors)
// from a plain description and collapses the whitespace they leave behind, so the
// NAME · DESCRIPTION list stays clean and column-aligned. Non-emoji symbols and
// letters (e.g. the Σ in "MathΣtral") are kept.
func stripEmoji(text string) string {
	var builder strings.Builder
	for _, char := range text {
		if isEmoji(char) {
			continue
		}
		builder.WriteRune(char)
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

// isEmoji reports whether char is a pictographic emoji rune (or an emoji
// modifier/joiner) that should be stripped from a description.
func isEmoji(char rune) bool {
	switch {
	case char >= 0x1F300 && char <= 0x1FAFF: // symbols & pictographs, emoticons, transport, supplemental
		return true
	case char >= 0x2600 && char <= 0x27BF: // misc symbols + dingbats
		return true
	case char >= 0x2B00 && char <= 0x2BFF: // stars, arrows-as-emoji
		return true
	case char >= 0x2300 && char <= 0x23FF: // watches, hourglasses, media controls
		return true
	case char >= 0x1F1E6 && char <= 0x1F1FF: // regional-indicator (flag) letters
		return true
	case char == 0x200D: // zero-width joiner
		return true
	case char == 0x20E3: // combining enclosing keycap
		return true
	case char >= 0xFE00 && char <= 0xFE0F: // variation selectors
		return true
	}
	return false
}

// descriptionWidth is the WIDEST column: everything left after NAME and the spacing.
// It is the EXACT remainder so " " + NAME + "  " + DESCRIPTION sums to exactly the
// pane width — the highlight bar fills the row on one line with no wrap. Guarded at
// ≥1 for a degenerate (tiny) pane.
func (view *LocalModels) descriptionWidth() int {
	if view.width <= 0 {
		return 44
	}
	// Leading space (1) + inter-column gap (2) = 3.
	desc := view.width - localNameWidth - 3
	if desc < 1 {
		return 1
	}
	return desc
}

// View renders the drill-down, the describe pane, or the two-section list.
func (view *LocalModels) View() string {
	if view.runtime != nil {
		return view.runtimeView()
	}
	if view.drill != nil {
		return view.drillView()
	}
	if view.describe.active() {
		return view.describe.view()
	}
	if !view.loaded {
		return ui.Muted.Render("loading local models…")
	}
	var body strings.Builder
	body.WriteString(view.header())
	if len(view.models) > 0 {
		body.WriteString(view.listView())
	}
	// Always emit the flash slot as the LAST line (blank when empty) so the list above
	// keeps its fixed height and the bottom sits at the constant margin.
	body.WriteString("\n" + flashLine(view.flash))
	return body.String()
}

// selectedStyle is the highlight applied to the cursor's model row: IDENTICAL to
// ui.TableStyles().Selected (bold, secondary foreground on the accent background), so
// the Local Models highlight matches the Cloud Models highlight exactly. The content
// line is already padded to the full pane width, so the background bar spans the whole
// row (no ragged short highlight).
func selectedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).
		Foreground(ui.Secondary()).
		Background(ui.Accent())
}

// listView renders the two-section list through the reusable listWindow: a FIXED-height
// window of section headers + model rows, the cursor's model row highlighted with the
// Cloud-Models-identical Selected style spanning the full width, padded to a constant
// height so the bottom margin never moves with scroll. The windowing/anchor-scroll
// rules live in listWindow (defined once); this just feeds it the current lines + size.
func (view *LocalModels) listView() string {
	view.syncWindow()
	return view.window.View(selectedStyle())
}

// drillView renders the per-model tag picker: each tag marked ● installed / ○ not,
// the cursor highlighted, ticked not-installed tags marked [x]. A long tag list
// (some models have dozens of tags) is WINDOWED so it scrolls to keep the cursor
// visible, with ↑/↓ overflow markers.
func (view *LocalModels) drillView() string {
	drill := view.drill
	var body strings.Builder
	body.WriteString(ui.Heading.Render(drill.model.name) + "\n")
	if drill.model.description != "" {
		body.WriteString(ui.Muted.Render(drill.model.description) + "\n")
	}
	body.WriteString(ui.Muted.Render("space select a tag to pull · enter/p pull · d remove · t test · esc back") + "\n\n")

	// Anchor-scroll the window so the cursor row stays on screen.
	maxVisible := view.drillMaxVisible()
	if drill.cursor < drill.scroll {
		drill.scroll = drill.cursor
	}
	if drill.cursor >= drill.scroll+maxVisible {
		drill.scroll = drill.cursor - maxVisible + 1
	}
	if drill.scroll < 0 {
		drill.scroll = 0
	}
	end := drill.scroll + maxVisible
	if end > len(drill.tags) {
		end = len(drill.tags)
	}

	if drill.scroll > 0 {
		body.WriteString(ui.Muted.Render("  ↑ more") + "\n")
	}
	for index := drill.scroll; index < end; index++ {
		tag := drill.tags[index]
		marker := ui.Muted.Render("○")
		if drill.model.installed[tag] {
			marker = ui.Success.Render("●")
		}
		check := " "
		if drill.selected[tag] {
			check = "x"
		}
		line := marker + " [" + check + "] " + tag
		if detail := tagDetail(drill.model, tag); detail != "" {
			line += ui.Muted.Render("  " + detail)
		}
		// Surface the recorded serving runtime for an installed tag (Ollama by
		// default, or vLLM when it was pulled with that runtime).
		if drill.model.installed[tag] {
			if badge := runtimeBadgeLabel(modelRuntimeLookup(drill.model.name + ":" + tag)); badge != "" {
				line += ui.Muted.Render("  · " + badge)
			}
		}
		if index == drill.cursor {
			line = ui.Primary.Bold(true).Render("› ") + line
		} else {
			line = "  " + line
		}
		body.WriteString(line + "\n")
	}
	if end < len(drill.tags) {
		body.WriteString(ui.Muted.Render("  ↓ more") + "\n")
	}
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// modelRuntimeLookup returns the recorded serving runtime for a model reference (its
// pull ref, e.g. "qwen2.5:7b"), or "" when none is recorded (the default Ollama path
// leaves no explicit record for a legacy install). It is a package var so tests inject
// a deterministic result without touching the on-disk store.
var modelRuntimeLookup = func(ref string) config.ModelRuntime {
	choices, err := config.LoadModelRuntimes()
	if err != nil {
		return ""
	}
	for _, choice := range choices {
		if choice.Model == ref {
			return choice.Runtime
		}
	}
	return ""
}

// runtimeBadgeLabel is the compact drill-down badge for a recorded serving runtime
// ("" hides it — a model with no recorded choice shows no badge).
func runtimeBadgeLabel(runtime config.ModelRuntime) string {
	switch runtime {
	case config.RuntimeOllama:
		return "ollama"
	case config.RuntimeVLLM:
		return "vllm"
	default:
		return ""
	}
}

// tagDetail renders a compact "size · context ctx · input" summary for a tag from
// the scraped library metadata (ollama.com /tags columns), falling back to the
// on-disk byte size for an installed tag the library doesn't describe.
func tagDetail(model localModel, tag string) string {
	if info, ok := model.tagInfo[tag]; ok {
		parts := make([]string, 0, 3)
		if info.Size != "" {
			parts = append(parts, info.Size)
		}
		if info.Context != "" {
			parts = append(parts, info.Context+" ctx")
		}
		if info.Input != "" {
			parts = append(parts, info.Input)
		}
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
	}
	if size, ok := model.sizes[tag]; ok && size > 0 {
		return ollama.HumanByteSize(size)
	}
	return ""
}

// describeTag renders the full /api/show detail for an installed ref (name:tag) into
// the describe pane.
func (view *LocalModels) describeTag(ref string) string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render(ref) + "\n")
	if view.show == nil {
		body.WriteString(ui.Muted.Render("model detail is unavailable (no /api/show fetcher wired)"))
		return body.String()
	}
	info, err := view.show(ref)
	if err != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail + " " + err.Error()))
		return body.String()
	}
	body.WriteString("\n" + ui.Heading.Render("details") + "\n")
	body.WriteString(field("family", info.Family))
	body.WriteString(field("parameter size", info.ParameterSize))
	body.WriteString(field("quantization", info.QuantizationLevel))
	body.WriteString(field("format", info.Format))
	body.WriteString(field("parent model", info.ParentModel))
	if len(info.Capabilities) > 0 {
		body.WriteString(field("capabilities", strings.Join(info.Capabilities, ", ")))
	}
	if len(info.ModelInfo) > 0 {
		body.WriteString("\n" + ui.Heading.Render("model_info") + "\n")
		keys := make([]string, 0, len(info.ModelInfo))
		for key := range info.ModelInfo {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			body.WriteString(field(key, valueString(info.ModelInfo[key])))
		}
	}
	if strings.TrimSpace(info.License) != "" {
		body.WriteString("\n" + ui.Heading.Render("license") + "\n")
		body.WriteString(truncate(info.License, 2000) + "\n")
	}
	return body.String()
}

// --- small helpers ----------------------------------------------------------

// splitRef splits an Ollama ref "base:tag" into its base name and tag; a ref with no
// colon is treated as the bare model with tag "latest".
func splitRef(ref string) (base, tag string) {
	if colon := strings.LastIndexByte(ref, ':'); colon >= 0 {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func copyBoolMap(source map[string]bool) map[string]bool {
	out := make(map[string]bool, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// padCell right-pads (or, when overlong, leaves) a plain cell value to width DISPLAY
// CELLS so the columns align even with wide runes. The value is assumed already
// clipped to width by truncateRunes.
func padCell(value string, width int) string {
	return runewidth.FillRight(value, width)
}

// padToWidth right-pads a plain (un-styled) line to width DISPLAY CELLS so a highlight
// bar applied over it spans the full pane; a line already at/over width is returned
// as-is. Measuring by display width (not rune count) keeps the right border aligned
// when a row contains wide runes.
func padToWidth(line string, width int) string {
	if width <= 0 {
		return line
	}
	return runewidth.FillRight(line, width)
}

// sortTags orders tags by ascending parameter magnitude where parseable (270m < 1b <
// 70b), then lexically — so the common size tags read small-to-large.
func sortTags(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.SliceStable(out, func(left, right int) bool {
		leftMag := paramMagnitude(out[left])
		rightMag := paramMagnitude(out[right])
		if leftMag != rightMag {
			return leftMag < rightMag
		}
		return out[left] < out[right]
	})
	return out
}
