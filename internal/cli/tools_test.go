package cli

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

// runTools drives a single `ai tools <args...>` invocation against a fresh command
// tree, returning the exit code the command set.
func runTools(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newToolsCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("tools %v returned error: %v", args, err)
	}
	return exit
}

// seedToolsProject registers a project with graphify pre-enabled, caveman
// pre-disabled, so toggle tests have a known baseline.
func seedToolsProject(test *testing.T, project string) string {
	test.Helper()
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := filepath.Join(home, "projects", project)
	graphifyEnabled := true
	cavemanEnabled := false
	settings := config.Config{
		Agent: config.AgentConfig{Tools: []string{"opencode"}},
		Context: config.ContextConfig{
			GraphifyEnabled: &graphifyEnabled,
			CavemanEnabled:  &cavemanEnabled,
		},
	}
	if err := config.WriteProject(root, &settings); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects[project] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
	return root
}

func TestToolsList(test *testing.T) {
	seedToolsProject(test, "demo")
	if exit := runTools(test, "list", "demo"); exit != output.ExitOK {
		test.Fatalf("tools list exit = %d, want %d", exit, output.ExitOK)
	}
}

func TestToolsEnableUnknownTool(test *testing.T) {
	seedToolsProject(test, "demo")
	if exit := runTools(test, "enable", "bogus-tool", "demo"); exit != output.ExitInvalidInput {
		test.Fatalf("unknown tool exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestToolsEnableDisableRoundTrip(test *testing.T) {
	root := seedToolsProject(test, "demo")

	if exit := runTools(test, "enable", "tokensave", "demo"); exit != output.ExitOK {
		test.Fatalf("enable tokensave exit = %d, want %d", exit, output.ExitOK)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if !projectConfig.Context.TokenSaveEnabledOrDefault() {
		test.Fatal("tokensave should be enabled in config.yaml after `ai tools enable tokensave`")
	}
	// graphify, pre-seeded on, must survive untouched.
	if !projectConfig.Context.GraphifyEnabledOrDefault() {
		test.Fatal("enabling tokensave must not disturb graphify's existing state")
	}

	if exit := runTools(test, "disable", "tokensave", "demo"); exit != output.ExitOK {
		test.Fatalf("disable tokensave exit = %d, want %d", exit, output.ExitOK)
	}
	projectConfig, err = config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if projectConfig.Context.TokenSaveEnabledOrDefault() {
		test.Fatal("tokensave should be disabled in config.yaml after `ai tools disable tokensave`")
	}
}
