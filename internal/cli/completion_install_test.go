package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/spf13/cobra"
)

// isolateCompletionHome points HOME at a temp dir and clears the XDG overrides so
// configHome/dataHome resolve deterministically under it.
func isolateCompletionHome(test *testing.T) string {
	test.Helper()
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("XDG_CONFIG_HOME", "")
	test.Setenv("XDG_DATA_HOME", "")
	return home
}

func TestConfigHomeAndDataHome(test *testing.T) {
	home := test.TempDir()
	test.Setenv("XDG_CONFIG_HOME", "")
	test.Setenv("XDG_DATA_HOME", "")
	if got := configHome(home); got != filepath.Join(home, ".config") {
		test.Fatalf("configHome default = %q", got)
	}
	if got := dataHome(home); got != filepath.Join(home, ".local", "share") {
		test.Fatalf("dataHome default = %q", got)
	}

	test.Setenv("XDG_CONFIG_HOME", "/custom/cfg")
	test.Setenv("XDG_DATA_HOME", "/custom/data")
	if got := configHome(home); got != "/custom/cfg" {
		test.Fatalf("configHome with XDG = %q", got)
	}
	if got := dataHome(home); got != "/custom/data" {
		test.Fatalf("dataHome with XDG = %q", got)
	}
}

func TestGenerateCompletionAllShells(test *testing.T) {
	root := &cobra.Command{Use: "ai"}
	for _, shell := range completionShells {
		script, err := generateCompletion(root, shell)
		if err != nil {
			test.Fatalf("generateCompletion(%q) error: %v", shell, err)
		}
		if len(script) == 0 {
			test.Fatalf("generateCompletion(%q) produced no script", shell)
		}
	}
}

func TestInstallCompletionFishAndBash(test *testing.T) {
	home := isolateCompletionHome(test)
	for _, testCase := range []struct {
		shell string
		want  string
	}{
		{"fish", filepath.Join(home, ".config", "fish", "completions", "ai.fish")},
		{"bash", filepath.Join(home, ".local", "share", "bash-completion", "completions", "ai")},
	} {
		path, hint, err := installCompletion(testCase.shell, []byte("# script"))
		if err != nil {
			test.Fatalf("installCompletion(%q) error: %v", testCase.shell, err)
		}
		if path != testCase.want {
			test.Fatalf("installCompletion(%q) path = %q, want %q", testCase.shell, path, testCase.want)
		}
		if hint == "" {
			test.Fatalf("installCompletion(%q) returned no hint", testCase.shell)
		}
		if _, statErr := os.Stat(path); statErr != nil {
			test.Fatalf("installCompletion(%q) did not write %q: %v", testCase.shell, path, statErr)
		}
	}
}

func TestInstallCompletionZshWiresRc(test *testing.T) {
	home := isolateCompletionHome(test)
	path, _, err := installCompletion("zsh", []byte("# script"))
	if err != nil {
		test.Fatalf("installCompletion(zsh) error: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		test.Fatalf("zsh completion not written: %v", statErr)
	}
	rc, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		test.Fatalf("reading .zshrc: %v", err)
	}
	if !bytes.Contains(rc, []byte(completionMarker)) {
		test.Fatalf(".zshrc missing the managed completion block:\n%s", rc)
	}
}

func TestInstallCompletionPowershellWiresProfile(test *testing.T) {
	home := isolateCompletionHome(test)
	path, _, err := installCompletion("powershell", []byte("# script"))
	if err != nil {
		test.Fatalf("installCompletion(powershell) error: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		test.Fatalf("powershell script not written: %v", statErr)
	}
	profile := filepath.Join(home, ".config", "powershell", "Microsoft.PowerShell_profile.ps1")
	data, err := os.ReadFile(profile)
	if err != nil {
		test.Fatalf("reading powershell profile: %v", err)
	}
	if !bytes.Contains(data, []byte(completionMarker)) {
		test.Fatalf("powershell profile missing managed block:\n%s", data)
	}
}

func TestInstallCompletionUnsupportedShell(test *testing.T) {
	isolateCompletionHome(test)
	if _, _, err := installCompletion("tcsh", []byte("x")); err == nil {
		test.Fatal("installCompletion(tcsh) should error on an unsupported shell")
	}
}

func TestAppendManagedIsIdempotent(test *testing.T) {
	dir := test.TempDir()
	rc := filepath.Join(dir, ".somerc")
	block := completionMarker + "\nexport X=1\n"

	if err := appendManaged(rc, block); err != nil {
		test.Fatalf("first appendManaged: %v", err)
	}
	if err := appendManaged(rc, block); err != nil {
		test.Fatalf("second appendManaged: %v", err)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		test.Fatal(err)
	}
	if count := bytes.Count(data, []byte(completionMarker)); count != 1 {
		test.Fatalf("marker appears %d times, want 1 (not idempotent):\n%s", count, data)
	}
}

func TestWriteCompletionFileCreatesDirs(test *testing.T) {
	path := filepath.Join(test.TempDir(), "nested", "deep", "ai")
	if err := writeCompletionFile(path, []byte("data")); err != nil {
		test.Fatalf("writeCompletionFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "data" {
		test.Fatalf("file content = %q err=%v", data, err)
	}
}

func TestCompletionResultHuman(test *testing.T) {
	result := completionResult{Shell: "bash", InstalledPath: "/tmp/ai", Note: "restart bash"}
	human := result.Human()
	for _, want := range []string{"bash", "/tmp/ai", "restart bash"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}

// `ai completion <shell> --print` writes the script to stdout at exit 0 without
// touching disk.
func TestCompletionCommandPrint(test *testing.T) {
	isolateCompletionHome(test)
	var out bytes.Buffer
	emitter := &output.Emitter{Out: &out, Err: io.Discard}
	exit := output.ExitOK
	cmd := newCompletionCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"bash", "--print"})
	if err := cmd.Execute(); err != nil {
		test.Fatalf("completion --print error: %v", err)
	}
	if exit != output.ExitOK {
		test.Fatalf("completion --print exit = %d, want 0", exit)
	}
	if out.Len() == 0 {
		test.Fatal("completion --print produced no output")
	}
}

// Non-interactive `ai completion` with no shell arg is invalid input (exit 2).
func TestCompletionCommandNoShellIsExit2(test *testing.T) {
	isolateCompletionHome(test)
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newCompletionCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("completion (no shell) error: %v", err)
	}
	if exit != output.ExitInvalidInput {
		test.Fatalf("completion with no shell exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}
