package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// runCreate drives `ai create` non-interactively (no TTY → flag-driven) and returns
// the exit code.
func runCreate(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exitCode := output.ExitOK
	command := newCreateCmd(emitter, &exitCode, "create [name]")
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		test.Fatalf("create returned error: %v", err)
	}
	return exitCode
}

// TestCreatePersistsShellAndLocation: --shell lands in config.yaml, and a missing
// --location directory is created.
func TestCreatePersistsShellAndLocation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	test.Chdir(parent)
	location := filepath.Join(parent, "nested", "ws") // does not exist yet

	exit := runCreate(test, "--name", "demo", "--location", location, "--shell", "zsh")
	if exit != output.ExitOK {
		test.Fatalf("create exit = %d, want %d", exit, output.ExitOK)
	}
	if info, err := os.Stat(location); err != nil || !info.IsDir() {
		test.Fatalf("missing --location directory was not created: %v", err)
	}
	configYAML, err := os.ReadFile(config.ProjectPath(location))
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Contains(configYAML, []byte("shell: zsh")) {
		test.Errorf("config.yaml missing shell: zsh:\n%s", configYAML)
	}
}

// TestCreateRejectsNestedLocation: a workspace may not be created inside another.
func TestCreateRejectsNestedLocation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	test.Chdir(parent)

	if exit := runCreate(test, "--name", "outer", "--location", parent); exit != output.ExitOK {
		test.Fatalf("first create exit = %d, want OK", exit)
	}
	nested := filepath.Join(parent, "inside")
	if exit := runCreate(test, "--name", "inner", "--location", nested); exit == output.ExitOK {
		test.Fatal("creating a workspace nested inside another must fail")
	}
}

// TestCreateRejectsUnknownAgent: an unknown --agents value is exit 2.
func TestCreateRejectsUnknownAgent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	test.Chdir(parent)
	if exit := runCreate(test, "--name", "demo", "--agents", "not-a-real-cli"); exit != output.ExitInvalidInput {
		test.Fatalf("unknown --agents exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestCreateHumanOutputMatchesPersistedConfig(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	projectRoot := test.TempDir()
	test.Chdir(projectRoot)

	var stdout bytes.Buffer
	emitter := &output.Emitter{Out: &stdout, Err: io.Discard}
	exitCode := output.ExitOK
	command := newCreateCmd(emitter, &exitCode, "create [name]")
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--name", "demo", "--agents", "codex,gemini"})

	if err := command.Execute(); err != nil {
		test.Fatalf("create returned error: %v", err)
	}
	if exitCode != output.ExitOK {
		test.Fatalf("create exit = %d, want %d", exitCode, output.ExitOK)
	}

	persistedConfig, err := os.ReadFile(config.ProjectPath(projectRoot))
	if err != nil {
		test.Fatal(err)
	}
	if stdout.String() != string(persistedConfig) {
		test.Fatalf("stdout must match config.yaml exactly\nstdout:\n%s\nconfig.yaml:\n%s", stdout.String(), string(persistedConfig))
	}
	if !bytes.Contains(persistedConfig, []byte("default_tool: codex")) {
		test.Fatalf("config.yaml missing selected default agent CLI:\n%s", string(persistedConfig))
	}
}
