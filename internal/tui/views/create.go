package views

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/tui/scope"
	"github.com/jt-helsinki/stack-genie/internal/ui"
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
	stepShell
	stepAgents // the agent-CLI multi-select
	stepDefault
	stepTools // unified AI-tools multi-select (caveman/graphify/code-review-graph/codebase-memory/…)
	stepModel // dynamic: shown only when graphify is among the selected tools
	stepAuth  // dynamic: one auth-mode select per OAuth-capable selected agent
	stepCount
)

// graphifyModelNone is the Graphify-model picker's "leave Graphify unconfigured" option
// (mirrors the CLI wizard's sentinel — internal/cli can't be imported here since cli
// imports this package).
const graphifyModelNone = "(none — leave Graphify unconfigured)"

// authModeDesc explains the per-agent auth-mode choice (mirrors the CLI wizard note).
const authModeDesc = "How this agent authenticates. api-key routes through the gateway (keeps the tool firewall + secret masking); oauth uses the agent's own subscription login DIRECTLY, bypassing the gateway (guardrails do NOT apply)."

// stepTitles labels each step for the header.
var stepTitles = map[int]string{
	stepLocation: "Location",
	stepName:     "Name",
	stepShell:    "Shell",
	stepAgents:   "Agent CLIs",
	stepDefault:  "Default agent",
	stepTools:    "AI tools",
	stepModel:    "Graphify model",
}

// Create is the in-TUI new-workspace wizard: a multi-step form built from the same
// bubbles widgets as the other views (textinput, the reusable listWindow), replacing
// the old shell-out to `ai create`. It walks the steps below, assembles a
// project.Spec, and emits CreateConfirmedMsg — the parent executes it in-process (no
// subprocess, no image build — there is nothing to build in the nono model).
type Create struct {
	step     int
	location *locationStep
	name     *textStep
	// lastAutoName is the name value the wizard last auto-filled from the location
	// step (see followLocationDefaultName) — distinguishes "still following the
	// location default" from "the user typed their own name", so a later location
	// change doesn't clobber a deliberate edit.
	lastAutoName string
	shell        *selectList
	// agents is the agent-CLI multi-select.
	agents *multiSelectList
	// tools is the unified AI-tools multi-select (caveman/graphify/code-review-graph/
	// codebase-memory-mcp/…), mirroring the agent-CLI list instead of a screen each.
	tools       *multiSelectList
	defaultTool *selectList
	model       *selectList // Graphify model picker, populated from omlx's live model list

	// Per-agent auth-mode phase (stepAuth): one selectList per OAuth-capable selected
	// agent, built when leaving the model step. authIndex walks them one at a time.
	authAgents []string
	authLists  []*selectList
	authIndex  int

	stepW, stepH int // per-step size, re-applied when a dynamic (auth-mode) step is built mid-wizard

	width, height int
}

// NewCreate builds the wizard seeded at startDir, with omlxModels as the injected
// live lister feeding the Graphify-model picker.
func NewCreate(startDir string, omlxModels func() []string) *Create {
	// The name defaults to the workspace's own (starting) directory — the same
	// default the user sees pre-filled in the location field just above it.
	defaultName := create.DefaultDirName(createStartDir(startDir))
	wizard := &Create{
		location: newLocationStep(startDir),
		name: newTextStep("name", "The workspace name (lowercase letters, digits, hyphens).",
			"my-workspace", defaultName, validateNameField),
		lastAutoName: defaultName,
		shell:        newSelectList("Default interactive shell for workspace sessions.", create.SupportedShells(), "bash"),
		agents: newMultiSelectList("Agent CLIs installed on the host, wired to the gateway — space to toggle. Pick at least one.",
			create.SupportedAgentCLIs(), []string{"opencode"}),
		tools: newMultiSelectListWithLabels("AI tools installed on the host — space to toggle. None selected by default.",
			create.SupportedAITools(), aiToolLabels(), create.DefaultAITools()),
		defaultTool: newSelectList("The agent CLI launched by default.", []string{"opencode"}, "opencode"),
		model: newSelectList("A model omlx is ALREADY serving (manage the list itself from its own admin "+
			"panel — `ai services console omlx`); routed through the gateway as omlx/<name>. No download happens here.",
			graphifyModelOptions(omlxModels), graphifyModelNone),
	}
	return wizard
}

// followLocationDefaultName re-derives the auto-filled name from a just-confirmed
// location change, UNLESS the user deliberately typed their own name.
func (view *Create) followLocationDefaultName() {
	if view.name.Value() != view.lastAutoName {
		return // the user edited it — leave their choice alone
	}
	next := create.DefaultDirName(view.location.dir)
	view.name.SetValue(next)
	view.lastAutoName = next
}

func (view *Create) Title() string { return "New Workspace" }

func (view *Create) Init() tea.Cmd { return textinput.Blink }

// Hints are step-aware, mirroring the widget in play.
func (view *Create) Hints() string {
	nav := " · shift+tab back · esc cancel"
	switch view.step {
	case stepLocation:
		return "type to filter · ↓/↑ pick a folder · tab open folder · enter next" + nav
	case stepAgents, stepTools:
		return "↑/↓ move · space toggle · enter next" + nav
	case stepShell, stepDefault, stepAuth, stepModel:
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
	view.stepW, view.stepH = stepWidth, stepHeight
	for _, list := range view.authLists {
		list.SetSize(stepWidth, stepHeight)
	}
	view.location.SetSize(stepWidth, stepHeight)
	view.name.SetSize(stepWidth, stepHeight)
	view.shell.SetSize(stepWidth, stepHeight)
	view.agents.SetSize(stepWidth, stepHeight)
	view.tools.SetSize(stepWidth, stepHeight)
	view.defaultTool.SetSize(stepWidth, stepHeight)
	view.model.SetSize(stepWidth, stepHeight)
}

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
		view.followLocationDefaultName()
		if advance {
			return view.next()
		}
		return cmd
	case stepName:
		return view.updateTextStep(view.name, msg)
	case stepShell, stepDefault:
		return view.updateSelectStep(view.selectStepFor(view.step), msg)
	case stepAgents, stepTools:
		return view.updateMultiStep(view.multiStepFor(view.step), msg)
	case stepModel:
		// The Graphify model step is relevant only when graphify is selected; skip
		// straight past it otherwise.
		if !view.graphifySelected() {
			return view.next()
		}
		return view.updateSelectStep(view.model, msg)
	case stepAuth:
		if len(view.authLists) == 0 {
			return view.next()
		}
		return view.updateSelectStep(view.authLists[view.authIndex], msg)
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

func (view *Create) selectStepFor(step int) *selectList {
	switch step {
	case stepShell:
		return view.shell
	case stepDefault:
		return view.defaultTool
	}
	return nil
}

func (view *Create) multiStepFor(step int) *multiSelectList {
	switch step {
	case stepAgents:
		return view.agents
	case stepTools:
		return view.tools
	}
	return nil
}

func (view *Create) graphifySelected() bool {
	return slices.Contains(view.tools.Values(), create.AIToolGraphify)
}

// aiToolLabels renders each AI tool as "<tool> — <description>" (create.AIToolDescription)
// so the picker doubles as documentation — the same convention the CLI wizard and
// `ai tools list` use.
func aiToolLabels() []string {
	keys := create.SupportedAITools()
	labels := make([]string, len(keys))
	for index, key := range keys {
		labels[index] = key + " — " + create.AIToolDescription(key)
	}
	return labels
}

func graphifyModelOptions(omlxModels func() []string) []string {
	var models []string
	if omlxModels != nil {
		models = omlxModels()
	}
	return append([]string{graphifyModelNone}, models...)
}

// next advances to the next step (or dynamic phase), skipping steps that don't apply
// to the current selection.
func (view *Create) next() tea.Cmd {
	switch view.step {
	case stepModel:
		return view.leaveModelStep()
	case stepAuth:
		if view.authIndex+1 < len(view.authAgents) {
			view.authIndex++
			return nil
		}
		return view.finish()
	}
	view.step++
	// Entering the default-agent step: its options are exactly the chosen agent CLIs.
	if view.step == stepDefault {
		view.defaultTool.SetOptions(view.agents.Values())
	}
	// The Graphify-model step is shown only when graphify is selected; otherwise skip
	// straight into the auth phase.
	if view.step == stepModel && !view.graphifySelected() {
		return view.leaveModelStep()
	}
	return nil
}

// leaveModelStep is the transition out of the (possibly-skipped) Graphify-model step:
// it builds the per-agent auth phase and enters it, or finishes when no OAuth-capable
// agent is selected.
func (view *Create) leaveModelStep() tea.Cmd {
	view.buildAuthSteps()
	if len(view.authAgents) == 0 {
		return view.finish()
	}
	view.step = stepAuth
	view.authIndex = 0
	return nil
}

// prev goes back a step (no-op at the first step). Within the auth phase it walks
// back through the per-agent auth selects, then to the model/tools step.
func (view *Create) prev() {
	switch {
	case view.step == stepAuth:
		if view.authIndex > 0 {
			view.authIndex--
			return
		}
		if view.graphifySelected() {
			view.step = stepModel
			return
		}
		view.step = stepTools
	case view.step > stepLocation:
		view.step--
	}
}

// buildAuthSteps computes the auth-mode phase from the current agent selection: one
// api-key/oauth select per OAuth-capable selected agent, seeded to api-key. Rebuilt
// each time the model step is left, so toggling agents earlier is reflected.
func (view *Create) buildAuthSteps() {
	view.authAgents = nil
	view.authLists = nil
	selected := view.agents.Values()
	for _, cli := range create.OAuthCapableCLIs() {
		if !slices.Contains(selected, cli) {
			continue
		}
		list := newSelectList(cli+" — "+authModeDesc, create.SupportedAuthModes(), "api-key")
		list.SetSize(view.stepW, view.stepH)
		view.authAgents = append(view.authAgents, cli)
		view.authLists = append(view.authLists, list)
	}
}

// finish assembles the project.Spec from the collected fields and emits it.
func (view *Create) finish() tea.Cmd {
	caveman, graphify, codeReviewGraph, codebaseMemory, openViking, tokenSave, agentMemory, ponytail, agentSkills :=
		create.SplitAITools(view.tools.Values())
	graphifyModel := ""
	if graphify {
		if value := view.model.Value(); value != graphifyModelNone {
			graphifyModel = value
		}
	}
	var authModes map[string]string
	if len(view.authAgents) > 0 {
		authModes = make(map[string]string, len(view.authAgents))
		for index, cli := range view.authAgents {
			authModes[cli] = view.authLists[index].Value()
		}
	}
	agentCLIs := view.agents.Values()
	spec := project.Spec{
		Name:                   view.name.Value(),
		Shell:                  view.shell.Value(),
		AgentCLIs:              agentCLIs,
		DefaultTool:            view.defaultTool.Value(),
		AuthModes:              authModes,
		GraphifyModel:          graphifyModel,
		CavemanEnabled:         caveman,
		GraphifyEnabled:        graphify,
		CodeReviewGraphEnabled: codeReviewGraph,
		CodebaseMemoryEnabled:  codebaseMemory,
		OpenVikingEnabled:      openViking,
		TokenSaveEnabled:       tokenSave,
		AgentMemoryEnabled:     agentMemory,
		PonytailEnabled:        ponytail,
		AgentSkillsEnabled:     agentSkills,
		Root:                   view.location.dir,
	}
	return func() tea.Msg { return CreateConfirmedMsg{Spec: spec} }
}

// View renders the step header (title + N/M) and the current step's body. The
// auth-mode phase is dynamic, so the total step count grows once the (per-agent)
// auth steps are known.
func (view *Create) View() string {
	baseSteps := stepModel + 1 // location..model inclusive
	total := baseSteps + len(view.authAgents)
	title := stepTitles[view.step]
	current := view.step + 1
	if view.step == stepAuth {
		title = view.authAgents[view.authIndex] + " authentication"
		current = baseSteps + view.authIndex + 1
	}
	header := ui.Heading.Render(title) +
		ui.Muted.Render(fmt.Sprintf("   (step %d of %d)", current, total)) + "\n\n"
	return header + view.stepBody()
}

func (view *Create) stepBody() string {
	switch view.step {
	case stepLocation:
		return view.location.View()
	case stepName:
		return view.name.View()
	case stepShell:
		return view.shell.View()
	case stepAgents:
		return view.agents.View()
	case stepDefault:
		return view.defaultTool.View()
	case stepTools:
		return view.tools.View()
	case stepModel:
		if !view.graphifySelected() {
			return ui.Muted.Render("Graphify is not selected — the Graphify model step is skipped.")
		}
		return view.model.View()
	case stepAuth:
		if len(view.authLists) == 0 {
			return ""
		}
		return view.authLists[view.authIndex].View()
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
