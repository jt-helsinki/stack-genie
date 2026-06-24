package cli

import (
	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// completer is the shell-completion function shape cobra expects for both
// ValidArgsFunction and RegisterFlagCompletionFunc.
type completer func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)

// completeProjectNames offers the known project names (from the global index).
// Used for the --project flag and the optional [project] positional.
func completeProjectNames(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	entries, err := project.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeProjectArg completes only the first positional with project names.
func completeProjectArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeProjectNames(cmd, args, toComplete)
}

// fixedValues completes from a fixed set (enum-style arguments).
func fixedValues(values ...string) completer {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeOptionalProjectThenValue handles `[project] <value>`: at the first
// position it offers the enum values and the project names (the project is
// optional, so either may come first); at the second position only the values.
func completeOptionalProjectThenValue(values []string) completer {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			names, _ := completeProjectNames(cmd, args, toComplete)
			return append(append([]string{}, values...), names...), cobra.ShellCompDirectiveNoFileComp
		case 1:
			return values, cobra.ShellCompDirectiveNoFileComp
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
}

// completeAgentArg completes `<cli> [project]`: the agent CLI names at the first
// position, project names at the second (the optional trailing [project]).
func completeAgentArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return workspace.AgentCLINames(), cobra.ShellCompDirectiveNoFileComp
	case 1:
		return completeProjectNames(cmd, args, toComplete)
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeServiceNames offers "all" plus the host service names (for services
// start/stop/restart).
func completeServiceNames(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return append([]string{"all"}, setup.ServiceNames()...), cobra.ShellCompDirectiveNoFileComp
}

// completeConsoleServices offers the host services that have an admin console.
func completeConsoleServices(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	var names []string
	for _, namedURL := range console.WithConsoles() {
		names = append(names, namedURL.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
