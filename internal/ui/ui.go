// Package ui is the platform's shared TUI design system: a colour palette and
// styles, status icons, a huh form theme, and the gate that decides when
// interactive TUI rendering is allowed.
//
// INVARIANT (protects the external-program JSON contract): interactive TUI
// (spinners, steppers, themed forms) renders to STDERR only and is used ONLY when
// Enabled reports true — never under --json, never under --plain, never when
// stderr is not a real terminal. stdout therefore always carries just the result
// (the JSON envelope for machine callers), and automation/CI never block on a
// cursor-controlling program.
package ui

import (
	"os"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// Palette — lipgloss adapts these to the terminal's colour profile and strips
// them entirely when output is not a TTY or NO_COLOR is set, so colour needs no
// manual gating.
const (
	colorPrimary = lipgloss.Color("#FF66FF") // bright pink   (R255 G102 B255)
	colorAccent  = lipgloss.Color("#33FFFF") // bright blue   (R51  G255 B255) — secondary
	colorSuccess = lipgloss.Color("#33FF99") // green
	colorWarn    = lipgloss.Color("#FF8000") // bright orange (R255 G128 B0)
	colorError   = lipgloss.Color("#FF3333") // bright red    (R255 G51  B51)
	colorMuted   = lipgloss.Color("245")     // grey
)

// Named styles, shared by every command's human output and the TUI components.
// CLI styling vocabulary — apply CONSISTENTLY across command Human renderers:
//   - Heading: section titles/headers (primary pink, bold). Restyled per theme by Apply.
//   - Primary: emphasis + the platform's own nouns/commands (primary pink).
//   - Value:   data — names, paths, counts, URLs, IDs, the "value" in key: value
//     (secondary blue). This is the "other text" colour.
//   - Label:   field/column labels, the "key" in key: value (muted/dim).
//   - Success/Warn/Failure: status words + their icons (green/orange/red).
//   - Muted:   secondary/dim/hint text.
var (
	Heading = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)
	Primary = lipgloss.NewStyle().Foreground(colorPrimary)
	Value   = lipgloss.NewStyle().Foreground(colorAccent)
	Success = lipgloss.NewStyle().Foreground(colorSuccess)
	Warn    = lipgloss.NewStyle().Foreground(colorWarn)
	Failure = lipgloss.NewStyle().Foreground(colorError)
	Muted   = lipgloss.NewStyle().Foreground(colorMuted)
	Label   = Muted
)

// Status icons (colour is applied by the styles above where rendered).
const (
	IconOK    = "✓"
	IconFail  = "✗"
	IconDot   = "•"
	IconArrow = "→"
)

// Enabled reports whether interactive TUI rendering should be used: not --json,
// not --plain, and BOTH stdin and stderr are real terminals. bubbletea reads
// stdin (it needs a TTY for raw mode) and the TUI renders to stderr (stdout is
// reserved for the result envelope), so both must be terminals. This is the
// single source of truth every command consults before using RunWithSpinner or
// RunSteps; when it is false, callers fall back to plain text. It also keeps the
// machine path safe: --json/--plain or any redirection disables the TUI.
func Enabled(emitter *output.Emitter) bool {
	if emitter.JSON || emitter.Plain {
		return false
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stderr.Fd())
}

// EmbeddedTerminalEnv is set to "1" in the child's environment by the `ai ui`
// terminal pane, so a nested `ai` command knows it is running INSIDE the pane. There
// the pane already renders the child's PTY through its own emulator, so a nested
// animated bubbletea spinner would fight the child's own streaming progress (msb's
// layered pull, model/docker downloads) for cursor control of the shared PTY —
// producing flicker, cascaded/garbled lines, and an apparent hang. Commands consult
// EmbeddedTerminal() to run the work directly (progress streams cleanly) instead.
const EmbeddedTerminalEnv = "AI_UI_TERMINAL"

// EmbeddedTerminal reports whether this process is running inside the `ai ui`
// terminal pane (see EmbeddedTerminalEnv).
func EmbeddedTerminal() bool { return os.Getenv(EmbeddedTerminalEnv) == "1" }

// HuhTheme is the form theme for the shared prompt helpers, so prompts match the
// rest of the UI. It reflects the active theme (see Apply / `ai theme`).
func HuhTheme() *huh.Theme {
	return active.huh()
}
