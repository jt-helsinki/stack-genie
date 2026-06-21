package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/spf13/cobra"
)

// TestCwdProjectNameResolvesFromAncestor confirms the cwd-only resolution used
// by `ai start|stop|restart` walks up to the `.ai-platform` workspace root.
func TestCwdProjectNameResolvesFromAncestor(test *testing.T) {
	root := seedProjectAt(test, "app")
	sub := filepath.Join(root, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		test.Fatal(err)
	}
	test.Chdir(sub)

	name, err := cwdProjectName()
	if err != nil {
		test.Fatalf("cwdProjectName error: %v", err)
	}
	if name != "app" {
		test.Fatalf("cwdProjectName = %q, want app", name)
	}
}

// TestCwdProjectNameOutsideWorkspaceIsExit2 confirms that running outside any
// workspace (no `.ai-platform` ancestor) is an invalid-input error at exit 2.
func TestCwdProjectNameOutsideWorkspaceIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())

	_, err := cwdProjectName()
	if err == nil {
		test.Fatal("expected an error when not inside a workspace")
	}
	var platformErr *output.Error
	if !errors.As(err, &platformErr) {
		test.Fatalf("error is not an *output.Error: %T", err)
	}
	if platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("exit code = %d, want %d (invalid input)", platformErr.Code, output.ExitInvalidInput)
	}
}

// TestLifecycleCommandsResolveCwdAndDelegate drives the top-level commands from
// inside a seeded project. Resolution succeeds (the cwd-only path), then the
// real Manager runs; on this non-Apple-Silicon host the microVM seam is deferred
// to hardware bring-up (ErrPending), so the command fails AFTER resolution — it
// must NOT fail with the exit-2 "not inside a workspace" error. A cwd outside a
// workspace fails at exit 2 before ever reaching the Manager.
func TestLifecycleCommandsResolveCwdAndDelegate(test *testing.T) {
	cases := []struct {
		name    string
		builder func(*output.Emitter, *int) *cobra.Command
	}{
		{"start", newStartCmd},
		{"stop", newStopCmd},
		{"restart", newRestartCmd},
	}

	const notInWorkspace = "not inside a workspace"

	for _, testCase := range cases {
		test.Run("inside_workspace/"+testCase.name, func(test *testing.T) {
			root := seedProjectAt(test, "app")
			test.Chdir(root)

			var stderr bytes.Buffer
			emitter := &output.Emitter{Out: io.Discard, Err: &stderr}
			exit := output.ExitOK
			cmd := testCase.builder(emitter, &exit)
			if err := cmd.RunE(cmd, nil); err != nil {
				test.Fatalf("RunE returned a non-nil error: %v", err)
			}
			// Resolution from the cwd succeeded: the command reached the Manager
			// (where the microVM seam is deferred to hardware bring-up), so it must
			// NOT have failed with the cwd "not inside a workspace" error.
			if strings.Contains(stderr.String(), notInWorkspace) {
				test.Fatalf("%s inside a workspace surfaced the cwd-not-found error: %q", testCase.name, stderr.String())
			}
		})

		test.Run("outside_workspace/"+testCase.name, func(test *testing.T) {
			test.Setenv("HOME", test.TempDir())
			test.Chdir(test.TempDir())

			var stderr bytes.Buffer
			emitter := &output.Emitter{Out: io.Discard, Err: &stderr}
			exit := output.ExitOK
			cmd := testCase.builder(emitter, &exit)
			if err := cmd.RunE(cmd, nil); err != nil {
				test.Fatalf("RunE returned a non-nil error: %v", err)
			}
			if exit != output.ExitInvalidInput {
				test.Fatalf("%s outside a workspace exit = %d, want %d", testCase.name, exit, output.ExitInvalidInput)
			}
			if !strings.Contains(stderr.String(), notInWorkspace) {
				test.Fatalf("%s outside a workspace lacked the actionable message: %q", testCase.name, stderr.String())
			}
		})
	}
}
