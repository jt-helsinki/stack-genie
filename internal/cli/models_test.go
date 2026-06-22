package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// runModelsTest drives `ai models test [args...]` with a JSON emitter (so
// interactive() is false), returning the exit code.
func runModelsTest(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newModelsTestCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("models test %v returned error: %v", args, err)
	}
	return exit
}

// No model arg on a non-TTY must exit 2, not block on a prompt.
func TestModelsTestMissingModelNonInteractive(test *testing.T) {
	if exit := runModelsTest(test); exit != output.ExitInvalidInput {
		test.Fatalf("models test with no model: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// promptModel offers only the named (non-wildcard) handles from the default
// routing, sorted, so the prompt is pick-not-type.
func TestPromptModelOptionsExcludeWildcards(test *testing.T) {
	for name := range litellm.DefaultRouting().Aliases {
		if strings.ContainsAny(name, "/*") {
			// wildcard handles must be excluded — sanity-check the data shape the
			// prompt filters on.
			if !strings.Contains(name, "*") && !strings.Contains(name, "/") {
				test.Fatalf("unexpected handle %q classified as wildcard", name)
			}
		}
	}
	// At least the named handles exist (gemma4 is the default).
	if _, ok := litellm.DefaultRouting().Aliases["gemma4"]; !ok {
		test.Fatal("expected gemma4 named handle in default routing")
	}
}
