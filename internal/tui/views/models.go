package views

import (
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ModelStatusFetcher returns the LiteLLM gateway status (health, default model,
// providers, base URL). Injected; the parent wires litellm.RealClient().Status.
type ModelStatusFetcher func() (litellm.StatusInfo, error)

// ModelTester probes one model through the gateway and returns the result.
// Injected; the parent wires litellm.RealClient().Test(model).
type ModelTester func(model string) (litellm.TestResult, error)

// LocalModelLister returns the models in the local Ollama store. Injected; the
// parent wires ollama.RealClient().List.
type LocalModelLister func() ([]ollama.Model, error)

type modelsRefreshedMsg struct {
	status litellm.StatusInfo
	err    error
}
type modelTestDoneMsg struct {
	result litellm.TestResult
	err    error
}
type localModelsRefreshedMsg struct {
	installed []ollama.Model
	err       error
}

// ModelPullRequestedMsg asks the parent to run `ai models pull` live in the
// terminal overlay (the interactive select-or-custom prompt + streaming progress).
type ModelPullRequestedMsg struct{}

// ModelRemoveRequestedMsg asks the parent to run `ai models rm <Name>` live in the
// terminal overlay (with its confirm prompt).
type ModelRemoveRequestedMsg struct{ Name string }

// localRow is one row of the local-store list: an INSTALLED model (the hardcoded
// catalog was dropped — installable suggestions live behind `ai models pull`,
// which opens the live popular picker).
type localRow struct {
	name      string
	installed bool
	size      int64
	params    string
}

// Models is the global model view: the LiteLLM gateway summary (reachability,
// default model, providers, endpoint) plus the LOCAL Ollama store as a merged
// installed+installable list. Keys: t test the default model, p pull a model
// (select-or-custom, runs `ai models pull`), d remove the selected installed model.
type Models struct {
	fetch    ModelStatusFetcher
	test     ModelTester
	list     LocalModelLister
	status   litellm.StatusInfo
	rows     []localRow
	cursor   int
	offset   int // scroll offset: index of the first VISIBLE local row
	width    int // pane width  (from SetSize)
	height   int // pane height (from SetSize)
	flash    string
	err      error
	localErr error
	loaded   bool
}

// maxServedModelsShown caps how many live served-model lines the routing section
// renders before collapsing the rest into a "+K more" note, so the cursor-driven
// local list always has room within the pane.
const maxServedModelsShown = 6

// NewModels builds the models view over the injected gateway status fetcher,
// gateway tester, and local-store lister.
func NewModels(fetch ModelStatusFetcher, test ModelTester, list LocalModelLister) *Models {
	return &Models{fetch: fetch, test: test, list: list}
}

func (view *Models) Title() string { return "Models" }
func (view *Models) Hints() string { return "↑/↓ select · t test default · p pull · d remove" }

// SetSize records the pane dimensions so View() can window the local list to fit
// (the routing/gateway header is fixed; the local list scrolls within what's left).
func (view *Models) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.clampScroll()
}

// Init kicks off the first gateway-status fetch and local-store list.
func (view *Models) Init() tea.Cmd {
	return tea.Batch(view.refreshCmd(), view.listCmd())
}

func (view *Models) refreshCmd() tea.Cmd {
	fetch := view.fetch
	return func() tea.Msg {
		status, err := fetch()
		return modelsRefreshedMsg{status: status, err: err}
	}
}

func (view *Models) listCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		if list == nil {
			return localModelsRefreshedMsg{}
		}
		installed, err := list()
		return localModelsRefreshedMsg{installed: installed, err: err}
	}
}

// Update advances the view: refreshes fill the gateway summary and local list; "t"
// test-probes the default model; "p" requests an interactive pull; "d" requests
// removal of the selected installed model; ↑/↓ move the local-list cursor.
func (view *Models) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case modelsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.status = message.status
		}
		return nil
	case localModelsRefreshedMsg:
		view.localErr = message.err
		view.rows = installedRows(message.installed)
		if view.cursor >= len(view.rows) {
			view.cursor = 0
		}
		view.clampScroll()
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return view.refreshCmd()
	case tea.KeyMsg:
		switch message.String() {
		case "up", "k":
			if view.cursor > 0 {
				view.cursor--
				view.scrollToCursor()
			}
		case "down", "j":
			if view.cursor < len(view.rows)-1 {
				view.cursor++
				view.scrollToCursor()
			}
		case "t":
			model := view.status.Default
			if model == "" {
				view.flash = ui.Muted.Render("no default model to test")
				return nil
			}
			view.flash = ui.Muted.Render("testing " + model + "…")
			return view.testCmd(model)
		case "p":
			// Pull is the interactive select-or-custom flow + streaming progress;
			// run it as the real `ai models pull` subprocess in the terminal overlay.
			return func() tea.Msg { return ModelPullRequestedMsg{} }
		case "d":
			row, ok := view.selectedRow()
			if !ok || !row.installed {
				view.flash = ui.Muted.Render("select an installed model to remove")
				return nil
			}
			name := row.name
			return func() tea.Msg { return ModelRemoveRequestedMsg{Name: name} }
		}
	}
	return nil
}

func (view *Models) selectedRow() (localRow, bool) {
	if view.cursor < 0 || view.cursor >= len(view.rows) {
		return localRow{}, false
	}
	return view.rows[view.cursor], true
}

// visibleRows is how many local-list rows fit below the fixed routing/gateway
// header within the pane. It is paneHeight minus the header lines (and the footer
// hint line), floored at 1 so there is always at least one visible row. When the
// pane height is unknown (0, e.g. before the first SetSize) it returns len(rows) so
// nothing is clipped.
func (view *Models) visibleRows() int {
	if view.height <= 0 {
		if len(view.rows) == 0 {
			return 1
		}
		return len(view.rows)
	}
	available := view.height - view.headerLines() - footerHintLines
	if available < 1 {
		available = 1
	}
	return available
}

// scrollToCursor applies the edge-scroll rule: the offset only moves when the
// cursor leaves the visible window. Cursor above the window top → offset = cursor;
// cursor below the window bottom → offset = cursor - visibleRows + 1; otherwise the
// offset stays put. Then it is clamped.
func (view *Models) scrollToCursor() {
	visible := view.visibleRows()
	if view.cursor < view.offset {
		view.offset = view.cursor
	} else if view.cursor >= view.offset+visible {
		view.offset = view.cursor - visible + 1
	}
	view.clampScroll()
}

// clampScroll keeps the offset within [0, maxOffset] where maxOffset leaves the last
// window of rows visible.
func (view *Models) clampScroll() {
	visible := view.visibleRows()
	maxOffset := len(view.rows) - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if view.offset > maxOffset {
		view.offset = maxOffset
	}
	if view.offset < 0 {
		view.offset = 0
	}
}

func (view *Models) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// footerHintLines is the number of lines View() reserves below the local list (the
// "installed models · …" hint plus, when present, a flash line). Kept fixed so the
// header/visible-row math is deterministic.
const footerHintLines = 1

// header renders everything ABOVE the local list: the gateway/routing summary and
// the "Local model store" heading (or its error/empty note). It is built once and
// reused by View() and headerLines() so the line-count math stays in sync with what
// is actually drawn.
func (view *Models) header() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Model gateway (LiteLLM routing)") + "\n")
	if view.err != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" "+view.err.Error()) + "\n")
	} else {
		body.WriteString(field("healthy", modelsHealth(view.status.Healthy)))
		body.WriteString(field("default", view.status.Default))
		// Providers are DERIVED from the LIVE served-model list (not hardcoded
		// routing); see litellm.Status.
		body.WriteString(field("providers", strings.Join(view.status.Providers, ", ")))
		body.WriteString(field("base url", view.status.BaseURL))
		body.WriteString(renderServedModels(view.status))
	}
	body.WriteString("\n" + ui.Heading.Render("Local model store (Ollama)") + "\n")
	return body.String()
}

// headerLines counts the rendered header lines (used to size the local-list window).
func (view *Models) headerLines() int {
	return strings.Count(view.header(), "\n")
}

// View renders the gateway summary, then a WINDOW of the local-store list sized to
// fit the pane (edge-scrolled via offset), and the latest test flash. Nothing
// overflows the bordered body: the routing section caps its served-model list and
// the local list only ever renders visibleRows rows.
func (view *Models) View() string {
	if !view.loaded {
		return ui.Muted.Render("loading model gateway status…")
	}
	var body strings.Builder
	body.WriteString(view.header())

	switch {
	case view.localErr != nil:
		body.WriteString(ui.Failure.Render(ui.IconFail+" "+view.localErr.Error()) +
			"\n" + ui.Muted.Render("start it with `ai services start ollama`") + "\n")
	case len(view.rows) == 0:
		body.WriteString(ui.Muted.Render("no models in the local store (p to pull)") + "\n")
	default:
		view.clampScroll()
		visible := view.visibleRows()
		start := view.offset
		end := start + visible
		if end > len(view.rows) {
			end = len(view.rows)
		}
		for index := start; index < end; index++ {
			body.WriteString(renderLocalRow(view.rows[index], index == view.cursor) + "\n")
		}
		hint := "installed models · p to pull (popular picker) · d to remove"
		if start > 0 || end < len(view.rows) {
			// The list is clipped: show a subtle scroll affordance.
			hint = "↑/↓ more · " + hint
		}
		body.WriteString(ui.Muted.Render(hint) + "\n")
	}

	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// renderServedModels renders the LIVE list of models the gateway serves (from
// litellm.StatusInfo.Models, sourced from the gateway's /model/info endpoint — not
// the hardcoded routing). When the gateway is reachable but the list could not be
// fetched, the note is shown instead of erroring the view.
func renderServedModels(status litellm.StatusInfo) string {
	if !status.Healthy {
		return ""
	}
	var section strings.Builder
	switch {
	case len(status.Models) > 0:
		section.WriteString(ui.Muted.Render("served models (live):") + "\n")
		// Cap the rendered served models so the cursor-driven local list always has
		// room within the pane; the remainder collapses into a "+K more" note.
		shown := status.Models
		if len(shown) > maxServedModelsShown {
			shown = shown[:maxServedModelsShown]
		}
		for _, model := range shown {
			descriptor := model.Provider
			if model.Mode != "" {
				if descriptor != "" {
					descriptor += ", "
				}
				descriptor += model.Mode
			}
			line := "  " + model.Name
			if descriptor != "" {
				line += "  (" + descriptor + ")"
			}
			section.WriteString(line + "\n")
		}
		if remaining := len(status.Models) - len(shown); remaining > 0 {
			section.WriteString(ui.Muted.Render("  +"+strconv.Itoa(remaining)+" more") + "\n")
		}
	case status.ModelsNote != "":
		section.WriteString(ui.Muted.Render("served models: "+status.ModelsNote) + "\n")
	}
	return section.String()
}

func renderLocalRow(row localRow, selected bool) string {
	marker := "  "
	if selected {
		marker = ui.Heading.Render("> ")
	}
	status := "available"
	if row.installed {
		status = "installed"
	}
	size := "-"
	if row.installed && row.size > 0 {
		size = humanByteSize(row.size)
	}
	params := row.params
	if params == "" {
		params = "-"
	}
	line := marker + padRight(row.name, 28) + "  " + padRight(status, 10) + "  " + padRight(size, 9) + "  " + params
	if selected {
		return ui.Success.Render(line)
	}
	return line
}

// installedRows maps the installed local-store models to rows, sorted by name.
func installedRows(installed []ollama.Model) []localRow {
	rows := make([]localRow, 0, len(installed))
	for _, model := range installed {
		rows = append(rows, localRow{name: model.Name, installed: true, size: model.Size, params: model.ParameterSize})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	return rows
}

func padRight(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

// humanByteSize formats a byte count as a compact binary-unit string (e.g. 1.5 GB).
func humanByteSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(bytes)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}

func modelsHealth(healthy bool) string {
	if healthy {
		return ui.Success.Render(ui.IconOK + " reachable")
	}
	return ui.Failure.Render(ui.IconFail + " unreachable")
}

// modelTestFlash renders the outcome of a model test-probe.
func modelTestFlash(msg modelTestDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " test " + msg.result.Model + ": " + msg.err.Error())
	}
	if !msg.result.OK {
		detail := msg.result.Error
		if detail == "" && msg.result.Status != 0 {
			detail = "status " + strconv.Itoa(msg.result.Status)
		}
		if detail == "" {
			detail = "unreachable"
		}
		return ui.Failure.Render(ui.IconFail + " " + msg.result.Model + ": " + detail)
	}
	return ui.Success.Render(ui.IconOK + " " + msg.result.Model + " reachable (" + strconv.Itoa(msg.result.LatencyMS) + "ms)")
}
