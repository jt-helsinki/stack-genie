package views

import (
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// SecretLister returns the credential entries in the LiteLLM store (names only —
// never values). Injected; the parent wires secrets.Broker.List.
type SecretLister func() ([]secrets.Entry, error)

// SecretRemover deletes one credential by name. Injected; the parent wires
// secrets.Broker.Remove.
type SecretRemover func(name string) error

type secretsRefreshedMsg struct {
	entries []secrets.Entry
	err     error
}
type secretRemoveDoneMsg struct {
	name string
	err  error
}

// Secrets is the global view of the LiteLLM credential store: a table of
// credential NAMES (never values) with delete + refresh. Adding a credential
// needs a hidden value prompt and is out of scope here — use `ai secrets set`.
type Secrets struct {
	list    SecretLister
	remove  SecretRemover
	table   table.Model
	entries []secrets.Entry
	flash   string
	err     error
	loaded  bool
}

// NewSecrets builds the secrets view over the injected lister and remover.
func NewSecrets(list SecretLister, remove SecretRemover) *Secrets {
	columns := []table.Column{
		{Title: "NAME", Width: 30},
		{Title: "ENV VAR", Width: 30},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	return &Secrets{list: list, remove: remove, table: built}
}

func (view *Secrets) Title() string { return "Secrets" }
func (view *Secrets) Hints() string { return "d delete · r refresh · (ai secrets set adds one)" }

// SetSize fits the table to the content area the parent allots it.
func (view *Secrets) SetSize(width, height int) {
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
}

// Init kicks off the first credential list.
func (view *Secrets) Init() tea.Cmd { return view.refreshCmd() }

func (view *Secrets) refreshCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		entries, err := list()
		return secretsRefreshedMsg{entries: entries, err: err}
	}
}

// Update advances the view: a refresh repopulates the table; "d" deletes the
// selected credential (async); "r" refreshes; other keys drive navigation.
func (view *Secrets) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case secretsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.entries = message.entries
			view.table.SetRows(secretRows(message.entries))
		}
		return nil
	case secretRemoveDoneMsg:
		view.flash = secretRemoveFlash(message)
		return view.refreshCmd()
	case tea.KeyMsg:
		switch message.String() {
		case "d":
			name := view.selectedName()
			if name == "" {
				return nil
			}
			view.flash = ui.Muted.Render("deleting " + name + "…")
			return view.removeCmd(name)
		case "r":
			return view.refreshCmd()
		}
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

func (view *Secrets) removeCmd(name string) tea.Cmd {
	remove := view.remove
	return func() tea.Msg {
		return secretRemoveDoneMsg{name: name, err: remove(name)}
	}
}

// selectedName returns the credential name in the highlighted row (or "").
func (view *Secrets) selectedName() string {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

// View renders the credential table (or a load/error line) with the latest flash.
func (view *Secrets) View() string {
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading credentials…")
	}
	if len(view.entries) == 0 {
		empty := ui.Muted.Render("no credentials stored — add one with `ai secrets set`")
		if view.flash != "" {
			return view.flash + "\n" + empty
		}
		return empty
	}
	if view.flash != "" {
		return view.flash + "\n" + view.table.View()
	}
	return view.table.View()
}

func secretRows(entries []secrets.Entry) []table.Row {
	rows := make([]table.Row, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, table.Row{entry.Name, entry.EnvVar})
	}
	return rows
}

// secretRemoveFlash renders the outcome of a credential delete.
func secretRemoveFlash(msg secretRemoveDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " delete " + msg.name + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " deleted " + msg.name)
}
