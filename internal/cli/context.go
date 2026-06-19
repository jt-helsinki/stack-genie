package cli

import (
	"errors"

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
		Use:   "status <project>",
		Short: "Show context-optimization status",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := resolveProjectRoot(args[0])
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
		Use:   "strategy <project> <conservative|balanced|aggressive>",
		Short: "Set the Headroom input-compression strategy",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := resolveProjectRoot(args[0])
			if err != nil {
				*exit = emitter.Failure("context.strategy", mapContextErr(err))
				return nil
			}
			if err := contextopt.SetStrategy(root, args[1]); err != nil {
				*exit = emitter.Failure("context.strategy", mapContextErr(err))
				return nil
			}
			*exit = emitter.Success("context.strategy", map[string]any{"project": args[0], "strategy": args[1]})
			return nil
		},
	}
}

func newContextCavemanCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "caveman <project> <lite|full|ultra|wenyan>",
		Short: "Set the Caveman output-compression level (reinstalls the skill)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := resolveProjectRoot(args[0])
			if err != nil {
				*exit = emitter.Failure("context.caveman", mapContextErr(err))
				return nil
			}
			if err := contextopt.SetCavemanLevel(root, args[1]); err != nil {
				*exit = emitter.Failure("context.caveman", mapContextErr(err))
				return nil
			}
			*exit = emitter.Success("context.caveman", map[string]any{"project": args[0], "level": args[1]})
			return nil
		},
	}
}
