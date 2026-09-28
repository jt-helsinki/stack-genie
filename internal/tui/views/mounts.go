package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// MountsGetter returns the current project's isolated dirs (config.yaml
// workspace.isolated_dirs) and shared mounts (config.yaml workspace.shared_mounts).
// Injected; the parent wires it over config.LoadProjectConfig.
type MountsGetter func(root string) (isolatedDirs []string, sharedMounts []config.SharedMount, err error)

// MountsMutator adds or removes one isolated dir (add/remove) — identified by its
// guest-relative path. Injected; the parent wires it over
// config.LoadProjectConfig/WriteProject so this view holds no config I/O.
type MountsMutator func(root, dir string) error

// SharedMountAdder adds one shared mount (an absolute guest path bind-mounted from a
// host path, visible both ways). Injected; the parent wires it over
// config.LoadProjectConfig/WriteProject.
type SharedMountAdder func(root, guestPath, hostPath string) error

// MountRemover removes one mount by its guest path — a relative path removes an
// isolated dir, an absolute path removes a shared mount (mirrors `ai mounts remove`'s
// disambiguation). Injected; the parent wires it over
// config.LoadProjectConfig/WriteProject.
type MountRemover func(root, guestPath string) error

type mountsRefreshedMsg struct {
	isolatedDirs []string
	sharedMounts []config.SharedMount
	err          error
}

type mountsMutateDoneMsg struct {
	action string
	dir    string
	err    error
}

// mountRow is one rendered row — an isolated dir or a shared mount, uniformly
// addressable by GuestPath (see MountRemover).
type mountRow struct {
	kind      string // "isolated" or "shared"
	guestPath string
	hostPath  string // only for kind == "shared"
}

// Mounts is the per-project view of every mount beyond the base project bind mount:
// ISOLATED dirs (config.yaml workspace.isolated_dirs — a guest-relative subpath
// excluded from the host mount and backed by a private, workspace-scoped volume
// instead, so sandbox-built artifacts never touch the host directory) and SHARED
// mounts (config.yaml workspace.shared_mounts — an extra host directory bind-mounted
// at an arbitrary guest path, visible and writable on both sides). The mount set is
// applied at microVM create, so every mutation here needs (and reminds about) a
// restart to take effect.
type Mounts struct {
	current   CurrentRoot
	get       MountsGetter
	addDir    MountsMutator
	addShared SharedMountAdder
	remove    MountRemover

	rows   []mountRow
	cursor int
	flash  string
	err    error
	loaded bool

	// inputMode is "add-dir" | "edit-dir" | "add-shared-guest" | "add-shared-host" |
	// "edit-shared-guest" | "edit-shared-host" while an inline prompt is active, empty
	// otherwise. input is the value typed so far; pendingGuest carries the guest path
	// while a shared-mount add/edit is between its two prompts; editIndex is the row
	// being replaced by an edit.
	inputMode    string
	input        string
	pendingGuest string
	editIndex    int
}

// NewMounts builds the Mounts view over the current project (add/remove isolated dirs
// and shared host mounts).
func NewMounts(current CurrentRoot, get MountsGetter, addDir MountsMutator, addShared SharedMountAdder, remove MountRemover) *Mounts {
	return &Mounts{current: current, get: get, addDir: addDir, addShared: addShared, remove: remove}
}

func (view *Mounts) Title() string { return "Mounts" }

func (view *Mounts) CapturingInput() bool { return view.inputMode != "" }

func (view *Mounts) Hints() string {
	return "a add isolated dir · s add shared mount · d delete · restart to apply"
}

func (view *Mounts) SetSize(int, int) {}

// Init refreshes the current project's mounts (no-op with no project).
func (view *Mounts) Init() tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	return view.refreshCmd(root)
}

func (view *Mounts) refreshCmd(root string) tea.Cmd {
	get := view.get
	return func() tea.Msg {
		isolatedDirs, sharedMounts, err := get(root)
		return mountsRefreshedMsg{isolatedDirs: isolatedDirs, sharedMounts: sharedMounts, err: err}
	}
}

// Update advances the view: a refresh fills the row list; add/edit/delete run async
// and re-refresh on completion so the list reflects the change immediately.
func (view *Mounts) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case mountsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.rows = mountRowsFrom(message.isolatedDirs, message.sharedMounts)
			if view.cursor >= len(view.rows) {
				view.cursor = max(0, len(view.rows)-1)
			}
		}
		return nil
	case mountsMutateDoneMsg:
		if message.err != nil {
			view.flash = ui.Failure.Render(ui.IconFail + " " + message.action + " " + message.dir + ": " + message.err.Error())
		} else {
			view.flash = ui.Success.Render(ui.IconOK+" "+message.action+" applied — restart the workspace (") +
				ui.Primary.Render("ai restart") + ui.Success.Render(") to apply it")
		}
		root, ok := view.current()
		if !ok {
			return nil
		}
		return view.refreshCmd(root)
	case tea.KeyMsg:
		if view.inputMode != "" {
			return view.handleInputKey(message)
		}
		return view.handleActionKey(message)
	}
	return nil
}

// mountRowsFrom combines isolated dirs and shared mounts into one displayed list,
// isolated dirs first.
func mountRowsFrom(isolatedDirs []string, sharedMounts []config.SharedMount) []mountRow {
	rows := make([]mountRow, 0, len(isolatedDirs)+len(sharedMounts))
	for _, dir := range isolatedDirs {
		rows = append(rows, mountRow{kind: "isolated", guestPath: dir})
	}
	for _, mount := range sharedMounts {
		rows = append(rows, mountRow{kind: "shared", guestPath: mount.GuestPath, hostPath: mount.HostPath})
	}
	return rows
}

// handleActionKey navigates the list or starts an inline add/edit prompt; "d" deletes
// the selected row immediately (reversible — re-add restores it).
func (view *Mounts) handleActionKey(key tea.KeyMsg) tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	switch key.String() {
	case "up", "k":
		if view.cursor > 0 {
			view.cursor--
		}
	case "down", "j":
		if view.cursor < len(view.rows)-1 {
			view.cursor++
		}
	case "a":
		view.startInput("add-dir", "", -1)
	case "s":
		view.startInput("add-shared-guest", "", -1)
	case "e":
		if view.cursor < len(view.rows) {
			row := view.rows[view.cursor]
			if row.kind == "shared" {
				view.editIndex = view.cursor
				view.inputMode, view.input, view.pendingGuest = "edit-shared-guest", row.guestPath, ""
				view.flash = ""
			} else {
				view.startInput("edit-dir", row.guestPath, view.cursor)
			}
		}
	case "d":
		if view.cursor < len(view.rows) {
			row := view.rows[view.cursor]
			view.flash = ui.Muted.Render("removing " + row.guestPath + "…")
			return view.mutateCmd("removed", row.guestPath, func() error { return view.remove(root, row.guestPath) })
		}
	}
	return nil
}

func (view *Mounts) startInput(mode, initial string, editIndex int) {
	view.inputMode, view.input, view.editIndex, view.pendingGuest = mode, initial, editIndex, ""
	view.flash = ""
}

// handleInputKey edits the active inline prompt: runes append, backspace deletes,
// enter applies (or, for a shared mount, advances to the second prompt), esc cancels
// the whole in-flight add/edit.
func (view *Mounts) handleInputKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc:
		view.inputMode, view.input, view.pendingGuest = "", "", ""
		return nil
	case tea.KeyEnter:
		return view.applyInput()
	case tea.KeyBackspace:
		runes := []rune(view.input)
		if len(runes) > 0 {
			view.input = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeyRunes:
		for _, char := range key.Runes {
			if char != ' ' {
				view.input += string(char)
			}
		}
		return nil
	}
	return nil
}

// applyInput advances or completes the active prompt on enter.
func (view *Mounts) applyInput() tea.Cmd {
	mode, value := view.inputMode, strings.TrimSpace(view.input)
	root, ok := view.current()
	if !ok {
		view.inputMode, view.input, view.pendingGuest = "", "", ""
		return nil
	}
	switch mode {
	case "add-dir":
		view.inputMode, view.input = "", ""
		if value == "" {
			return nil
		}
		view.flash = ui.Muted.Render("adding " + value + "…")
		return view.mutateCmd("added", value, func() error { return view.addDir(root, value) })
	case "edit-dir":
		editIndex := view.editIndex
		view.inputMode, view.input = "", ""
		if value == "" || editIndex < 0 || editIndex >= len(view.rows) {
			return nil
		}
		oldDir := view.rows[editIndex].guestPath
		view.flash = ui.Muted.Render("updating " + oldDir + " → " + value + "…")
		return tea.Sequence(
			view.mutateCmd("removed", oldDir, func() error { return view.remove(root, oldDir) }),
			view.mutateCmd("added", value, func() error { return view.addDir(root, value) }),
		)
	case "add-shared-guest", "edit-shared-guest":
		if value == "" {
			return nil
		}
		view.pendingGuest = value
		view.input = ""
		view.inputMode = "add-shared-host"
		if mode == "edit-shared-guest" {
			view.inputMode = "edit-shared-host"
		}
		return nil
	case "add-shared-host":
		guestPath := view.pendingGuest
		view.inputMode, view.input, view.pendingGuest = "", "", ""
		if value == "" {
			return nil
		}
		view.flash = ui.Muted.Render("adding " + guestPath + " ↔ " + value + "…")
		return view.mutateCmd("added", guestPath, func() error { return view.addShared(root, guestPath, value) })
	case "edit-shared-host":
		editIndex := view.editIndex
		guestPath := view.pendingGuest
		hostPath := value
		view.inputMode, view.input, view.pendingGuest = "", "", ""
		if hostPath == "" || editIndex < 0 || editIndex >= len(view.rows) {
			return nil
		}
		oldGuest := view.rows[editIndex].guestPath
		view.flash = ui.Muted.Render("updating " + oldGuest + " → " + guestPath + " ↔ " + hostPath + "…")
		return tea.Sequence(
			view.mutateCmd("removed", oldGuest, func() error { return view.remove(root, oldGuest) }),
			view.mutateCmd("added", guestPath, func() error { return view.addShared(root, guestPath, hostPath) }),
		)
	}
	return nil
}

func (view *Mounts) mutateCmd(action, dir string, run func() error) tea.Cmd {
	return func() tea.Msg {
		return mountsMutateDoneMsg{action: action, dir: dir, err: run()}
	}
}

// View renders the mount list (cursor-highlighted, kind-labeled) or the no-project /
// load / error line, with the active inline prompt(s) or the latest flash.
func (view *Mounts) View() string {
	if _, ok := view.current(); !ok {
		return ui.Muted.Render("no project selected — open one from the Projects view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading mounts…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Mounts") + "\n")
	body.WriteString(ui.Muted.Render("Isolated dirs are guest-private (own volume, not on host); shared mounts are visible both ways.") + "\n")
	if len(view.rows) == 0 {
		body.WriteString(ui.Muted.Render("(none — only the project directory is mounted into the workspace)") + "\n")
	}
	for index, row := range view.rows {
		cursor := "  "
		if index == view.cursor && view.inputMode == "" {
			cursor = ui.Primary.Render("> ")
		}
		if row.kind == "shared" {
			body.WriteString(cursor + ui.Muted.Render("[shared] ") + ui.Value.Render(row.guestPath+" ↔ "+row.hostPath) + "\n")
		} else {
			body.WriteString(cursor + ui.Muted.Render("[isolated] ") + ui.Value.Render(row.guestPath) + "\n")
		}
	}
	switch {
	case view.inputMode != "":
		body.WriteString("\n" + mountsPromptLine(view.inputMode, view.pendingGuest, view.input))
	case view.flash != "":
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// mountsPromptLine renders the active inline prompt's title + hint for the current
// step of an add/edit.
func mountsPromptLine(mode, pendingGuest, input string) string {
	title, hint := "add", "guest-relative dir · enter apply · esc cancel"
	switch mode {
	case "edit-dir":
		title, hint = "edit", "guest-relative dir · enter apply · esc cancel"
	case "add-shared-guest":
		title, hint = "add shared: guest path", "absolute guest path (e.g. /home/workspace/shared) · enter next · esc cancel"
	case "edit-shared-guest":
		title, hint = "edit shared: guest path", "absolute guest path · enter next · esc cancel"
	case "add-shared-host", "edit-shared-host":
		title, hint = "add shared: "+pendingGuest+" ↔ host path", "host directory, e.g. ~/Downloads · enter apply · esc cancel"
	}
	return ui.Heading.Render(title+" ") + input + "▏" + ui.Muted.Render("  ("+hint+")")
}
