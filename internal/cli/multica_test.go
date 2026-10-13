package cli

import (
	"reflect"
	"testing"
)

func TestSplitMulticaExecArgs(test *testing.T) {
	cli, agentArgs, err := splitMulticaExecArgs([]string{"claude-code", "--", "-p", "--model", "x"})
	if err != nil || cli != "claude-code" || !reflect.DeepEqual(agentArgs, []string{"-p", "--model", "x"}) {
		test.Errorf("got %q %v %v", cli, agentArgs, err)
	}
	// The agent's own `--` after ours must survive verbatim.
	_, agentArgs, _ = splitMulticaExecArgs([]string{"codex", "--", "exec", "--", "x"})
	if !reflect.DeepEqual(agentArgs, []string{"exec", "--", "x"}) {
		test.Errorf("inner -- lost: %v", agentArgs)
	}
	for _, bad := range [][]string{nil, {"claude-code"}, {"claude-code", "-p"}} {
		if _, _, err := splitMulticaExecArgs(bad); err == nil {
			test.Errorf("expected a usage error for %v", bad)
		}
	}
}
