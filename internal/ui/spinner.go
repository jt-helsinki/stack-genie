package ui

import (
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// ErrInterrupted is returned by RunWithSpinner/RunSteps when the user cancels
// with ctrl+c. The underlying work may still be running in the background when
// this is returned (there is no cancellation plumbing into it), but the CLI
// process exits right after the caller reports this error, which kills it.
var ErrInterrupted = output.Errorf(output.ExitRuntimeFailure, "interrupted (ctrl+c)")

// RunWithSpinner runs work while showing an animated spinner with title on out
// (callers pass the emitter's stderr), then prints a final ✓/✗ line and returns
// work's error. Call this ONLY when Enabled is true; otherwise run work directly.
// The spinner animates on its own ticks while work runs in the tea runtime, so
// the user never sees a frozen screen.
func RunWithSpinner(out io.Writer, title string, work func() error) error {
	if EmbeddedTerminal() {
		// Inside the `ai ui` terminal pane a nested bubbletea spinner would fight the
		// child's own streaming progress (msb's layered pull, model/docker downloads)
		// for cursor control of the shared PTY — the flicker/garbled/hung output. Run
		// the work directly so its progress streams cleanly into the pane; print the
		// title up front (labelling the streaming output) and a final ✓/✗ line.
		_, _ = fmt.Fprintf(out, "%s\n", title)
		err := work()
		if err != nil {
			_, _ = fmt.Fprintf(out, "%s %s\n", Failure.Render(IconFail), title)
		} else {
			_, _ = fmt.Fprintf(out, "%s %s\n", Success.Render(IconOK), title)
		}
		return err
	}
	model := spinnerModel{spinner: newSpinner(), title: title, work: work}
	finished, runErr := tea.NewProgram(model, tea.WithOutput(out)).Run()
	if runErr != nil {
		return runErr
	}
	result := finished.(spinnerModel)
	if result.err != nil {
		_, _ = fmt.Fprintf(out, "%s %s\n", Failure.Render(IconFail), title)
	} else {
		_, _ = fmt.Fprintf(out, "%s %s\n", Success.Render(IconOK), title)
	}
	return result.err
}

// newSpinner returns the platform's standard spinner, styled with the accent.
func newSpinner() spinner.Model {
	model := spinner.New(spinner.WithSpinner(spinner.Dot))
	model.Style = Success
	return model
}

// NewSpinner returns the platform's standard themed spinner for embedding in
// another bubbletea model (e.g. the `ai ui` terminal overlay's progress header
// while a child process is starting/running with no output yet). Callers drive it
// the usual way: return model.Tick from Init, feed spinner.TickMsg to Update, and
// re-issue the returned tick command so it keeps animating.
func NewSpinner() spinner.Model { return newSpinner() }

type spinnerModel struct {
	spinner spinner.Model
	title   string
	work    func() error
	done    bool
	err     error
}

type spinnerDoneMsg struct{ err error }

func (model spinnerModel) Init() tea.Cmd {
	return tea.Batch(model.spinner.Tick, func() tea.Msg {
		return spinnerDoneMsg{err: model.work()}
	})
}

func (model spinnerModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := message.(type) {
	case spinnerDoneMsg:
		model.done = true
		model.err = typed.err
		return model, tea.Quit
	case tea.KeyMsg:
		// The raw-mode terminal bubbletea owns while this runs disables the kernel's
		// normal ctrl+c → SIGINT delivery, so without this the keystroke is just
		// silently swallowed as an ordinary (meaningless) key event and the program
		// never quits. work keeps running in the background (no cancellation
		// plumbing into it), but the CLI process exits right after RunWithSpinner's
		// caller reports ErrInterrupted, which kills it.
		if typed.Type == tea.KeyCtrlC {
			model.done = true
			model.err = ErrInterrupted
			return model, tea.Quit
		}
		return model, nil
	default:
		var cmd tea.Cmd
		model.spinner, cmd = model.spinner.Update(message)
		return model, cmd
	}
}

func (model spinnerModel) View() string {
	if model.done {
		return "" // the final ✓/✗ line is printed by RunWithSpinner after quit
	}
	return fmt.Sprintf("  %s %s\n", model.spinner.View(), model.title)
}
