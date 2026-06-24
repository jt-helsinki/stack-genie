// Package cli wires the `ai` command tree (CLI spec). Commands stay thin:
// they parse flags, call a package, and render via internal/output.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// globalFlags holds the flags accepted by every command (CLI spec §17).
type globalFlags struct {
	json    bool
	plain   bool
	verbose bool
	dryRun  bool
	project string
	yes     bool
	version bool
}

// Execute builds the root command, runs it, and returns a process exit code
// (CLI spec §18). main() passes this to os.Exit.
func Execute() int {
	flags := &globalFlags{}
	emitter := &output.Emitter{Out: os.Stdout, Err: os.Stderr}
	exitCode := output.ExitOK

	root := &cobra.Command{
		Use:   "ai",
		Short: "AI Development Platform CLI",
		Long:  "ai — reproducible, isolated AI development environments.",
		// No positional args at the root: `ai <unknown>` must exit 2 (§17.0).
		// Once subcommands are registered, cobra resolves them first and this
		// still rejects any unrecognized token.
		Args:          cobra.NoArgs,
		SilenceUsage:  true, // we render errors ourselves (§19)
		SilenceErrors: true,
		// Replace cobra's print-only `completion` command with our own that
		// installs the script (§1.7). The hidden __complete runtime command,
		// which powers Tab completion, is unaffected.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			emitter.JSON = flags.json
			emitter.Plain = flags.plain
			// Apply the persisted UI theme so every command's prompts, forms, and
			// headings match the user's choice (falls back to the default theme).
			_ = ui.Apply(ui.LoadThemeName())
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.version {
				if flags.json {
					exitCode = emitter.Success("version", versionData())
				} else {
					_, _ = fmt.Fprintf(emitter.Out, "ai version %s\n", version.Version)
				}
				return nil
			}
			// `ai` with no subcommand prints help and exits 0 (§17.0).
			return cmd.Help()
		},
	}

	persistentFlags := root.PersistentFlags()
	persistentFlags.BoolVar(&flags.json, "json", false, "machine-readable output (CLI spec §19)")
	persistentFlags.BoolVar(&flags.plain, "plain", false, "plain, non-interactive human output — no TUI spinners/colour")
	persistentFlags.BoolVar(&flags.verbose, "verbose", false, "extra human-readable detail (ignored with --json)")
	persistentFlags.BoolVar(&flags.dryRun, "dry-run", false, "compute and print planned actions; mutate nothing")
	persistentFlags.StringVar(&flags.project, "project", "", "scope the command to a named workspace")
	persistentFlags.BoolVar(&flags.yes, "yes", false, `assume "yes" for destructive confirmation prompts`)
	root.Flags().BoolVar(&flags.version, "version", false, "print version and exit")

	// Shell completion for --project values (the names from the global index).
	_ = root.RegisterFlagCompletionFunc("project", completeProjectNames)

	// Command surface (CLI §17.0). The canonical surface is FLAT: workspace
	// management is top-level verbs (`ai create`/`ai list`/`ai start`/`ai shell`/
	// …). The `ai workspace …` and `ai project …` groups are kept as HIDDEN
	// back-compat aliases (see newWorkspaceCmd / newProjectCmd) so existing scripts
	// keep working without cluttering help.
	root.AddCommand(
		newSetupCmd(emitter, &exitCode),
		newUninstallCmd(emitter, &exitCode),
		// Canonical flat workspace verbs.
		newCreateCmd(emitter, &exitCode, "create [name]"),
		newListCmd(emitter, &exitCode, "list"),
		newDeleteCmd(emitter, &exitCode, "delete [name]"),
		newStartCmd(emitter, &exitCode),
		newStopCmd(emitter, &exitCode),
		newRestartCmd(emitter, &exitCode),
		newDestroyCmd(emitter, &exitCode),
		newExecCmd(emitter, &exitCode),
		newShellCmd(emitter, &exitCode),
		newAgentCmd(emitter, &exitCode),
		newAttachCmd(emitter, &exitCode),
		newSessionsCmd(emitter, &exitCode),
		// Other top-level commands.
		newServicesCmd(emitter, &exitCode),
		newSecretsCmd(emitter, &exitCode),
		newModelsCmd(emitter, &exitCode),
		newContextCmd(emitter, &exitCode),
		newNetworkCmd(emitter, &exitCode),
		newGatewayCmd(emitter, &exitCode),
		newDoctorCmd(emitter, &exitCode),
		newLogsCmd(emitter, &exitCode),
		newCompletionCmd(emitter, &exitCode),
		newStateCmd(emitter, &exitCode),
		newThemeCmd(emitter, &exitCode),
		newUICmd(emitter, &exitCode),
		// Hidden back-compat alias groups.
		newWorkspaceCmd(emitter, &exitCode),
		newProjectCmd(emitter, &exitCode),
	)

	// With --json, help is a structured data.help object (§17.0); otherwise the
	// default human help is used.
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if flags.json {
			emitter.JSON = true
			exitCode = emitJSONHelp(emitter, cmd)
			return
		}
		defaultHelp(cmd, args)
	})

	root.SetArgs(os.Args[1:])
	// ExecuteC returns the command that failed, so usage errors (wrong args,
	// unknown flag/command) can show that command's own help.
	executedCmd, err := root.ExecuteC()
	if err != nil {
		// Arg/flag errors abort before PersistentPreRun runs, so mirror the
		// --json/--plain flags here to honor the requested output mode (§19).
		emitter.JSON = flags.json
		emitter.Plain = flags.plain
		// Unknown command / bad flag / wrong args → invalid input (exit 2, §18).
		friendly := humanizeUsageError(err.Error())
		if emitter.JSON {
			// JSON consumers get one line: the friendly message + the usage line.
			message := friendly
			if executedCmd != nil {
				message += " (usage: " + executedCmd.UseLine() + ")"
			}
			exitCode = emitter.Failure(usageCommandName(executedCmd), output.Errorf(output.ExitInvalidInput, "%s", message))
		} else {
			// Humans get the friendly message, then the command's full help.
			exitCode = emitter.Failure(usageCommandName(executedCmd), output.Errorf(output.ExitInvalidInput, "%s", friendly))
			if executedCmd != nil {
				_, _ = fmt.Fprintln(emitter.Err)
				_, _ = fmt.Fprint(emitter.Err, executedCmd.UsageString())
			}
		}
	}
	return exitCode
}

// humanizeUsageError rephrases cobra's terse usage errors into something a human
// can act on. Other messages pass through unchanged.
func humanizeUsageError(message string) string {
	// "accepts 1 arg(s), received 0" / "requires at least 1 arg(s), only
	// received 0" → "needs 1 argument(s), received 0".
	replacer := strings.NewReplacer(
		"arg(s)", "argument(s)",
		"accepts ", "needs ",
		"requires ", "needs ",
		"only received", "received",
	)
	return replacer.Replace(message)
}

// usageCommandName names the failing command for the envelope's `command` field,
// e.g. "ai project create" → "project.create"; the root stays "ai".
func usageCommandName(cmd *cobra.Command) string {
	if cmd == nil {
		return "ai"
	}
	path := strings.TrimPrefix(cmd.CommandPath(), "ai ")
	if path == "" || path == "ai" {
		return "ai"
	}
	return strings.ReplaceAll(path, " ", ".")
}

// emitJSONHelp renders a command's help as the structured data.help object
// required by §17.0, and returns ExitOK.
func emitJSONHelp(emitter *output.Emitter, cmd *cobra.Command) int {
	flags := []map[string]any{}
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, map[string]any{
			"name":      flag.Name,
			"shorthand": flag.Shorthand,
			"usage":     flag.Usage,
			"default":   flag.DefValue,
		})
	})
	subcommands := []map[string]any{}
	for _, sub := range cmd.Commands() {
		if sub.Hidden || sub.Name() == "help" {
			continue
		}
		subcommands = append(subcommands, map[string]any{"name": sub.Name(), "short": sub.Short})
	}
	return emitter.Success("help", map[string]any{
		"help": map[string]any{
			"name":        cmd.Name(),
			"path":        cmd.CommandPath(),
			"short":       cmd.Short,
			"long":        cmd.Long,
			"usage":       cmd.UseLine(),
			"flags":       flags,
			"subcommands": subcommands,
		},
	})
}

// versionData is the data payload for `ai --version --json`.
func versionData() map[string]any {
	return map[string]any{
		"name":    "ai",
		"version": version.Version,
	}
}
