// Package cli wires the `ai` command tree (CLI spec). Commands stay thin:
// they parse flags, call a package, and render via internal/output.
package cli

import (
	"fmt"
	"os"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// globalFlags holds the flags accepted by every command (CLI spec §17).
type globalFlags struct {
	json    bool
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
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			emitter.JSON = flags.json
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
	persistentFlags.BoolVar(&flags.verbose, "verbose", false, "extra human-readable detail (ignored with --json)")
	persistentFlags.BoolVar(&flags.dryRun, "dry-run", false, "compute and print planned actions; mutate nothing")
	persistentFlags.StringVar(&flags.project, "project", "", "scope the command to a project")
	persistentFlags.BoolVar(&flags.yes, "yes", false, `assume "yes" for destructive confirmation prompts`)
	root.Flags().BoolVar(&flags.version, "version", false, "print version and exit")

	// Subcommand groups (the full Slice 1 surface, CLI §17.0).
	root.AddCommand(
		newSetupCmd(emitter, &exitCode),
		newProjectCmd(emitter, &exitCode),
		newServicesCmd(emitter, &exitCode),
		newSecretsCmd(emitter, &exitCode),
		newModelsCmd(emitter, &exitCode),
		newContextCmd(emitter, &exitCode),
		newWorkspaceCmd(emitter, &exitCode),
		newDoctorCmd(emitter, &exitCode),
		newLogsCmd(emitter, &exitCode),
		newStateCmd(emitter, &exitCode),
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
	if err := root.Execute(); err != nil {
		// Arg/flag errors abort before PersistentPreRun runs, so mirror the
		// --json flag here to honor the requested output mode (§19).
		emitter.JSON = flags.json
		// Unknown command / bad flag → invalid input (exit 2, §18). The error
		// kind is derived from the code.
		exitCode = emitter.Failure("ai", output.Errorf(output.ExitInvalidInput, "%s", err.Error()))
	}
	return exitCode
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
		if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
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
