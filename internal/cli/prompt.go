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
// positional args/flags to reduce entry errors: on a TTY a command prompts for
// any value it was not given, validating as the user types. Positional args and
// flags remain the non-interactive fallback, so automation, CI, and `--json`
// stay fully scriptable (and a value passed on the command line is used as-is,
// skipping its prompt).
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

// runForm runs a huh form built from the given groups and normalizes the outcome
// to the platform's exit codes: a user abort (ctrl-c / esc) and any other form
// error both surface as exit 2 (invalid input), with abort reported as a plain
// "cancelled". Pass every prompt for one command as groups of a SINGLE call so
// the user can navigate back between them (see the back-navigation note above).
func runForm(groups ...*huh.Group) error {
	form := huh.NewForm(groups...).WithTheme(ui.HuhTheme())
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return output.Errorf(output.ExitInvalidInput, "cancelled")
		}
		return output.Errorf(output.ExitInvalidInput, "prompt: %s", err)
	}
	return nil
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
