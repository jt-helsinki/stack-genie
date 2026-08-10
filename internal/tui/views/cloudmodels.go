package views

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/ui"
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

	table    listTable
	describe describePane

	models      []cloudModel
	byName      map[string]cloudModel
	catalogRows int // count of catalog entries loaded (for the cached-copy flash)
	source      catalog.Source
	cloudErr    error
	liveErr     error

	width  int
	height int
	flash  string
	loaded bool
	// refreshing marks a plain `r` reload so the transient "refreshing…" notice is
	// cleared when the result lands — without wiping a resync success message (that
	// path leaves refreshing false so its flash survives the follow-up reload).
	refreshing bool
}

// NewCloudModels builds the Cloud Models view over the injected catalog loader, live
// (registered) lister, catalog refresher, and gateway tester.
func NewCloudModels(load CloudCatalogLoader, live LiveModelLister, refresh CatalogRefresher, test ModelTester) *CloudModels {
	columns := []listColumn{
		{title: "MODEL", width: 34},
		{title: "PROVIDER", width: 14},
		{title: "STATUS", width: 11},
		{title: "CONTEXT", width: 9},
	}
	return &CloudModels{loadCatalog: load, listLive: live, refresh: refresh, test: test, table: newListTable(columns), describe: newDescribePane()}
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
	if view.height > 0 {
		// Reserve the header rows plus one row for the always-rendered flash slot, so
		// the table fills the remaining content height EXACTLY and its bottom sits at
		// the constant margin regardless of scroll or whether a flash shows. The last
		// column is stretched to the pane width by listTable so the highlight spans it.
		tableHeight := view.height - view.headerLines() - 1
		if tableHeight < 1 {
			tableHeight = 1
		}
		view.table.SetSize(view.width, tableHeight)
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
		// Clear the transient "refreshing…" notice (a plain reload); a resync success
		// message is preserved (refreshing was not set on that path). catalogFlash
		// re-sets a source-availability warning only when the catalog fetch failed.
		if view.refreshing {
			view.flash = ""
			view.refreshing = false
		}
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
	return view.table.Update(msg)
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
		view.refreshing = true
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

// buildModels builds the cloud rows from the gateway's REGISTERED set only — i.e.
// exactly the models whose provider has a stored API key (adding a key via
// `ai keys add` syncs that provider's models into the gateway; removing un-syncs
// them). The unkeyed/"available" catalog rows are dropped. The models.dev catalog is
// used only to enrich each registered model with metadata (context/modalities/family)
// where a matching catalog entry exists. Rows are alpha by name.
func (view *CloudModels) buildModels(cloud []catalog.Model, registered []litellm.LiveModel) {
	view.catalogRows = len(cloud)
	byCatalogID := make(map[string]catalog.Model, len(cloud))
	for _, model := range cloud {
		byCatalogID[model.ID] = model
	}
	rows := make([]cloudModel, 0, len(registered))
	index := make(map[string]cloudModel, len(registered))
	seen := make(map[string]bool, len(registered))
	for _, live := range registered {
		// Cloud Models lists only CLOUD-provider models. Local vLLM models are also
		// registered in the gateway (public model_name "vllm/<alias>", provider
		// "vllm") via `ai models pull` — they belong to the Local Models tab, so
		// skip them here.
		if live.Provider == "vllm" || strings.HasPrefix(live.Name, "vllm/") {
			continue
		}
		if seen[live.Name] {
			continue
		}
		seen[live.Name] = true
		row := cloudModel{
			name:     live.Name,
			provider: live.Provider,
			status:   statusRegistered,
		}
		if row.provider == "" {
			row.provider = providerOf(live.Name)
		}
		// Join to the catalog entry (keyed by the public model_name = catalog id) for
		// the describe-pane metadata where the catalog carries it.
		if entry, ok := byCatalogID[live.Name]; ok {
			row.family = entry.Family
			row.releaseDate = entry.ReleaseDate
			row.lastUpdated = entry.LastUpdated
			row.contextLen = entry.Limit.Context
			row.outputLen = entry.Limit.Output
			row.inputModes = entry.Modalities.Input
			row.outputModes = entry.Modalities.Output
			if row.provider == "" {
				row.provider = providerOf(entry.ID)
			}
		}
		rows = append(rows, row)
		index[row.name] = row
	}
	sort.SliceStable(rows, func(left, right int) bool { return rows[left].name < rows[right].name })
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
	view.flash = sourceFlash("models.dev catalog", view.catalogRows > 0)
}

// describeCloud renders a cloud model's models.dev metadata.
func (view *CloudModels) describeCloud(model cloudModel) string {
	if indexed, ok := view.byName[model.name]; ok {
		model = indexed
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(model.name) + "\n")
	if model.registered() {
		body.WriteString(ui.Success.Render(ui.IconOK+" Registered — the gateway serves this model") + "\n")
	} else {
		body.WriteString(ui.Muted.Render("Available — add the provider key (API Keys tab) to register it") + "\n")
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
	body.WriteString(ui.Heading.Render("Cloud models — providers with an API key") + "\n")
	if len(view.models) == 0 {
		body.WriteString(ui.Muted.Render("No cloud models — add a provider key in the API Keys tab to register its models") + "\n")
	}
	body.WriteString(ui.Muted.Render("Only models for providers with a stored key are shown (add keys in the API Keys tab)") + "\n")
	return body.String()
}

func (view *CloudModels) headerLines() int { return strings.Count(view.header(), "\n") }

func (view *CloudModels) View() string {
	if view.describe.active() {
		return view.describe.view()
	}
	if !view.loaded {
		return ui.Muted.Render("Loading cloud models…")
	}
	var body strings.Builder
	body.WriteString(view.header())
	if len(view.models) > 0 {
		body.WriteString(view.table.View())
	}
	// Always emit the flash slot as the LAST line (blank when empty) so the table
	// above keeps its fixed height and the bottom sits at the constant margin.
	body.WriteString("\n" + flashLine(view.flash))
	return body.String()
}

// cloudRows builds the table rows MODEL · PROVIDER · STATUS · CONTEXT.
func cloudRows(models []cloudModel) [][]string {
	rows := make([][]string, 0, len(models))
	for _, model := range models {
		context := "-"
		if model.contextLen > 0 {
			context = humanTokenCount(model.contextLen)
		}
		provider := model.provider
		if provider == "" {
			provider = "-"
		}
		rows = append(rows, []string{model.name, provider, string(model.status), context})
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
