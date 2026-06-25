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

// localRow is one merged row of the local-store list: an installed model and/or a
// catalog ("available") model.
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
	flash    string
	err      error
	localErr error
	loaded   bool
}

// NewModels builds the models view over the injected gateway status fetcher,
// gateway tester, and local-store lister.
func NewModels(fetch ModelStatusFetcher, test ModelTester, list LocalModelLister) *Models {
	return &Models{fetch: fetch, test: test, list: list}
}

func (view *Models) Title() string    { return "Models" }
func (view *Models) Hints() string    { return "↑/↓ select · t test default · p pull · d remove" }
func (view *Models) SetSize(int, int) {}

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
		view.rows = mergeLocalRows(message.installed, ollama.Catalog())
		if view.cursor >= len(view.rows) {
			view.cursor = 0
		}
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return view.refreshCmd()
	case tea.KeyMsg:
		switch message.String() {
		case "up", "k":
			if view.cursor > 0 {
				view.cursor--
			}
		case "down", "j":
			if view.cursor < len(view.rows)-1 {
				view.cursor++
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

func (view *Models) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// View renders the gateway summary, the local-store list (installed + installable,
// with a cursor), and the latest test flash.
func (view *Models) View() string {
	if !view.loaded {
		return ui.Muted.Render("loading model gateway status…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Model gateway (LiteLLM routing)") + "\n")
	if view.err != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" "+view.err.Error()) + "\n")
	} else {
		body.WriteString(field("healthy", modelsHealth(view.status.Healthy)))
		body.WriteString(field("default", view.status.Default))
		body.WriteString(field("providers", strings.Join(view.status.Providers, ", ")))
		body.WriteString(field("base url", view.status.BaseURL))
	}

	body.WriteString("\n" + ui.Heading.Render("Local model store (Ollama)") + "\n")
	if view.localErr != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" "+view.localErr.Error()) +
			"\n" + ui.Muted.Render("start it with `ai services start ollama`") + "\n")
	} else if len(view.rows) == 0 {
		body.WriteString(ui.Muted.Render("no local models and an empty catalog") + "\n")
	} else {
		for index, row := range view.rows {
			body.WriteString(renderLocalRow(row, index == view.cursor) + "\n")
		}
		body.WriteString(ui.Muted.Render("installed = in the store · available = installable (p to pull)") + "\n")
	}

	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
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

// mergeLocalRows merges installed models with the catalog into one sorted list:
// catalog models not installed appear as "available"; installed models always
// appear. Catalog/installed matching is by base name (gemma4 vs gemma4:31b).
func mergeLocalRows(installed []ollama.Model, catalog []ollama.CatalogModel) []localRow {
	byBase := make(map[string]bool, len(installed))
	rows := make([]localRow, 0, len(installed)+len(catalog))
	for _, model := range installed {
		byBase[localBaseName(model.Name)] = true
		rows = append(rows, localRow{name: model.Name, installed: true, size: model.Size, params: model.ParameterSize})
	}
	for _, candidate := range catalog {
		if byBase[localBaseName(candidate.Name)] {
			continue
		}
		rows = append(rows, localRow{name: candidate.Name})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].installed != rows[j].installed {
			return rows[i].installed
		}
		return rows[i].name < rows[j].name
	})
	return rows
}

func localBaseName(name string) string {
	if idx := strings.IndexByte(name, ':'); idx >= 0 {
		return name[:idx]
	}
	return name
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
