package cli

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

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
	command.SetArgs([]string{"--name", "demo", "--os", "ubuntu", "--agents", "codex,gemini", "--stacks", "go"})

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
