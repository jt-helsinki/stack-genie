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
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// Palette — lipgloss adapts these to the terminal's colour profile and strips
// them entirely when output is not a TTY or NO_COLOR is set, so colour needs no
// manual gating.
const (
	colorAccent  = lipgloss.Color("39")  // blue
	colorSuccess = lipgloss.Color("42")  // green
	colorWarn    = lipgloss.Color("214") // amber
	colorError   = lipgloss.Color("203") // red
	colorMuted   = lipgloss.Color("245") // grey
)

// Named styles, shared by every command's human output and the TUI components.
var (
	Heading = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	Success = lipgloss.NewStyle().Foreground(colorSuccess)
	Warn    = lipgloss.NewStyle().Foreground(colorWarn)
	Failure = lipgloss.NewStyle().Foreground(colorError)
	Muted   = lipgloss.NewStyle().Foreground(colorMuted)
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

// HuhTheme is the form theme for the shared prompt helpers, so prompts match the
// rest of the UI.
func HuhTheme() *huh.Theme {
	return huh.ThemeCharm()
}
