package views

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// APIKeyProvider is one routable provider row for the API Keys view: its catalog
// provider id, display name, whether a key is stored, and its catalog model count.
// It carries NO key value (the view never sees a secret).
type APIKeyProvider struct {
	Provider string
	Name     string
	HasKey   bool
	Models   int
}

// APIKeyLister returns the LiteLLM-routable catalog providers with their keyed
// status + model count. Injected; the parent wires the catalog + ListCredentials
// join (the same join `ai keys list` uses). It never returns key values.
type APIKeyLister func() ([]APIKeyProvider, error)

type apiKeysRefreshedMsg struct {
	providers []APIKeyProvider
	err       error
}

// APIKeyAddRequestedMsg asks the parent to run `ai keys add <Provider>` live in the
// terminal overlay (its hidden key prompt shows in the pane), then refresh.
type APIKeyAddRequestedMsg struct{ Provider string }

// APIKeyRemoveRequestedMsg asks the parent to run `ai keys remove <Provider>` live
// in the terminal overlay, then refresh.
type APIKeyRemoveRequestedMsg struct{ Provider string }

// APIKeys is the per-host API Keys view: a Services-style table of the routable
// catalog providers (PROVIDER · NAME · KEY? · MODELS) with add (run `ai keys add`
// for the selected provider in the embedded terminal), remove (run `ai keys
// remove`), and refresh. The key value never enters this view.
type APIKeys struct {
	list      APIKeyLister
	table     table.Model
	providers []APIKeyProvider
	flash     string
	err       error
	loaded    bool
}

// NewAPIKeys builds the API Keys view over the injected provider lister.
func NewAPIKeys(list APIKeyLister) *APIKeys {
	columns := []table.Column{
		{Title: "PROVIDER", Width: 16},
		{Title: "NAME", Width: 24},
		{Title: "KEY?", Width: 6},
		{Title: "MODELS", Width: 8},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &APIKeys{list: list, table: built}
}

func (view *APIKeys) Title() string { return "API Keys" }
func (view *APIKeys) Hints() string {
	return "↑/↓ select · a add key · d remove key · r refresh"
}

// SetSize fits the table to the content area the parent allots it.
func (view *APIKeys) SetSize(width, height int) {
	view.table.SetStyles(ui.TableStyles()) // pick up a live theme change
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
}

// Init kicks off the first provider list.
func (view *APIKeys) Init() tea.Cmd { return view.refreshCmd() }

func (view *APIKeys) refreshCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		providers, err := list()
		return apiKeysRefreshedMsg{providers: providers, err: err}
	}
}

// Update advances the view: a refresh repopulates the table; "a" requests an add for
// the selected provider (run live in the terminal overlay so the hidden key prompt
// shows there); "d" requests a remove; "r" refreshes.
func (view *APIKeys) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case apiKeysRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.providers = message.providers
			view.table.SetRows(apiKeyRows(message.providers))
		}
		return nil
	case tea.KeyMsg:
		switch message.String() {
		case "a":
			provider, ok := view.selectedProvider()
			if !ok {
				view.flash = ui.Muted.Render("select a provider to add a key for")
				return nil
			}
			name := provider.Provider
			return func() tea.Msg { return APIKeyAddRequestedMsg{Provider: name} }
		case "d":
			provider, ok := view.selectedProvider()
			if !ok {
				view.flash = ui.Muted.Render("select a provider to remove its key")
				return nil
			}
			if !provider.HasKey {
				view.flash = ui.Muted.Render(provider.Provider + " has no key (press a to add one)")
				return nil
			}
			name := provider.Provider
			return func() tea.Msg { return APIKeyRemoveRequestedMsg{Provider: name} }
		case "r":
			view.flash = ui.Muted.Render("refreshing…")
			return view.refreshCmd()
		}
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// selectedProvider returns the provider in the highlighted table row.
func (view *APIKeys) selectedProvider() (APIKeyProvider, bool) {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return APIKeyProvider{}, false
	}
	for _, provider := range view.providers {
		if provider.Provider == row[0] {
			return provider, true
		}
	}
	return APIKeyProvider{}, false
}

// View renders the provider table (or a load/error line) with the latest flash.
func (view *APIKeys) View() string {
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading providers…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Provider API keys (LiteLLM)") + "\n")
	if len(view.providers) == 0 {
		body.WriteString(ui.Muted.Render("no routable providers in the catalog"))
	} else {
		body.WriteString(view.table.View())
	}
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// apiKeyRows builds the table rows PROVIDER · NAME · KEY? · MODELS.
func apiKeyRows(providers []APIKeyProvider) []table.Row {
	rows := make([]table.Row, 0, len(providers))
	for _, provider := range providers {
		key := "—"
		if provider.HasKey {
			key = "yes"
		}
		rows = append(rows, table.Row{provider.Provider, provider.Name, key, strconv.Itoa(provider.Models)})
	}
	return rows
}
