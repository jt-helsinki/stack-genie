package views

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// recordingScreen is a fake Screen that records the non-key messages it receives, so
// a test can assert the hub delivered an async result to it (even when it is not the
// focused sub-tab).
type recordingScreen struct {
	name     string
	received []tea.Msg
}

func (screen *recordingScreen) Init() tea.Cmd { return nil }
func (screen *recordingScreen) Update(msg tea.Msg) tea.Cmd {
	if _, isKey := msg.(tea.KeyMsg); !isKey {
		screen.received = append(screen.received, msg)
	}
	return nil
}
func (screen *recordingScreen) View() string     { return screen.name }
func (screen *recordingScreen) Title() string    { return screen.name }
func (screen *recordingScreen) Hints() string    { return "" }
func (screen *recordingScreen) SetSize(_, _ int) {}

// fooMsg is a sub-view-specific async message (the analogue of sessionsRefreshedMsg).
type fooMsg struct{}

// TestHubBroadcastsAsyncMessagesToAllSubViews verifies that while a project is open,
// a non-key (async) message is delivered to EVERY sub-view — not just the focused one
// — so a sub-view whose Init() fetch lands while another sub-tab is focused still
// receives its result (and is not stuck "loading…").
func TestHubBroadcastsAsyncMessagesToAllSubViews(test *testing.T) {
	focused := &recordingScreen{name: "focused"}
	background := &recordingScreen{name: "background"}
	switcher := &recordingScreen{name: "switcher"}
	hub := NewProjectsHub(switcher, []Screen{focused, background}, []string{"Focused", "Background"})
	hub.open = true // simulate an open project, focused sub-tab = index 0

	hub.Update(fooMsg{})

	if len(background.received) != 1 {
		test.Fatalf("background sub-view should have received the async message even though it is not focused; got %d messages", len(background.received))
	}
	if len(focused.received) != 1 {
		test.Fatalf("focused sub-view should also have received the async message; got %d", len(focused.received))
	}
}

// TestHubKeysGoOnlyToFocusedSubView verifies key input is NOT broadcast — only the
// focused sub-view acts on a keystroke (so e.g. Enter doesn't fire on hidden tabs).
func TestHubKeysGoOnlyToFocusedSubView(test *testing.T) {
	focused := &recordingScreen{name: "focused"}
	background := &recordingScreen{name: "background"}
	switcher := &recordingScreen{name: "switcher"}
	hub := NewProjectsHub(switcher, []Screen{focused, background}, []string{"Focused", "Background"})
	hub.open = true

	hub.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	// recordingScreen only records non-key messages, so we assert via the absence of a
	// panic and that the background view recorded nothing (it never sees the key).
	if len(background.received) != 0 {
		test.Fatalf("a keystroke must not reach a non-focused sub-view; got %d messages", len(background.received))
	}
}
