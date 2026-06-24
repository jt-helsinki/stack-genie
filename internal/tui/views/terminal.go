package views

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// vt10x glyph attribute bits (it does not export them). Mirrors the iota order in
// the library's state.go; vt10x is unmaintained, so these are stable.
const (
	attrReverse   = 1 << 0
	attrUnderline = 1 << 1
	attrBold      = 1 << 2
)

// defaultColorFloor is vt10x's first "default" color sentinel (DefaultFG); any
// Color at or above it is a terminal default rather than a palette index.
const defaultColorFloor = 1 << 24

// Terminal is a LIVE embedded terminal pane: it runs a command on a pseudo-terminal
// (creack/pty) and renders the program's screen inside the TUI via a vt10x emulator,
// forwarding keystrokes to the program. It is how `ai ui` runs workspace lifecycle
// (start/stop/restart/destroy) and interactive sessions (shell/agent/attach) WITHOUT
// suspending the TUI — msb's build/boot progress and a full interactive shell both
// render in the pane. The parent app owns it as an overlay (see tui.go): while it is
// open, input is routed here; ctrl+q force-detaches and, once the process exits, any
// key closes it.
//
// The host-side mechanism (PTY + emulator + key encoding + render) is exercised by
// tests with a real local command; the in-VM behavior of `msb exec -t` over this PTY
// is a hardware-bring-up seam verified on Apple Silicon.
type Terminal struct {
	label string
	argv  []string

	emulator vt10x.Terminal
	dirty    chan struct{}
	done     chan error

	// mu guards ptmx/cmd, which are set by the spawn command (a bubbletea cmd
	// goroutine) and read by SetSize / key-forwarding / Close on the main loop.
	mu   sync.Mutex
	ptmx *os.File
	cmd  *exec.Cmd

	width, height int
	exited        bool
	exitErr       error
}

// terminalStartedMsg signals the PTY spawn succeeded; the parent then begins the
// output wait loop.
type terminalStartedMsg struct{}

// terminalDirtyMsg signals new program output is available to render; the parent
// re-issues the wait command so the loop continues until the process exits.
type terminalDirtyMsg struct{}

// terminalExitMsg signals the program has exited (cleanly or with err).
type terminalExitMsg struct{ err error }

// NewTerminal builds a terminal that will run argv (argv[0] is the program). label
// is a short description for the header.
func NewTerminal(label string, argv []string) *Terminal {
	return &Terminal{label: label, argv: argv}
}

// Label is the short description shown in the chrome while the terminal is open.
func (term *Terminal) Label() string { return term.label }

// Exited reports whether the program has finished (so the parent can let any key
// close the overlay).
func (term *Terminal) Exited() bool { return term.exited }

func (term *Terminal) cols() int {
	if term.width > 0 {
		return term.width
	}
	return 80
}

func (term *Terminal) rows() int {
	if term.height > 0 {
		return term.height
	}
	return 24
}

// Init prepares the emulator + channels (synchronously, on the main loop) and
// returns the command that SPAWNS the PTY. Spawning is deferred to the command so
// it happens when bubbletea runs it, not while Update is processing the open —
// callers that only inspect the model (tests) never launch a process.
func (term *Terminal) Init() tea.Cmd {
	term.emulator = vt10x.New(vt10x.WithSize(term.cols(), term.rows()))
	term.dirty = make(chan struct{}, 1)
	term.done = make(chan error, 1)
	return func() tea.Msg { return term.spawn() }
}

// spawn starts the command on a PTY sized to the pane and begins streaming its
// output into the emulator. Runs in a bubbletea command goroutine.
func (term *Terminal) spawn() tea.Msg {
	command := exec.Command(term.argv[0], term.argv[1:]...) // #nosec G204 — argv is built from fixed `ai` subcommands, not user input
	command.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(command)
	if err != nil {
		return terminalExitMsg{err: err}
	}
	term.mu.Lock()
	term.cmd = command
	term.ptmx = ptmx
	term.mu.Unlock()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(term.rows()), Cols: uint16(term.cols())})

	go term.readLoop(ptmx)
	return terminalStartedMsg{}
}

// readLoop pumps PTY output into the emulator (which locks internally) and signals
// the UI to re-render; on read error (EOF when the program exits) it reports done.
func (term *Terminal) readLoop(ptmx *os.File) {
	buffer := make([]byte, 4096)
	for {
		read, err := ptmx.Read(buffer)
		if read > 0 {
			_, _ = term.emulator.Write(buffer[:read])
			select {
			case term.dirty <- struct{}{}:
			default: // a redraw is already pending — coalesce
			}
		}
		if err != nil {
			term.done <- err
			return
		}
	}
}

// waitCmd blocks until there is new output to render or the program exits.
func (term *Terminal) waitCmd() tea.Cmd {
	dirty, done := term.dirty, term.done
	return func() tea.Msg {
		select {
		case <-dirty:
			return terminalDirtyMsg{}
		case err := <-done:
			return terminalExitMsg{err: err}
		}
	}
}

// Update advances the terminal: dirty re-arms the wait loop (the re-render is
// implicit), exit records completion, and a key is encoded and written to the PTY.
func (term *Terminal) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case terminalStartedMsg:
		return term.waitCmd()
	case terminalDirtyMsg:
		return term.waitCmd()
	case terminalExitMsg:
		term.exited = true
		term.exitErr = message.err
		return nil
	case tea.KeyMsg:
		if term.exited {
			return nil
		}
		data := encodeKey(message)
		if len(data) == 0 {
			return nil
		}
		term.mu.Lock()
		ptmx := term.ptmx
		term.mu.Unlock()
		if ptmx != nil {
			_, _ = ptmx.Write(data)
		}
		return nil
	}
	return nil
}

// SetSize resizes both the emulator and the PTY so the program reflows to the pane.
func (term *Terminal) SetSize(width, height int) {
	term.width, term.height = width, height
	if term.emulator != nil {
		term.emulator.Resize(term.cols(), term.rows())
	}
	term.mu.Lock()
	ptmx := term.ptmx
	term.mu.Unlock()
	if ptmx != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(term.rows()), Cols: uint16(term.cols())})
	}
}

// View renders the program's current screen; once it exits a one-line footer hints
// that any key closes the pane.
func (term *Terminal) View() string {
	if term.emulator == nil {
		if term.exitErr != nil {
			return ui.Failure.Render(ui.IconFail + " " + term.exitErr.Error())
		}
		return ui.Muted.Render("starting…")
	}
	screen := term.render()
	if term.exited {
		return screen + "\n" + ui.Muted.Render(term.exitNote())
	}
	return screen
}

// render walks the emulator's cell grid and produces a styled string, coalescing
// runs of same-styled cells per line so the output stays compact (one styled
// segment per colour run, not per character).
func (term *Terminal) render() string {
	term.emulator.Lock()
	defer term.emulator.Unlock()
	cols, rows := term.emulator.Size()

	var screen strings.Builder
	for row := 0; row < rows; row++ {
		var line strings.Builder
		var run strings.Builder
		var runStyle lipgloss.Style
		var runFG, runBG vt10x.Color
		var runMode int16
		hasRun := false
		flush := func() {
			if hasRun {
				line.WriteString(runStyle.Render(run.String()))
				run.Reset()
				hasRun = false
			}
		}
		for col := 0; col < cols; col++ {
			glyph := term.emulator.Cell(col, row)
			if !hasRun || glyph.FG != runFG || glyph.BG != runBG || glyph.Mode != runMode {
				flush()
				runFG, runBG, runMode = glyph.FG, glyph.BG, glyph.Mode
				runStyle = glyphStyle(glyph)
				hasRun = true
			}
			char := glyph.Char
			if char == 0 {
				char = ' '
			}
			run.WriteRune(char)
		}
		flush()
		screen.WriteString(strings.TrimRight(line.String(), " "))
		if row < rows-1 {
			screen.WriteByte('\n')
		}
	}
	return screen.String()
}

// glyphStyle maps a glyph's colours + attributes to a lipgloss style (256-colour
// palette; default colours stay unstyled; honours reverse / bold / underline).
func glyphStyle(glyph vt10x.Glyph) lipgloss.Style {
	style := lipgloss.NewStyle()
	foreground, background := glyph.FG, glyph.BG
	if glyph.Mode&attrReverse != 0 {
		foreground, background = background, foreground
	}
	if color, ok := paletteColor(foreground); ok {
		style = style.Foreground(color)
	}
	if color, ok := paletteColor(background); ok {
		style = style.Background(color)
	}
	if glyph.Mode&attrBold != 0 {
		style = style.Bold(true)
	}
	if glyph.Mode&attrUnderline != 0 {
		style = style.Underline(true)
	}
	return style
}

// paletteColor converts a vt10x palette index (0–255) to a lipgloss colour; a
// "default" sentinel (≥ 1<<24) returns ok=false so the cell keeps the terminal
// default.
func paletteColor(color vt10x.Color) (lipgloss.Color, bool) {
	if color >= defaultColorFloor {
		return "", false
	}
	return lipgloss.Color(strconv.Itoa(int(color))), true
}

// exitNote summarizes how the program ended.
func (term *Terminal) exitNote() string {
	if term.exitErr != nil {
		return "process exited (" + term.exitErr.Error() + ") — press any key to close"
	}
	return "process finished — press any key to close"
}

// Close kills the program (if still running) and releases the PTY, stopping the
// read goroutine. Idempotent.
func (term *Terminal) Close() {
	term.mu.Lock()
	cmd, ptmx := term.cmd, term.ptmx
	term.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if ptmx != nil {
		_ = ptmx.Close()
	}
}

// encodeKey converts a bubbletea key event into the bytes a terminal sends to a
// child program, so keystrokes typed in the pane reach the PTY. It covers printable
// runes, the common control keys, the arrow/navigation keys, and ctrl+<letter>.
func encodeKey(key tea.KeyMsg) []byte {
	switch key.Type {
	case tea.KeyRunes:
		return []byte(string(key.Runes))
	case tea.KeySpace:
		return []byte{' '}
	case tea.KeyEnter:
		return []byte{'\r'}
	case tea.KeyTab:
		return []byte{'\t'}
	case tea.KeyEsc:
		return []byte{0x1b}
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	}
	// ctrl+<letter> → the control byte 0x01..0x1a (ctrl+a..ctrl+z).
	if name := key.String(); strings.HasPrefix(name, "ctrl+") && len(name) == len("ctrl+")+1 {
		if letter := name[len(name)-1]; letter >= 'a' && letter <= 'z' {
			return []byte{letter - 'a' + 1}
		}
	}
	return nil
}
