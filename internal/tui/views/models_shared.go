package views

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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

// modelStatus distinguishes a model already in the local store from a catalog model
// that is installable but not yet pulled, and (for cloud) a model the gateway serves.
type modelStatus string

const (
	statusInstalled modelStatus = "installed"
	statusAvailable modelStatus = "available"
	// statusRegistered marks a CLOUD catalog model the gateway currently serves
	// (registered in its DB — i.e. the provider is keyed). A cloud model with no
	// stored key is statusAvailable.
	statusRegistered modelStatus = "registered"
)

// ModelsPullRequestedMsg asks the parent to run `ai models pull <Refs...>` live in
// the terminal overlay (streaming progress) for one or more exact references (e.g.
// ["qwen2.5:7b", "qwen2.5:72b"]). Emitted by the Local Models tag drill-down.
type ModelsPullRequestedMsg struct{ Refs []string }

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

// truncateRunes clips a CELL value to width runes with a trailing ellipsis. The
// bubbles/table truncation is NOT ANSI-aware, so cell VALUES are kept plain (no
// inline colour) and clipped here; row highlight comes from the Selected style.
func truncateRunes(value string, width int) string {
	runes := []rune(value)
	if width <= 0 {
		return ""
	}
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return string(runes[:1])
	}
	return string(runes[:width-1]) + "…"
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

// tagSummary renders a compact tag column: the first few tags + an overflow count,
// with the installed count called out (e.g. "7b, 14b, 72b (+2) · 1 installed").
func tagSummary(tags []string, installed map[string]bool) string {
	if len(tags) == 0 {
		return "—"
	}
	const showFirst = 3
	shown := tags
	overflow := 0
	if len(tags) > showFirst {
		shown = tags[:showFirst]
		overflow = len(tags) - showFirst
	}
	summary := strings.Join(shown, ", ")
	if overflow > 0 {
		summary += fmt.Sprintf(" (+%d)", overflow)
	}
	installedCount := 0
	for _, tag := range tags {
		if installed[tag] {
			installedCount++
		}
	}
	if installedCount > 0 {
		summary += fmt.Sprintf(" · %d installed", installedCount)
	}
	return summary
}
