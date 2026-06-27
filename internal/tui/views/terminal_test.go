package views

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// runCmd executes a command and feeds its message into the terminal, returning the
// next command. It flattens tea.Batch results (e.g. Init batches the PTY spawn with
// the spinner's first tick) the way the bubbletea runtime would, draining the
// spinner-tick branch so the loop converges on the spawn/wait branch. A spinner tick
// re-issues itself only while running, so we stop pumping ticks once the process has
// exited to avoid an infinite loop.
func runCmd(term *Terminal, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	switch message := cmd().(type) {
	case tea.BatchMsg:
		var next tea.Cmd
		for _, batched := range message {
			produced := runCmd(term, batched)
			if next == nil && produced != nil {
				next = produced // keep the FIRST follow-up (the spawn's wait loop, not the spinner tick)
			}
		}
		return next
	default:
		return term.Update(message)
	}
}

// pump drives the terminal's async loop to completion: each returned command is run
// (it blocks for output or exit) and its message fed back, until the process exits
// (Update returns a nil command on the exit message).
func pump(test *testing.T, term *Terminal, cmd tea.Cmd) {
	test.Helper()
	for iterations := 0; cmd != nil && iterations < 1000; iterations++ {
		if term.Exited() {
			return
		}
		cmd = runCmd(term, cmd)
	}
}

// TestTerminalRunsCommandAndRenders: the terminal starts a real command on a PTY,
// streams its output through the emulator, and renders it — verifying the
// PTY+emulator+render path end to end (the msb side is hardware-verified).
func TestTerminalRunsCommandAndRenders(test *testing.T) {
	term := NewTerminal("print", []string{"sh", "-c", "printf HELLO; exit 0"}, true)
	term.SetSize(40, 10)

	cmd := term.Init()
	if cmd == nil {
		test.Fatal("Init must start the PTY and return a wait command")
	}
	pump(test, term, cmd)

	if !term.Exited() {
		test.Fatal("the terminal should report the process exited")
	}
	if !strings.Contains(term.View(), "HELLO") {
		test.Errorf("rendered screen should contain the program output, got:\n%s", term.View())
	}
}

// TestTerminalForwardsKeystrokes: keys typed in the pane reach the program through
// the PTY — the program reads a line of stdin and echoes it back.
func TestTerminalForwardsKeystrokes(test *testing.T) {
	term := NewTerminal("read", []string{"sh", "-c", "read line; printf 'GOT:%s' \"$line\""}, true)
	term.SetSize(40, 10)

	cmd := term.Init()
	if cmd == nil {
		test.Fatal("Init must return the spawn command")
	}
	// Run the spawn command (it starts the PTY) before typing, so the keystrokes
	// reach a live PTY rather than a nil one. runCmd flattens Init's batch (spawn +
	// spinner tick), running the spawn so the PTY is live.
	cmd = runCmd(term, cmd)
	term.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	term.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pump(test, term, cmd)

	if !strings.Contains(term.View(), "GOT:abc") {
		test.Errorf("program should have received the typed input, got:\n%s", term.View())
	}
}

// TestTerminalShowsSpinnerWhileRunningBlank: before the child produces any output
// the pane shows the animated progress header (no black pane), and the spinner's
// tick command is scheduled (Init batches it) so the glyph actually animates. Once
// the process exits, the spinner stops re-ticking.
func TestTerminalShowsSpinnerWhileRunningBlank(test *testing.T) {
	term := NewTerminal("models pull gemma4", []string{"sh", "-c", "sleep 0.3; printf DONE"}, false)
	term.SetSize(40, 10)

	cmd := term.Init()
	if cmd == nil {
		test.Fatal("Init must return commands (spawn + spinner tick)")
	}
	// Drain Init's batch (spawn + spinner tick) WITHOUT pumping to completion, so we
	// observe the running-but-blank state.
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		test.Fatalf("Init should batch the spawn with the spinner tick, got %T", cmd())
	}
	var tickCmd, waitCmd tea.Cmd
	for _, batched := range batch {
		switch message := batched().(type) {
		case spinner.TickMsg:
			tickCmd = term.Update(message) // advancing the spinner returns the next tick
		default:
			waitCmd = term.Update(message) // the spawn (terminalStartedMsg) → the output/exit wait loop
		}
	}
	if term.Exited() {
		test.Fatal("process should still be running (it sleeps before printing)")
	}
	// While running with a blank screen, the view shows the progress header, not a
	// black/empty pane.
	view := term.View()
	if !strings.Contains(view, "running `ai models pull gemma4`") {
		test.Errorf("running view should show the spinner progress header, got:\n%q", view)
	}
	// The spinner tick is scheduled so the glyph keeps animating.
	if tickCmd == nil {
		test.Error("the spinner tick command should be scheduled while running")
	}
	if _, ok := tickCmd().(spinner.TickMsg); !ok {
		test.Error("the scheduled command should be a spinner tick")
	}

	// Drive the wait loop to completion; the process finishes and the spinner stops.
	pump(test, term, waitCmd)
	if !term.Exited() {
		test.Fatal("process should have exited after pumping the wait loop")
	}
	// A late spinner tick after exit must NOT re-schedule another tick.
	if stopped := term.Update(spinner.TickMsg{}); stopped != nil {
		test.Error("the spinner must stop re-ticking after the process exits")
	}
}

func TestEncodeKey(test *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
		want []byte
	}{
		{"runes", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ab")}, []byte("ab")},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, []byte{'\r'}},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, []byte{'\t'}},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}, []byte{0x1b}},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, []byte{0x7f}},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, []byte("\x1b[A")},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}, []byte{0x03}},
		{"ctrl+d", tea.KeyMsg{Type: tea.KeyCtrlD}, []byte{0x04}},
	}
	for _, testCase := range cases {
		if got := encodeKey(testCase.key); string(got) != string(testCase.want) {
			test.Errorf("%s: encodeKey = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestTerminalScrollbackCapturesLines verifies the pane captures the child's output
// into a plain-text scrollback (escape sequences stripped, \r overwrite handled) and
// that scroll mode renders that history.
func TestTerminalScrollbackCapturesLines(test *testing.T) {
	term := NewTerminal("log", []string{"true"}, false)
	// Feed bytes directly (no PTY needed) to exercise the accumulator: an ANSI colour
	// sequence (stripped), three lines, and a \r progress redraw whose final text wins.
	term.appendScrollback([]byte("\x1b[31mred\x1b[0m line1\nline2\nprog 1%\rprog 100%\n"))
	if len(term.scrollback) != 3 {
		test.Fatalf("expected 3 captured lines, got %d: %q", len(term.scrollback), term.scrollback)
	}
	if term.scrollback[0] != "red line1" {
		test.Errorf("escape sequences not stripped: %q", term.scrollback[0])
	}
	if term.scrollback[2] != "prog 100%" {
		test.Errorf("carriage-return overwrite not handled: %q", term.scrollback[2])
	}
	// With more captured lines than the visible body there IS something to scroll, so
	// entering scroll mode + scrolling up stays in scroll mode and shows the indicator.
	for index := 0; index < 10; index++ {
		term.appendScrollback([]byte("extra\n"))
	}
	term.SetSize(40, 4) // body = rows-1 = 3
	term.enterScroll()
	term.scrollBy(2)
	if !term.InScrollMode() {
		test.Fatal("should be in scroll mode after scrollBy with more lines than the body")
	}
	out := term.scrollView()
	if !strings.Contains(out, "scrollback") {
		test.Errorf("scrollView missing indicator:\n%s", out)
	}
}

// TestTerminalNonInteractiveArrowEntersScrollback verifies that in a non-interactive
// (streaming) pane the up arrow enters scrollback rather than being forwarded (the
// ^[[A echo bug), while an interactive pane keeps the arrow for the child.
func TestTerminalNonInteractiveArrowEntersScrollback(test *testing.T) {
	streaming := NewTerminal("restart", []string{"true"}, false)
	streaming.SetSize(40, 6)
	streaming.appendScrollback([]byte("a\nb\nc\nd\ne\nf\ng\nh\n"))
	if _, ok := streaming.liveScrollEntry("up"); !ok {
		test.Error("non-interactive pane should enter scrollback on up arrow")
	}
	interactive := NewTerminal("shell", []string{"true"}, true)
	if _, ok := interactive.liveScrollEntry("up"); ok {
		test.Error("interactive pane must forward the up arrow to the child, not scroll")
	}
	// PgUp scrolls in any pane.
	if _, ok := interactive.liveScrollEntry("pgup"); !ok {
		test.Error("pgup should enter scrollback even in an interactive pane")
	}
}
