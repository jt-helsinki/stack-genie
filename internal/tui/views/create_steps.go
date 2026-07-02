package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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

// modelPicker is the Graphify-model step: a two-level picker mirroring the Local Models
// pane — a model list (a leading "(none)" + the cached Ollama library names), and, on
// enter into a model, a tag list. Selecting a tag (or "(none)") yields the ref.
type modelPicker struct {
	library []ollama.LibraryModel
	models  *selectList // "(none)" + library names
	tags    *selectList // the drilled model's tags (nil until drilled)
	drill   string      // the model name being drilled (empty = model list)
	value   string      // final ref (name:tag) or "" for none
	done    bool        // a selection was confirmed
	width   int
	height  int
}

const modelPickerNone = "(none)"

func newModelPicker(library []ollama.LibraryModel, initial string) *modelPicker {
	names := make([]string, 0, len(library)+1)
	names = append(names, modelPickerNone)
	initialName := ""
	if index := strings.LastIndex(initial, ":"); index >= 0 {
		initialName = initial[:index]
	} else {
		initialName = initial
	}
	for _, model := range library {
		names = append(names, model.Name)
	}
	picker := &modelPicker{
		library: library,
		models:  newSelectList("Graphify model (Ollama; routed through the gateway, pulled if absent). enter selects — space/none for no model.", names, initialName),
	}
	return picker
}

func (picker *modelPicker) SetSize(width, height int) {
	picker.width, picker.height = width, height
	picker.models.SetSize(width, height)
	if picker.tags != nil {
		picker.tags.SetSize(width, height)
	}
}

// Update handles the picker's own keys and reports whether the step is DONE (a ref was
// chosen) so the wizard can advance. esc inside the tag list backs out to the model
// list (it does NOT cancel the wizard).
func (picker *modelPicker) Update(msg tea.Msg) (done bool) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false
	}
	if picker.drill != "" { // tag list
		switch key.String() {
		case "up", "k":
			picker.tags.Move(-1)
		case "down", "j":
			picker.tags.Move(1)
		case "esc":
			picker.drill = ""
			picker.tags = nil
		case "enter", "tab":
			picker.value = picker.drill + ":" + picker.tags.Value()
			picker.done = true
			return true
		}
		return false
	}
	switch key.String() { // model list
	case "up", "k":
		picker.models.Move(-1)
	case "down", "j":
		picker.models.Move(1)
	case "enter", "tab":
		name := picker.models.Value()
		if name == modelPickerNone || name == "" {
			picker.value = ""
			picker.done = true
			return true
		}
		picker.enterModel(name)
	}
	return false
}

// enterModel drills into a model's tags. A model with no scraped tags resolves to
// "<name>:latest" immediately (no tag list to show).
func (picker *modelPicker) enterModel(name string) {
	var tags []string
	for _, model := range picker.library {
		if model.Name == name {
			tags = model.TagNames()
			break
		}
	}
	if len(tags) == 0 {
		picker.value = name + ":latest"
		picker.done = true
		return
	}
	picker.drill = name
	picker.tags = newSelectList("Tag for "+name+" — enter selects · esc back", tags, tags[0])
	picker.tags.SetSize(picker.width, picker.height)
}

// Value is the chosen ref ("name:tag") or "" for none. Valid once Update returned done.
func (picker *modelPicker) Value() string { return picker.value }

func (picker *modelPicker) View() string {
	if picker.drill != "" && picker.tags != nil {
		return picker.tags.View()
	}
	return picker.models.View()
}
