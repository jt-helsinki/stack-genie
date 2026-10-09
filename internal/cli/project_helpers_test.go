package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

func TestCreatePlan(test *testing.T) {
	spec := project.Spec{
		Name:      "app",
		AgentCLIs: []string{"opencode", "codex"},
	}
	plan := createPlan(spec, "/tmp/app")
	joined := strings.Join(plan, "\n")
	for _, want := range []string{"/tmp/app", "opencode", "codex", "app"} {
		if !strings.Contains(joined, want) {
			test.Fatalf("createPlan missing %q:\n%s", want, joined)
		}
	}
}

func TestDeletePlan(test *testing.T) {
	keep := deletePlan("app", "/tmp/app", false)
	joinedKeep := strings.Join(keep, "\n")
	if !strings.Contains(joinedKeep, ".ai-platform") || !strings.Contains(joinedKeep, "agent config folders") {
		test.Fatalf("non-purge plan should keep files + list agent dirs:\n%s", joinedKeep)
	}

	purge := deletePlan("app", "/tmp/app", true)
	joinedPurge := strings.Join(purge, "\n")
	if !strings.Contains(joinedPurge, "whole directory /tmp/app") {
		test.Fatalf("purge plan should remove the whole directory:\n%s", joinedPurge)
	}
	if strings.Contains(joinedPurge, "agent config folders") {
		test.Fatalf("purge plan should not mention agent dirs (whole dir goes):\n%s", joinedPurge)
	}
}

func TestMapProjectErr(test *testing.T) {
	// A pre-coded *output.Error passes through unchanged.
	coded := output.Errorf(output.ExitPermission, "denied")
	mapped := mapProjectErr(coded)
	var platformErr *output.Error
	if !errors.As(mapped, &platformErr) || platformErr.Code != output.ExitPermission {
		test.Fatalf("output.Error should pass through: %v", mapped)
	}

	// Known project sentinels map to invalid input (exit 2).
	for _, sentinel := range []error{project.ErrInvalidName, project.ErrAlreadyExists, project.ErrUnknownProject} {
		mapped := mapProjectErr(sentinel)
		if !errors.As(mapped, &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Fatalf("%v should map to invalid input, got %v", sentinel, mapped)
		}
	}

	// Anything else is a runtime failure (exit 4).
	mapped = mapProjectErr(errors.New("boom"))
	if !errors.As(mapped, &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("generic error should map to runtime failure, got %v", mapped)
	}
}

func TestDefaultProjectName(test *testing.T) {
	if got := defaultProjectName([]string{"explicit"}, "/some/location"); got != "explicit" {
		test.Fatalf("explicit arg should win: %q", got)
	}
	// An explicit --location's basename wins over the cwd, so the name defaults to
	// the WORKSPACE's own directory rather than wherever `ai create` happens to run.
	if got := defaultProjectName(nil, "/tmp/My Location"); got != "my-location" {
		test.Fatalf("location-derived name = %q, want my-location", got)
	}
	child := filepath.Join(test.TempDir(), "My Repo")
	if err := os.MkdirAll(child, 0o755); err != nil {
		test.Fatal(err)
	}
	test.Chdir(child)
	if got := defaultProjectName(nil, ""); got != "my-repo" {
		test.Fatalf("cwd-derived name = %q, want my-repo", got)
	}
}

func TestAiToolsFromSpecAndOptions(test *testing.T) {
	spec := project.Spec{CavemanEnabled: true, CodeReviewGraphEnabled: true}
	tools := aiToolsFromSpec(spec)
	if !slices.Contains(tools, create.AIToolCaveman) || !slices.Contains(tools, create.AIToolCodeReviewGraph) {
		test.Fatalf("aiToolsFromSpec = %v, want caveman + code-review-graph", tools)
	}
	if slices.Contains(tools, create.AIToolGraphify) {
		test.Fatalf("graphify should not appear when disabled: %v", tools)
	}
	if got := len(aiToolOptions()); got != 9 {
		test.Fatalf("aiToolOptions len = %d, want 9", got)
	}
}

func TestSeededAuthModeAndCollect(test *testing.T) {
	if seededAuthMode(map[string]string{"codex": "oauth"}, "codex") != "oauth" {
		test.Fatal("oauth mode should be preserved")
	}
	if seededAuthMode(map[string]string{}, "codex") != "api-key" {
		test.Fatal("default should be api-key")
	}

	modes := collectAuthModes([]string{"claude-code", "opencode"}, map[string]string{"claude-code": "oauth"})
	if modes["claude-code"] != "oauth" {
		test.Fatalf("collectAuthModes should keep the selected OAuth-capable CLI: %v", modes)
	}
	if _, ok := modes["opencode"]; ok {
		test.Fatalf("opencode is not OAuth-capable, should be absent: %v", modes)
	}
	if got := collectAuthModes([]string{"opencode"}, nil); got != nil {
		test.Fatalf("no OAuth-capable CLI selected should yield nil, got %v", got)
	}
}

// The list command lists workspaces from the global index (exit 0, even empty).
func TestNewListCmdEmpty(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newListCmd(emitter, &exit, "list")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("list error: %v", err)
	}
	if exit != output.ExitOK {
		test.Fatalf("list exit = %d, want 0", exit)
	}
}

// Deleting an unknown workspace is invalid input (exit 2).
func TestNewDeleteCmdUnknownProject(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newDeleteCmd(emitter, &exit, "delete")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"ghost"})
	if err := cmd.Execute(); err != nil {
		test.Fatalf("delete error: %v", err)
	}
	if exit != output.ExitInvalidInput {
		test.Fatalf("delete unknown project exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// Deleting outside any workspace (no resolvable name) is invalid input (exit 2).
func TestNewDeleteCmdNoResolution(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newDeleteCmd(emitter, &exit, "delete")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("delete error: %v", err)
	}
	if exit != output.ExitInvalidInput {
		test.Fatalf("delete with no resolvable workspace exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// A seeded workspace deletion without --yes and non-interactively refuses the
// destructive action (exit 2), leaving the project intact.
func TestNewDeleteCmdRequiresYesNonInteractive(test *testing.T) {
	seedProjectAt(test, "app")
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newDeleteCmd(emitter, &exit, "delete")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"app"})
	if err := cmd.Execute(); err != nil {
		test.Fatalf("delete error: %v", err)
	}
	if exit != output.ExitInvalidInput {
		test.Fatalf("destructive delete without --yes exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}
