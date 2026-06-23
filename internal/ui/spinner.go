package ui

import (
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// RunWithSpinner runs work while showing an animated spinner with title on out
// (callers pass the emitter's stderr), then prints a final ✓/✗ line and returns
// work's error. Call this ONLY when Enabled is true; otherwise run work directly.
// The spinner animates on its own ticks while work runs in the tea runtime, so
// the user never sees a frozen screen.
func RunWithSpinner(out io.Writer, title string, work func() error) error {
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
