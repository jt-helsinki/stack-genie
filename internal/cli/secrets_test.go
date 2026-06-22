package cli

import (
	"io"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// runSecretsSet drives `ai secrets set [args...]` with a JSON emitter (so
// interactive() is false), returning the exit code.
func runSecretsSet(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newSecretsSetCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("secrets set %v returned error: %v", args, err)
	}
	return exit
}

func runSecretsRm(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newSecretsRmCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("secrets rm %v returned error: %v", args, err)
	}
	return exit
}

func runSecretsMap(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newSecretsMapCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("secrets map %v returned error: %v", args, err)
	}
	return exit
}

// `secrets set` with a name but no value/--stdin/--value on a non-TTY must exit
// 2 (without reaching the broker), not block on a prompt.
func TestSecretsSetMissingValueNonInteractive(test *testing.T) {
	if exit := runSecretsSet(test, "OPENAI_API_KEY"); exit != output.ExitInvalidInput {
		test.Fatalf("secrets set with no value: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `secrets set` with neither name nor value on a non-TTY must exit 2.
func TestSecretsSetMissingNameAndValueNonInteractive(test *testing.T) {
	if exit := runSecretsSet(test); exit != output.ExitInvalidInput {
		test.Fatalf("secrets set with no args: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `secrets rm` with no name on a non-TTY must exit 2 (without reaching the
// broker), not block on a prompt.
func TestSecretsRmMissingNameNonInteractive(test *testing.T) {
	if exit := runSecretsRm(test); exit != output.ExitInvalidInput {
		test.Fatalf("secrets rm with no name: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `secrets map` with no name on a non-TTY must exit 2.
func TestSecretsMapMissingNameNonInteractive(test *testing.T) {
	if exit := runSecretsMap(test, "--env", "OPENAI_API_KEY"); exit != output.ExitInvalidInput {
		test.Fatalf("secrets map with no name: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `secrets map` with a name but no --env on a non-TTY must exit 2 (keeping the
// existing "--env is required" semantics), without reaching the broker.
func TestSecretsMapMissingEnvNonInteractive(test *testing.T) {
	if exit := runSecretsMap(test, "MY_SECRET"); exit != output.ExitInvalidInput {
		test.Fatalf("secrets map with no --env: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}
