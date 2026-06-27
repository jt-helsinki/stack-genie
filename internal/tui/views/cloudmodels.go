package views

import (
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/catalog"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// CloudCatalogLoader returns the models.dev catalog plus the data source (fresh vs
// cached) and the live-fetch error, so the view can message availability. Injected;
// the parent wires catalog.LoadOrFetchStatus.
type CloudCatalogLoader func() (*catalog.Catalog, catalog.Source, error)

// LiveModelLister returns the models the LiteLLM gateway currently serves
// (registered in its DB), used for the "registered" status. Injected; the parent
// wires litellm.NewKeyManager(...).ListModels. Nil/error is tolerated.
type LiveModelLister func() ([]litellm.LiveModel, error)

// CatalogRefresher re-fetches the models.dev catalog (fetch + Save) and re-runs the
// gateway model sync, so `r` brings both displayed data and the gateway up to date.
// Injected; nil is tolerated — `r` then refreshes only the displayed data.
type CatalogRefresher func() error

// cloudModel is one cloud (models.dev) catalog row: the model id + provider +
// registered/available status + the catalog metadata for the describe pane.
type cloudModel struct {
	name        string
	provider    string
	family      string
	releaseDate string
	lastUpdated string
	contextLen  int
	outputLen   int
	inputModes  []string
	outputModes []string
	status      modelStatus
}

func (model cloudModel) registered() bool { return model.status == statusRegistered }

// cloudCatalogLoadedMsg carries the loaded catalog (rows) + the live (registered)
// set + the source/errors so the view can message availability.
type cloudCatalogLoadedMsg struct {
	cloud      []catalog.Model
	source     catalog.Source
	registered []litellm.LiveModel
	cloudErr   error
	liveErr    error
}

// cloudResyncedMsg is the result of the `r` network work (catalog re-fetch + gateway
// resync). Its error (if any) flashes; the reload that follows repopulates the table.
type cloudResyncedMsg struct{ err error }

// CloudModels is the Cloud Models tab: the models.dev catalog ⨯ the gateway's live
// (registered) model set as a table (MODEL · PROVIDER · STATUS · CONTEXT). enter
// opens the catalog metadata in a describe pane; t tests a registered model; r
// re-fetches the catalog + resyncs the gateway. Keys come are managed in the API
// Keys tab.
type CloudModels struct {
	loadCatalog CloudCatalogLoader
	listLive    LiveModelLister
	refresh     CatalogRefresher
	test        ModelTester

	table    table.Model
	describe describePane

	models   []cloudModel
	byName   map[string]cloudModel
	source   catalog.Source
	cloudErr error
	liveErr  error

	width  int
	height int
	flash  string
	loaded bool
}

// NewCloudModels builds the Cloud Models view over the injected catalog loader, live
// (registered) lister, catalog refresher, and gateway tester.
func NewCloudModels(load CloudCatalogLoader, live LiveModelLister, refresh CatalogRefresher, test ModelTester) *CloudModels {
	columns := []table.Column{
		{Title: "MODEL", Width: 34},
		{Title: "PROVIDER", Width: 14},
		{Title: "STATUS", Width: 11},
		{Title: "CONTEXT", Width: 9},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &CloudModels{loadCatalog: load, listLive: live, refresh: refresh, test: test, table: built, describe: newDescribePane()}
}

func (view *CloudModels) Title() string { return "Cloud Models" }

func (view *CloudModels) Hints() string {
	return "↑/↓ select · enter details · t test (registered) · r refresh · add keys in the API Keys tab"
}

func (view *CloudModels) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.fitTable()
	view.describe.setSize(width, height)
}

func (view *CloudModels) fitTable() {
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

// Init loads the catalog + the registered set.
func (view *CloudModels) Init() tea.Cmd { return view.loadCmd() }

// loadCmd loads the catalog (with source) and the live registered set.
func (view *CloudModels) loadCmd() tea.Cmd {
	load := view.loadCatalog
	live := view.listLive
	return func() tea.Msg {
		var msg cloudCatalogLoadedMsg
		if load != nil {
			cat, source, err := load()
			msg.source = source
			msg.cloudErr = err
			if cat != nil {
				msg.cloud = cat.Models()
			}
		}
		if live != nil {
			msg.registered, msg.liveErr = live()
		}
		return msg
	}
}

// resyncCmd runs the catalog re-fetch + gateway resync off the UI thread. nil when no
// refresher is wired.
func (view *CloudModels) resyncCmd() tea.Cmd {
	refresh := view.refresh
	if refresh == nil {
		return nil
	}
	return func() tea.Msg { return cloudResyncedMsg{err: refresh()} }
}

func (view *CloudModels) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

func (view *CloudModels) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case cloudCatalogLoadedMsg:
		view.loaded = true
		view.cloudErr = message.cloudErr
		view.liveErr = message.liveErr
		view.source = message.source
		view.buildModels(message.cloud, message.registered)
		view.catalogFlash()
		view.fitTable()
		return nil
	case cloudResyncedMsg:
		if message.err != nil {
			view.flash = ui.Warn.Render(ui.IconArrow + " catalog refresh: " + message.err.Error())
		} else {
			view.flash = ui.Success.Render(ui.IconOK + " catalog refreshed + gateway resynced")
		}
		return view.loadCmd()
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return nil
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

func (view *CloudModels) handleAction(key tea.KeyMsg) (tea.Cmd, bool) {
	switch key.String() {
	case "enter":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("no model selected")
			return nil, true
		}
		view.describe.show(view.describeCloud(model))
		return nil, true
	case "t":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to test")
			return nil, true
		}
		if !model.registered() {
			view.flash = ui.Muted.Render(model.name + " is not registered — add its provider key in the API Keys tab")
			return nil, true
		}
		view.flash = ui.Muted.Render("testing " + model.name + "…")
		view.describe.close()
		return view.testCmd(model.name), true
	case "r":
		view.describe.close()
		if resync := view.resyncCmd(); resync != nil {
			view.flash = ui.Muted.Render("refreshing catalog + resyncing gateway…")
			return resync, true
		}
		view.flash = ui.Muted.Render("refreshing…")
		return view.loadCmd(), true
	}
	return nil, false
}

// selectedModel returns the cloud model in the highlighted row.
func (view *CloudModels) selectedModel() (cloudModel, bool) {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return cloudModel{}, false
	}
	if model, ok := view.byName[row[0]]; ok {
		return model, true
	}
	return cloudModel{}, false
}

// buildModels builds the cloud rows + name index, registered-first then available,
// each alpha by name.
func (view *CloudModels) buildModels(cloud []catalog.Model, registered []litellm.LiveModel) {
	registeredNames := make(map[string]bool, len(registered))
	for _, model := range registered {
		registeredNames[model.Name] = true
	}
	rows := make([]cloudModel, 0, len(cloud))
	index := make(map[string]cloudModel, len(cloud))
	for _, model := range cloud {
		status := statusAvailable
		if registeredNames[model.ID] {
			status = statusRegistered
		}
		row := cloudModel{
			name:        model.ID,
			provider:    providerOf(model.ID),
			family:      model.Family,
			releaseDate: model.ReleaseDate,
			lastUpdated: model.LastUpdated,
			contextLen:  model.Limit.Context,
			outputLen:   model.Limit.Output,
			inputModes:  model.Modalities.Input,
			outputModes: model.Modalities.Output,
			status:      status,
		}
		rows = append(rows, row)
		index[model.ID] = row
	}
	sort.SliceStable(rows, func(left, right int) bool {
		leftReg := rows[left].registered()
		rightReg := rows[right].registered()
		if leftReg != rightReg {
			return leftReg // registered first
		}
		return rows[left].name < rows[right].name
	})
	view.models = rows
	view.byName = index
	view.table.SetRows(cloudRows(rows))
	if len(rows) > 0 && len(view.table.SelectedRow()) == 0 {
		view.table.SetCursor(0)
	}
}

// catalogFlash sets the source-availability warning when the catalog fetch failed.
func (view *CloudModels) catalogFlash() {
	if view.cloudErr == nil {
		return
	}
	view.flash = sourceFlash("models.dev catalog", len(view.models) > 0)
}

// describeCloud renders a cloud model's models.dev metadata.
func (view *CloudModels) describeCloud(model cloudModel) string {
	if indexed, ok := view.byName[model.name]; ok {
		model = indexed
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(model.name) + "\n")
	if model.registered() {
		body.WriteString(ui.Success.Render(ui.IconOK+" registered — the gateway serves this model") + "\n")
	} else {
		body.WriteString(ui.Muted.Render("available — add the provider key (API Keys tab) to register it") + "\n")
	}
	body.WriteString("\n" + ui.Heading.Render("catalog (models.dev)") + "\n")
	body.WriteString(field("provider", model.provider))
	body.WriteString(field("family", model.family))
	body.WriteString(field("release date", model.releaseDate))
	body.WriteString(field("last updated", model.lastUpdated))
	if model.contextLen > 0 {
		body.WriteString(field("context limit", humanTokenCount(model.contextLen)+" tokens"))
	}
	if model.outputLen > 0 {
		body.WriteString(field("output limit", humanTokenCount(model.outputLen)+" tokens"))
	}
	if len(model.inputModes) > 0 {
		body.WriteString(field("input modalities", strings.Join(model.inputModes, ", ")))
	}
	if len(model.outputModes) > 0 {
		body.WriteString(field("output modalities", strings.Join(model.outputModes, ", ")))
	}
	return body.String()
}

// header renders everything above the table.
func (view *CloudModels) header() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Cloud models — models.dev catalog") + "\n")
	if view.cloudErr != nil && len(view.models) == 0 {
		body.WriteString(ui.Muted.Render("catalog unavailable — connect and press r to refresh") + "\n")
	}
	if len(view.models) == 0 && view.cloudErr == nil {
		body.WriteString(ui.Muted.Render("no cloud models in the catalog") + "\n")
	}
	body.WriteString(ui.Muted.Render("add keys in the API Keys tab to register a provider's models") + "\n")
	return body.String()
}

func (view *CloudModels) headerLines() int { return strings.Count(view.header(), "\n") }

func (view *CloudModels) View() string {
	if view.describe.active() {
		return view.describe.view()
	}
	if !view.loaded {
		return ui.Muted.Render("loading cloud models…")
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

// cloudRows builds the table rows MODEL · PROVIDER · STATUS · CONTEXT.
func cloudRows(models []cloudModel) []table.Row {
	rows := make([]table.Row, 0, len(models))
	for _, model := range models {
		context := "-"
		if model.contextLen > 0 {
			context = humanTokenCount(model.contextLen)
		}
		provider := model.provider
		if provider == "" {
			provider = "-"
		}
		rows = append(rows, table.Row{model.name, provider, string(model.status), context})
	}
	return rows
}

// providerOf returns the provider id of a catalog model id (the segment before the
// first "/"); "" when the id has no slash.
func providerOf(modelID string) string {
	if slash := strings.IndexByte(modelID, '/'); slash >= 0 {
		return modelID[:slash]
	}
	return ""
}
