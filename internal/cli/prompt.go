package cli

import (
	"errors"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// This file is the shared interactive-prompt surface every command uses to
// collect its inputs (CLI §21). The platform prefers prompted inputs over bare
// positional args/flags to reduce entry errors: on a TTY a command ALWAYS shows
// its prompt, PRE-SEEDED with any value the user passed on the command line, so
// the user confirms or edits it (the value never silently bypasses the TUI),
// validating as the user types. Under `--json` / no TTY a provided value is used
// directly with no prompt and a missing required value is exit 2 — so automation,
// CI, and `--json` stay fully scriptable. (Two exceptions: a hidden credential
// value can't display a seed, so when one is provided via --value/--stdin it is
// used directly even on a TTY; and `ai services` start/stop/restart act directly
// on an explicit name and only show the multi-select checkbox with no arg.)
//
// BACK-NAVIGATION: a command's prompts must all live in ONE huh form so the user
// can step BACK to a previous prompt before submitting. runForm runs a single
// form; multi-field groups navigate field-to-field, and multiple groups navigate
// page-to-page (huh's built-in back keybinding). Never collect a command's
// inputs through a sequence of separate runForm/promptText calls — that breaks
// back-navigation, because each form submits independently.

// interactive reports whether the CLI may prompt for missing inputs: stdin is a
// real terminal AND output is not the machine-readable JSON envelope. Automation
// and `--json` therefore never block on a prompt.
func interactive(emitter *output.Emitter) bool {
	return !emitter.JSON && term.IsTerminal(os.Stdin.Fd())
}

// formWidth returns the width to give every huh form so option rows never fill
// the terminal EXACTLY. huh's default layout leaves the width unchanged and its
// form defaults to the FULL terminal width (form.go WindowSizeMsg handler when
// width==0); each field then renders with styles.Base.Width(w), padding every
// line with trailing spaces up to w. A line that reaches the last terminal column
// phantom-wraps in many terminals, inserting a blank line between option rows —
// the "gap" bug. huh only trims trailing spaces on a group's LAST line, so the
// rows above still overflow. Bounding the form a couple of columns short of the
// terminal keeps every padded line strictly inside the viewport, so nothing
// wraps. Falls back to a sane default when the terminal size is unavailable.
func formWidth() int {
	const (
		margin       = 2  // columns of headroom below the terminal width
		fallback     = 80 // used when the terminal size is unknown
		maxFormWidth = 96 // a form wider than this reads poorly regardless
	)
	width, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || width <= 0 {
		width, _, err = term.GetSize(os.Stdin.Fd())
	}
	if err != nil || width <= 0 {
		return fallback
	}
	width -= margin
	if width > maxFormWidth {
		width = maxFormWidth
	}
	if width < 1 {
		width = 1
	}
	return width
}

// runForm runs a huh form built from the given groups and normalizes the outcome
// to the platform's exit codes: a user abort (ctrl-c / esc) and any other form
// error both surface as exit 2 (invalid input), with abort reported as a plain
// "cancelled". Pass every prompt for one command as groups of a SINGLE call so
// the user can navigate back between them (see the back-navigation note above).
func runForm(groups ...*huh.Group) error {
	form := huh.NewForm(groups...).WithTheme(ui.HuhTheme()).WithWidth(formWidth())
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return output.Errorf(output.ExitInvalidInput, "cancelled")
		}
		return output.Errorf(output.ExitInvalidInput, "prompt: %s", err)
	}
	return nil
}

// promptConfirm shows a yes/no dialog (default No) and returns the choice. It is
// used to confirm DESTRUCTIVE commands on a terminal before they act. A user abort
// (ctrl-c / esc) returns false (treated as "do not proceed"), NOT an error —
// declining is a normal outcome, not a cancellation error. It runs the huh form
// directly rather than through runForm precisely to get that abort-as-decline
// semantics (runForm maps huh.ErrUserAborted to an exit-2 error). Any OTHER form
// error is returned. Only call this on an interactive terminal (see interactive).
func promptConfirm(title, description string) (bool, error) {
	return promptConfirmDefault(title, description, false)
}

// promptConfirmDefault is promptConfirm with the selection pre-defaulted to
// initial (e.g. a `--purge` flag seeds the purge prompt to Yes). A user abort
// (ctrl-c/esc) is treated as a decline (false), not an error.
func promptConfirmDefault(title, description string, initial bool) (bool, error) {
	confirmed := initial
	field := huh.NewConfirm().Title(title).Value(&confirmed)
	if description != "" {
		field = field.Description(description)
	}
	form := huh.NewForm(huh.NewGroup(field)).WithTheme(ui.HuhTheme()).WithWidth(formWidth())
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, output.Errorf(output.ExitInvalidInput, "prompt: %s", err)
	}
	return confirmed, nil
}

// promptText prompts for a single trimmed line of text, seeded with initial and
// validated by validate (nil to skip). It is the single-input convenience over
// runForm; for a command with more than one input, build one runForm with all
// the fields instead so back-navigation works.
func promptText(title, description, initial string, validate func(string) error) (string, error) {
	value := initial
	input := huh.NewInput().Title(title).Value(&value)
	if description != "" {
		input = input.Description(description)
	}
	if validate != nil {
		input = input.Validate(validate)
	}
	if err := runForm(huh.NewGroup(input)); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// promptSecret is promptText with the entry hidden (for credentials). The value
// is NOT trimmed — a credential may legitimately contain surrounding whitespace.
func promptSecret(title, description string, validate func(string) error) (string, error) {
	var value string
	input := huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(&value)
	if description != "" {
		input = input.Description(description)
	}
	if validate != nil {
		input = input.Validate(validate)
	}
	if err := runForm(huh.NewGroup(input)); err != nil {
		return "", err
	}
	return value, nil
}

// promptChoice prompts the user to pick one of options (label/value pairs),
// pre-selecting initial. It is the single-input select convenience over runForm.
func promptChoice(title, description string, options []huh.Option[string], initial string) (string, error) {
	value := initial
	choice := huh.NewSelect[string]().Title(title).Options(options...).Value(&value)
	if description != "" {
		choice = choice.Description(description)
	}
	if err := runForm(huh.NewGroup(choice)); err != nil {
		return "", err
	}
	return value, nil
}

// promptMultiChoice prompts the user to check zero or more of options (label/value
// pairs) and returns the selected values, in option order. Use for "pick one or
// more" flows (e.g. choosing which services to act on) so the user toggles a
// checkbox list instead of typing names.
func promptMultiChoice(title, description string, options []huh.Option[string]) ([]string, error) {
	var selected []string
	choice := huh.NewMultiSelect[string]().Title(title).Options(options...).Value(&selected)
	if description != "" {
		choice = choice.Description(description)
	}
	if err := runForm(huh.NewGroup(choice)); err != nil {
		return nil, err
	}
	return selected, nil
}
