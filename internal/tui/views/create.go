package views

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/tui/scope"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// CreateConfirmedMsg is emitted when the user confirms a valid location for a new
// workspace. Dir is an absolute, existing directory (the view creates it, and any
// intermediate folders, before emitting). The parent app then runs `ai create`
// with its working directory set to Dir and refreshes the Workspaces switcher.
type CreateConfirmedMsg struct {
	Dir string
}

// CreateCancelledMsg is emitted when the user cancels the create flow (esc); the
// parent returns to whichever view it came from (typically Projects).
type CreateCancelledMsg struct{}

// dropdownMaxVisible caps the visible rows of the folder dropdown when the pane
// height is unknown or generous.
const dropdownMaxVisible = 10

// folderRow is one dropdown entry: a folder under the entered path. A folder that
// already IS a workspace is disabled — greyed out and not selectable.
type folderRow struct {
	path     string // absolute path, with a trailing separator (for completion)
	name     string // display base name
	disabled bool   // already a workspace — cannot host a new one
}

// Create is the location field for a new workspace: a text input plus a live
// dropdown of the folders under the entered path. The user either types to filter
// the dropdown (tab completes the highlighted row) or presses ↓ to move into the
// list and select a folder. A leading ~ expands to the home directory. The path is
// validated with scope.ValidateCreateTarget — a path that is (or is nested inside)
// an existing workspace is rejected — and a non-existent path is CREATED (with
// intermediate folders) on confirm.
type Create struct {
	input         textinput.Model
	suggestions   []folderRow // folders under the input's parent (filtered)
	selected      int         // index into suggestions; -1 = typing in the input, no row highlighted
	scroll        int         // first visible dropdown row
	validationErr error
	width         int
	height        int
}

// NewCreate builds the workspace-create location field, pre-filled with startDir
// (the current directory) so the dropdown starts by listing its folders. When
// startDir is empty it falls back to the process working directory, then the home.
func NewCreate(startDir string) *Create {
	input := textinput.New()
	input.Prompt = "location: "
	input.Placeholder = "~/path/to/new-workspace"
	input.ShowSuggestions = false // we render our own navigable dropdown instead
	input.SetValue(ensureTrailingSep(createStartDir(startDir)))
	input.CursorEnd()
	input.Focus()
	view := &Create{input: input, selected: -1}
	view.refreshSuggestions()
	return view
}

// createStartDir resolves the initial location: startDir when given, else the
// process working directory, else the home directory, else ".".
func createStartDir(startDir string) string {
	if strings.TrimSpace(startDir) != "" {
		return startDir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}

// Title is the view's name (used by the menu/header).
func (view *Create) Title() string { return "New Workspace" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Create) Hints() string {
	return "type to filter · ↓/↑ select folder · tab complete · enter create here · esc cancel"
}

// SetSize records the pane size and fits the input to the width.
func (view *Create) SetSize(width, height int) {
	view.width = width
	view.height = height
	if width > len(view.input.Prompt)+8 {
		view.input.Width = width - len(view.input.Prompt) - 4
	}
}

// Init starts the input cursor blinking.
func (view *Create) Init() tea.Cmd { return textinput.Blink }

// Update advances the field. esc cancels; enter confirms (create + emit); ↓/↑ move
// the dropdown highlight; tab completes the highlighted (or first) folder into the
// input, descending into it. Any key that edits the text re-filters the dropdown
// and drops the highlight back to the input row.
func (view *Create) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			return func() tea.Msg { return CreateCancelledMsg{} }
		case "enter":
			return view.confirm()
		case "down":
			view.moveSelection(1)
			return nil
		case "up":
			view.moveSelection(-1)
			return nil
		case "tab":
			view.complete()
			return nil
		}
	}

	before := view.input.Value()
	var cmd tea.Cmd
	view.input, cmd = view.input.Update(msg)
	if view.input.Value() != before {
		// The text changed: re-filter the dropdown and return to the input row.
		view.refreshSuggestions()
		view.selected = -1
		view.scroll = 0
		view.validationErr = nil
	}
	return cmd
}

// moveSelection moves the dropdown highlight by delta, SKIPPING disabled
// (already-a-workspace) rows so only selectable folders can be highlighted.
// selected == -1 is the input row (no folder highlighted); ↓ from there enters the
// list, ↑ past the top selectable row returns to the input. When no selectable row
// exists in the delta direction, the highlight stays put.
func (view *Create) moveSelection(delta int) {
	if len(view.suggestions) == 0 {
		view.selected = -1
		view.scroll = 0
		return
	}
	index := view.selected
	for {
		index += delta
		if index < 0 { // moved above the top → back to the input row
			view.selected = -1
			view.scroll = 0
			return
		}
		if index >= len(view.suggestions) { // nothing selectable below → stay put
			return
		}
		if !view.suggestions[index].disabled {
			view.selected = index
			view.adjustScroll()
			return
		}
	}
}

// firstSelectable returns the index of the first non-disabled row, or -1.
func (view *Create) firstSelectable() int {
	for index := range view.suggestions {
		if !view.suggestions[index].disabled {
			return index
		}
	}
	return -1
}

// complete fills the input with the highlighted folder (or the first selectable
// one, when none is highlighted), so the dropdown then lists that folder's
// children — i.e. descend. A disabled row is never completed.
func (view *Create) complete() {
	index := view.selected
	if index < 0 {
		index = view.firstSelectable()
	}
	if index < 0 || index >= len(view.suggestions) || view.suggestions[index].disabled {
		return
	}
	view.input.SetValue(view.suggestions[index].path) // rows carry a trailing separator
	view.input.CursorEnd()
	view.refreshSuggestions()
	view.selected = -1
	view.scroll = 0
	view.validationErr = nil
}

// confirm resolves the entered location and, if valid, creates it and emits
// CreateConfirmedMsg. Any failure is recorded in validationErr and no message is
// emitted (the user stays on the field to correct it).
func (view *Create) confirm() tea.Cmd {
	target, err := expandTilde(strings.TrimSpace(view.input.Value()))
	if err != nil {
		view.validationErr = err
		return nil
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		view.validationErr = fmt.Errorf("resolve %q: %w", target, err)
		return nil
	}
	if err := scope.ValidateCreateTarget(absolute); err != nil {
		view.validationErr = err
		return nil
	}
	// Create the location (and any intermediate folders) so the wizard runs in an
	// existing directory. Validation above already rejected nesting inside an
	// existing workspace, so this only ever creates fresh directories.
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		view.validationErr = fmt.Errorf("create %q: %w", absolute, err)
		return nil
	}
	view.validationErr = nil
	return func() tea.Msg { return CreateConfirmedMsg{Dir: absolute} }
}

// refreshSuggestions recomputes the folder dropdown for the current input value.
func (view *Create) refreshSuggestions() {
	view.suggestions = dirSuggestions(view.input.Value())
}

// maxVisible is how many dropdown rows fit: the pane height minus the intro,
// input, and error lines, clamped to a sane band.
func (view *Create) maxVisible() int {
	visible := dropdownMaxVisible
	if view.height > 0 {
		visible = view.height - 6 // intro (2) + input (1) + spacing/error headroom
	}
	if visible < 3 {
		visible = 3
	}
	if visible > dropdownMaxVisible {
		visible = dropdownMaxVisible
	}
	return visible
}

// adjustScroll keeps the highlighted row inside the visible dropdown window.
func (view *Create) adjustScroll() {
	if view.selected < 0 {
		view.scroll = 0
		return
	}
	maxVisible := view.maxVisible()
	if view.selected < view.scroll {
		view.scroll = view.selected
	}
	if view.selected >= view.scroll+maxVisible {
		view.scroll = view.selected - maxVisible + 1
	}
}

// View renders the intro, the input, the folder dropdown, and any inline error.
func (view *Create) View() string {
	body := ui.Muted.Render("Enter a location for the new workspace. A path that doesn't exist is created (with intermediate folders).") + "\n\n"
	body += view.input.View() + "\n"
	body += view.dropdownView()
	if view.validationErr != nil {
		body += "\n\n" + ui.Failure.Render(ui.IconFail+" "+view.validationErr.Error())
	}
	return body
}

// dropdownView renders the visible window of folder rows: the highlighted one
// accented, plain selectable rows in normal text, and folders that are already a
// workspace GREYED OUT with a "workspace already exists" note (never selectable).
// ↑/↓ overflow markers show when the list scrolls.
func (view *Create) dropdownView() string {
	if len(view.suggestions) == 0 {
		return ui.Muted.Render("  (no matching folders — enter creates the path as typed)")
	}
	maxVisible := view.maxVisible()
	end := view.scroll + maxVisible
	if end > len(view.suggestions) {
		end = len(view.suggestions)
	}
	var builder strings.Builder
	if view.scroll > 0 {
		builder.WriteString(ui.Muted.Render("  ↑ more") + "\n")
	}
	for index := view.scroll; index < end; index++ {
		row := view.suggestions[index]
		name := row.name + string(os.PathSeparator)
		switch {
		case row.disabled:
			builder.WriteString(ui.Muted.Render("  "+name+"  — workspace already exists") + "\n")
		case index == view.selected:
			builder.WriteString(ui.Primary.Bold(true).Render("› "+name) + "\n")
		default:
			builder.WriteString("  " + ui.Value.Render(name) + "\n")
		}
	}
	if end < len(view.suggestions) {
		builder.WriteString(ui.Muted.Render("  ↓ more"))
	}
	return strings.TrimRight(builder.String(), "\n")
}

// dirSuggestions returns the folders under the input's parent that prefix-match the
// partial final segment — the dropdown set. Hidden dirs are skipped; each row's
// path carries a trailing separator so completing one descends into it. A folder
// that is already a workspace root is flagged disabled. A leading ~ is expanded
// first.
func dirSuggestions(path string) []folderRow {
	expanded, err := expandTilde(path)
	if err != nil {
		return nil
	}
	dir, partial := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	rows := make([]folderRow, 0, 64)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(entry.Name(), partial) {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		rows = append(rows, folderRow{
			path:     ensureTrailingSep(full),
			name:     entry.Name(),
			disabled: scope.IsProjectRoot(full),
		})
		if len(rows) >= 64 {
			break
		}
	}
	return rows
}

// expandTilde replaces a leading ~ (or ~/…) with the user's home directory. It
// preserves a trailing separator (filepath.Join would drop it) so a value like
// "~/" expands to "<home>/" and the dropdown splitter sees an empty partial
// segment — i.e. lists the home directory's children, not its siblings.
func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	expanded := filepath.Join(home, path[2:])
	if strings.HasSuffix(path, "/") && !strings.HasSuffix(expanded, string(os.PathSeparator)) {
		expanded += string(os.PathSeparator)
	}
	return expanded, nil
}

// ensureTrailingSep appends the OS path separator when absent so a directory value
// lists its own children in the dropdown (rather than its siblings).
func ensureTrailingSep(path string) string {
	if path == "" || strings.HasSuffix(path, string(os.PathSeparator)) {
		return path
	}
	return path + string(os.PathSeparator)
}
