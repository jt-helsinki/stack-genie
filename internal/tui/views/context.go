package views

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// ContextGetter returns the per-project context-optimization status (Headroom
// strategy, Caveman level, install state). Injected; the parent wires
// contextopt.GetStatus(root).
type ContextGetter func(root string) (contextopt.Status, error)

// StrategySetter sets the project's Headroom strategy. Injected; the parent
// wires contextopt.SetStrategy(root, strategy).
type StrategySetter func(root, strategy string) error

// CavemanSetter sets the project's Caveman output-compression level. Injected;
// the parent wires contextopt.SetCavemanLevel(root, level).
type CavemanSetter func(root, level string) error

type contextRefreshedMsg struct {
	status contextopt.Status
	err    error
}
type contextStrategyDoneMsg struct {
	strategy string
	err      error
}
type contextCavemanDoneMsg struct {
	level string
	err   error
}

// Context is the per-project view of context optimization: the Headroom strategy,
// the Caveman level, and whether the Caveman skill is installed, with keys to
// cycle the strategy and the Caveman level.
type Context struct {
	current     CurrentRoot
	get         ContextGetter
	setStrategy StrategySetter
	setCaveman  CavemanSetter
	status      contextopt.Status
	flash       string
	err         error
	loaded      bool
}

// NewContext builds the context view over the injected current-project resolver,
// status getter, and the strategy + Caveman setters.
func NewContext(current CurrentRoot, get ContextGetter, setStrategy StrategySetter, setCaveman CavemanSetter) *Context {
	return &Context{current: current, get: get, setStrategy: setStrategy, setCaveman: setCaveman}
}

func (view *Context) Title() string    { return "Context" }
func (view *Context) Hints() string    { return "s cycle strategy · c cycle caveman" }
func (view *Context) SetSize(int, int) {}

// Init refreshes the current project's context status (no-op with no project).
func (view *Context) Init() tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	return view.refreshCmd(root)
}

func (view *Context) refreshCmd(root string) tea.Cmd {
	get := view.get
	return func() tea.Msg {
		status, err := get(root)
		return contextRefreshedMsg{status: status, err: err}
	}
}

// Update advances the view: a refresh fills the status; "s" cycles the strategy
// and "c" cycles the Caveman level (async); each result flashes and re-refreshes.
func (view *Context) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case contextRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.status = message.status
		}
		return nil
	case contextStrategyDoneMsg:
		view.flash = contextStrategyFlash(message)
		return view.refreshCurrent()
	case contextCavemanDoneMsg:
		view.flash = contextCavemanFlash(message)
		return view.refreshCurrent()
	case tea.KeyMsg:
		root, ok := view.current()
		if !ok {
			return nil
		}
		switch message.String() {
		case "s":
			next := nextChoice(contextopt.Strategies, view.status.Strategy)
			view.flash = ui.Muted.Render("setting strategy " + next + "…")
			return view.strategyCmd(root, next)
		case "c":
			next := nextChoice(contextopt.CavemanLevels, view.status.CavemanLevel)
			view.flash = ui.Muted.Render("setting caveman " + next + "…")
			return view.cavemanCmd(root, next)
		}
	}
	return nil
}

func (view *Context) refreshCurrent() tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	return view.refreshCmd(root)
}

func (view *Context) strategyCmd(root, strategy string) tea.Cmd {
	setStrategy := view.setStrategy
	return func() tea.Msg {
		return contextStrategyDoneMsg{strategy: strategy, err: setStrategy(root, strategy)}
	}
}

func (view *Context) cavemanCmd(root, level string) tea.Cmd {
	setCaveman := view.setCaveman
	return func() tea.Msg {
		return contextCavemanDoneMsg{level: level, err: setCaveman(root, level)}
	}
}

// nextChoice returns the choice after current in choices, wrapping around (an
// unknown current value starts the cycle at the first choice).
func nextChoice(choices []string, current string) string {
	index := slices.Index(choices, current)
	if index < 0 {
		return choices[0]
	}
	return choices[(index+1)%len(choices)]
}

// View renders the strategy, Caveman level, and install state (or the
// no-project / load / error line) with the latest flash.
func (view *Context) View() string {
	if _, ok := view.current(); !ok {
		return ui.Muted.Render("no project selected — open one from the Projects view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading context status…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Context optimization") + "\n")
	body.WriteString(field("strategy", view.status.Strategy))
	body.WriteString(field("caveman", view.status.CavemanLevel))
	body.WriteString(field("caveman installed", contextInstalled(view.status.CavemanInstalled)))
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

func contextInstalled(installed bool) string {
	if installed {
		return ui.Success.Render(ui.IconOK + " yes")
	}
	return ui.Warn.Render("no")
}

// contextStrategyFlash renders the outcome of a strategy change.
func contextStrategyFlash(msg contextStrategyDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " set strategy " + msg.strategy + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " strategy set to " + msg.strategy)
}

// contextCavemanFlash renders the outcome of a Caveman-level change.
func contextCavemanFlash(msg contextCavemanDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " set caveman " + msg.level + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " caveman set to " + msg.level)
}
