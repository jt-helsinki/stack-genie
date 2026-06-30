package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
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

// TestCreatePersistsResourcesPortsAndLocation: --cpus/--memory/--ports land in
// config.yaml, and a missing --location directory is created.
func TestCreatePersistsResourcesPortsAndLocation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	test.Chdir(parent)
	location := filepath.Join(parent, "nested", "ws") // does not exist yet

	exit := runCreate(test, "--name", "demo", "--os", "ubuntu", "--location", location,
		"--cpus", "1", "--memory", "2G", "--ports", "8080,9000:3000")
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
	for _, want := range []string{"cpu_limit: 1", "memory_limit: 2G", "publish_ports:", "host: 8080", "guest: 3000", "host: 9000"} {
		if !bytes.Contains(configYAML, []byte(want)) {
			test.Errorf("config.yaml missing %q:\n%s", want, configYAML)
		}
	}
}

// TestCreateRejectsNestedLocation: a workspace may not be created inside another.
func TestCreateRejectsNestedLocation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	test.Chdir(parent)

	if exit := runCreate(test, "--name", "outer", "--os", "ubuntu", "--location", parent); exit != output.ExitOK {
		test.Fatalf("first create exit = %d, want OK", exit)
	}
	nested := filepath.Join(parent, "inside")
	if exit := runCreate(test, "--name", "inner", "--os", "ubuntu", "--location", nested); exit == output.ExitOK {
		test.Fatal("creating a workspace nested inside another must fail")
	}
}

// TestCreateRejectsOverHostCPUs: a CPU request beyond the host is rejected (exit 2).
func TestCreateRejectsOverHostCPUs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	test.Chdir(parent)
	if exit := runCreate(test, "--name", "demo", "--os", "ubuntu", "--cpus", "100000"); exit != output.ExitInvalidInput {
		test.Fatalf("over-host --cpus exit = %d, want %d", exit, output.ExitInvalidInput)
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
