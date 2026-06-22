package cli

import (
	"errors"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/spf13/cobra"
)

// newContextCmd builds `ai context` (CLI §9): Headroom strategy + Caveman level.
func newContextCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Inspect and tune context optimization (Headroom + Caveman)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newContextStatusCmd(emitter, exit),
		newContextStrategyCmd(emitter, exit),
		newContextCavemanCmd(emitter, exit),
	)
	return cmd
}

func mapContextErr(err error) error {
	switch {
	case errors.Is(err, contextopt.ErrInvalidStrategy),
		errors.Is(err, contextopt.ErrInvalidCavemanLevel),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// resolveProjectRoot maps a project name to its root via the global index.
func resolveProjectRoot(name string) (string, error) {
	root, exists, err := project.Path(name)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", project.ErrUnknownProject
	}
	return root, nil
}

func newContextStatusCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "status [project]",
		Short:             "Show context-optimization status",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("context.status", err)
				return nil
			}
			root, err := resolveProjectRoot(name)
			if err != nil {
				*exit = emitter.Failure("context.status", mapContextErr(err))
				return nil
			}
			status, err := contextopt.GetStatus(root)
			if err != nil {
				*exit = emitter.Failure("context.status", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("context.status", status)
			return nil
		},
	}
}

func newContextStrategyCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "strategy [project] [conservative|balanced|aggressive]",
		Short: "Set the Headroom input-compression strategy (prompts for it if omitted)",
		Long: "Set the Headroom input-compression strategy for a project. Pass the value as\n" +
			"an argument; or, on a terminal, omit it to be prompted with the choices\n" +
			"(conservative|balanced|aggressive). [project] defaults to the current directory.",
		Args:              cobra.MaximumNArgs(2),
		ValidArgsFunction: completeOptionalProjectThenValue(contextopt.Strategies),
		RunE: func(cmd *cobra.Command, args []string) error {
			explicit, value := splitProjectAndValue(args)
			name, err := resolveProjectName(cmd, explicit)
			if err != nil {
				*exit = emitter.Failure("context.strategy", err)
				return nil
			}
			value, err = resolveContextValue(emitter, value, "Headroom input-compression strategy", contextopt.Strategies)
			if err != nil {
				*exit = emitter.Failure("context.strategy", err)
				return nil
			}
			root, err := resolveProjectRoot(name)
			if err != nil {
				*exit = emitter.Failure("context.strategy", mapContextErr(err))
				return nil
			}
			if err := contextopt.SetStrategy(root, value); err != nil {
				*exit = emitter.Failure("context.strategy", mapContextErr(err))
				return nil
			}
			*exit = emitter.Success("context.strategy", map[string]any{"project": name, "strategy": value})
			return nil
		},
	}
}

func newContextCavemanCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "caveman [project] [lite|full|ultra|wenyan]",
		Short: "Set the Caveman output-compression level (prompts for it if omitted)",
		Long: "Set the Caveman output-compression level for a project (reinstalls the skill).\n" +
			"Pass the value as an argument; or, on a terminal, omit it to be prompted with\n" +
			"the choices (lite|full|ultra|wenyan). [project] defaults to the current directory.",
		Args:              cobra.MaximumNArgs(2),
		ValidArgsFunction: completeOptionalProjectThenValue(contextopt.CavemanLevels),
		RunE: func(cmd *cobra.Command, args []string) error {
			explicit, value := splitProjectAndValue(args)
			name, err := resolveProjectName(cmd, explicit)
			if err != nil {
				*exit = emitter.Failure("context.caveman", err)
				return nil
			}
			value, err = resolveContextValue(emitter, value, "Caveman output-compression level", contextopt.CavemanLevels)
			if err != nil {
				*exit = emitter.Failure("context.caveman", err)
				return nil
			}
			root, err := resolveProjectRoot(name)
			if err != nil {
				*exit = emitter.Failure("context.caveman", mapContextErr(err))
				return nil
			}
			if err := contextopt.SetCavemanLevel(root, value); err != nil {
				*exit = emitter.Failure("context.caveman", mapContextErr(err))
				return nil
			}
			*exit = emitter.Success("context.caveman", map[string]any{"project": name, "level": value})
			return nil
		},
	}
}

// splitProjectAndValue interprets `[project] [value]`: two args are
// project+value, one arg is just the value (project comes from --project/CWD),
// and zero args leaves both empty (the value is prompted/required).
func splitProjectAndValue(args []string) (explicit, value string) {
	switch len(args) {
	case 2:
		return args[0], args[1]
	case 1:
		return "", args[0]
	default:
		return "", ""
	}
}

// resolveContextValue returns value when it was given on the command line; when
// it was omitted it prompts the user to pick from allowed on a TTY, or — when not
// interactive — fails with exit 2 naming the allowed choices (§21). The label
// titles the prompt and the not-a-TTY error.
func resolveContextValue(emitter *output.Emitter, value, label string, allowed []string) (string, error) {
	if value != "" {
		return value, nil
	}
	if !interactive(emitter) {
		return "", output.Errorf(output.ExitInvalidInput,
			"%s required (one of %s)", label, strings.Join(allowed, "|"))
	}
	options := make([]huh.Option[string], 0, len(allowed))
	for _, choice := range allowed {
		options = append(options, huh.NewOption(choice, choice))
	}
	return promptChoice(label, "", options, allowed[0])
}
