// Package cli wires the `ai` command tree (CLI spec). Commands stay thin:
// they parse flags, call a package, and render via internal/output.
package cli

import (
	"fmt"
	"os"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/version"
	"github.com/spf13/cobra"
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
	g := &globalFlags{}
	em := &output.Emitter{Out: os.Stdout, Err: os.Stderr}
	exit := output.ExitOK

	root := &cobra.Command{
		Use:   "ai",
		Short: "Ideal Robit AI Development Platform CLI",
		Long:  "Ideal Robot — reproducible, isolated AI development environments.",
		// No positional args at the root: `ai <unknown>` must exit 2 (§17.0).
		// Once subcommands are registered, cobra resolves them first and this
		// still rejects any unrecognized token.
		Args:          cobra.NoArgs,
		SilenceUsage:  true, // we render errors ourselves (§19)
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			em.JSON = g.json
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.version {
				if g.json {
					exit = em.Success("version", versionData())
				} else {
					_, _ = fmt.Fprintf(em.Out, "ai version %s\n", version.Version)
				}
				return nil
			}
			// `ai` with no subcommand prints help and exits 0 (§17.0).
			return cmd.Help()
		},
	}

	pf := root.PersistentFlags()
	pf.BoolVar(&g.json, "json", false, "machine-readable output (CLI spec §19)")
	pf.BoolVar(&g.verbose, "verbose", false, "extra human-readable detail (ignored with --json)")
	pf.BoolVar(&g.dryRun, "dry-run", false, "compute and print planned actions; mutate nothing")
	pf.StringVar(&g.project, "project", "", "scope the command to a project")
	pf.BoolVar(&g.yes, "yes", false, `assume "yes" for destructive confirmation prompts`)
	root.Flags().BoolVar(&g.version, "version", false, "print version and exit")

	// Subcommand groups. More (project/workspace/agent/secrets/...) are
	// registered here in later milestones.
	root.AddCommand(
		newSetupCmd(em, &exit),
		newServicesCmd(em, &exit),
		newStateCmd(em, &exit),
	)

	root.SetArgs(os.Args[1:])
	if err := root.Execute(); err != nil {
		// Arg/flag errors abort before PersistentPreRun runs, so mirror the
		// --json flag here to honor the requested output mode (§19).
		em.JSON = g.json
		// Unknown command / bad flag → invalid input (exit 2, §18). The error
		// kind is derived from the code.
		exit = em.Failure("ai", output.Errorf(output.ExitInvalidInput, "%s", err.Error()))
	}
	return exit
}

// versionData is the data payload for `ai --version --json`.
func versionData() map[string]any {
	return map[string]any{
		"name":    "ai",
		"version": version.Version,
	}
}
