package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// The create wizard's step widgets — all built on the SAME primitives as the other
// views (bubbles/textinput and the reusable listWindow), so they look and feel like the
// rest of the TUI. A single-select and multi-select list back the option steps; a
// two-level model picker (mirroring the Local Models pane) backs the Graphify step.

// textStep is a single labelled text field (name, cpus, memory, ports, idle timeout),
// with an optional validator run on advance.
type textStep struct {
	input    textinput.Model
	desc     string
	validate func(string) error
	err      error
}

func newTextStep(label, desc, placeholder, initial string, validate func(string) error) *textStep {
	input := textinput.New()
	input.Prompt = label + ": "
	input.Placeholder = placeholder
	input.SetValue(initial)
	input.CursorEnd()
	input.Focus()
	return &textStep{input: input, desc: desc, validate: validate}
}

func (step *textStep) SetSize(width, _ int) {
	if width > len(step.input.Prompt)+8 {
		step.input.Width = width - len(step.input.Prompt) - 4
	}
}

func (step *textStep) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	step.input, cmd = step.input.Update(msg)
	step.err = nil
	return cmd
}

// Validate runs the validator against the trimmed value, recording any error for the
// view. A nil validator always passes.
func (step *textStep) Validate() error {
	if step.validate == nil {
		return nil
	}
	step.err = step.validate(step.Value())
	return step.err
}

func (step *textStep) Value() string { return strings.TrimSpace(step.input.Value()) }

func (step *textStep) View() string {
	var body strings.Builder
	if step.desc != "" {
		body.WriteString(ui.Muted.Render(step.desc) + "\n\n")
	}
	body.WriteString(step.input.View())
	if step.err != nil {
		body.WriteString("\n\n" + ui.Failure.Render(ui.IconFail+" "+step.err.Error()))
	}
	return body.String()
}

// selectList is a single-select list over string options, rendered with the reusable
// listWindow so it matches the Local/Cloud Models highlight.
type selectList struct {
	options []string
	desc    string
	window  listWindow
	pending int // desired cursor until SetSize builds the window
	width   int
	height  int
}

func newSelectList(desc string, options []string, initial string) *selectList {
	list := &selectList{desc: desc, options: options}
	for index, option := range options {
		if option == initial {
			list.pending = index
		}
	}
	return list
}

// SetOptions replaces the options (e.g. the default-agent list depends on the chosen
// agents) and reseats the cursor, keeping the current selection when still present.
func (list *selectList) SetOptions(options []string) {
	current := list.Value()
	list.options = options
	list.pending = 0
	for index, option := range options {
		if option == current {
			list.pending = index
		}
	}
	list.rebuild()
}

func (list *selectList) SetSize(width, height int) {
	list.width, list.height = width, height
	list.rebuild()
}

func (list *selectList) rebuild() {
	lines := make([]ListLine, len(list.options))
	for index, option := range list.options {
		lines[index] = ListLine{Text: padToWidth("  "+option, list.width), Selectable: true}
	}
	list.window.SetContent(lines, list.width, list.listHeight())
	if list.pending > 0 {
		list.window.SetCursor(list.pending)
		list.pending = 0
	}
}

// listHeight reserves the description line(s) above the list.
func (list *selectList) listHeight() int {
	height := list.height - 2 // description + blank
	if height < 1 {
		height = 1
	}
	return height
}

func (list *selectList) Move(step int) { list.window.Move(step) }

func (list *selectList) Value() string {
	cursor := list.window.Cursor()
	if cursor < 0 || cursor >= len(list.options) {
		return ""
	}
	return list.options[cursor]
}

func (list *selectList) View() string {
	body := ui.Muted.Render(list.desc) + "\n\n"
	return body + list.window.View(selectedStyle())
}

// multiSelectList is a space-toggle checkbox list over string options.
type multiSelectList struct {
	options []string
	desc    string
	checked map[string]bool
	window  listWindow
	width   int
	height  int
}

func newMultiSelectList(desc string, options, initial []string) *multiSelectList {
	checked := make(map[string]bool, len(initial))
	for _, value := range initial {
		checked[value] = true
	}
	return &multiSelectList{desc: desc, options: options, checked: checked}
}

func (list *multiSelectList) SetSize(width, height int) {
	list.width, list.height = width, height
	list.rebuild()
}

func (list *multiSelectList) rebuild() {
	cursor := list.window.Cursor()
	lines := make([]ListLine, len(list.options))
	for index, option := range list.options {
		box := "[ ] "
		if list.checked[option] {
			box = "[x] "
		}
		lines[index] = ListLine{Text: padToWidth("  "+box+option, list.width), Selectable: true}
	}
	height := list.height - 2
	if height < 1 {
		height = 1
	}
	list.window.SetContent(lines, list.width, height)
	list.window.SetCursor(cursor)
}

func (list *multiSelectList) Move(step int) { list.window.Move(step) }

// Toggle flips the checkbox on the cursor's option.
func (list *multiSelectList) Toggle() {
	cursor := list.window.Cursor()
	if cursor < 0 || cursor >= len(list.options) {
		return
	}
	option := list.options[cursor]
	list.checked[option] = !list.checked[option]
	list.rebuild()
}

// Values are the checked options, in option order.
func (list *multiSelectList) Values() []string {
	values := make([]string, 0, len(list.options))
	for _, option := range list.options {
		if list.checked[option] {
			values = append(values, option)
		}
	}
	return values
}

func (list *multiSelectList) View() string {
	body := ui.Muted.Render(list.desc) + "\n\n"
	return body + list.window.View(selectedStyle())
}

// modelPicker is the Graphify-model step: a single-level picker of curated,
// vLLM-servable Hugging Face repos — a leading "(none)" followed by each curated repo
// id. Selecting a repo (or "(none)") yields the value; vLLM is the sole local runtime,
// so there are no ollama-style tags to drill into.
type modelPicker struct {
	models *selectList // "(none)" + curated repo ids
	value  string      // final repo id or "" for none
	done   bool        // a selection was confirmed
	width  int
	height int
}

const modelPickerNone = "(none)"

func newModelPicker(curated []hf.CuratedModel, initial string) *modelPicker {
	repos := make([]string, 0, len(curated)+1)
	repos = append(repos, modelPickerNone)
	for _, model := range curated {
		repos = append(repos, model.Repo)
	}
	initialRepo := initial
	if initialRepo == "" {
		initialRepo = modelPickerNone
	}
	return &modelPicker{
		models: newSelectList("Graphify model (vLLM; a curated Hugging Face repo, routed through the gateway, pulled if absent). enter selects — (none) for no model.", repos, initialRepo),
	}
}

func (picker *modelPicker) SetSize(width, height int) {
	picker.width, picker.height = width, height
	picker.models.SetSize(width, height)
}

// Update handles the picker's own keys and reports whether the step is DONE (a repo was
// chosen) so the wizard can advance.
func (picker *modelPicker) Update(msg tea.Msg) (done bool) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false
	}
	switch key.String() {
	case "up", "k":
		picker.models.Move(-1)
	case "down", "j":
		picker.models.Move(1)
	case "enter", "tab":
		repo := picker.models.Value()
		if repo == modelPickerNone || repo == "" {
			picker.value = ""
		} else {
			picker.value = repo
		}
		picker.done = true
		return true
	}
	return false
}

// Value is the chosen repo id or "" for none. Valid once Update returned done.
func (picker *modelPicker) Value() string { return picker.value }

func (picker *modelPicker) View() string {
	return picker.models.View()
}
