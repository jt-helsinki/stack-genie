package views

import (
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/mattn/go-runewidth"
)

// LocalModelLister returns the repos downloaded into the local vLLM store (`hf cache
// ls`). Injected; the parent wires hf.RealClient().CacheList.
type LocalModelLister func() ([]hf.CachedModel, error)

// CuratedLister returns the curated, vLLM-servable Hugging Face repos for this host
// platform (mlx-community/* on darwin, safetensors repos on linux). Injected; the parent
// wires hf.CuratedModels(goos).
type CuratedLister func() []hf.CuratedModel

// HFWhoamiFn returns the logged-in Hugging Face user (`hf auth whoami`). Injected; the
// parent wires hf.RealClient().Whoami. An error (not logged in / `hf` absent) reads as
// "not logged in" — it never breaks the view.
type HFWhoamiFn func() (string, error)

// ModelDisabledFn reports whether an installed repo's recorded vLLM runtime choice is
// disabled (`ai models disable` — excluded from ensureVLLMServers' auto-start pass
// without touching its weights or gateway registration). Injected; the parent wires
// it over config.LoadModelRuntimes(). nil / an unrecorded repo reads as not disabled.
type ModelDisabledFn func(repo string) bool

// localModel is one row of the Local Models view: a Hugging Face repo, either
// downloaded into the local vLLM store (Installed) or curated-but-not-installed
// (Available). vLLM is the sole local runtime, so a row is a single repo id — there are
// no per-variant tags.
type localModel struct {
	repo        string // Hugging Face repo id (e.g. "mlx-community/Qwen2.5-7B-Instruct-4bit")
	description string // curated description ("" for an installed repo not in the curated set)
	size        string // approximate/on-disk size when known
	installed   bool   // whether the repo is in the local vLLM store
	disabled    bool   // installed AND excluded from vLLM auto-start (`ai models disable`)
}

// localModelsRefreshedMsg carries the installed cache list result.
type localModelsRefreshedMsg struct {
	installed []hf.CachedModel
	listErr   error
}

// hfWhoamiMsg carries the Hugging Face login-state probe result (`hf auth whoami`).
// An error (not logged in / `hf` absent) leaves user empty and loggedIn false.
type hfWhoamiMsg struct {
	user     string
	loggedIn bool
}

// localNameWidth is the fixed NAME (repo) column width; DESCRIPTION takes the rest.
const localNameWidth = 46

// LocalModels is the Local Models tab: the local vLLM weight store (Installed, via
// `hf cache ls`) plus the curated set of installable, vLLM-servable Hugging Face repos
// (Available). Rendered as a custom viewport-windowed list (NOT a bubbles table) so each
// section header can be bold + accent-coloured. One cursor spans both sections over
// MODEL ROWS ONLY. enter/p pulls the selected Available repo; d removes an Installed
// one; t tests it. Keys: enter/p pull · t test · d remove · l login · o logout ·
// n pull by name · r refresh.
type LocalModels struct {
	test     ModelTester
	list     LocalModelLister
	curated  CuratedLister
	whoami   HFWhoamiFn
	disabled ModelDisabledFn

	models         []localModel // Installed rows first, then Available rows
	installedCount int          // how many of models are in the Installed section
	window         listWindow

	listErr error

	// Hugging Face auth state (best-effort, via `hf auth whoami`): whoamiUser is the
	// logged-in user when loggedIn is true; a whoami error just leaves loggedIn false.
	loggedIn   bool
	whoamiUser string

	// pulling + pullInput drive the inline "pull by name" prompt (n): while pulling,
	// keystrokes edit an arbitrary Hugging Face repo id and enter submits it as a
	// ModelsPullRequestedMsg (esc cancels). This lets a user pull ANY repo, not just a
	// curated Available row.
	pulling   bool
	pullInput textinput.Model

	width  int
	height int
	flash  string
	loaded bool
}

// NewLocalModels builds the Local Models view over the injected installed-store lister
// (hf cache ls), the curated-list provider, the gateway tester, the Hugging Face
// whoami probe (login-state line + gated-repo login/logout), and the disabled-state
// lookup (`ai models disable`, over config.LoadModelRuntimes()).
func NewLocalModels(list LocalModelLister, curated CuratedLister, test ModelTester, whoami HFWhoamiFn, disabled ModelDisabledFn) *LocalModels {
	input := textinput.New()
	input.Prompt = "pull repo: "
	input.Placeholder = "e.g. mlx-community/Qwen2.5-7B-Instruct-4bit"
	return &LocalModels{list: list, curated: curated, test: test, whoami: whoami, disabled: disabled, pullInput: input}
}

func (view *LocalModels) Title() string { return "Local Models" }

func (view *LocalModels) Hints() string {
	return "↑/↓ select · enter/p pull · t test · e enable/disable · d remove · l login · o logout · n pull by name · r refresh"
}

// CapturingInput reports whether the inline "pull by name" prompt is open, so the app
// routes all keys here (not the global shortcuts / list nav) while the user types a repo.
func (view *LocalModels) CapturingInput() bool { return view.pulling }

// SetSize records the pane dimensions.
func (view *LocalModels) SetSize(width, height int) {
	view.width = width
	view.height = height
	if width > len(view.pullInput.Prompt)+8 {
		view.pullInput.Width = width - len(view.pullInput.Prompt) - 4
	}
	view.syncWindow()
}

// sectionHeadingStyle is the bold, secondary-coloured style for the section headers.
func sectionHeadingStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(ui.Secondary())
}

func (view *LocalModels) windowLines() []ListLine {
	lines := make([]ListLine, 0, len(view.models)+6)
	appendSection := func(label string, from, to int) {
		if from >= to {
			return
		}
		lines = append(lines,
			ListLine{Text: ""},
			ListLine{Text: sectionHeadingStyle().Render(label)},
			ListLine{Text: ""},
		)
		for index := from; index < to; index++ {
			lines = append(lines, ListLine{Text: view.contentLine(view.models[index]), Selectable: true})
		}
	}
	appendSection("Installed", 0, view.installedCount)
	appendSection("Available", view.installedCount, len(view.models))
	return lines
}

// syncWindow pushes the current rendered lines + pane geometry into the listWindow.
func (view *LocalModels) syncWindow() {
	view.window.SetContent(view.windowLines(), view.width, view.listHeight())
}

// Init kicks off the first installed-store list AND the Hugging Face login-state probe.
func (view *LocalModels) Init() tea.Cmd {
	return tea.Batch(view.listCmd(), view.whoamiCmd())
}

// whoamiCmd probes the Hugging Face login state (`hf auth whoami`) best-effort. A nil
// whoami func or an error reads as "not logged in" — it never surfaces an error.
func (view *LocalModels) whoamiCmd() tea.Cmd {
	whoami := view.whoami
	return func() tea.Msg {
		if whoami == nil {
			return hfWhoamiMsg{}
		}
		user, err := whoami()
		if err != nil {
			return hfWhoamiMsg{}
		}
		user = strings.TrimSpace(user)
		if user == "" {
			return hfWhoamiMsg{}
		}
		return hfWhoamiMsg{user: user, loggedIn: true}
	}
}

// listCmd lists the locally-downloaded repos (`hf cache ls`).
func (view *LocalModels) listCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		var msg localModelsRefreshedMsg
		if list != nil {
			msg.installed, msg.listErr = list()
		}
		return msg
	}
}

func (view *LocalModels) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// Update advances the view: a refresh rebuilds the two sections; otherwise the action
// keys + list navigation apply.
func (view *LocalModels) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case localModelsRefreshedMsg:
		view.loaded = true
		view.listErr = message.listErr
		view.buildModels(message.installed)
		view.flash = ""
		view.syncWindow()
		return nil
	case hfWhoamiMsg:
		view.loggedIn = message.loggedIn
		view.whoamiUser = message.user
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return nil
	case tea.KeyMsg:
		// The inline "pull by name" prompt owns the keyboard while open.
		if view.pulling {
			return view.handlePullKey(message)
		}
		return view.handleKey(message)
	}
	return nil
}

// handlePullKey edits the inline "pull by name" repo prompt: enter submits the typed
// Hugging Face repo id as a ModelsPullRequestedMsg (empty input just closes the prompt),
// esc cancels, and every other key is fed to the text input.
func (view *LocalModels) handlePullKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc:
		view.pulling = false
		view.pullInput.Blur()
		view.pullInput.SetValue("")
		return nil
	case tea.KeyEnter:
		repo := strings.TrimSpace(view.pullInput.Value())
		view.pulling = false
		view.pullInput.Blur()
		view.pullInput.SetValue("")
		if repo == "" {
			return nil
		}
		return func() tea.Msg { return ModelsPullRequestedMsg{Refs: []string{repo}} }
	}
	var cmd tea.Cmd
	view.pullInput, cmd = view.pullInput.Update(key)
	return cmd
}

// handleKey routes a key: the list actions / navigation.
func (view *LocalModels) handleKey(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "enter", "p":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("no model selected")
			return nil
		}
		if model.installed {
			view.flash = ui.Muted.Render(model.repo + " is already installed (d removes it, t tests it)")
			return nil
		}
		repo := model.repo
		return func() tea.Msg { return ModelsPullRequestedMsg{Refs: []string{repo}} }
	case "t":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to test")
			return nil
		}
		if !model.installed {
			view.flash = ui.Muted.Render(model.repo + " is not installed (enter/p to pull it)")
			return nil
		}
		handle := "vllm/" + baseRepoName(model.repo)
		view.flash = ui.Muted.Render("testing " + handle + "…")
		return view.testCmd(handle)
	case "d":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to remove")
			return nil
		}
		if !model.installed {
			view.flash = ui.Muted.Render(model.repo + " is not installed (enter/p to pull it)")
			return nil
		}
		repo := model.repo
		return func() tea.Msg { return ModelRemoveRequestedMsg{Name: repo} }
	case "e":
		model, ok := view.selectedModel()
		if !ok {
			view.flash = ui.Muted.Render("select a model to enable/disable")
			return nil
		}
		if !model.installed {
			view.flash = ui.Muted.Render(model.repo + " is not installed (enter/p to pull it)")
			return nil
		}
		repo := model.repo
		if model.disabled {
			return func() tea.Msg { return ModelEnableRequestedMsg{Name: repo} }
		}
		return func() tea.Msg { return ModelDisableRequestedMsg{Name: repo} }
	case "l":
		// Authenticate `hf` for gated repos — runs `ai models login` in the REAL
		// terminal (the hidden token prompt needs a TTY).
		return func() tea.Msg { return ModelsLoginRequestedMsg{} }
	case "o":
		// Clear the `hf` credentials — runs `ai models logout` in the real terminal.
		return func() tea.Msg { return ModelsLogoutRequestedMsg{} }
	case "n":
		// Open the inline "pull by name" prompt: type an arbitrary Hugging Face repo id
		// (not just a curated Available row) and enter pulls it.
		view.pulling = true
		view.pullInput.SetValue("")
		view.pullInput.Focus()
		view.flash = ""
		return textinput.Blink
	case "r":
		view.flash = ui.Muted.Render("refreshing local models…")
		return tea.Batch(view.listCmd(), view.whoamiCmd())
	case "up", "k":
		view.moveCursor(-1)
		return nil
	case "down", "j":
		view.moveCursor(1)
		return nil
	}
	return nil
}

// --- data build -------------------------------------------------------------

// buildModels groups the installed store + the curated list into Installed-first then
// Available rows and seats the cursor on the first model row. A curated repo that is
// already installed appears ONLY in the Installed section (with its curated description).
func (view *LocalModels) buildModels(installed []hf.CachedModel) {
	var curated []hf.CuratedModel
	if view.curated != nil {
		curated = view.curated()
	}
	curatedByRepo := make(map[string]hf.CuratedModel, len(curated))
	for _, model := range curated {
		curatedByRepo[model.Repo] = model
	}

	installedSet := make(map[string]bool, len(installed))
	installedRows := make([]localModel, 0, len(installed))
	for _, model := range installed {
		installedSet[model.Repo] = true
		row := localModel{repo: model.Repo, size: model.Size, installed: true}
		if view.disabled != nil {
			row.disabled = view.disabled(model.Repo)
		}
		if meta, ok := curatedByRepo[model.Repo]; ok {
			row.description = meta.Description
			if row.size == "" {
				row.size = meta.Size
			}
		}
		installedRows = append(installedRows, row)
	}

	availableRows := make([]localModel, 0, len(curated))
	for _, model := range curated {
		if installedSet[model.Repo] {
			continue
		}
		availableRows = append(availableRows, localModel{
			repo:        model.Repo,
			description: model.Description,
			size:        model.Size,
		})
	}

	sort.Slice(installedRows, func(left, right int) bool { return installedRows[left].repo < installedRows[right].repo })
	sort.Slice(availableRows, func(left, right int) bool { return availableRows[left].repo < availableRows[right].repo })

	view.models = append(installedRows, availableRows...)
	view.installedCount = len(installedRows)
	view.window.Reset()
}

// moveCursor steps the cursor by step (±1) over MODEL rows only.
func (view *LocalModels) moveCursor(step int) {
	if len(view.models) == 0 {
		return
	}
	view.syncWindow()
	view.window.Move(step)
}

// selectedModel returns the model under the cursor.
func (view *LocalModels) selectedModel() (localModel, bool) {
	cursor := view.window.Cursor()
	if cursor < 0 || cursor >= len(view.models) {
		return localModel{}, false
	}
	return view.models[cursor], true
}

// --- rendering --------------------------------------------------------------

func (view *LocalModels) headerLines() int { return strings.Count(view.header(), "\n") }

// hfStatusLine renders the Hugging Face login state (`hf auth whoami`): the logged-in
// user when known, else a not-logged-in hint pointing at the `l` login key (needed for
// gated repos: meta-llama/*, google/gemma-*, mistralai/*).
func (view *LocalModels) hfStatusLine() string {
	if view.loggedIn && view.whoamiUser != "" {
		return ui.Muted.Render("Hugging Face: ") + ui.Success.Render("logged in as "+view.whoamiUser)
	}
	return ui.Muted.Render("Hugging Face: not logged in — press l to log in for gated repos")
}

// header renders everything above the two-section list.
func (view *LocalModels) header() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Local models — vLLM store + curated available") + "\n")
	body.WriteString(view.hfStatusLine() + "\n")
	if view.listErr != nil {
		body.WriteString(ui.Failure.Render(ui.IconFail+" could not list the local store: "+view.listErr.Error()) +
			"\n" + ui.Muted.Render("the Hugging Face CLI (`hf`) is installed by `ai setup` into the platform venv; run it if `hf` is missing") + "\n")
	}
	if len(view.models) == 0 {
		body.WriteString(ui.Muted.Render("no models to show (enter/p on an Available row to pull one)") + "\n")
	}
	return body.String()
}

// listHeight is the FIXED number of rendered lines the two-section list block occupies.
func (view *LocalModels) listHeight() int {
	if view.height <= 0 {
		return len(view.models)
	}
	height := view.height - view.headerLines() - 1 // -1 for the flash slot
	if height < 1 {
		height = 1
	}
	return height
}

// contentLine renders one model row's "NAME  DESCRIPTION" content padded to the full
// pane width. DESCRIPTION takes everything left over after the fixed NAME column; the
// size is appended to the description when known.
func (view *LocalModels) contentLine(model localModel) string {
	descWidth := view.descriptionWidth()
	desc := model.description
	if model.size != "" {
		if desc != "" {
			desc += "  ·  " + model.size
		} else {
			desc = model.size
		}
	}
	if model.disabled {
		if desc != "" {
			desc += "  ·  [disabled]"
		} else {
			desc = "[disabled]"
		}
	}
	name := padCell(truncateRunes(model.repo, localNameWidth), localNameWidth)
	desc = truncateRunes(desc, descWidth)
	line := " " + name + "  " + desc
	return padToWidth(line, view.width)
}

// descriptionWidth is everything left after NAME and the spacing.
func (view *LocalModels) descriptionWidth() int {
	if view.width <= 0 {
		return 44
	}
	desc := view.width - localNameWidth - 3
	if desc < 1 {
		return 1
	}
	return desc
}

// View renders the two-section list.
func (view *LocalModels) View() string {
	if !view.loaded {
		return ui.Muted.Render("loading local models…")
	}
	var body strings.Builder
	body.WriteString(view.header())
	if len(view.models) > 0 {
		body.WriteString(view.listView())
	}
	// The inline "pull by name" prompt replaces the flash slot while open.
	if view.pulling {
		body.WriteString("\n" + view.pullInput.View() + ui.Muted.Render("  (enter pull · esc cancel)"))
	} else {
		body.WriteString("\n" + flashLine(view.flash))
	}
	return body.String()
}

// selectedStyle is the highlight applied to the cursor's model row: IDENTICAL to
// ui.TableStyles().Selected, so the Local Models highlight matches Cloud Models exactly.
func selectedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).
		Foreground(ui.Secondary()).
		Background(ui.Accent())
}

// listView renders the two-section list through the reusable listWindow.
func (view *LocalModels) listView() string {
	view.syncWindow()
	return view.window.View(selectedStyle())
}

// --- small helpers ----------------------------------------------------------

// baseRepoName returns the repo id's last path segment (the gateway alias vLLM uses:
// "mlx-community/Qwen2.5-7B-Instruct-4bit" → "Qwen2.5-7B-Instruct-4bit").
func baseRepoName(repo string) string {
	if slash := strings.LastIndexByte(repo, '/'); slash >= 0 && slash < len(repo)-1 {
		return repo[slash+1:]
	}
	return repo
}

// padCell right-pads a plain cell value to width DISPLAY CELLS.
func padCell(value string, width int) string {
	return runewidth.FillRight(value, width)
}

// padToWidth right-pads a plain (un-styled) line to width DISPLAY CELLS.
func padToWidth(line string, width int) string {
	if width <= 0 {
		return line
	}
	return runewidth.FillRight(line, width)
}
