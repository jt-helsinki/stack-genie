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
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

// Execute runs the full in-process create against a fresh location: it scaffolds the
// project, seeds the context-optimization defaults + Caveman skill, and reports the
// expected progress steps in order. A blank GraphifyModel keeps the path network-free
// (no Ollama pull), so no fakes are needed for the happy path.
func TestExecuteHappyPath(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	root := filepath.Join(test.TempDir(), "location")
	spec := project.Spec{
		Name:        "my-app",
		OS:          SupportedOSes()[0],
		Stacks:      []string{SupportedStacks()[0]},
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
		test.Fatalf("warnings = %v, want none (blank Graphify model is network-free)", warnings)
	}

	// The Result mirrors the spec.
	if result.Name != "my-app" || result.Root != root || result.OS != spec.OS {
		test.Fatalf("Result = %+v, want name=my-app root=%q os=%q", result, root, spec.OS)
	}
	if len(result.Tools) != 2 || result.Tools[0] != "opencode" || result.Tools[1] != "omp" {
		test.Fatalf("Result.Tools = %v, want [opencode omp]", result.Tools)
	}
	if len(result.Stacks) != 1 || result.Stacks[0] != SupportedStacks()[0] {
		test.Fatalf("Result.Stacks = %v, want [%s]", result.Stacks, SupportedStacks()[0])
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
	// The Caveman skill is NOT seeded at create — it is installed by the real toolkit at
	// workspace start (workspace.registerCaveman).
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "skills", "caveman", "SKILL.md")); err == nil {
		test.Error("Caveman SKILL.md should not be seeded at create (installed at workspace start)")
	}

	// The report callback saw the expected steps, in order (no pull step for a blank
	// Graphify model).
	want := []string{
		"scaffolding project (Dockerfile, config, skills)",
		"seeding context-optimization defaults",
		"workspace scaffolded — start it to build the image + boot the microVM",
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

// An explicit over-host CPU request is rejected with exit 2 (invalid input) before any
// scaffolding happens.
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
				OS:          SupportedOSes()[0],
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
				OS:             SupportedOSes()[0],
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
				OS:                     SupportedOSes()[0],
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

// TestExecutePersistsGraphifyChoice verifies that the Graphify AI-tool toggle flows
// through project.Spec → Scaffold → config.yaml (context.graphify_enabled) AND controls
// whether the graphify install lands in the scaffolded Dockerfile (conditional snippet,
// no longer baked into the base).
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
				OS:              SupportedOSes()[0],
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
			dockerfile, err := os.ReadFile(filepath.Join(root, ".ai-platform", "Dockerfile"))
			if err != nil {
				test.Fatalf("read Dockerfile: %v", err)
			}
			hasGraphify := strings.Contains(string(dockerfile), "graphifyy[")
			if hasGraphify != enabled {
				test.Errorf("Dockerfile contains graphify install = %v, want %v", hasGraphify, enabled)
			}
		})
	}
}

func TestExecuteRejectsOverHostResources(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	spec := project.Spec{
		Name: "big-app",
		OS:   SupportedOSes()[0],
		Root: filepath.Join(test.TempDir(), "location"),
		CPUs: 1 << 20, // far more than any host has
	}
	_, _, err := Execute(spec, "t", nil)
	if err == nil {
		test.Fatal("Execute must reject an over-host CPU request")
	}
	var platformErr *output.Error
	if !errors.As(err, &platformErr) {
		test.Fatalf("error %v is not an *output.Error", err)
	}
	if platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("exit code = %d, want %d (invalid input)", platformErr.Code, output.ExitInvalidInput)
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

func TestCappedResourcesCapsDefaultAtHost(test *testing.T) {
	// Small host: the 4-cpu / 8G defaults are capped down to the host's 2 cpu and the
	// USABLE memory (4096 MiB host → reserve max(2048, 4096/4=1024)=2048 → 2048 usable).
	cpus, memory := cappedResources(0, "", 2, 4096, true)
	if cpus != 2 {
		test.Errorf("cpus = %d, want capped to host 2", cpus)
	}
	if memory != "2048M" {
		test.Errorf("memory = %q, want capped to usable 2048M", memory)
	}
	// Large host: defaults fit, so they pass through unchanged.
	cpus, memory = cappedResources(0, "", 16, 32768, true)
	if cpus != config.Default().Workspace.CPULimit || memory != config.Default().Workspace.MemoryLimit {
		test.Errorf("large host: got %d/%q, want defaults %d/%q", cpus, memory, config.Default().Workspace.CPULimit, config.Default().Workspace.MemoryLimit)
	}
	// Explicit values are preserved (capping only fills unset).
	cpus, memory = cappedResources(1, "2G", 2, 4096, true)
	if cpus != 1 || memory != "2G" {
		test.Errorf("explicit values changed: got %d/%q, want 1/2G", cpus, memory)
	}
	// Unknown host RAM: the memory default is left as-is (no cap when we can't tell).
	if _, memory = cappedResources(0, "", 2, 0, false); memory != config.Default().Workspace.MemoryLimit {
		test.Errorf("unknown host RAM: memory = %q, want default unchanged", memory)
	}
}

func TestValidateResourcesWithinHostRejectsOverCommit(test *testing.T) {
	// A clearly-impossible CPU request must be rejected (host has far fewer).
	if err := ValidateResourcesWithinHost(1<<20, ""); err == nil {
		test.Error("an over-host CPU request must be rejected")
	}
	// A negative CPU count is invalid.
	if err := ValidateResourcesWithinHost(-1, ""); err == nil {
		test.Error("a negative CPU count must be rejected")
	}
	// A reasonable request (1 CPU, 1 GB) passes on any host.
	if err := ValidateResourcesWithinHost(1, "1"); err != nil {
		test.Errorf("1 CPU / 1 GB should be valid: %v", err)
	}
	// Below the boot minimum is rejected (also guards the unit footgun).
	if err := ValidateResourcesWithinHost(1, "256M"); err == nil {
		test.Error("256M (below the 512 MiB minimum) must be rejected")
	}
}

func TestValidateDisk(test *testing.T) {
	// Blank is allowed (the platform default applies).
	if err := ValidateDisk(""); err != nil {
		test.Errorf("blank disk should be valid: %v", err)
	}
	if err := ValidateDisk("  "); err != nil {
		test.Errorf("whitespace disk should be valid: %v", err)
	}
	// A plain positive GB number is valid.
	if err := ValidateDisk("20"); err != nil {
		test.Errorf("20 GB should be valid: %v", err)
	}
	// Zero / non-numeric / negative are rejected.
	for _, bad := range []string{"0", "abc", "-5", "10x"} {
		if err := ValidateDisk(bad); err == nil {
			test.Errorf("disk %q must be rejected", bad)
		}
	}
}

func TestModelInstalled(test *testing.T) {
	installed := []ollama.Model{{Name: "llama3.2:latest"}, {Name: "qwen2.5-coder:7b"}}
	if !modelInstalled(installed, "qwen2.5-coder:7b") {
		test.Error("exact ref should be installed")
	}
	if !modelInstalled(installed, "llama3.2") {
		test.Error("bare name should match name:latest")
	}
	if modelInstalled(installed, "mistral") {
		test.Error("absent model should not be installed")
	}
}

// pullGraphifyModelIfAbsent skips the pull entirely when the model is already
// installed (no Pull call, no warning).
func TestPullGraphifyModelSkipsWhenInstalled(test *testing.T) {
	fake := &ollama.Fake{ListModels: []ollama.Model{{Name: "llama3.2:latest"}}}
	restore := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restore }()

	if warnings := pullGraphifyModelIfAbsent("llama3.2", func(Progress) {}); len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none (already installed)", warnings)
	}
	if fake.PulledNames != nil {
		test.Fatalf("Pull should not run for an installed model; pulled %v", fake.PulledNames)
	}
}

// A blank model ref is a no-op (Graphify has no configured model).
func TestPullGraphifyModelBlankNoOp(test *testing.T) {
	fake := &ollama.Fake{}
	restore := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restore }()

	if warnings := pullGraphifyModelIfAbsent("  ", func(Progress) {}); warnings != nil {
		test.Fatalf("blank ref should be a no-op, got %v", warnings)
	}
	if fake.PulledNames != nil {
		test.Fatalf("blank ref must not pull; pulled %v", fake.PulledNames)
	}
}

// An absent model is pulled and then registered in the gateway.
func TestPullGraphifyModelPullsAndRegisters(test *testing.T) {
	fake := &ollama.Fake{}
	restoreClient := ollamaClient
	ollamaClient = func() ollama.Client { return fake }
	defer func() { ollamaClient = restoreClient }()

	registrar := &fakeModelRegistrar{}
	restoreReg := newRegistrar
	newRegistrar = func() modelRegistrar { return registrar }
	defer func() { newRegistrar = restoreReg }()

	if warnings := pullGraphifyModelIfAbsent("qwen2.5-coder:7b", func(Progress) {}); len(warnings) != 0 {
		test.Fatalf("warnings = %v, want none", warnings)
	}
	if len(fake.PulledNames) != 1 || fake.PulledNames[0] != "qwen2.5-coder:7b" {
		test.Fatalf("Pulled = %v, want [qwen2.5-coder:7b]", fake.PulledNames)
	}
	if len(registrar.registered) != 1 || registrar.registered[0] != "qwen2.5-coder:7b" {
		test.Fatalf("registered = %v, want [qwen2.5-coder:7b]", registrar.registered)
	}
}

// fakeModelRegistrar records gateway registrations for the pull test.
type fakeModelRegistrar struct {
	registered []string
}

func (fake *fakeModelRegistrar) RegisterOllamaModel(name string, _ bool) error {
	fake.registered = append(fake.registered, name)
	return nil
}

// TestExecuteWarnsOnOAuth verifies Execute emits a bypass warning for each oauth agent,
// and none when all agents are api-key.
func TestExecuteWarnsOnOAuth(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := filepath.Join(test.TempDir(), "location")
	spec := project.Spec{
		Name:        "oauth-app",
		OS:          SupportedOSes()[0],
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

// TestSupportedAgentCLIsIncludesCopilot pins copilot as a selectable agent CLI.
func TestSupportedAgentCLIsIncludesCopilot(test *testing.T) {
	if !slices.Contains(SupportedAgentCLIs(), "copilot") {
		test.Errorf("SupportedAgentCLIs() must include copilot: %v", SupportedAgentCLIs())
	}
}

// TestSupportedAgentCLIsIncludesHermes pins the hermes gateway agent, confirms
// SplitAgentsAndApps classes it as an AGENT (not an app), and that the removed openclaw is
// absent.
func TestSupportedAgentCLIsIncludesHermes(test *testing.T) {
	if !slices.Contains(SupportedAgentCLIs(), "hermes") {
		test.Errorf("SupportedAgentCLIs() must include hermes: %v", SupportedAgentCLIs())
	}
	if slices.Contains(SupportedAgentCLIs(), "openclaw") {
		test.Errorf("openclaw was removed and must not be a supported agent CLI: %v", SupportedAgentCLIs())
	}
	agentCLIs, appKeys := SplitAgentsAndApps([]string{"hermes", "openwebui"})
	if !slices.Contains(agentCLIs, "hermes") {
		test.Errorf("hermes must split as an agent, got agents=%v apps=%v", agentCLIs, appKeys)
	}
	if slices.Contains(appKeys, "hermes") {
		test.Errorf("hermes must NOT be classed as an app: %v", appKeys)
	}
}

// TestSplitAgentsAndApps verifies the combined agents+apps wizard selection splits into
// the two known sets regardless of selection order, dropping unknown values.
func TestSplitAgentsAndApps(test *testing.T) {
	agentCLIs, appKeys := SplitAgentsAndApps([]string{"anythingllm", "opencode", "openwebui", "omp", "bogus"})
	if strings.Join(agentCLIs, ",") != "opencode,omp" {
		test.Errorf("agent CLIs = %v, want [opencode omp]", agentCLIs)
	}
	if strings.Join(appKeys, ",") != "anythingllm,openwebui" {
		test.Errorf("app keys = %v, want [anythingllm openwebui]", appKeys)
	}
	agentCLIs, appKeys = SplitAgentsAndApps(nil)
	if len(agentCLIs) != 0 || len(appKeys) != 0 {
		test.Errorf("empty selection must split to empty sides, got %v / %v", agentCLIs, appKeys)
	}
}

// TestExecuteWarnsOnCopilot verifies copilot (forced-oauth, gateway-incapable) triggers a
// direct-to-provider security warning like any oauth agent.
func TestExecuteWarnsOnCopilot(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := filepath.Join(test.TempDir(), "location")
	spec := project.Spec{
		Name:        "copilot-app",
		OS:          SupportedOSes()[0],
		AgentCLIs:   []string{"opencode", "copilot"},
		DefaultTool: "opencode",
		Root:        root,
	}
	_, warnings, err := Execute(spec, "2026-07-05T00:00:00Z", nil)
	if err != nil {
		test.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "copilot") {
		test.Errorf("expected a direct-to-provider warning for copilot, got: %v", warnings)
	}
}
