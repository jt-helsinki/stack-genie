package views

import (
	"github.com/charmbracelet/bubbles/filepicker"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/tui/scope"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// CreateConfirmedMsg is emitted when the user confirms a valid directory for a new
// workspace. The parent app then runs `ai create` with its working directory
// set to Dir, and refreshes the Workspaces switcher.
type CreateConfirmedMsg struct {
	Dir string
}

// CreateCancelledMsg is emitted when the user cancels the create flow (esc); the
// parent returns to whichever view it came from (typically Projects).
type CreateCancelledMsg struct{}

// Create is a directory picker for choosing where to create a new workspace. It is
// configured for directories only and validates the chosen directory with
// scope.ValidateCreateTarget — a directory that is already a workspace root is
// rejected (with an inline error), while a child or parent of an existing workspace
// is allowed.
type Create struct {
	picker        filepicker.Model
	targetDir     string
	validationErr error
}

// NewCreate builds the workspace-create view starting at startDir (the cwd),
// configured to browse and select directories only.
func NewCreate(startDir string) *Create {
	picker := filepicker.New()
	picker.DirAllowed = true
	picker.FileAllowed = false
	picker.CurrentDirectory = startDir
	return &Create{picker: picker}
}

// Title is the view's name (used by the menu/header).
func (view *Create) Title() string { return "New Workspace" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Create) Hints() string { return "↑/↓ browse · enter choose dir · esc cancel" }

// SetSize fits the filepicker to the content area the parent allots it.
func (view *Create) SetSize(_, height int) {
	if height > 0 {
		view.picker.AutoHeight = false
		view.picker.SetHeight(height)
	}
}

// Init reads the starting directory.
func (view *Create) Init() tea.Cmd { return view.picker.Init() }

// Update advances the picker: esc cancels (CreateCancelledMsg); selecting a
// directory validates it — a valid choice emits CreateConfirmedMsg, an invalid one
// records the error inline and stays on the picker. All other input drives the
// filepicker (browsing in/out of directories).
func (view *Create) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "esc" {
		return func() tea.Msg { return CreateCancelledMsg{} }
	}

	var cmd tea.Cmd
	view.picker, cmd = view.picker.Update(msg)

	if selected, path := view.picker.DidSelectFile(msg); selected {
		view.targetDir = path
		if err := scope.ValidateCreateTarget(path); err != nil {
			view.validationErr = err
			return cmd
		}
		view.validationErr = nil
		return func() tea.Msg { return CreateConfirmedMsg{Dir: path} }
	}
	return cmd
}

// View renders the picker, the current target, and any inline validation error.
func (view *Create) View() string {
	body := view.picker.View()
	if view.targetDir != "" {
		body += "\n" + ui.Muted.Render("target: "+view.targetDir)
	}
	if view.validationErr != nil {
		body += "\n" + ui.Failure.Render(ui.IconFail+" "+view.validationErr.Error())
	}
	return body
}
