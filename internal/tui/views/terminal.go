package views

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"github.com/jt-helsinki/stack-genie/internal/ui"
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

	// spinner animates a "running …" header while the child has been spawned but has
	// not yet produced any visible output (the vt10x screen is still blank/black), so
	// the user sees progress instead of an empty pane. It keeps ticking until the
	// process exits; once the program streams output, render() shows the live screen
	// and the spinner header is dropped.
	spinner spinner.Model

	// mu guards ptmx/cmd AND the scrollback buffer, which the read goroutine appends
	// to while the main loop reads it for rendering.
	mu   sync.Mutex
	ptmx *os.File
	cmd  *exec.Cmd

	// interactive marks a child that needs the arrow keys (a shell / attached
	// session); for it arrows are forwarded to the PTY. When false (a command run
	// whose output streams — lifecycle/apps/models/keys) the arrow/page keys instead
	// scroll the pane's scrollback, so they don't echo as ^[[A.
	interactive bool

	// scrollback holds the plain-text lines the child has emitted (escape sequences
	// stripped) so the pane can be scrolled back — vt10x keeps no history. partial is
	// the current not-yet-newline-terminated line being assembled.
	scrollback []string
	partial    string
	// scrollMode pauses the live view and shows a window into scrollback; scrollOffset
	// is how many lines up from the bottom the window's bottom edge sits (0 = tail =
	// live).
	scrollMode   bool
	scrollOffset int

	width, height int
	exited        bool
	exitErr       error
}

// maxScrollback bounds the captured history (lines) so a long-running session does
// not grow memory without limit.
const maxScrollback = 5000

// scrollStep / the page size are how far the wheel and the page keys move.
const scrollStep = 3

// ansiPattern matches the escape sequences stripped from captured text before it is
// stored in the scrollback (CSI, OSC, and the common two-byte escapes), so the
// scrollback reads as plain lines.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]" +
	"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" +
	"|\x1b[@-Z\\\\-_]")

// terminalStartedMsg signals the PTY spawn succeeded; the parent then begins the
// output wait loop.
type terminalStartedMsg struct{}

// terminalDirtyMsg signals new program output is available to render; the parent
// re-issues the wait command so the loop continues until the process exits.
type terminalDirtyMsg struct{}

// terminalExitMsg signals the program has exited (cleanly or with err).
type terminalExitMsg struct{ err error }

// NewTerminal builds a terminal that will run argv (argv[0] is the program). label
// is a short description for the header. interactive forwards the arrow keys to the
// child (a shell / attached session); when false the arrow/page keys scroll the
// pane's captured scrollback instead.
func NewTerminal(label string, argv []string, interactive bool) *Terminal {
	return &Terminal{label: label, argv: argv, interactive: interactive}
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
	term.spinner = ui.NewSpinner()
	// Batch the PTY spawn with the spinner's first tick so the progress header
	// animates from the moment the overlay opens — before any child output arrives.
	return tea.Batch(func() tea.Msg { return term.spawn() }, term.spinner.Tick)
}

// spawn starts the command on a PTY sized to the pane and begins streaming its
// output into the emulator. Runs in a bubbletea command goroutine.
func (term *Terminal) spawn() tea.Msg {
	command := exec.Command(term.argv[0], term.argv[1:]...) // #nosec G204 — argv is built from fixed `ai` subcommands, not user input
	// Mark the child as embedded so nested `ai` commands skip their own animated
	// bubbletea spinner (which would fight the child's streaming progress for cursor
	// control of this PTY — flicker/garbled output); their progress streams cleanly
	// into this pane's emulator instead. See ui.EmbeddedTerminalEnv.
	command.Env = append(os.Environ(), "TERM=xterm-256color", ui.EmbeddedTerminalEnv+"=1")
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
			term.appendScrollback(buffer[:read])
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
		return nil // the process is done — stop re-ticking the spinner
	case spinner.TickMsg:
		// Keep the progress spinner animating until the process exits; re-issue its
		// tick so the header glyph advances even before the child produces output.
		if term.exited {
			return nil
		}
		var cmd tea.Cmd
		term.spinner, cmd = term.spinner.Update(message)
		return cmd
	case tea.MouseMsg:
		// The wheel scrolls the captured scrollback (entering scroll mode on the way
		// up); this works during the run AND after the child has exited.
		switch message.Button {
		case tea.MouseButtonWheelUp:
			term.enterScroll()
			term.scrollBy(scrollStep)
		case tea.MouseButtonWheelDown:
			if term.scrollMode {
				term.scrollBy(-scrollStep)
			}
		}
		return nil
	case tea.KeyMsg:
		// In scroll mode every key drives the scrollback (nav scrolls; esc/q resume
		// live); the child sees nothing.
		if term.scrollMode {
			term.handleScrollKey(message)
			return nil
		}
		// In live mode an upward key enters the scrollback instead of being forwarded
		// (so it does not echo as ^[[A): page-up in any pane, and the up arrow in a
		// non-interactive streaming pane (lifecycle/apps/models/keys).
		if delta, ok := term.liveScrollEntry(message.String()); ok {
			term.enterScroll()
			term.scrollBy(delta)
			return nil
		}
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

// InScrollMode reports whether the pane is currently paused in scrollback (so the
// app routes keys to the pane rather than closing it).
func (term *Terminal) InScrollMode() bool { return term.scrollMode }

// liveScrollEntry maps a key pressed in LIVE mode to a scrollback entry: page-up
// (and shift+up) scroll a page in any pane; the up arrow scrolls one line in a
// non-interactive pane (an interactive shell keeps the arrow for itself). ok=false
// means the key is not a scroll-entry key and should be forwarded to the child.
func (term *Terminal) liveScrollEntry(key string) (int, bool) {
	switch key {
	case "pgup", "shift+up":
		return term.pageStep(), true
	case "up":
		if !term.interactive {
			return 1, true
		}
	}
	return 0, false
}

// handleScrollKey advances the scrollback window while in scroll mode.
func (term *Terminal) handleScrollKey(key tea.KeyMsg) {
	switch key.String() {
	case "esc", "q", "i":
		term.exitScroll()
	case "up", "k":
		term.scrollBy(1)
	case "down", "j":
		term.scrollBy(-1)
	case "pgup", "shift+up", "b":
		term.scrollBy(term.pageStep())
	case "pgdown", "pgdn", "shift+down", " ", "f":
		term.scrollBy(-term.pageStep())
	case "home", "g":
		term.scrollToTop()
	case "end", "G":
		term.exitScroll()
	}
}

// enterScroll switches the pane into scrollback mode at the tail (offset 0).
func (term *Terminal) enterScroll() {
	if !term.scrollMode {
		term.scrollMode = true
		term.scrollOffset = 0
	}
}

// exitScroll resumes the live view at the tail.
func (term *Terminal) exitScroll() {
	term.scrollMode = false
	term.scrollOffset = 0
}

// scrollBy moves the scrollback window by delta lines (positive = older). Reaching
// the bottom (offset 0) resumes live; the top is clamped to the captured history.
func (term *Terminal) scrollBy(delta int) {
	maxOffset := term.maxScrollOffset()
	term.scrollOffset += delta
	if term.scrollOffset > maxOffset {
		term.scrollOffset = maxOffset
	}
	if term.scrollOffset <= 0 {
		term.exitScroll()
	}
}

// scrollToTop jumps to the oldest captured line.
func (term *Terminal) scrollToTop() {
	term.scrollMode = true
	term.scrollOffset = term.maxScrollOffset()
}

// maxScrollOffset is how far up the window can go: total captured lines minus the
// visible body height.
func (term *Terminal) maxScrollOffset() int {
	term.mu.Lock()
	total := len(term.scrollback)
	if term.partial != "" {
		total++
	}
	term.mu.Unlock()
	maxOffset := total - term.scrollBodyRows()
	if maxOffset < 0 {
		return 0
	}
	return maxOffset
}

// scrollBodyRows is the number of content rows in scroll mode (the pane height minus
// the one-line scrollback indicator).
func (term *Terminal) scrollBodyRows() int {
	body := term.rows() - 1
	if body < 1 {
		return 1
	}
	return body
}

// pageStep is how far PgUp/PgDn move (nearly a full page).
func (term *Terminal) pageStep() int {
	step := term.rows() - 2
	if step < 1 {
		return 1
	}
	return step
}

// appendScrollback captures a chunk of the child's output into the plain-text
// scrollback: escape sequences are stripped, \n finalises a line, \r overwrites the
// current line (so progress-bar redraws keep their final text), and the buffer is
// capped at maxScrollback lines.
func (term *Terminal) appendScrollback(chunk []byte) {
	text := ansiPattern.ReplaceAllString(string(chunk), "")
	term.mu.Lock()
	defer term.mu.Unlock()
	for _, char := range text {
		switch char {
		case '\n':
			term.scrollback = append(term.scrollback, term.partial)
			term.partial = ""
		case '\r':
			term.partial = ""
		case '\t':
			term.partial += "    "
		default:
			if char >= 0x20 {
				term.partial += string(char)
			}
		}
	}
	if len(term.scrollback) > maxScrollback {
		term.scrollback = term.scrollback[len(term.scrollback)-maxScrollback:]
	}
}

// scrollView renders the scrollback window (the current offset) plus a one-line
// indicator, padded to the pane height so the bottom stays put.
func (term *Terminal) scrollView() string {
	term.mu.Lock()
	lines := make([]string, len(term.scrollback), len(term.scrollback)+1)
	copy(lines, term.scrollback)
	if term.partial != "" {
		lines = append(lines, term.partial)
	}
	offset := term.scrollOffset
	term.mu.Unlock()

	body := term.scrollBodyRows()
	end := len(lines) - offset
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}
	start := end - body
	if start < 0 {
		start = 0
	}
	var screen strings.Builder
	for row := 0; row < body; row++ {
		index := start + row
		if index >= 0 && index < end {
			screen.WriteString(lines[index])
		}
		screen.WriteByte('\n')
	}
	screen.WriteString(ui.Muted.Render("— scrollback · ↑/↓ scroll · PgUp/PgDn page · esc/q resume live —"))
	return screen.String()
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
		return term.spinnerHeader()
	}
	// Scrollback mode shows a window into the captured history (works during the run
	// and after exit) instead of the live screen.
	if term.scrollMode {
		return term.scrollView()
	}
	screen := term.render()
	if term.exited {
		return screen + "\n" + ui.Muted.Render(term.exitNote())
	}
	// Before the child produces any visible output the vt10x screen is blank (a
	// black pane). Show the animated spinner header so the user sees that work is
	// underway; once output streams in, render the live screen.
	if isBlank(screen) {
		return term.spinnerHeader()
	}
	return screen
}

// spinnerHeader is the animated "running `ai <label>` ⣾" progress line shown while
// the child has been spawned but has produced no visible output yet.
func (term *Terminal) spinnerHeader() string {
	return ui.Muted.Render("running `ai "+term.label+"` ") + term.spinner.View()
}

// isBlank reports whether the rendered screen has no visible content (only spaces
// and newlines) — i.e. the child has not painted anything yet.
func isBlank(screen string) bool {
	return strings.TrimSpace(screen) == ""
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
