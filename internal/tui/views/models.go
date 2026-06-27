package views

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/catalog"
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

// CatalogLoader returns the saved models.dev catalog (cloud models with release
// date / limits / modalities). Injected; the parent wires catalog.Load. Nil/error
// is tolerated — the view then shows only the Ollama side.
type CatalogLoader func() (*catalog.Catalog, error)

// LiveModelLister returns the models the LiteLLM gateway currently serves
// (registered in its DB), used for the "registered" status indicator. Injected;
// the parent wires litellm.NewKeyManager(...).ListModels. Nil/error is tolerated —
// the view then shows no model as registered.
type LiveModelLister func() ([]litellm.LiveModel, error)

// CatalogRefresher re-fetches the models.dev catalog (fetch + Save) and re-runs the
// gateway model sync, so the `r` refresh brings BOTH the displayed data and the
// gateway up to date. Injected; the parent wires catalog.Fetch+Save then
// litellm.SyncModels. Nil is tolerated — `r` then refreshes only the displayed data.
type CatalogRefresher func() error

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

// catalogRefreshedMsg carries the loaded cloud catalog + the live (registered)
// gateway model set. Both degrade independently (a load/list error leaves that
// side empty); a nil catalog/live just means no cloud rows / no registered marks.
type catalogRefreshedMsg struct {
	cloud      []catalog.Model
	registered []litellm.LiveModel
	cloudErr   error
	liveErr    error
}

// catalogResyncedMsg is the result of the `r` network work (catalog re-fetch +
// gateway resync). Its error (if any) is surfaced as a flash; the data refresh that
// follows repopulates the table regardless.
type catalogResyncedMsg struct{ err error }

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
	// statusRegistered marks a CLOUD catalog model the gateway currently serves
	// (registered in its DB — i.e. the provider is keyed). A cloud model with no
	// stored key is statusAvailable.
	statusRegistered modelStatus = "registered"
)

// localModel is one row of the merged model table. It spans BOTH the local Ollama
// store (installed / installable) and the models.dev cloud catalog (registered /
// available). For an installed row size is the on-disk size and repoURL is empty;
// for an installable (popular) row size is the download size and repoURL is the
// ollama.com/library page. A cloud row carries the catalog metadata (release date,
// last updated, context/output limits, input/output modalities) and no size/params.
type localModel struct {
	name    string
	size    int64
	params  string
	status  modelStatus
	repoURL string

	// cloud marks a models.dev catalog (cloud-provider) model; false for an Ollama
	// store/popular row. The describe pane renders catalog metadata for a cloud row
	// and /api/show (installed) or the popular snapshot (available) for an Ollama row.
	cloud bool
	// Cloud catalog metadata (zero for an Ollama row).
	provider    string
	family      string
	releaseDate string
	lastUpdated string
	contextLen  int
	outputLen   int
	inputModes  []string
	outputModes []string
}

func (model localModel) installed() bool { return model.status == statusInstalled }

// registered reports whether a CLOUD model is currently served by the gateway.
func (model localModel) registered() bool { return model.status == statusRegistered }

// Models is the global model view, Services-style: the LiteLLM gateway/routing
// summary (reachability, default model, providers, endpoint, the collapsed served
// models) rendered as a header above a bubbles table of the LOCAL Ollama store
// (NAME · PARAMETERS · SIZE · STATUS). enter drills into the selected model's full
// /api/show detail in a scrollable describe pane (esc closes); the action keys stay
// live while the pane is open. Keys: enter details, p pull, d remove, t test, r refresh.
type Models struct {
	fetch      ModelStatusFetcher
	test       ModelTester
	list       LocalModelLister
	popular    PopularModelLister
	show       ModelShowFetcher
	status     litellm.StatusInfo
	table      table.Model
	describe   describePane
	models     []localModel                   // the COMBINED table rows (ollama + cloud), in display order
	ollamaRows []localModel                   // the Ollama-store rows (installed + popular)
	catalog    map[string]ollama.PopularModel // by exact name, for the available-row describe pane

	// Cloud-catalog side (wired via WithCatalog; nil when not wired — the view then
	// shows only the Ollama store).
	loadCatalog  CatalogLoader
	listLive     LiveModelLister
	refreshCloud CatalogRefresher
	cloudModels  []localModel          // built from the catalog ⨯ live (registered) set
	cloudByName  map[string]localModel // by model_name, for the cloud describe pane
	cloudErr     error
	liveErr      error

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
		{Title: "NAME", Width: 28},
		{Title: "PARAMETERS", Width: 11},
		{Title: "SIZE", Width: 9},
		{Title: "CONTEXT", Width: 9},
		{Title: "STATUS", Width: 11},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &Models{fetch: fetch, test: test, list: list, popular: popular, show: show, table: built, describe: newDescribePane()}
}

// WithCatalog wires the cloud-catalog side of the view: the saved models.dev
// catalog (cloud models with release/limits/modalities), the gateway's live
// (registered) model list (for the "registered" status), and the `r`-refresh
// re-fetch+resync. All are injected funcs so the view is faked in tests. Any may be
// nil — the view then degrades gracefully (no cloud rows / no registered marks / a
// data-only refresh). Returns the receiver for chaining at construction.
func (view *Models) WithCatalog(load CatalogLoader, live LiveModelLister, refresh CatalogRefresher) *Models {
	view.loadCatalog = load
	view.listLive = live
	view.refreshCloud = refresh
	return view
}

func (view *Models) Title() string { return "Models" }
func (view *Models) Hints() string {
	return "↑/↓ select · enter details · p pull (local) · d remove (installed) · t test · r refresh catalog + resync"
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

// Init kicks off the first gateway-status fetch, local-store list, and (when the
// catalog side is wired) the cloud-catalog + registered-model load.
func (view *Models) Init() tea.Cmd {
	return tea.Batch(view.refreshCmd(), view.listCmd(), view.catalogCmd())
}

// catalogCmd loads the saved cloud catalog and the gateway's live (registered)
// model set. It is a no-op (nil command) when the catalog side is not wired. Each
// side degrades independently: a failed Load still shows the registered set, a
// failed ListModels still shows the catalog as "available".
func (view *Models) catalogCmd() tea.Cmd {
	load := view.loadCatalog
	live := view.listLive
	if load == nil && live == nil {
		return nil
	}
	return func() tea.Msg {
		var msg catalogRefreshedMsg
		if load != nil {
			cat, err := load()
			msg.cloudErr = err
			if err == nil && cat != nil {
				msg.cloud = cat.Models()
			}
		}
		if live != nil {
			msg.registered, msg.liveErr = live()
		}
		return msg
	}
}

// resyncCmd runs the catalog re-fetch + gateway resync (the network half of the `r`
// refresh) off the UI thread, reporting completion so the data refresh that follows
// repopulates the table. nil when no refresher is wired.
func (view *Models) resyncCmd() tea.Cmd {
	refresh := view.refreshCloud
	if refresh == nil {
		return nil
	}
	return func() tea.Msg { return catalogResyncedMsg{err: refresh()} }
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
		view.ollamaRows, view.catalog = mergeModels(message.installed, message.popular, message.popErr)
		view.rebuildRows()
		view.fitTable() // the local-store header (note line) can change height
		return nil
	case catalogRefreshedMsg:
		view.cloudErr = message.cloudErr
		view.liveErr = message.liveErr
		view.cloudModels, view.cloudByName = mergeCloudModels(message.cloud, message.registered)
		view.rebuildRows()
		view.fitTable()
		return nil
	case catalogResyncedMsg:
		if message.err != nil {
			view.flash = ui.Warn.Render(ui.IconArrow + " catalog refresh: " + message.err.Error())
		} else {
			view.flash = ui.Success.Render(ui.IconOK + " catalog refreshed + gateway resynced")
		}
		// Reload the displayed data now the catalog/gateway are up to date.
		return tea.Batch(view.refreshCmd(), view.listCmd(), view.catalogCmd())
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
		// A cloud catalog model shows its models.dev metadata; an installed Ollama
		// model has /api/show detail; an available (not-yet-pulled) Ollama model
		// would error on /api/show, so render its popular-catalog metadata instead.
		switch {
		case model.cloud:
			view.describe.show(view.describeCloud(model))
		case model.installed():
			view.describe.show(view.describeModel(model.name))
		default:
			view.describe.show(view.describeAvailable(model))
		}
		return nil, true
	case "t":
		// Test the SELECTED row's model through the gateway (mirrors p/d). The
		// catalog-driven system has no default model, so testing the cursor's model
		// is the only meaningful behavior: a round-trip that reports success or the
		// gateway's error for whatever is selected.
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to test")
			return nil, true
		}
		view.flash = ui.Muted.Render("testing " + model.name + "…")
		view.describe.close()
		return view.testCmd(model.name), true
	case "p":
		// Pull the SELECTED row's exact reference: `ai models pull <ref>` (streaming
		// progress in the terminal overlay). An available row installs it; an installed
		// row re-pulls (updates). Run it as the real subprocess.
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to pull")
			return nil, true
		}
		if model.cloud {
			// Cloud models are not pulled — they are keyed (`ai keys add <provider>`).
			view.flash = ui.Muted.Render(model.name + " is a cloud model — add its provider key in the API Keys tab")
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
		view.describe.close()
		// When the catalog refresher is wired, the `r` refresh ALSO re-fetches the
		// models.dev catalog + resyncs the gateway (off the UI thread); the resync's
		// completion then reloads the displayed data. Otherwise it just reloads the
		// displayed data (status + local store + cloud catalog).
		if resync := view.resyncCmd(); resync != nil {
			view.flash = ui.Muted.Render("refreshing catalog + resyncing gateway…")
			return resync, true
		}
		view.flash = ui.Muted.Render("refreshing…")
		return tea.Batch(view.refreshCmd(), view.listCmd(), view.catalogCmd()), true
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
		// The served models themselves live in the (scrollable) table below — the
		// header stays compact so it never overflows the pane. Surface only the
		// gateway's model-list NOTE (reachable but the list couldn't be fetched).
		if len(view.status.Models) == 0 && view.status.ModelsNote != "" {
			body.WriteString(ui.Muted.Render("served models: "+view.status.ModelsNote) + "\n")
		}
	}
	body.WriteString("\n" + ui.Heading.Render("Models — local (Ollama) + cloud (models.dev catalog)") + "\n")
	// The table merges Ollama (installed/installable) + cloud (registered/available)
	// rows. When Ollama is unreachable the installed status can't be read, but the
	// offline popular catalog still lists installable models — so show the note yet
	// keep the table.
	if view.localErr != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" Ollama unreachable: "+view.localErr.Error()) +
			"\n" + ui.Muted.Render("installed status is unavailable — start it with `ai services start ollama`") + "\n")
	}
	// A cloud-catalog load error degrades to the Ollama-only table; note it but keep
	// going (the catalog is just a model-picker convenience).
	if view.cloudErr != nil {
		body.WriteString(ui.Muted.Render("cloud catalog unavailable: "+view.cloudErr.Error()+" — run `ai keys list` / press r to refresh") + "\n")
	}
	if len(view.models) == 0 {
		body.WriteString(ui.Muted.Render("no models to show (p to pull a local model · add a provider key for cloud models)") + "\n")
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
		downloadSize = ollama.HumanByteSize(candidate.DownloadSize)
	}
	body.WriteString(field("download size", downloadSize))
	body.WriteString(field("repo", candidate.RepoURL))
	if candidate.PullCount != "" {
		body.WriteString(field("pulls", candidate.PullCount))
	}
	return body.String()
}

// describeCloud renders a CLOUD (models.dev catalog) model's metadata in the
// describe pane: provider, family, release date, last updated, context/output token
// limits, input/output modalities, and whether the gateway currently serves it
// (registered) or the provider is unkeyed (available). Each field is shown only when
// present (blank fields are skipped).
func (view *Models) describeCloud(model localModel) string {
	// Prefer the indexed row (identical data; kept for symmetry with describeAvailable).
	if indexed, ok := view.cloudByName[model.name]; ok {
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

// rebuildRows concatenates the Ollama-store rows and the cloud-catalog rows into
// the combined table row set + table, preserving each side's internal order:
// installed Ollama first, then installable Ollama, then registered cloud, then
// available cloud (mergeModels / mergeCloudModels each pre-sort their own group).
func (view *Models) rebuildRows() {
	combined := make([]localModel, 0, len(view.ollamaRows)+len(view.cloudModels))
	combined = append(combined, view.ollamaRows...)
	combined = append(combined, view.cloudModels...)
	view.models = combined
	view.table.SetRows(modelRows(view.models))
	// bubbles' table.SetRows clamps the cursor DOWN to len-1 (→ -1 when rows go
	// empty) but never restores it when rows reappear, leaving SelectedRow() empty.
	// Because the table is built incrementally (an empty Ollama list can land before
	// the cloud rows), re-seat the cursor on the first row whenever it has fallen out
	// of range and there are rows to select.
	if len(combined) > 0 && len(view.table.SelectedRow()) == 0 {
		view.table.SetCursor(0)
	}
}

// mergeCloudModels builds the CLOUD (models.dev catalog) rows + a name→row index for
// the describe pane. Each catalog model carries its release date / last updated /
// context-output limits / input-output modalities. A model the gateway currently
// serves (its model_name is in the registered set) is statusRegistered; the rest are
// statusAvailable (the provider is not keyed). Registered rows sort first, then
// available, each alphabetically by name (deterministic).
func mergeCloudModels(cloud []catalog.Model, registered []litellm.LiveModel) ([]localModel, map[string]localModel) {
	registeredNames := make(map[string]bool, len(registered))
	for _, model := range registered {
		registeredNames[model.Name] = true
	}
	rows := make([]localModel, 0, len(cloud))
	index := make(map[string]localModel, len(cloud))
	for _, model := range cloud {
		status := statusAvailable
		if registeredNames[model.ID] {
			status = statusRegistered
		}
		row := localModel{
			name:        model.ID,
			status:      status,
			cloud:       true,
			provider:    providerOf(model.ID),
			family:      model.Family,
			releaseDate: model.ReleaseDate,
			lastUpdated: model.LastUpdated,
			contextLen:  model.Limit.Context,
			outputLen:   model.Limit.Output,
			inputModes:  model.Modalities.Input,
			outputModes: model.Modalities.Output,
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
	return rows, index
}

// providerOf returns the provider id of a catalog model id (the segment before the
// first "/"); "" when the id has no slash.
func providerOf(modelID string) string {
	if slash := strings.IndexByte(modelID, '/'); slash >= 0 {
		return modelID[:slash]
	}
	return ""
}

// mergeModels merges the INSTALLED local-store models with the INSTALLABLE catalog
// (popular) models into the table's row set, plus a name→catalog index for the
// available-row describe pane. Dedup is by EXACT name: a catalog entry whose name is
// already installed is dropped (shown once, as installed). Installed models with no
// catalog match still appear. The result is ordered installed-first (alpha by name),
// then the available group sorted ALPHABETICALLY by model name then ascending by
// parameter size (matching the bundled snapshot's order) — deterministic. When the
// catalog lookup failed (popErr), only the installed rows are produced
// (installed-only degrade).
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
		sortAvailableRows(availableRows)
	}

	return append(installedRows, availableRows...), catalog
}

// sortAvailableRows orders the installable rows ALPHABETICALLY by bare model name
// (case-insensitive), then ascending by parameter size within a model (so
// 270m < 1b < 3b < 70b), mirroring the bundled snapshot's order. The bare model name
// is the row name up to any ":" tag.
func sortAvailableRows(rows []localModel) {
	sort.SliceStable(rows, func(left, right int) bool {
		leftModel := bareModelName(rows[left].name)
		rightModel := bareModelName(rows[right].name)
		if leftModel != rightModel {
			return leftModel < rightModel
		}
		leftSize := paramMagnitude(rows[left].params)
		rightSize := paramMagnitude(rows[right].params)
		if leftSize != rightSize {
			return leftSize < rightSize
		}
		return rows[left].name < rows[right].name
	})
}

// bareModelName returns the lowercased model name without its ":<size>" tag.
func bareModelName(name string) string {
	if colon := strings.IndexByte(name, ':'); colon >= 0 {
		name = name[:colon]
	}
	return strings.ToLower(name)
}

// paramMagnitude converts a parameter-size label (e.g. "270m", "1.5b", "70b") into a
// comparable magnitude in parameters so the within-model order is ascending. An
// unparseable/empty label yields 0 (sorts first).
func paramMagnitude(label string) float64 {
	label = strings.TrimSpace(strings.ToLower(label))
	if label == "" {
		return 0
	}
	suffix := byte(0)
	if last := label[len(label)-1]; last == 'm' || last == 'b' {
		suffix = last
		label = label[:len(label)-1]
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(label), 64)
	if err != nil {
		return 0
	}
	switch suffix {
	case 'm':
		return value * 1e6
	case 'b':
		return value * 1e9
	default:
		return value
	}
}

// modelRows builds the table rows in the column order
// NAME · PARAMETERS · SIZE · CONTEXT · STATUS. Ollama rows show params + on-disk/
// download size (and a blank context); cloud rows show a blank params/size and the
// catalog context-window limit. Blank where N/A.
func modelRows(models []localModel) []table.Row {
	rows := make([]table.Row, 0, len(models))
	for _, model := range models {
		params := model.params
		if params == "" {
			params = "-"
		}
		size := "-"
		if model.size > 0 {
			size = ollama.HumanByteSize(model.size)
		}
		context := "-"
		if model.contextLen > 0 {
			context = humanTokenCount(model.contextLen)
		}
		rows = append(rows, table.Row{model.name, params, size, context, string(model.status)})
	}
	return rows
}

// humanTokenCount formats a token limit compactly (e.g. 200000 → "200K", 1000000 →
// "1M"); small counts print verbatim. Used for the context/output limit columns.
func humanTokenCount(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return strconv.FormatFloat(float64(tokens)/1_000_000, 'g', 3, 64) + "M"
	case tokens >= 1_000:
		return strconv.FormatFloat(float64(tokens)/1_000, 'g', 3, 64) + "K"
	default:
		return strconv.Itoa(tokens)
	}
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
