package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pump drives the terminal's async loop to completion: each returned command is run
// (it blocks for output or exit) and its message fed back, until the process exits
// (Update returns a nil command on the exit message).
func pump(test *testing.T, term *Terminal, cmd tea.Cmd) {
	test.Helper()
	for iterations := 0; cmd != nil && iterations < 1000; iterations++ {
		cmd = term.Update(cmd())
	}
}

// TestTerminalRunsCommandAndRenders: the terminal starts a real command on a PTY,
// streams its output through the emulator, and renders it — verifying the
// PTY+emulator+render path end to end (the msb side is hardware-verified).
func TestTerminalRunsCommandAndRenders(test *testing.T) {
	term := NewTerminal("print", []string{"sh", "-c", "printf HELLO; exit 0"})
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
	term := NewTerminal("read", []string{"sh", "-c", "read line; printf 'GOT:%s' \"$line\""})
	term.SetSize(40, 10)

	cmd := term.Init()
	if cmd == nil {
		test.Fatal("Init must return the spawn command")
	}
	// Run the spawn command (it starts the PTY) before typing, so the keystrokes
	// reach a live PTY rather than a nil one.
	cmd = term.Update(cmd())
	term.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	term.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pump(test, term, cmd)

	if !strings.Contains(term.View(), "GOT:abc") {
		test.Errorf("program should have received the typed input, got:\n%s", term.View())
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
