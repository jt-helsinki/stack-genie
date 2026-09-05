package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// RunSteps runs a multi-step operation while rendering a live checklist on out
// (the emitter's stderr): the running step shows a spinner, completed steps show
// ✓. work runs in a goroutine and reports each step via the emit callback (the
// same shape as the platform's existing Progress func(string) callbacks). Call
// this ONLY when Enabled is true. Returns work's error.
//
// Goroutine→program communication uses channels read from a tea.Cmd (not
// Program.Send), so there is no start-up race and no data race on the result.
func RunSteps(out io.Writer, title string, work func(emit func(step string)) error) error {
	stepCh := make(chan string, 64)
	resultCh := make(chan error, 1)
	go func() {
		emit := func(step string) { stepCh <- step }
		resultCh <- work(emit)
		close(stepCh)
	}()
	model := stepsModel{spinner: newSpinner(), title: title, stepCh: stepCh, resultCh: resultCh}
	finished, runErr := tea.NewProgram(model, tea.WithOutput(out)).Run()
	if runErr != nil {
		return runErr
	}
	return finished.(stepsModel).err
}

type stepMsg string
type stepsDoneMsg struct{ err error }

type stepsModel struct {
	spinner  spinner.Model
	title    string
	steps    []string
	stepCh   chan string
	resultCh chan error
	done     bool
	err      error
}

// waitStep blocks on the next step (or the closed channel → done) off the tea
// runtime, turning channel activity into messages.
func (model stepsModel) waitStep() tea.Cmd {
	return func() tea.Msg {
		step, ok := <-model.stepCh
		if !ok {
			return stepsDoneMsg{err: <-model.resultCh}
		}
		return stepMsg(step)
	}
}

func (model stepsModel) Init() tea.Cmd {
	return tea.Batch(model.spinner.Tick, model.waitStep())
}

func (model stepsModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := message.(type) {
	case stepMsg:
		model.steps = append(model.steps, string(typed))
		return model, model.waitStep()
	case stepsDoneMsg:
		model.done = true
		model.err = typed.err
		return model, tea.Quit
	case tea.KeyMsg:
		// See spinnerModel.Update: raw mode disables ctrl+c's normal SIGINT delivery,
		// so it must be handled here or the keystroke does nothing at all.
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

func (model stepsModel) View() string {
	var builder strings.Builder
	if model.title != "" {
		builder.WriteString(Heading.Render(model.title) + "\n")
	}
	for index, step := range model.steps {
		running := index == len(model.steps)-1 && !model.done
		if running {
			_, _ = fmt.Fprintf(&builder, "  %s %s\n", model.spinner.View(), step)
		} else {
			_, _ = fmt.Fprintf(&builder, "  %s %s\n", Success.Render(IconOK), step)
		}
	}
	return builder.String()
}
