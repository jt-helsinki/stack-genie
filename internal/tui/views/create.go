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

// Create is the location field for a new workspace: a single text input that
// starts in the current directory, expands a leading ~, offers directory
// autocompletion (tab to accept, ↑/↓ to cycle), and validates the entered
// location with scope.ValidateCreateTarget — rejecting a path that is (or is
// nested inside) an existing workspace. A path that does not exist is allowed and
// is CREATED (with intermediate folders) on confirm.
type Create struct {
	input         textinput.Model
	validationErr error
	width         int
}

// NewCreate builds the workspace-create location field, pre-filled with startDir
// (the current directory) so browsing/autocompletion starts there. When startDir
// is empty it falls back to the process working directory, then the home dir.
func NewCreate(startDir string) *Create {
	input := textinput.New()
	input.Prompt = "location: "
	input.Placeholder = "~/path/to/new-workspace"
	input.ShowSuggestions = true
	input.SetValue(ensureTrailingSep(createStartDir(startDir)))
	input.CursorEnd()
	input.Focus()
	view := &Create{input: input}
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
	return "type a path · tab complete · ↑/↓ suggestions · enter create · esc cancel"
}

// SetSize records the pane width and fits the input to it.
func (view *Create) SetSize(width, _ int) {
	view.width = width
	if width > len(view.input.Prompt)+8 {
		view.input.Width = width - len(view.input.Prompt) - 4
	}
}

// Init starts the input cursor blinking.
func (view *Create) Init() tea.Cmd { return textinput.Blink }

// Update advances the input: esc cancels (CreateCancelledMsg); enter expands ~,
// validates the location, creates it (and intermediate folders) if missing, and on
// success emits CreateConfirmedMsg — recording any error inline and staying on the
// field otherwise. All other input drives the text field (typing, autocompletion).
func (view *Create) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			return func() tea.Msg { return CreateCancelledMsg{} }
		case "enter":
			return view.confirm()
		}
	}

	var cmd tea.Cmd
	view.input, cmd = view.input.Update(msg)
	view.refreshSuggestions()
	view.validationErr = nil // clear a stale error as the user edits
	return cmd
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

// refreshSuggestions recomputes the directory autocompletion for the current input.
func (view *Create) refreshSuggestions() {
	view.input.SetSuggestions(dirSuggestions(view.input.Value()))
}

// View renders the prompt, the input, and any inline validation error.
func (view *Create) View() string {
	body := ui.Muted.Render("Enter a location for the new workspace. A path that doesn't exist is created (with intermediate folders).") + "\n\n"
	body += view.input.View()
	if view.validationErr != nil {
		body += "\n\n" + ui.Failure.Render(ui.IconFail+" "+view.validationErr.Error())
	}
	return body
}

// dirSuggestions returns absolute directory paths under the input's parent that
// prefix-match the partial final segment — the autocompletion set. Hidden dirs are
// skipped; each suggestion carries a trailing separator so accepting one lets the
// user keep descending. A leading ~ is expanded first.
func dirSuggestions(path string) []string {
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
	suggestions := make([]string, 0, 64)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(entry.Name(), partial) {
			continue
		}
		suggestions = append(suggestions, ensureTrailingSep(filepath.Join(dir, entry.Name())))
		if len(suggestions) >= 64 {
			break
		}
	}
	return suggestions
}

// expandTilde replaces a leading ~ (or ~/…) with the user's home directory. It
// preserves a trailing separator (filepath.Join would drop it) so a value like
// "~/" expands to "<home>/" and the autocompletion splitter sees an empty partial
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
// lists its own children in the autocompletion (rather than its siblings).
func ensureTrailingSep(path string) string {
	if path == "" || strings.HasSuffix(path, string(os.PathSeparator)) {
		return path
	}
	return path + string(os.PathSeparator)
}
