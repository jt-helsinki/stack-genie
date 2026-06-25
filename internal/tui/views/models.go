package views

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
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

// PopularModelLister returns the bundled popular-models catalog (installable
// models). Injected; the parent wires ollama.Popular. Nil is tolerated (the view
// then degrades to installed-only).
type PopularModelLister func() ([]ollama.PopularModel, error)

// ModelShowFetcher returns the full /api/show detail for one local model.
// Injected; the parent wires ollama.RealClient().Show. Nil is tolerated (the
// describe pane then reports detail is unavailable).
type ModelShowFetcher func(model string) (ollama.ModelInfo, error)

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
	popular   []ollama.PopularModel
	listErr   error
	popErr    error
}

// ModelPullRequestedMsg asks the parent to run `ai models pull <Name>` live in the
// terminal overlay (streaming progress) for the SELECTED row's exact reference. An
// "available" row installs it; an "installed" row re-pulls (updates) it.
type ModelPullRequestedMsg struct{ Name string }

// ModelRemoveRequestedMsg asks the parent to run `ai models rm <Name>` live in the
// terminal overlay (with its confirm prompt).
type ModelRemoveRequestedMsg struct{ Name string }

// modelStatus distinguishes a model already in the local store from a catalog
// model that is installable but not yet pulled.
type modelStatus string

const (
	statusInstalled modelStatus = "installed"
	statusAvailable modelStatus = "available"
)

// localModel is one row of the merged local-store table: an installed model (from
// /api/tags) or an installable catalog model (from the popular snapshot). For an
// installed row size is the on-disk size and repoURL is empty; for an available row
// size is the download size and repoURL is the ollama.com/library page.
type localModel struct {
	name    string
	size    int64
	params  string
	status  modelStatus
	repoURL string
}

func (model localModel) installed() bool { return model.status == statusInstalled }

// Models is the global model view, Services-style: the LiteLLM gateway/routing
// summary (reachability, default model, providers, endpoint, the collapsed served
// models) rendered as a header above a bubbles table of the LOCAL Ollama store
// (NAME · PARAMETERS · SIZE · STATUS). enter drills into the selected model's full
// /api/show detail in a scrollable describe pane (esc closes); the action keys stay
// live while the pane is open. Keys: enter details, p pull, d remove, t test, r refresh.
type Models struct {
	fetch    ModelStatusFetcher
	test     ModelTester
	list     LocalModelLister
	popular  PopularModelLister
	show     ModelShowFetcher
	status   litellm.StatusInfo
	table    table.Model
	describe describePane
	models   []localModel
	catalog  map[string]ollama.PopularModel // by exact name, for the available-row describe pane
	width    int
	height   int
	flash    string
	err      error
	localErr error
	loaded   bool
}

// NewModels builds the models view over the injected gateway status fetcher,
// gateway tester, local-store lister, installable-catalog lister (popular), and
// per-model detail fetcher (/api/show).
func NewModels(fetch ModelStatusFetcher, test ModelTester, list LocalModelLister, popular PopularModelLister, show ModelShowFetcher) *Models {
	columns := []table.Column{
		{Title: "NAME", Width: 30},
		{Title: "PARAMETERS", Width: 12},
		{Title: "SIZE", Width: 10},
		{Title: "STATUS", Width: 10},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &Models{fetch: fetch, test: test, list: list, popular: popular, show: show, table: built, describe: newDescribePane()}
}

func (view *Models) Title() string { return "Models" }
func (view *Models) Hints() string {
	return "↑/↓ select · enter details · p pull selected · d remove (installed) · t test · r refresh"
}

// SetSize records the pane dimensions and fits the table to the body BELOW the
// fixed routing/gateway header (mirroring how Services sizes its table within the
// content area), then refits the describe pane.
func (view *Models) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.fitTable()
	view.describe.setSize(width, height)
}

// fitTable sizes the table to the height left after the routing/gateway header and
// re-applies the (theme-aware) styles so a live theme change is picked up.
func (view *Models) fitTable() {
	view.table.SetStyles(ui.TableStyles())
	view.table.SetWidth(view.width)
	if view.height > 0 {
		tableHeight := view.height - view.headerLines()
		if tableHeight < 1 {
			tableHeight = 1
		}
		view.table.SetHeight(tableHeight)
	}
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

// listCmd fetches BOTH the installed store and the installable catalog. Each side
// degrades independently: a failed List() still shows the offline catalog (with an
// "Ollama unreachable" note), and a failed Popular() falls back to installed-only.
func (view *Models) listCmd() tea.Cmd {
	list := view.list
	popular := view.popular
	return func() tea.Msg {
		var msg localModelsRefreshedMsg
		if list != nil {
			msg.installed, msg.listErr = list()
		}
		if popular != nil {
			msg.popular, msg.popErr = popular()
		}
		return msg
	}
}

// Update advances the view: refreshes fill the gateway summary and the local table;
// the action keys (enter/p/d/t/r) stay live even while the describe pane is open
// (Services-style); other keys drive table navigation or scroll the pane.
func (view *Models) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case modelsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.status = message.status
		}
		view.fitTable() // the header height can change once the status arrives
		return nil
	case localModelsRefreshedMsg:
		view.localErr = message.listErr
		view.models, view.catalog = mergeModels(message.installed, message.popular, message.popErr)
		view.table.SetRows(modelRows(view.models))
		view.fitTable() // the local-store header (note line) can change height
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return view.refreshCmd()
	case tea.KeyMsg:
		if cmd, handled := view.handleAction(message); handled {
			return cmd
		}
		if view.describe.active() {
			return view.describe.update(message)
		}
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// handleAction maps the action keys; the bool reports whether the key was an action
// (so it is not also passed to the table for navigation). Action keys stay live
// while the describe pane is open.
func (view *Models) handleAction(key tea.KeyMsg) (tea.Cmd, bool) {
	switch key.String() {
	case "enter":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("no model selected")
			return nil, true
		}
		// An installed model has /api/show detail; an available (not-yet-pulled)
		// model would error on /api/show, so render its catalog metadata instead.
		if model.installed() {
			view.describe.show(view.describeModel(model.name))
		} else {
			view.describe.show(view.describeAvailable(model))
		}
		return nil, true
	case "t":
		model := view.status.Default
		if model == "" {
			view.flash = ui.Muted.Render("no default model to test")
			return nil, true
		}
		view.flash = ui.Muted.Render("testing " + model + "…")
		view.describe.close()
		return view.testCmd(model), true
	case "p":
		// Pull the SELECTED row's exact reference: `ai models pull <ref>` (streaming
		// progress in the terminal overlay). An available row installs it; an installed
		// row re-pulls (updates). Run it as the real subprocess.
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to pull")
			return nil, true
		}
		name := model.name
		return func() tea.Msg { return ModelPullRequestedMsg{Name: name} }, true
	case "d":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select an installed model to remove")
			return nil, true
		}
		if !model.installed() {
			view.flash = ui.Muted.Render(model.name + " is not installed (press p to pull)")
			return nil, true
		}
		name := model.name
		return func() tea.Msg { return ModelRemoveRequestedMsg{Name: name} }, true
	case "r":
		view.flash = ui.Muted.Render("refreshing…")
		view.describe.close()
		return tea.Batch(view.refreshCmd(), view.listCmd()), true
	}
	return nil, false
}

// selectedModel returns the installed model in the highlighted table row.
func (view *Models) selectedModel() (localModel, bool) {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return localModel{}, false
	}
	for _, model := range view.models {
		if model.name == row[0] {
			return model, true
		}
	}
	return localModel{}, false
}

func (view *Models) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// header renders everything ABOVE the table: the gateway/routing summary (health,
// default, providers, base url, the collapsed served-model list) and the "Local
// model store" heading (or its error/empty note). Built once and reused by View()
// and headerLines() so the table-sizing math stays in sync with what is drawn.
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
	// The table merges INSTALLED + INSTALLABLE rows. When Ollama is unreachable the
	// installed status can't be read, but the offline catalog still lists installable
	// models — so show the note yet keep the table.
	if view.localErr != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" Ollama unreachable: "+view.localErr.Error()) +
			"\n" + ui.Muted.Render("installed status is unavailable — start it with `ai services start ollama`") + "\n")
	}
	if len(view.models) == 0 {
		body.WriteString(ui.Muted.Render("no models to show (p to pull a reference)") + "\n")
	}
	return body.String()
}

// headerLines counts the rendered header lines (used to size the table).
func (view *Models) headerLines() int { return strings.Count(view.header(), "\n") }

// View renders the describe pane when open (Services-style), else the routing/gateway
// header above the local-store table, with the latest test flash.
func (view *Models) View() string {
	if view.describe.active() {
		return view.describe.view()
	}
	if !view.loaded {
		return ui.Muted.Render("loading model gateway status…")
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

// renderServedModels renders the LIVE list of models the gateway serves, collapsed
// via litellm.DisplayModels so concrete models already covered by their provider's
// `*/` wildcard are dropped (the providers line already summarizes the wildcards).
// When the filtered set is empty, the heading is omitted entirely. When the gateway
// is reachable but the list could not be fetched, the note is shown instead.
func renderServedModels(status litellm.StatusInfo) string {
	if !status.Healthy {
		return ""
	}
	display := litellm.DisplayModels(status.Models)
	var section strings.Builder
	switch {
	case len(display) > 0:
		section.WriteString(ui.Muted.Render("served models (live):") + "\n")
		for _, model := range display {
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
	case len(status.Models) == 0 && status.ModelsNote != "":
		section.WriteString(ui.Muted.Render("served models: "+status.ModelsNote) + "\n")
	}
	return section.String()
}

// describeModel fetches the full /api/show detail for the named model and renders
// every field Ollama exposes (details block, parameters, template, license,
// capabilities, and the model_info map) into the describe pane.
func (view *Models) describeModel(name string) string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render(name) + "\n")
	if view.show == nil {
		body.WriteString(ui.Muted.Render("model detail is unavailable (no /api/show fetcher wired)"))
		return body.String()
	}
	info, err := view.show(name)
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
	if strings.TrimSpace(info.Parameters) != "" {
		body.WriteString("\n" + ui.Heading.Render("parameters") + "\n")
		body.WriteString(info.Parameters + "\n")
	}
	if strings.TrimSpace(info.Template) != "" {
		body.WriteString("\n" + ui.Heading.Render("template") + "\n")
		body.WriteString(info.Template + "\n")
	}
	if strings.TrimSpace(info.License) != "" {
		body.WriteString("\n" + ui.Heading.Render("license") + "\n")
		body.WriteString(truncate(info.License, 2000) + "\n")
	}
	return body.String()
}

// describeAvailable renders the CATALOG metadata for a not-yet-installed model
// (name, parameters, download size, repo URL) — /api/show is NOT called, as it
// would error for a model that is not in the local store. Prefers the indexed
// catalog entry (richer) and falls back to the row's own fields.
func (view *Models) describeAvailable(model localModel) string {
	candidate, ok := view.catalog[model.name]
	if !ok {
		candidate = ollama.PopularModel{
			Name:         model.name,
			Parameters:   model.params,
			DownloadSize: model.size,
			RepoURL:      model.repoURL,
		}
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(candidate.Name) + "\n")
	body.WriteString(ui.Muted.Render("not installed — press p to pull") + "\n")
	body.WriteString("\n" + ui.Heading.Render("catalog") + "\n")
	params := candidate.Parameters
	if params == "" {
		params = "-"
	}
	body.WriteString(field("parameters", params))
	downloadSize := "unknown"
	if candidate.DownloadSize > 0 {
		downloadSize = humanByteSize(candidate.DownloadSize)
	}
	body.WriteString(field("download size", downloadSize))
	body.WriteString(field("repo", candidate.RepoURL))
	if candidate.PullCount != "" {
		body.WriteString(field("pulls", candidate.PullCount))
	}
	return body.String()
}

// valueString renders a model_info value as a readable single line. Scalars print
// cleanly; the rare non-scalar value (slice/map) falls back to %v.
func valueString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprintf("%v", value)
	}
}

// truncate clips an overlong string (e.g. a multi-KB license) to limit runes with a
// trailing ellipsis note so the pane stays scrollable rather than enormous.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "\n… (truncated)"
}

// mergeModels merges the INSTALLED local-store models with the INSTALLABLE catalog
// (popular) models into the table's row set, plus a name→catalog index for the
// available-row describe pane. Dedup is by EXACT name: a catalog entry whose name is
// already installed is dropped (shown once, as installed). Installed models with no
// catalog match still appear. The result is ordered installed-first (alpha by name),
// then available (alpha) — deterministic. When the catalog lookup failed (popErr),
// only the installed rows are produced (installed-only degrade).
func mergeModels(installed []ollama.Model, popular []ollama.PopularModel, popErr error) ([]localModel, map[string]ollama.PopularModel) {
	installedNames := make(map[string]struct{}, len(installed))
	installedRows := make([]localModel, 0, len(installed))
	for _, model := range installed {
		installedNames[model.Name] = struct{}{}
		installedRows = append(installedRows, localModel{
			name:   model.Name,
			size:   model.Size,
			params: model.ParameterSize,
			status: statusInstalled,
		})
	}
	sort.Slice(installedRows, func(i, j int) bool { return installedRows[i].name < installedRows[j].name })

	catalog := make(map[string]ollama.PopularModel, len(popular))
	availableRows := make([]localModel, 0, len(popular))
	if popErr == nil {
		for _, candidate := range popular {
			catalog[candidate.Name] = candidate
			if _, isInstalled := installedNames[candidate.Name]; isInstalled {
				continue // dedup by exact name — already shown as installed
			}
			availableRows = append(availableRows, localModel{
				name:    candidate.Name,
				size:    candidate.DownloadSize,
				params:  candidate.Parameters,
				status:  statusAvailable,
				repoURL: candidate.RepoURL,
			})
		}
		sort.Slice(availableRows, func(i, j int) bool { return availableRows[i].name < availableRows[j].name })
	}

	return append(installedRows, availableRows...), catalog
}

// modelRows builds the table rows in the column order NAME · PARAMETERS · SIZE · STATUS.
func modelRows(models []localModel) []table.Row {
	rows := make([]table.Row, 0, len(models))
	for _, model := range models {
		params := model.params
		if params == "" {
			params = "-"
		}
		size := "-"
		if model.size > 0 {
			size = humanByteSize(model.size)
		}
		rows = append(rows, table.Row{model.name, params, size, string(model.status)})
	}
	return rows
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
