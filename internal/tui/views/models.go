package views

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ModelStatusFetcher returns the LiteLLM gateway status (health, default model,
// providers, base URL). Injected; the parent wires litellm.RealClient().Status.
type ModelStatusFetcher func() (litellm.StatusInfo, error)

// ModelTester probes one model through the gateway and returns the result.
// Injected; the parent wires litellm.RealClient().Test(model).
type ModelTester func(model string) (litellm.TestResult, error)

type modelsRefreshedMsg struct {
	status litellm.StatusInfo
	err    error
}
type modelTestDoneMsg struct {
	result litellm.TestResult
	err    error
}

// Models is the global view of the LiteLLM model gateway: reachability, the
// default model, configured providers, and the gateway endpoint, with a key to
// test-probe the default model.
type Models struct {
	fetch  ModelStatusFetcher
	test   ModelTester
	status litellm.StatusInfo
	flash  string
	err    error
	loaded bool
}

// NewModels builds the models view over the injected status fetcher and tester.
func NewModels(fetch ModelStatusFetcher, test ModelTester) *Models {
	return &Models{fetch: fetch, test: test}
}

func (view *Models) Title() string    { return "Models" }
func (view *Models) Hints() string    { return "t test default" }
func (view *Models) SetSize(int, int) {}

// Init kicks off the first gateway-status fetch.
func (view *Models) Init() tea.Cmd { return view.refreshCmd() }

func (view *Models) refreshCmd() tea.Cmd {
	fetch := view.fetch
	return func() tea.Msg {
		status, err := fetch()
		return modelsRefreshedMsg{status: status, err: err}
	}
}

// Update advances the view: a refresh fills the summary; "t" test-probes the
// default model (async); the test result flashes and re-refreshes.
func (view *Models) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case modelsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.status = message.status
		}
		return nil
	case modelTestDoneMsg:
		view.flash = modelTestFlash(message)
		return view.refreshCmd()
	case tea.KeyMsg:
		if message.String() == "t" {
			model := view.status.Default
			if model == "" {
				view.flash = ui.Muted.Render("no default model to test")
				return nil
			}
			view.flash = ui.Muted.Render("testing " + model + "…")
			return view.testCmd(model)
		}
	}
	return nil
}

func (view *Models) testCmd(model string) tea.Cmd {
	test := view.test
	return func() tea.Msg {
		result, err := test(model)
		return modelTestDoneMsg{result: result, err: err}
	}
}

// View renders the gateway summary (health, default, providers, base URL) with
// the latest test flash.
func (view *Models) View() string {
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading model gateway status…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Model gateway") + "\n")
	body.WriteString(field("healthy", modelsHealth(view.status.Healthy)))
	body.WriteString(field("default", view.status.Default))
	body.WriteString(field("providers", strings.Join(view.status.Providers, ", ")))
	body.WriteString(field("base url", view.status.BaseURL))
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

func modelsHealth(healthy bool) string {
	if healthy {
		return ui.Success.Render(ui.IconOK + " reachable")
	}
	return ui.Failure.Render(ui.IconFail + " unreachable")
}

// modelTestFlash renders the outcome of a model test-probe.
func modelTestFlash(msg modelTestDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " test " + msg.result.Model + ": " + msg.err.Error())
	}
	if !msg.result.OK {
		detail := msg.result.Error
		if detail == "" && msg.result.Status != 0 {
			detail = "status " + strconv.Itoa(msg.result.Status)
		}
		if detail == "" {
			detail = "unreachable"
		}
		return ui.Failure.Render(ui.IconFail + " " + msg.result.Model + ": " + detail)
	}
	return ui.Success.Render(ui.IconOK + " " + msg.result.Model + " reachable (" + strconv.Itoa(msg.result.LatencyMS) + "ms)")
}
