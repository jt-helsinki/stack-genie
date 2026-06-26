package cli

import (
	"regexp"
	"text/template"

	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/spf13/cobra"
)

// flagTokenRe matches a short/long flag token (-h, --help, --dry-run) inside a
// FlagUsages block so each can be coloured. lipgloss codes are zero-width, so the
// column alignment cobra computed is preserved (and stripped to plain off a TTY).
var flagTokenRe = regexp.MustCompile(`--?[a-zA-Z][\w-]*`)

// styledUsageTemplate is cobra's default usage template with the platform palette
// applied via the template funcs registered in applyStyledHelp: section HEADERS in
// the heading style (pink, bold), command NAMES in primary (pink), flag tokens in
// the value style (blue), and the footer hint muted. Set on the root command and
// inherited by every subcommand, so all `ai <cmd> --help` output is styled.
const styledUsageTemplate = `{{aiHeading "Usage:"}}{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

{{aiHeading "Aliases:"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{aiHeading "Examples:"}}
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

{{aiHeading "Available Commands:"}}{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{aiCmd (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{aiHeading "Flags:"}}
{{aiFlags (.LocalFlags.FlagUsages | trimTrailingWhitespaces)}}{{end}}{{if .HasAvailableInheritedFlags}}

{{aiHeading "Global Flags:"}}
{{aiFlags (.InheritedFlags.FlagUsages | trimTrailingWhitespaces)}}{{end}}{{if .HasHelpSubCommands}}

{{aiHeading "Additional help topics:"}}{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{aiCmd (rpad .CommandPath .CommandPathPadding)}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

{{aiHint (printf "Use \"%s [command] --help\" for more information about a command." .CommandPath)}}{{end}}
`

// styledHelpTemplate renders a command's description then its (styled) usage. The
// short tagline of a parent command (the root's "ai — …") is shown in the heading
// style; a subcommand's longer prose stays plain for readability.
const styledHelpTemplate = `{{with (or .Long .Short)}}{{if not $.HasParent}}{{aiHeading (trimTrailingWhitespaces .)}}{{else}}{{trimTrailingWhitespaces .}}{{end}}

{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

// applyStyledHelp registers the palette template funcs and installs the styled
// usage/help templates on cmd. Subcommands inherit a parent's templates, so calling
// it once on the root command styles the whole `--help` surface.
func applyStyledHelp(cmd *cobra.Command) {
	cobra.AddTemplateFuncs(template.FuncMap{
		"aiHeading": func(text string) string { return ui.Heading.Render(text) },
		"aiCmd":     func(text string) string { return ui.Primary.Render(text) },
		"aiHint":    func(text string) string { return ui.Muted.Render(text) },
		"aiFlags": func(block string) string {
			return flagTokenRe.ReplaceAllStringFunc(block, func(token string) string {
				return ui.Value.Render(token)
			})
		},
	})
	cmd.SetUsageTemplate(styledUsageTemplate)
	cmd.SetHelpTemplate(styledHelpTemplate)
}
