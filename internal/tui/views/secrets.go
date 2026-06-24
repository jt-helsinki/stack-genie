package views

import (
	"strings"

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
	list     SecretLister
	remove   SecretRemover
	table    table.Model
	describe describePane // detail pane for the selected credential (enter / d)
	entries  []secrets.Entry
	flash    string
	err      error
	loaded   bool
}

// NewSecrets builds the secrets view over the injected lister and remover.
func NewSecrets(list SecretLister, remove SecretRemover) *Secrets {
	columns := []table.Column{
		{Title: "NAME", Width: 30},
		{Title: "ENV VAR", Width: 30},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &Secrets{list: list, remove: remove, table: built, describe: newDescribePane()}
}

func (view *Secrets) Title() string { return "Secrets" }

// WantsEsc reports that esc should close the open describe pane first (rather than
// the Projects hub using esc to back out of the project). See escConsumer.
func (view *Secrets) WantsEsc() bool { return view.describe.active() }
func (view *Secrets) Hints() string {
	return "enter describe · d delete · r refresh · (ai secrets set adds one)"
}

// SetSize fits the table + describe pane to the content area the parent allots it.
func (view *Secrets) SetSize(width, height int) {
	view.table.SetStyles(ui.TableStyles()) // pick up a live theme change
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
	view.describe.setSize(width, height)
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
		// Menu keys stay live even while the describe pane is open; scroll keys + esc
		// fall through to the pane.
		switch message.String() {
		case "enter":
			// Drill into the selected credential's detail.
			name := view.selectedName()
			if name == "" {
				return nil
			}
			view.describe.show(describeSecret(view.entryByName(name)))
			return nil
		case "d":
			name := view.selectedName()
			if name == "" {
				return nil
			}
			view.describe.close() // surface the delete flash over the table
			view.flash = ui.Muted.Render("deleting " + name + "…")
			return view.removeCmd(name)
		case "r":
			return view.refreshCmd()
		}
		if view.describe.active() {
			return view.describe.update(message)
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

// entryByName returns the cached entry for a credential (a name-only fallback if
// it is not in the latest list).
func (view *Secrets) entryByName(name string) secrets.Entry {
	for _, entry := range view.entries {
		if entry.Name == name {
			return entry
		}
	}
	return secrets.Entry{Name: name}
}

// describeSecret renders a credential's detail for the describe pane — name and
// the workspace env-var placeholder only; the value never leaves LiteLLM.
func describeSecret(entry secrets.Entry) string {
	envVar := entry.EnvVar
	if envVar == "" {
		envVar = ui.Muted.Render("(not mapped to a workspace env var)")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(entry.Name) + "\n")
	body.WriteString(field("env var", envVar))
	body.WriteString(field("value", ui.Muted.Render("stored in LiteLLM — never on platform disk")))
	return body.String()
}

// View renders the describe pane when open, else the credential table (or a
// load/error line) with the latest flash.
func (view *Secrets) View() string {
	if view.describe.active() {
		return view.describe.view()
	}
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
