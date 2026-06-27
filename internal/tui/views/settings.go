package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ThemeApplier applies AND persists a theme by name. Injected; the parent wires
// ui.Apply followed by ui.SaveThemeName. Returns an error if the name is invalid
// or the save fails.
type ThemeApplier func(name string) error

// ThemeChangedMsg is emitted after a theme is applied so the parent app can
// re-push sizes to every view (their tables re-pick the theme's styles in
// SetSize), making the change fully live across the UI — not just the chrome.
type ThemeChangedMsg struct{ Name string }

// Settings is the global-settings screen: a live theme picker plus read-only
// platform info (deployment role / model gateway). Selecting a theme applies it
// to the whole UI immediately and persists it for future sessions; the role and
// gateway are shown for reference (changed via `ai gateway` / `ai setup`).
type Settings struct {
	themes  []string
	current func() string // the currently-applied theme name
	apply   ThemeApplier
	role    string
	gateway string
	table   table.Model
	flash   string
}

// NewSettings builds the settings view over the theme list, a getter for the
// currently-applied theme, the apply+persist func, and the platform role/gateway
// to display.
func NewSettings(themes []string, current func() string, apply ThemeApplier, role, gateway string) *Settings {
	columns := []table.Column{{Title: "THEME", Width: 22}, {Title: "", Width: 16}}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	view := &Settings{themes: themes, current: current, apply: apply, role: role, gateway: gateway, table: built}
	view.refreshRows()
	return view
}

// Title is the tab label.
func (view *Settings) Title() string { return "Settings" }

// Hints are the key bindings shown in the header grid.
func (view *Settings) Hints() string { return "enter apply theme · ↑/↓ select" }

// SetSize fits the theme table, leaving room for the platform info block below.
// It also re-applies the table styles so a just-applied theme recolors the table.
func (view *Settings) SetSize(width, height int) {
	view.table.SetStyles(ui.TableStyles())
	view.table.SetWidth(width)
	// Reserve the exact non-table lines View() emits (must match it): Theme heading
	// (1) + flash slot (1) + Platform heading (1) + role/gateway/change lines (3) = 6,
	// so the table fills the rest and the block sits at the constant bottom margin.
	if tableHeight := height - 6; tableHeight > 0 {
		view.table.SetHeight(tableHeight)
	}
}

// Init has nothing to fetch (the theme list + role/gateway are static).
func (view *Settings) Init() tea.Cmd { return nil }

// refreshRows rebuilds the theme rows, marking the currently-applied one.
func (view *Settings) refreshRows() {
	current := view.current()
	rows := make([]table.Row, 0, len(view.themes))
	for _, name := range view.themes {
		marker := ""
		if name == current {
			marker = "● applied"
		}
		// Cell values are PLAIN (no ANSI): bubbles/table truncates cells with a
		// width function that is NOT ANSI-aware, so colour codes inside a cell get
		// counted as width — cutting the visible text early and slicing through a
		// colour sequence (the stray "[[0m" artefact). The applied theme is marked
		// by the "● applied" text; the highlighted row is coloured by the table's
		// own Selected style (ui.TableStyles), which is applied AFTER truncation.
		rows = append(rows, table.Row{name, marker})
	}
	view.table.SetRows(rows)
}

// Update applies the highlighted theme on enter (emitting ThemeChangedMsg so the
// whole UI recolors); other keys drive table navigation.
func (view *Settings) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "enter" {
		// Resolve the theme name from the cursor index into the source list (robust
		// regardless of cell rendering).
		cursor := view.table.Cursor()
		if cursor < 0 || cursor >= len(view.themes) {
			return nil
		}
		name := view.themes[cursor]
		if err := view.apply(name); err != nil {
			view.flash = ui.Failure.Render(ui.IconFail + " " + err.Error())
			return nil
		}
		view.flash = ui.Success.Render(ui.IconOK + " theme " + name + " applied")
		view.refreshRows()
		return func() tea.Msg { return ThemeChangedMsg{Name: name} }
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// View renders the theme picker above a read-only platform info block. The line
// budget here MUST match SetSize's reserve so the table fills the content height and
// the block sits at the constant bottom margin (the flash slot is always rendered,
// blank when empty, so the height is invariant whether or not a flash shows).
func (view *Settings) View() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Theme") + "\n")                            // 1
	body.WriteString(view.table.View() + "\n")                                     // table + flash slot:
	body.WriteString(flashLine(view.flash) + "\n")                                 // 1 (blank when empty)
	body.WriteString(ui.Heading.Render("Platform") + "\n")                         // 1
	body.WriteString(field("role", view.role))                                     // 1
	body.WriteString(field("gateway", view.gateway))                               // 1
	body.WriteString(ui.Muted.Render("  (change with `ai gateway` / `ai setup`)")) // 1
	return body.String()
}
