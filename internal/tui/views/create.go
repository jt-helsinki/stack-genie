package views

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/create"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/tui/scope"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// CreateConfirmedMsg is emitted when the user completes the create wizard. Spec is the
// fully-assembled workspace spec (with an absolute, already-created Root); the parent
// runs create.Execute(Spec) IN-PROCESS and refreshes the Workspaces hub.
type CreateConfirmedMsg struct {
	Spec project.Spec
}

// CreateCancelledMsg is emitted when the user cancels the create flow (esc from the
// first step); the parent returns to the Projects hub.
type CreateCancelledMsg struct{}

// Wizard step order.
const (
	stepLocation = iota
	stepName
	stepOS
	stepAgents
	stepDefault
	stepStacks
	stepApps
	stepCPUs
	stepMemory
	stepPorts
	stepIdle
	stepModel
	stepCount
)

// stepTitles labels each step for the header.
var stepTitles = map[int]string{
	stepLocation: "Location",
	stepName:     "Name",
	stepOS:       "Operating system",
	stepAgents:   "Agent CLIs",
	stepDefault:  "Default agent",
	stepStacks:   "Software stacks",
	stepApps:     "AI apps",
	stepCPUs:     "vCPUs",
	stepMemory:   "Memory",
	stepPorts:    "Ports",
	stepIdle:     "Idle timeout",
	stepModel:    "Graphify model",
}

// Create is the in-TUI new-workspace wizard: a multi-step form built from the same
// bubbles widgets as the other views (textinput, the reusable listWindow), replacing
// the old shell-out to `ai create`. It walks the steps below, assembles a project.Spec,
// and emits CreateConfirmedMsg — the parent executes it in-process (no subprocess).
type Create struct {
	step int

	location    *locationStep
	name        *textStep
	osList      *selectList
	agents      *multiSelectList
	defaultTool *selectList
	stacks      *multiSelectList
	apps        *multiSelectList
	cpus        *textStep
	memory      *textStep
	ports       *textStep
	idle        *textStep
	model       *modelPicker // nil when no Ollama library is cached (step skipped)

	width  int
	height int
}

// NewCreate builds the wizard. startDir seeds the location field; library is the cached
// Ollama library for the Graphify-model step (empty → that step is skipped); hostGB /
// usableGB annotate the memory hint.
func NewCreate(startDir string, library []ollama.LibraryModel, hostGB, usableGB int) *Create {
	defaultCPUs := config.Default().Workspace.CPULimit
	cpuHint := fmt.Sprintf("Blank uses the default (%d); host has %d logical CPUs", defaultCPUs, sysinfoCPUs())
	memHint := fmt.Sprintf("A plain number in GB; blank uses the default (%s); usable max %d GB (host %d GB)",
		config.Default().Workspace.MemoryLimit, usableGB, hostGB)

	wizard := &Create{
		location:    newLocationStep(startDir),
		name:        newTextStep("name", "The workspace name (lowercase letters, digits, hyphens).", "my-workspace", "", validateNameField),
		osList:      newSelectList("Base operating system.", create.SupportedOSes(), "debian-trixie"),
		agents:      newMultiSelectList("Agent CLIs to install (space to toggle; opencode + pi are the defaults).", create.SupportedAgentCLIs(), []string{"opencode", "pi"}),
		defaultTool: newSelectList("The agent CLI launched by default.", []string{"opencode"}, "opencode"),
		stacks:      newMultiSelectList("Extra software stacks (Python, Node, uv + Graphify are installed by default).", create.SupportedStacks(), nil),
		apps:        newMultiSelectList("In-VM AI apps to install (opt-in; default none).", create.SupportedApps(), nil),
		cpus:        newTextStep("vCPUs", cpuHint, "", "", validateCPUsField),
		memory:      newTextStep("memory (GB)", memHint, "", "", validateMemoryField),
		ports:       newTextStep("ports", "Ports to open: PORT or HOST:GUEST, comma-separated (e.g. 8080,9000:3000).", "", "", validatePortsField),
		idle:        newTextStep("idle timeout", "How long msb may leave the workspace idle before stopping it (e.g. 30m, 24h).", "", "", validateIdleField),
	}
	if len(library) > 0 {
		wizard.model = newModelPicker(library, "")
	}
	return wizard
}

func (view *Create) Title() string { return "New Workspace" }

// Hints are step-aware, mirroring the widget in play.
func (view *Create) Hints() string {
	nav := " · shift+tab back · esc cancel"
	switch view.step {
	case stepLocation:
		return "type to filter · ↓/↑ pick folder · tab open folder · enter next" + nav
	case stepAgents, stepStacks, stepApps:
		return "↑/↓ move · space toggle · enter next" + nav
	case stepOS, stepDefault, stepModel:
		return "↑/↓ move · enter select/next" + nav
	default:
		return "type · enter next" + nav
	}
}

// SetSize records the pane size and propagates it to every step widget.
func (view *Create) SetSize(width, height int) {
	view.width, view.height = width, height
	// Reserve the header (title + step counter) so the step body fits the pane.
	stepWidth, stepHeight := width, height-2
	if stepHeight < 3 {
		stepHeight = 3
	}
	view.location.SetSize(stepWidth, stepHeight)
	view.name.SetSize(stepWidth, stepHeight)
	view.osList.SetSize(stepWidth, stepHeight)
	view.agents.SetSize(stepWidth, stepHeight)
	view.defaultTool.SetSize(stepWidth, stepHeight)
	view.stacks.SetSize(stepWidth, stepHeight)
	view.apps.SetSize(stepWidth, stepHeight)
	view.cpus.SetSize(stepWidth, stepHeight)
	view.memory.SetSize(stepWidth, stepHeight)
	view.ports.SetSize(stepWidth, stepHeight)
	view.idle.SetSize(stepWidth, stepHeight)
	if view.model != nil {
		view.model.SetSize(stepWidth, stepHeight)
	}
}

func (view *Create) Init() tea.Cmd { return textinput.Blink }

// Update advances the wizard. esc cancels (from any step); shift+tab goes to the
// previous step; enter/tab validates + advances (or finishes on the last step). Each
// step delegates its own keys to its widget.
func (view *Create) Update(msg tea.Msg) tea.Cmd {
	key, isKey := msg.(tea.KeyMsg)
	if isKey {
		switch key.String() {
		case "esc":
			return func() tea.Msg { return CreateCancelledMsg{} }
		case "shift+tab":
			view.prev()
			return nil
		}
	}
	switch view.step {
	case stepLocation:
		advance, cmd := view.location.Update(msg)
		if advance {
			return view.next()
		}
		return cmd
	case stepName, stepCPUs, stepMemory, stepPorts, stepIdle:
		return view.updateTextStep(view.textStepFor(view.step), msg)
	case stepOS, stepDefault:
		return view.updateSelectStep(view.selectStepFor(view.step), msg)
	case stepAgents, stepStacks, stepApps:
		return view.updateMultiStep(view.multiStepFor(view.step), msg)
	case stepModel:
		if view.model == nil {
			return view.next()
		}
		if done := view.model.Update(msg); done {
			return view.next()
		}
		return nil
	}
	return nil
}

func (view *Create) updateTextStep(step *textStep, msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter", "tab":
			if err := step.Validate(); err != nil {
				return nil
			}
			return view.next()
		}
	}
	return step.Update(msg)
}

func (view *Create) updateSelectStep(step *selectList, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "up", "k":
		step.Move(-1)
	case "down", "j":
		step.Move(1)
	case "enter", "tab":
		return view.next()
	}
	return nil
}

func (view *Create) updateMultiStep(step *multiSelectList, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "up", "k":
		step.Move(-1)
	case "down", "j":
		step.Move(1)
	case " ":
		step.Toggle()
	case "enter", "tab":
		// The agents step requires at least one selection.
		if view.step == stepAgents && len(step.Values()) == 0 {
			return nil
		}
		return view.next()
	}
	return nil
}

func (view *Create) textStepFor(step int) *textStep {
	switch step {
	case stepName:
		return view.name
	case stepCPUs:
		return view.cpus
	case stepMemory:
		return view.memory
	case stepPorts:
		return view.ports
	default:
		return view.idle
	}
}

func (view *Create) selectStepFor(step int) *selectList {
	if step == stepOS {
		return view.osList
	}
	return view.defaultTool
}

func (view *Create) multiStepFor(step int) *multiSelectList {
	switch step {
	case stepAgents:
		return view.agents
	case stepStacks:
		return view.stacks
	default:
		return view.apps
	}
}

// next advances to the following step, applying any cross-step wiring; finishing after
// the last step emits the assembled spec.
func (view *Create) next() tea.Cmd {
	if view.step >= stepModel {
		return view.finish()
	}
	view.step++
	// Entering the default-agent step: its options are exactly the chosen agents.
	if view.step == stepDefault {
		agents := view.agents.Values()
		view.defaultTool.SetOptions(agents)
	}
	return nil
}

// prev goes back a step (no-op at the first step).
func (view *Create) prev() {
	if view.step > stepLocation {
		view.step--
	}
}

// finish assembles the project.Spec from the collected fields and emits it.
func (view *Create) finish() tea.Cmd {
	cpus := 0
	if raw := view.cpus.Value(); raw != "" {
		cpus, _ = strconv.Atoi(raw)
	}
	ports, _ := parsePortsForSpec(view.ports.Value())
	graphifyModel := ""
	if view.model != nil {
		graphifyModel = view.model.Value()
	}
	spec := project.Spec{
		Name:          view.name.Value(),
		OS:            view.osList.Value(),
		AgentCLIs:     view.agents.Values(),
		DefaultTool:   view.defaultTool.Value(),
		Stacks:        view.stacks.Values(),
		Apps:          view.apps.Values(),
		CPUs:          cpus,
		Memory:        view.memory.Value(),
		PublishPorts:  ports,
		IdleTimeout:   view.idle.Value(),
		GraphifyModel: graphifyModel,
		Root:          view.location.dir,
	}
	return func() tea.Msg { return CreateConfirmedMsg{Spec: spec} }
}

// View renders the step header (title + N/M) and the current step's body.
func (view *Create) View() string {
	total := stepCount
	header := ui.Heading.Render(stepTitles[view.step]) +
		ui.Muted.Render(fmt.Sprintf("   (step %d of %d)", view.step+1, total)) + "\n\n"
	return header + view.stepBody()
}

func (view *Create) stepBody() string {
	switch view.step {
	case stepLocation:
		return view.location.View()
	case stepName:
		return view.name.View()
	case stepOS:
		return view.osList.View()
	case stepAgents:
		return view.agents.View()
	case stepDefault:
		return view.defaultTool.View()
	case stepStacks:
		return view.stacks.View()
	case stepApps:
		return view.apps.View()
	case stepCPUs:
		return view.cpus.View()
	case stepMemory:
		return view.memory.View()
	case stepPorts:
		return view.ports.View()
	case stepIdle:
		return view.idle.View()
	case stepModel:
		if view.model == nil {
			return ui.Muted.Render("No Ollama library cached — the Graphify model is left unset (run `ai models` to populate it).")
		}
		return view.model.View()
	}
	return ""
}

// --- field validators (immediate per-step feedback; create.Execute re-validates) ---

func validateNameField(value string) error {
	if value == "" {
		return fmt.Errorf("a workspace name is required")
	}
	return project.ValidateName(value)
}

func validateCPUsField(value string) error {
	if value == "" {
		return nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return fmt.Errorf("vCPUs must be a positive whole number")
	}
	return create.ValidateResourcesWithinHost(number, "")
}

func validateMemoryField(value string) error {
	if value == "" {
		return nil
	}
	return create.ValidateResourcesWithinHost(1, value)
}

func validatePortsField(value string) error {
	_, err := parsePortsForSpec(value)
	return err
}

func validateIdleField(value string) error {
	if value == "" {
		return nil
	}
	return config.ValidateIdleTimeout(value)
}

// parsePortsForSpec parses a comma-separated ports field into host↔guest mappings. Each
// entry is "PORT" (host == guest) or "HOST:GUEST" (Docker-style host-first). Blank
// yields no mappings.
func parsePortsForSpec(value string) ([]config.PortMapping, error) {
	var ports []config.PortMapping
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		hostText, guestText, hasGuest := strings.Cut(raw, ":")
		hostPort, err := strconv.Atoi(strings.TrimSpace(hostText))
		if err != nil || hostPort < 1 || hostPort > 65535 {
			return nil, fmt.Errorf("invalid port %q (expected PORT or HOST:GUEST, 1-65535)", raw)
		}
		guestPort := hostPort
		if hasGuest {
			guestPort, err = strconv.Atoi(strings.TrimSpace(guestText))
			if err != nil || guestPort < 1 || guestPort > 65535 {
				return nil, fmt.Errorf("invalid port %q (expected PORT or HOST:GUEST, 1-65535)", raw)
			}
		}
		ports = append(ports, config.PortMapping{Host: hostPort, Guest: guestPort})
	}
	return ports, nil
}

// sysinfoCPUs is a tiny indirection so the memory/cpu hints don't force a sysinfo
// import at the top when unavailable; it returns the host logical CPU count.
func sysinfoCPUs() int { return create.HostCPUs() }

// ============================ location step ============================
//
// The location step is the original folder-picker: a text input plus a live dropdown
// of the folders under the entered path, with ~ expansion, tab-to-descend, and rejection
// of a path that is (or is nested in) an existing workspace. On confirm it creates the
// directory (and intermediate folders) and hands the absolute path to the wizard.

const dropdownMaxVisible = 10

type folderRow struct {
	path     string
	name     string
	disabled bool
}

type locationStep struct {
	input         textinput.Model
	suggestions   []folderRow
	selected      int
	scroll        int
	validationErr error
	dir           string // set on a successful confirm
	// typed is the last value the USER typed, captured so moving the selection back
	// above the list restores it (moving DOWN autocompletes the highlighted folder into
	// the input; moving back to the top restores what was typed).
	typed  string
	width  int
	height int
}

func newLocationStep(startDir string) *locationStep {
	input := textinput.New()
	input.Prompt = "location: "
	input.Placeholder = "~/path/to/new-workspace"
	input.ShowSuggestions = false
	input.SetValue(ensureTrailingSep(createStartDir(startDir)))
	input.CursorEnd()
	input.Focus()
	step := &locationStep{input: input, selected: -1, typed: input.Value()}
	step.refreshSuggestions()
	return step
}

func createStartDir(startDir string) string {
	if strings.TrimSpace(startDir) != "" {
		return startDir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}

func (step *locationStep) SetSize(width, height int) {
	step.width = width
	step.height = height
	if width > len(step.input.Prompt)+8 {
		step.input.Width = width - len(step.input.Prompt) - 4
	}
}

// Update handles the location field's keys. It returns advance=true when the user
// confirmed a valid location (the wizard then reads step.dir); other keys edit the
// input / move the dropdown.
func (step *locationStep) Update(msg tea.Msg) (advance bool, cmd tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter":
			return step.confirm(), nil
		case "down":
			step.moveSelection(1)
			return false, nil
		case "up":
			step.moveSelection(-1)
			return false, nil
		case "tab":
			step.complete()
			return false, nil
		}
	}
	before := step.input.Value()
	step.input, cmd = step.input.Update(msg)
	if step.input.Value() != before {
		step.typed = step.input.Value()
		step.refreshSuggestions()
		step.selected = -1
		step.scroll = 0
		step.validationErr = nil
	}
	return false, cmd
}

// previewSelection autocompletes the highlighted folder into the input as the selection
// moves — WITHOUT regenerating the suggestion list, so the user keeps navigating the same
// siblings (enter confirms the previewed path; tab drills into it). Moving back above the
// list (selected < 0) restores what the user had typed.
func (step *locationStep) previewSelection() {
	if step.selected >= 0 && step.selected < len(step.suggestions) {
		step.input.SetValue(step.suggestions[step.selected].path)
	} else {
		step.input.SetValue(step.typed)
	}
	step.input.CursorEnd()
}

func (step *locationStep) moveSelection(delta int) {
	defer step.previewSelection() // autocomplete the highlighted folder into the input
	if len(step.suggestions) == 0 {
		step.selected = -1
		step.scroll = 0
		return
	}
	index := step.selected
	for {
		index += delta
		if index < 0 {
			step.selected = -1
			step.scroll = 0
			return
		}
		if index >= len(step.suggestions) {
			return
		}
		if !step.suggestions[index].disabled {
			step.selected = index
			step.adjustScroll()
			return
		}
	}
}

func (step *locationStep) firstSelectable() int {
	for index := range step.suggestions {
		if !step.suggestions[index].disabled {
			return index
		}
	}
	return -1
}

func (step *locationStep) complete() {
	index := step.selected
	if index < 0 {
		index = step.firstSelectable()
	}
	if index < 0 || index >= len(step.suggestions) || step.suggestions[index].disabled {
		return
	}
	step.input.SetValue(step.suggestions[index].path)
	step.input.CursorEnd()
	step.typed = step.input.Value()
	step.refreshSuggestions()
	step.selected = -1
	step.scroll = 0
	step.validationErr = nil
}

// confirm resolves + validates the location and creates it. Returns true on success
// (step.dir set); false leaves the user on the field with validationErr recorded.
func (step *locationStep) confirm() bool {
	target, err := expandTilde(strings.TrimSpace(step.input.Value()))
	if err != nil {
		step.validationErr = err
		return false
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		step.validationErr = fmt.Errorf("resolve %q: %w", target, err)
		return false
	}
	if err := scope.ValidateCreateTarget(absolute); err != nil {
		step.validationErr = err
		return false
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		step.validationErr = fmt.Errorf("create %q: %w", absolute, err)
		return false
	}
	step.validationErr = nil
	step.dir = absolute
	return true
}

func (step *locationStep) refreshSuggestions() {
	step.suggestions = dirSuggestions(step.input.Value())
}

func (step *locationStep) maxVisible() int {
	visible := dropdownMaxVisible
	if step.height > 0 {
		visible = step.height - 6
	}
	if visible < 3 {
		visible = 3
	}
	if visible > dropdownMaxVisible {
		visible = dropdownMaxVisible
	}
	return visible
}

func (step *locationStep) adjustScroll() {
	if step.selected < 0 {
		step.scroll = 0
		return
	}
	maxVisible := step.maxVisible()
	if step.selected < step.scroll {
		step.scroll = step.selected
	}
	if step.selected >= step.scroll+maxVisible {
		step.scroll = step.selected - maxVisible + 1
	}
}

func (step *locationStep) View() string {
	body := ui.Muted.Render("Enter a location for the new workspace. A path that doesn't exist is created (with intermediate folders).") + "\n\n"
	body += step.input.View() + "\n"
	body += step.dropdownView()
	if step.validationErr != nil {
		body += "\n\n" + ui.Failure.Render(ui.IconFail+" "+step.validationErr.Error())
	}
	return body
}

func (step *locationStep) dropdownView() string {
	if len(step.suggestions) == 0 {
		return ui.Muted.Render("  (no matching folders — enter creates the path as typed)")
	}
	maxVisible := step.maxVisible()
	end := step.scroll + maxVisible
	if end > len(step.suggestions) {
		end = len(step.suggestions)
	}
	var builder strings.Builder
	if step.scroll > 0 {
		builder.WriteString(ui.Muted.Render("  ↑ more") + "\n")
	}
	for index := step.scroll; index < end; index++ {
		row := step.suggestions[index]
		name := row.name + string(os.PathSeparator)
		switch {
		case row.disabled:
			builder.WriteString(ui.Muted.Render("  "+name+"  — workspace already exists") + "\n")
		case index == step.selected:
			// Match every other list in the app: a full-width highlighted row
			// (selectedStyle) rather than a bespoke chevron marker.
			builder.WriteString(selectedStyle().Render(padToWidth("  "+name, step.width)) + "\n")
		default:
			builder.WriteString("  " + ui.Value.Render(name) + "\n")
		}
	}
	if end < len(step.suggestions) {
		builder.WriteString(ui.Muted.Render("  ↓ more"))
	}
	return strings.TrimRight(builder.String(), "\n")
}

func dirSuggestions(path string) []folderRow {
	expanded, err := expandTilde(path)
	if err != nil {
		return nil
	}
	dir, partial := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	rows := make([]folderRow, 0, 64)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(entry.Name(), partial) {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		rows = append(rows, folderRow{
			path:     ensureTrailingSep(full),
			name:     entry.Name(),
			disabled: scope.IsProjectRoot(full),
		})
		if len(rows) >= 64 {
			break
		}
	}
	return rows
}

func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	expanded := filepath.Join(home, path[2:])
	if strings.HasSuffix(path, "/") && !strings.HasSuffix(expanded, string(os.PathSeparator)) {
		expanded += string(os.PathSeparator)
	}
	return expanded, nil
}

func ensureTrailingSep(path string) string {
	if path == "" || strings.HasSuffix(path, string(os.PathSeparator)) {
		return path
	}
	return path + string(os.PathSeparator)
}
