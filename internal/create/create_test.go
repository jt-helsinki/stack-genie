package create

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

// Real tool installers (git clones, uv/npm installs) must never run from unit tests.
func TestMain(main *testing.M) {
	prepareWorkspace = func(string) error { return nil }
	os.Exit(main.Run())
}

func TestExecuteInstallsToolsAtCreation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	var prepared string
	prepareWorkspace = func(root string) error { prepared = root; return nil }
	test.Cleanup(func() { prepareWorkspace = func(string) error { return nil } })

	root := filepath.Join(test.TempDir(), "location")
	spec := project.Spec{Name: "tools-app", AgentCLIs: []string{"opencode"}, DefaultTool: "opencode", Root: root}
	if _, _, err := Execute(spec, "2026-07-05T00:00:00Z", nil); err != nil {
		test.Fatalf("Execute: %v", err)
	}
	if prepared != root {
		test.Fatalf("tool install ran for %q, want %q", prepared, root)
	}
}

// Execute runs the full in-process create against a fresh location: it scaffolds the
// project, seeds the context-optimization defaults + Caveman skill, and reports the
// expected progress steps in order. There is no image to build and nothing to
// download, so no fakes are needed for the happy path.
func TestExecuteHappyPath(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	root := filepath.Join(test.TempDir(), "location")
	spec := project.Spec{
		Name:        "my-app",
		AgentCLIs:   []string{"opencode", "omp"},
		DefaultTool: "opencode",
		Root:        root,
	}

	var steps []string
	report := func(progress Progress) { steps = append(steps, progress.Step) }

	result, warnings, err := Execute(spec, "2026-07-05T00:00:00Z", report)
	if err != nil {
		test.Fatalf("Execute returned error: %v", err)
	}
	if len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none (no OAuth agents)", warnings)
	}

	// The Result mirrors the spec.
	if result.Name != "my-app" || result.Root != root {
		test.Fatalf("Result = %+v, want name=my-app root=%q", result, root)
	}
	if len(result.Tools) != 2 || result.Tools[0] != "opencode" || result.Tools[1] != "omp" {
		test.Fatalf("Result.Tools = %v, want [opencode omp]", result.Tools)
	}
	if result.ConfigYAML == "" {
		test.Error("Result.ConfigYAML is empty, want the scaffolded config.yaml contents")
	}

	// config.yaml was scaffolded and is readable.
	if _, err := os.Stat(config.ProjectPath(root)); err != nil {
		test.Fatalf("config.yaml not scaffolded on disk: %v", err)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatalf("LoadProjectConfig: %v", err)
	}

	// The context-optimization defaults + Caveman level were seeded.
	if projectConfig.Context.Strategy != contextopt.DefaultStrategy {
		test.Errorf("context strategy = %q, want %q", projectConfig.Context.Strategy, contextopt.DefaultStrategy)
	}
	if projectConfig.Context.CavemanLevel != contextopt.DefaultCavemanLevel {
		test.Errorf("caveman level = %q, want %q", projectConfig.Context.CavemanLevel, contextopt.DefaultCavemanLevel)
	}
	// The Caveman skill is NOT seeded at create — it is installed at the next
	// `ai agent`/`ai shell` (workspace.registerCaveman).
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "skills", "caveman", "SKILL.md")); err == nil {
		test.Error("Caveman SKILL.md should not be seeded at create (installed at workspace start)")
	}

	// The report callback saw the expected steps, in order.
	want := []string{
		"scaffolding workspace (config, skills)",
		"seeding context-optimization defaults",
		"installing AI tools",
		"workspace registered — run `ai agent <cli>` or `ai shell` to start a sandboxed session",
	}
	if len(steps) != len(want) {
		test.Fatalf("progress steps = %v, want %v", steps, want)
	}
	for index := range want {
		if steps[index] != want[index] {
			test.Fatalf("progress step %d = %q, want %q (full: %v)", index, steps[index], want[index], steps)
		}
	}
}

// TestExecutePersistsShellChoice verifies the create wizard/flag shell choice flows
// through project.Spec → Scaffold → config.yaml: an explicit zsh persists
// workspace.shell: zsh, while an unset shell defaults to bash.
func TestExecutePersistsShellChoice(test *testing.T) {
	for _, tc := range []struct {
		name  string
		shell string
		want  string
	}{
		{name: "zsh-explicit", shell: "zsh", want: "zsh"},
		{name: "default-bash", shell: "", want: "bash"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			test.Setenv("HOME", test.TempDir())
			root := filepath.Join(test.TempDir(), "location")
			spec := project.Spec{
				Name:        "shell-app",
				AgentCLIs:   []string{"opencode", "omp"},
				DefaultTool: "opencode",
				Shell:       tc.shell,
				Root:        root,
			}
			if _, _, err := Execute(spec, "2026-07-05T00:00:00Z", nil); err != nil {
				test.Fatalf("Execute: %v", err)
			}
			projectConfig, err := config.LoadProjectConfig(root)
			if err != nil {
				test.Fatalf("LoadProjectConfig: %v", err)
			}
			if projectConfig.Workspace.Shell != tc.want {
				test.Errorf("workspace.shell = %q, want %q", projectConfig.Workspace.Shell, tc.want)
			}
		})
	}
}

// TestExecutePersistsCavemanChoice verifies the create wizard/flag Caveman toggle
// flows through project.Spec → Scaffold → config.yaml context.caveman_enabled, and
// that CavemanEnabledOrDefault reflects it.
func TestExecutePersistsCavemanChoice(test *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	} {
		test.Run(tc.name, func(test *testing.T) {
			test.Setenv("HOME", test.TempDir())
			root := filepath.Join(test.TempDir(), "location")
			spec := project.Spec{
				Name:           "caveman-app",
				AgentCLIs:      []string{"opencode", "omp"},
				DefaultTool:    "opencode",
				Root:           root,
				CavemanEnabled: tc.enabled,
			}
			if _, _, err := Execute(spec, "2026-07-05T00:00:00Z", nil); err != nil {
				test.Fatalf("Execute: %v", err)
			}
			projectConfig, err := config.LoadProjectConfig(root)
			if err != nil {
				test.Fatalf("LoadProjectConfig: %v", err)
			}
			if projectConfig.Context.CavemanEnabled == nil {
				test.Fatal("context.caveman_enabled should be persisted (non-nil)")
			}
			if got := projectConfig.Context.CavemanEnabledOrDefault(); got != tc.enabled {
				test.Errorf("CavemanEnabledOrDefault = %v, want %v", got, tc.enabled)
			}
		})
	}
}

// TestExecutePersistsCodeGraphAndMemoryChoices verifies the opt-in code-review-graph /
// codebase-memory-mcp create toggles flow through project.Spec → Scaffold → config.yaml
// (context.code_review_graph_enabled / context.codebase_memory_enabled) and that the
// *OrDefault helpers reflect them.
func TestExecutePersistsCodeGraphAndMemoryChoices(test *testing.T) {
	for _, tc := range []struct {
		name       string
		codeReview bool
		codeMemory bool
	}{
		{name: "both off (default)", codeReview: false, codeMemory: false},
		{name: "both on", codeReview: true, codeMemory: true},
		{name: "code-review-graph only", codeReview: true, codeMemory: false},
		{name: "codebase-memory only", codeReview: false, codeMemory: true},
	} {
		test.Run(tc.name, func(test *testing.T) {
			test.Setenv("HOME", test.TempDir())
			root := filepath.Join(test.TempDir(), "location")
			spec := project.Spec{
				Name:                   "tools-app",
				AgentCLIs:              []string{"opencode", "omp"},
				DefaultTool:            "opencode",
				Root:                   root,
				CodeReviewGraphEnabled: tc.codeReview,
				CodebaseMemoryEnabled:  tc.codeMemory,
			}
			if _, _, err := Execute(spec, "2026-07-05T00:00:00Z", nil); err != nil {
				test.Fatalf("Execute: %v", err)
			}
			projectConfig, err := config.LoadProjectConfig(root)
			if err != nil {
				test.Fatalf("LoadProjectConfig: %v", err)
			}
			if got := projectConfig.Context.CodeReviewGraphEnabledOrDefault(); got != tc.codeReview {
				test.Errorf("CodeReviewGraphEnabledOrDefault = %v, want %v", got, tc.codeReview)
			}
			if got := projectConfig.Context.CodebaseMemoryEnabledOrDefault(); got != tc.codeMemory {
				test.Errorf("CodebaseMemoryEnabledOrDefault = %v, want %v", got, tc.codeMemory)
			}
		})
	}
}

// TestExecutePersistsGraphifyChoice verifies the Graphify AI-tool toggle flows through
// project.Spec → Scaffold → config.yaml (context.graphify_enabled). There is no image
// to inspect for an install snippet anymore — graphify installs at the next
// `ai agent`/`ai shell` (workspace.setupGraphifyMCP), not at create time.
func TestExecutePersistsGraphifyChoice(test *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "graphify-off"
		if enabled {
			name = "graphify-on"
		}
		test.Run(name, func(test *testing.T) {
			test.Setenv("HOME", test.TempDir())
			root := filepath.Join(test.TempDir(), "location")
			spec := project.Spec{
				Name:            "g-app",
				AgentCLIs:       []string{"opencode", "omp"},
				DefaultTool:     "opencode",
				Root:            root,
				GraphifyEnabled: enabled,
			}
			if _, _, err := Execute(spec, "2026-07-05T00:00:00Z", nil); err != nil {
				test.Fatalf("Execute: %v", err)
			}
			projectConfig, err := config.LoadProjectConfig(root)
			if err != nil {
				test.Fatalf("LoadProjectConfig: %v", err)
			}
			if got := projectConfig.Context.GraphifyEnabledOrDefault(); got != enabled {
				test.Errorf("GraphifyEnabledOrDefault = %v, want %v", got, enabled)
			}
		})
	}
}

// mapProjectErr maps the project sentinels (here ErrAlreadyExists — creating over an
// existing workspace) to exit 2, so create surfaces the right code.
func TestMapProjectErrAlreadyExistsIsInvalidInput(test *testing.T) {
	err := mapProjectErr(project.ErrAlreadyExists)
	var platformErr *output.Error
	if !errors.As(err, &platformErr) {
		test.Fatalf("mapProjectErr returned %v, not an *output.Error", err)
	}
	if platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("exit code = %d, want %d for ErrAlreadyExists", platformErr.Code, output.ExitInvalidInput)
	}
}

// TestExecuteWarnsOnOAuth verifies Execute emits a bypass warning for each oauth agent,
// and none when all agents are api-key.
func TestExecuteWarnsOnOAuth(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := filepath.Join(test.TempDir(), "location")
	spec := project.Spec{
		Name:        "oauth-app",
		AgentCLIs:   []string{"opencode", "claude-code", "gemini"},
		DefaultTool: "opencode",
		AuthModes:   map[string]string{"claude-code": "oauth", "gemini": "api-key"},
		Root:        root,
	}
	_, warnings, err := Execute(spec, "2026-07-05T00:00:00Z", nil)
	if err != nil {
		test.Fatalf("Execute returned error: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "claude-code") || !strings.Contains(joined, "bypassing the gateway") {
		test.Errorf("expected an OAuth bypass warning for claude-code, got: %v", warnings)
	}
	if strings.Contains(joined, "gemini DIRECTLY") {
		test.Errorf("api-key gemini must not trigger a bypass warning: %v", warnings)
	}
}

// TestSupportedAgentCLIsIncludesHermes pins the hermes gateway agent, and confirms the
// removed openclaw is absent.
func TestSupportedAgentCLIsIncludesHermes(test *testing.T) {
	if !slices.Contains(SupportedAgentCLIs(), "hermes") {
		test.Errorf("SupportedAgentCLIs() must include hermes: %v", SupportedAgentCLIs())
	}
	if slices.Contains(SupportedAgentCLIs(), "openclaw") {
		test.Errorf("openclaw was removed and must not be a supported agent CLI: %v", SupportedAgentCLIs())
	}
}
