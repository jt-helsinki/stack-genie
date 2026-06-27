package views

import (
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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
// tags that are installed locally. The table shows NAME · DESCRIPTION · TAGS; enter
// drills into the per-tag picker.
type localModel struct {
	name        string          // base model name (e.g. "qwen2.5")
	description string          // library description ("" for a synthesized custom)
	tags        []string        // all known tags (library tags ∪ installed tags)
	installed   map[string]bool // which of tags are installed locally
	sizes       map[string]int64
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

// tagPicker is the in-view drill-down sub-state: the selected model's tags, each
// installed or not, with a moving cursor and a multi-select of NOT-installed tags to
// pull. esc backs out to the list.
type tagPicker struct {
	model    localModel
	tags     []string        // the model's tags, in display order
	cursor   int             // index into tags
	selected map[string]bool // not-installed tags ticked for pull
}

// LocalModels is the Local Models tab: the local Ollama store (installed) + the
// installable ollama.com library, grouped into an "Installed" section (library models
// with ≥1 pulled tag, plus installed customs) and an "Installable" section (the rest).
// One cursor spans both sections (header rows are skipped). enter drills into a
// per-model tag picker to pull/remove/test individual tags. Keys: enter manage, r
// refresh, t test, d remove.
type LocalModels struct {
	test    ModelTester
	list    LocalModelLister
	library LibraryLister
	show    ModelShowFetcher

	table    table.Model
	columns  []table.Column
	describe describePane
	drill    *tagPicker

	models    []localModel // every model row (installed-first then installable)
	rowModel  []int        // table-row index → models index (-1 for a header row)
	installed map[string]bool

	source     ollama.Source
	libraryErr error
	listErr    error

	width  int
	height int
	flash  string
	loaded bool
}

// NewLocalModels builds the Local Models view over the injected installed-store
// lister, library lister, per-model /api/show fetcher, and gateway tester.
func NewLocalModels(list LocalModelLister, library LibraryLister, show ModelShowFetcher, test ModelTester) *LocalModels {
	columns := []table.Column{
		{Title: "NAME", Width: 22},
		{Title: "DESCRIPTION", Width: 44},
		{Title: "TAGS", Width: 30},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &LocalModels{list: list, library: library, show: show, test: test, table: built, columns: columns, describe: newDescribePane()}
}

func (view *LocalModels) Title() string { return "Local Models" }

func (view *LocalModels) Hints() string {
	return "↑/↓ select · enter manage tags · t test · d remove · r refresh"
}

// SetSize records the pane dimensions and fits the table + describe pane.
func (view *LocalModels) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.fitTable()
	view.describe.setSize(width, height)
}

func (view *LocalModels) fitTable() {
	view.table.SetStyles(ui.TableStyles())
	view.table.SetWidth(view.width)
	view.table.SetColumns(ui.StretchColumns(view.columns, view.width))
	if view.height > 0 {
		tableHeight := view.height - view.headerLines()
		if tableHeight < 1 {
			tableHeight = 1
		}
		view.table.SetHeight(tableHeight)
	}
}

// Init kicks off the first install-store list + library load.
func (view *LocalModels) Init() tea.Cmd { return view.listCmd() }

// listCmd lists the installed store and loads the library (live, cache-backed). Each
// degrades independently.
func (view *LocalModels) listCmd() tea.Cmd {
	list := view.list
	library := view.library
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

func (view *LocalModels) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// Update advances the view: a refresh rebuilds the two sections; the drill-down
// owns keys while open; otherwise the action keys + table navigation apply.
func (view *LocalModels) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case localModelsRefreshedMsg:
		view.loaded = true
		view.listErr = message.listErr
		view.libraryErr = message.libraryErr
		view.source = message.source
		view.buildModels(message.installed, message.library)
		view.libraryFlash()
		view.fitTable()
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return nil
	case tea.KeyMsg:
		return view.handleKey(message)
	}
	if view.describe.active() || view.drill != nil {
		return nil
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// handleKey routes a key: the drill-down first (when open), then the describe pane,
// then the list actions / navigation.
func (view *LocalModels) handleKey(key tea.KeyMsg) tea.Cmd {
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
		view.flash = ui.Muted.Render("refreshing…")
		return view.listCmd()
	case "up", "k":
		view.moveCursor(-1)
		return nil
	case "down", "j":
		view.moveCursor(1)
		return nil
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(key)
	view.snapCursorToModel(1)
	return cmd
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
		view.drill = nil
		return func() tea.Msg { return ModelsPullRequestedMsg{Refs: refs} }
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
// Installable rows, rebuilds the table (with section header rows), and seats the
// cursor on the first selectable row.
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
		tags := append([]string(nil), libModel.Tags...)
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
	view.rebuildTable(len(installedRows))
}

// rebuildTable lays the two sections into the bubbles table with non-selectable
// header rows, keeping rowModel[] in sync (header rows map to -1). installedCount is
// how many of view.models are in the Installed section.
func (view *LocalModels) rebuildTable(installedCount int) {
	tableRows := make([]table.Row, 0, len(view.models)+2)
	view.rowModel = make([]int, 0, len(view.models)+2)

	addHeader := func(label string) {
		tableRows = append(tableRows, table.Row{label, "", ""})
		view.rowModel = append(view.rowModel, -1)
	}
	addModel := func(index int) {
		model := view.models[index]
		tableRows = append(tableRows, table.Row{
			model.name,
			truncateRunes(model.description, 44),
			truncateRunes(tagSummary(model.tags, model.installed), 30),
		})
		view.rowModel = append(view.rowModel, index)
	}

	if installedCount > 0 {
		addHeader("— Installed —————————————————————————")
		for index := 0; index < installedCount; index++ {
			addModel(index)
		}
	}
	if installedCount < len(view.models) {
		addHeader("— Installable ———————————————————————")
		for index := installedCount; index < len(view.models); index++ {
			addModel(index)
		}
	}
	view.table.SetRows(tableRows)
	// Seat the cursor on the first selectable (model) row.
	view.table.SetCursor(0)
	view.snapCursorToModel(1)
}

// moveCursor steps the table cursor by step (±1) and skips header rows.
func (view *LocalModels) moveCursor(step int) {
	if len(view.rowModel) == 0 {
		return
	}
	next := view.table.Cursor() + step
	for next >= 0 && next < len(view.rowModel) {
		if view.rowModel[next] >= 0 {
			view.table.SetCursor(next)
			return
		}
		next += step
	}
}

// snapCursorToModel nudges the cursor off a header row in the direction step (after
// a table.Update moved it); it never lands on a header.
func (view *LocalModels) snapCursorToModel(step int) {
	if len(view.rowModel) == 0 {
		return
	}
	cursor := view.table.Cursor()
	if cursor >= 0 && cursor < len(view.rowModel) && view.rowModel[cursor] >= 0 {
		return
	}
	view.moveCursor(step)
	if cursor := view.table.Cursor(); cursor < 0 || cursor >= len(view.rowModel) || view.rowModel[cursor] < 0 {
		view.moveCursor(-step)
	}
}

// selectedModel returns the model under the cursor (skipping header rows).
func (view *LocalModels) selectedModel() (localModel, bool) {
	cursor := view.table.Cursor()
	if cursor < 0 || cursor >= len(view.rowModel) {
		return localModel{}, false
	}
	index := view.rowModel[cursor]
	if index < 0 || index >= len(view.models) {
		return localModel{}, false
	}
	return view.models[index], true
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
		if model.description != "" || !model.anyInstalled() {
			return true
		}
	}
	return false
}

// headerLines counts the rendered header lines (used to size the table).
func (view *LocalModels) headerLines() int { return strings.Count(view.header(), "\n") }

// header renders everything above the table.
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

// View renders the drill-down, the describe pane, or the two-section table.
func (view *LocalModels) View() string {
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
		body.WriteString(view.table.View())
	}
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// drillView renders the per-model tag picker: each tag marked ● installed / ○ not,
// the cursor highlighted, ticked not-installed tags marked [x].
func (view *LocalModels) drillView() string {
	drill := view.drill
	var body strings.Builder
	body.WriteString(ui.Heading.Render(drill.model.name) + "\n")
	if drill.model.description != "" {
		body.WriteString(ui.Muted.Render(drill.model.description) + "\n")
	}
	body.WriteString(ui.Muted.Render("space select a tag to pull · enter/p pull · d remove · t test · esc back") + "\n\n")
	for index, tag := range drill.tags {
		marker := ui.Muted.Render("○")
		if drill.model.installed[tag] {
			marker = ui.Success.Render("●")
		}
		check := " "
		if drill.selected[tag] {
			check = "x"
		}
		line := marker + " [" + check + "] " + tag
		if size, ok := drill.model.sizes[tag]; ok && size > 0 {
			line += ui.Muted.Render("  " + ollama.HumanByteSize(size))
		}
		if index == drill.cursor {
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
