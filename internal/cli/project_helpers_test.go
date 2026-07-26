package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

func TestOrNoneOrDefault(test *testing.T) {
	if orNone("") != "(none)" || orNone("x") != "x" {
		test.Fatalf("orNone bad: %q %q", orNone(""), orNone("x"))
	}
	if orDefault(0) != "(default)" || orDefault(-1) != "(default)" || orDefault(4) != "4" {
		test.Fatalf("orDefault bad: %q %q %q", orDefault(0), orDefault(-1), orDefault(4))
	}
	if orDefaultStr("") != "(default)" || orDefaultStr("8G") != "8G" {
		test.Fatalf("orDefaultStr bad: %q %q", orDefaultStr(""), orDefaultStr("8G"))
	}
}

func TestFormatPublishPorts(test *testing.T) {
	cases := []struct {
		ports []config.PortMapping
		want  string
	}{
		{nil, ""},
		{[]config.PortMapping{{Host: 8080, Guest: 8080}}, "8080"},
		{[]config.PortMapping{{Host: 8080, Guest: 80}}, "8080:80"},
		{[]config.PortMapping{{Host: 3000, Guest: 3000}, {Host: 5432, Guest: 5000}}, "3000,5432:5000"},
	}
	for _, testCase := range cases {
		if got := formatPublishPorts(testCase.ports); got != testCase.want {
			test.Fatalf("formatPublishPorts(%v) = %q, want %q", testCase.ports, got, testCase.want)
		}
	}
}

func TestWizardAppPortValidator(test *testing.T) {
	if wizardAppPortValidator("") != nil || wizardAppPortValidator("  ") != nil {
		test.Fatal("blank should be valid (auto-assign)")
	}
	if wizardAppPortValidator("8080") != nil {
		test.Fatal("8080 should be valid")
	}
	for _, bad := range []string{"0", "70000", "abc", "-1"} {
		if wizardAppPortValidator(bad) == nil {
			test.Fatalf("port %q should be rejected", bad)
		}
	}
}

func TestSelectedAppPorts(test *testing.T) {
	valid := "8080"
	blank := "  "
	auto := "0"
	values := map[string]*string{"openwebui": &valid, "webui-alt": &blank, "hermes": &auto}
	got := selectedAppPorts([]string{"openwebui", "webui-alt", "hermes", "missing"}, values)
	if got["openwebui"] != 8080 {
		test.Fatalf("openwebui port = %d, want 8080", got["openwebui"])
	}
	if len(got) != 1 {
		test.Fatalf("only the valid entry should survive: %v", got)
	}
}

func TestParsePublishPortsInvalid(test *testing.T) {
	if _, err := parsePublishPorts([]string{"notaport"}); err == nil {
		test.Fatal("non-numeric host port should error")
	}
	if _, err := parsePublishPorts([]string{"8080:99999"}); err == nil {
		test.Fatal("out-of-range guest port should error")
	}
	ports, err := parsePublishPorts([]string{"8080", "3000:80", "  "})
	if err != nil {
		test.Fatalf("valid ports errored: %v", err)
	}
	if len(ports) != 2 || ports[0].Host != 8080 || ports[1].Guest != 80 {
		test.Fatalf("parsePublishPorts result unexpected: %v", ports)
	}
}

func TestCreatePlan(test *testing.T) {
	spec := project.Spec{
		Name:         "app",
		OS:           "debian-trixie",
		Stacks:       []string{"go"},
		AgentCLIs:    []string{"opencode"},
		IdleTimeout:  "10m",
		Apps:         []string{"openwebui"},
		CPUs:         2,
		Memory:       "8G",
		PublishPorts: []config.PortMapping{{Host: 8080, Guest: 80}},
	}
	plan := createPlan(spec, "/tmp/app")
	joined := strings.Join(plan, "\n")
	for _, want := range []string{"/tmp/app", "debian-trixie", "opencode", "8080:80", "10m", "openwebui", "app"} {
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

func TestSanitizeName(test *testing.T) {
	cases := map[string]string{
		"My Project!": "my-project",
		"already-ok":  "already-ok",
		"UPPER_CASE":  "upper-case",
		"--edge--":    "edge",
	}
	for input, want := range cases {
		if got := sanitizeName(input); got != want {
			test.Fatalf("sanitizeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDefaultProjectName(test *testing.T) {
	if got := defaultProjectName([]string{"explicit"}); got != "explicit" {
		test.Fatalf("explicit arg should win: %q", got)
	}
	child := filepath.Join(test.TempDir(), "My Repo")
	if err := os.MkdirAll(child, 0o755); err != nil {
		test.Fatal(err)
	}
	test.Chdir(child)
	if got := defaultProjectName(nil); got != "my-repo" {
		test.Fatalf("cwd-derived name = %q, want my-repo", got)
	}
}

func TestSplitCommaList(test *testing.T) {
	got := splitCommaList(" a, b ,,c ")
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		test.Fatalf("splitCommaList = %v, want [a b c]", got)
	}
	if len(splitCommaList("   ")) != 0 {
		test.Fatal("blank input should yield no items")
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
	if got := len(aiToolOptions()); got != 4 {
		test.Fatalf("aiToolOptions len = %d, want 4", got)
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
