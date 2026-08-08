package views

import (
	"strconv"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/mattn/go-runewidth"
)

// This file holds the bits shared by the two model views (Local Models and Cloud
// Models): the injected-func types, the request-message types, the status constants,
// and the small render helpers (test flash, token formatting, truncation). Each view
// (localmodels.go / cloudmodels.go) owns its own table, data build, and key handling.

// ModelStatusFetcher returns the LiteLLM gateway status (health, default model,
// providers, base URL). Injected; the parent wires litellm.RealClient().Status.
type ModelStatusFetcher func() (litellm.StatusInfo, error)

// ModelTester probes one model through the gateway and returns the result.
// Injected; the parent wires litellm.RealClient().Test(model).
type ModelTester func(model string) (litellm.TestResult, error)

// modelStatus is the Cloud Models row status. The Cloud Models view now shows ONLY
// models the gateway serves (the provider is keyed), so every row is statusRegistered;
// the type is retained for the STATUS column + the describe pane wording.
type modelStatus string

const (
	// statusRegistered marks a CLOUD catalog model the gateway currently serves
	// (registered in its DB — i.e. the provider is keyed).
	statusRegistered modelStatus = "registered"
)

// ModelsPullRequestedMsg asks the parent to run `ai models pull <Refs...>` live in
// the terminal overlay (streaming progress) for one or more Hugging Face repo ids
// (e.g. ["mlx-community/Qwen2.5-7B-Instruct-4bit"]). Emitted by the Local Models view.
// vLLM is the sole local runtime, so there is no engine choice to carry.
type ModelsPullRequestedMsg struct {
	Refs []string
}

// ModelRemoveRequestedMsg asks the parent to run `ai models rm <Name>` live in the
// terminal overlay (with its confirm prompt).
type ModelRemoveRequestedMsg struct{ Name string }

// modelTestDoneMsg carries the outcome of a model test-probe through the gateway.
type modelTestDoneMsg struct {
	result litellm.TestResult
	err    error
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

// humanTokenCount formats a token limit compactly (e.g. 200000 → "200K", 1000000 →
// "1M"); small counts print verbatim. Used for the context/output limit columns.
func humanTokenCount(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return strconv.FormatFloat(float64(tokens)/1_000_000, 'g', 3, 64) + "M"
	case tokens >= 1_000:
		return strconv.FormatFloat(float64(tokens)/1_000, 'g', 3, 64) + "K"
	default:
		return strconv.Itoa(tokens)
	}
}

// truncateRunes clips a CELL value to width DISPLAY CELLS (terminal columns, not
// rune count) with a trailing ellipsis when clipped. Measuring by display width is
// what keeps columns aligned when a value contains wide runes (emoji like 🌋/🎩 or
// CJK occupy two cells each). Cell VALUES are kept plain (no inline colour) and
// clipped here; the row highlight comes from the Selected style.
func truncateRunes(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return runewidth.Truncate(value, width, "…")
}

// flashLine renders a view's flash message as EXACTLY one line: the message when
// set, an empty (blank) line otherwise. Views reserve one row for it in SetSize and
// always emit it so the table/list above fills a FIXED content height — the bottom
// never shifts depending on whether a flash is currently shown.
func flashLine(flash string) string {
	if flash == "" {
		return ""
	}
	return flash
}

// sourceFlash builds the source-availability warning for an offline live source
// (the Ollama library or models.dev). cached reports whether a cached copy was
// shown; source names the live source for the message. Returns "" when there is no
// warning (the caller only calls this on a fetch error).
func sourceFlash(source string, cached bool) string {
	if cached {
		return ui.Warn.Render(ui.IconArrow + " couldn't reach the " + source +
			"; showing the cached copy — press r to retry")
	}
	return ui.Warn.Render(ui.IconArrow + " couldn't reach the " + source +
		" and no cached copy — connect and press r")
}
