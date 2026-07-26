package views

import (
	"fmt"
	"strconv"
	"strings"

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
// the terminal overlay (streaming progress) for one or more exact references (e.g.
// ["qwen2.5:7b", "qwen2.5:72b"]). Emitted by the Local Models tag drill-down.
// Runtime is the chosen install ENGINE ("ollama" / "docker-model-runner"), mirroring
// the CLI `--runtime`; an EMPTY value means Ollama (back-compat — a caller that builds
// the message without a runtime keeps the default Ollama path).
type ModelsPullRequestedMsg struct {
	Refs    []string
	Runtime string
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

// paramMagnitude converts a parameter-size label (e.g. "270m", "1.5b", "70b") into a
// comparable magnitude so a within-model tag order is ascending. An
// unparseable/empty label yields 0 (sorts first).
func paramMagnitude(label string) float64 {
	label = strings.TrimSpace(strings.ToLower(label))
	if label == "" {
		return 0
	}
	suffix := byte(0)
	if last := label[len(label)-1]; last == 'm' || last == 'b' {
		suffix = last
		label = label[:len(label)-1]
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(label), 64)
	if err != nil {
		return 0
	}
	switch suffix {
	case 'm':
		return value * 1e6
	case 'b':
		return value * 1e9
	default:
		return value
	}
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

// valueString renders a model_info value as a readable single line. Scalars print
// cleanly; the rare non-scalar value (slice/map) falls back to %v.
func valueString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprintf("%v", value)
	}
}

// truncate clips an overlong string (e.g. a multi-KB license) to limit runes with a
// trailing ellipsis note so the describe pane stays scrollable rather than enormous.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "\n… (truncated)"
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
