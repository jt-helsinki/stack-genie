package cli

import (
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/spf13/cobra"
)

// themeResult is the `ai theme` payload: the active theme name and the full set
// of selectable themes.
type themeResult struct {
	Theme     string   `json:"theme"`
	Available []string `json:"available"`
}

// Human lists the selectable themes with the active one marked, so an operator
// can see what is set and what else is available.
func (result themeResult) Human() string {
	var builder strings.Builder
	builder.WriteString(ui.Heading.Render("UI theme") + "\n")
	for _, name := range result.Available {
		if name == result.Theme {
			builder.WriteString("  " + ui.Success.Render(ui.IconOK+" ") + ui.Primary.Render(name) + ui.Muted.Render(" (active)") + "\n")
			continue
		}
		builder.WriteString("    " + ui.Value.Render(name) + "\n")
	}
	builder.WriteString(ui.Muted.Render("Change it with ") + ui.Primary.Render("`ai theme <name>`") + ui.Muted.Render(" (prompts on a terminal)."))
	return builder.String()
}

// newThemeCmd builds `ai theme [name]` (CLI §1.8): select the CLI colour theme
// used by the interactive prompts, forms, the stepper, and headings. The choice
// is persisted per host (~/.ai-platform/config/ui.yaml) and applied at startup.
//
// On a terminal it ALWAYS shows a select prompt, pre-seeded with the [name] arg
// (else the active theme), so the user confirms or changes it. Under --json / no
// TTY a provided name is applied directly (unknown → exit 2); with no name it
// just reports the current theme (it cannot prompt).
func newThemeCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "theme [name]",
		Short:             "Show or select the CLI colour theme",
		Long:              "Select the CLI colour theme (prompts, forms, stepper, headings). The choice is\nsaved per host and applied to every command. Available: " + strings.Join(ui.ThemeNames(), ", ") + ".",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeThemeNames,
		RunE: func(_ *cobra.Command, args []string) error {
			seed := ui.CurrentTheme()
			if len(args) == 1 {
				seed = args[0]
			}

			var chosen string
			switch {
			case interactive(emitter):
				options := make([]huh.Option[string], 0, len(ui.ThemeNames()))
				for _, name := range ui.ThemeNames() {
					options = append(options, huh.NewOption(name, name))
				}
				picked, err := promptChoice("Theme", "the CLI colour theme (prompts, forms, headings)", options, seed)
				if err != nil {
					*exit = emitter.Failure("theme", err)
					return nil
				}
				chosen = picked
			case len(args) == 0:
				// Non-interactive with no name: report the active theme, change nothing.
				*exit = emitter.Success("theme", themeResult{Theme: ui.CurrentTheme(), Available: ui.ThemeNames()})
				return nil
			default:
				chosen = args[0]
			}

			if !ui.IsTheme(chosen) {
				*exit = emitter.Failure("theme", output.Errorf(output.ExitInvalidInput,
					"unknown theme %q (one of %v)", chosen, ui.ThemeNames()))
				return nil
			}
			_ = ui.Apply(chosen) // validated above
			if err := ui.SaveThemeName(chosen); err != nil {
				*exit = emitter.Failure("theme", output.Errorf(output.ExitRuntimeFailure, "could not save theme: %s", err))
				return nil
			}
			*exit = emitter.Success("theme", themeResult{Theme: chosen, Available: ui.ThemeNames()})
			return nil
		},
	}
}

// completeThemeNames is the shell-completion func for the [name] arg.
func completeThemeNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return ui.ThemeNames(), cobra.ShellCompDirectiveNoFileComp
}
