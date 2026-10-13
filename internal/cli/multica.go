package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// splitMulticaExecArgs splits the shim's `<cli> -- <agent args...>` argv. The `--` is
// mandatory so the agent's own flags can never be mistaken for ours.
func splitMulticaExecArgs(args []string) (cli string, agentArgs []string, err error) {
	if len(args) < 2 || args[1] != "--" {
		return "", nil, errors.New("usage: ai multica-exec <cli> -- [agent args...]")
	}
	return args[0], args[2:], nil
}

// newMulticaExecCmd builds the hidden `ai multica-exec <cli> -- args…`: the target of
// every Multica shim (see internal/multica.WriteShims). It runs the real agent CLI
// for a Multica task inside nono. Unlike every other command it must NOT emit the
// JSON/human envelope — stdout belongs to the agent, whose stdio the daemon parses —
// so failures go to stderr and the agent's own exit code is propagated verbatim.
func newMulticaExecCmd(exit *int) *cobra.Command {
	return &cobra.Command{
		Use:    "multica-exec <cli> -- [agent args...]",
		Short:  "Run an agent CLI for a Multica task inside the nono sandbox (used by Multica shims)",
		Hidden: true,
		// The agent's flags (-p, --model, …) must reach it untouched.
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			cli, agentArgs, err := splitMulticaExecArgs(args)
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				*exit = output.ExitInvalidInput
				return nil
			}
			workdir, err := os.Getwd()
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				*exit = output.ExitRuntimeFailure
				return nil
			}
			if err := workspace.RealManager(nowRFC3339).RunMulticaTask(cli, workdir, agentArgs); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
					*exit = exitErr.ExitCode()
					return nil
				}
				_, _ = fmt.Fprintln(os.Stderr, "ai multica-exec:", err)
				*exit = output.ExitRuntimeFailure
			}
			return nil
		},
	}
}
