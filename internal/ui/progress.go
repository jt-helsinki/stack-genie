package ui

import (
	"fmt"
	"io"
	"strings"
)

// progressBarWidth is the character width of the drawn meter.
const progressBarWidth = 24

// ProgressBar renders a single carriage-return-updated progress line — a meter + percent
// + human byte counts + status — for a streaming model download. It works
// in a plain TTY (the line overwrites in place) AND in the `ai ui` embedded terminal
// (the LogView's normalizeTerminalOutput interprets the \r + erase-line). Call Update per
// progress frame and Finish once at the end. Use only when ui.Enabled is true (skip it
// for --json / no-TTY, where the line noise is unwanted).
type ProgressBar struct {
	out   io.Writer
	label string
}

// NewProgressBar builds a progress bar that draws to out with a fixed leading label.
func NewProgressBar(out io.Writer, label string) *ProgressBar {
	return &ProgressBar{out: out, label: label}
}

// Update redraws the line for the current frame: a meter + percent + completed/total when
// total is known, else a status-only line (e.g. "pulling manifest" / "verifying"). The
// \033[K erases any remnant of a longer previous line, so shrinking lines stay clean.
func (bar *ProgressBar) Update(completed, total int64, status string) {
	var line string
	switch {
	case total > 0:
		line = bar.label + "  " + ProgressBarLine(completed, total)
	case status != "":
		line = bar.label + "  " + status
	default:
		line = bar.label
	}
	_, _ = fmt.Fprintf(bar.out, "\r%s\033[K", line)
}

// ProgressBarLine renders just the meter + percent + human byte counts (no label), e.g.
// "████████░░░░ 62%  2.9 GiB / 4.7 GiB". Shared by ProgressBar (the \r-streaming CLI bar)
// and static string renderers like the TUI create-progress pane.
func ProgressBarLine(completed, total int64) string {
	if total <= 0 {
		return ""
	}
	fraction := float64(completed) / float64(total)
	if fraction > 1 {
		fraction = 1
	}
	filled := int(fraction * float64(progressBarWidth))
	meter := strings.Repeat("█", filled) + strings.Repeat("░", progressBarWidth-filled)
	return fmt.Sprintf("%s %3.0f%%  %s / %s", meter, fraction*100, humanBytes(completed), humanBytes(total))
}

// Finish clears the progress line and prints the terminal ✓/✗ summary line.
func (bar *ProgressBar) Finish(err error) {
	glyph := Success.Render(IconOK)
	if err != nil {
		glyph = Failure.Render(IconFail)
	}
	_, _ = fmt.Fprintf(bar.out, "\r%s %s\033[K\n", glyph, bar.label)
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB/…) suffix.
func humanBytes(count int64) string {
	const unit = 1024
	if count < unit {
		return fmt.Sprintf("%d B", count)
	}
	div, exp := int64(unit), 0
	for value := count / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(count)/float64(div), "KMGTPE"[exp])
}
