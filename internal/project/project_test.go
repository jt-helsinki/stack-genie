package project

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

// TestMain stubs the nono session lister for the whole package's test run —
// List()'s live `nono ps` query is a best-effort read that must never shell
// out to a real `nono` binary (or depend on one being installed) in unit tests.
func TestMain(m *testing.M) {
	listSessionsFn = func() ([]nono.Session, error) { return nil, nil }
	os.Exit(m.Run())
}

func sampleSpec() Spec {
	return Spec{
		Name:        "my-app",
		AgentCLIs:   []string{"opencode", "codex"},
		DefaultTool: "opencode",
	}
}

func TestScaffoldWritesArtifactsAndIndex(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	root, err := Scaffold(sampleSpec(), "2026-06-18T00:00:00Z")
	if err != nil {
		test.Fatal(err)
	}

	for _, file := range []string{"config.yaml", "project.yaml", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, ".ai-platform", file)); err != nil {
			test.Errorf("missing %s: %v", file, err)
		}
	}

	index, err := state.LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if _, ok := index.Projects["my-app"]; !ok {
		test.Fatal("project not registered in index")
	}
}

func TestScaffoldAtExplicitRoot(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	home, _ := os.UserHomeDir()

	// A directory entirely outside ~/projects — `ai create` sets Spec.Root to the
	// current directory, which can be anywhere.
	root := filepath.Join(home, "workspace", "testvm")
	spec := sampleSpec()
	spec.Root = root

	got, err := Scaffold(spec, "t")
	if err != nil {
		test.Fatal(err)
	}
	if got != root {
		test.Fatalf("Scaffold root = %q, want %q", got, root)
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "project.yaml")); err != nil {
		test.Errorf("project.yaml not written at explicit root: %v", err)
	}
	// The index records the explicit path, so downstream resolves it by name.
	registered, ok, err := Path(spec.Name)
	if err != nil || !ok {
		test.Fatalf("project not registered: ok=%v err=%v", ok, err)
	}
	if registered != root {
		test.Errorf("index path = %q, want %q", registered, root)
	}
}

func TestScaffoldInvalidName(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	spec := sampleSpec()
	spec.Name = "-bad-"
	if _, err := Scaffold(spec, "t"); !errors.Is(err, ErrInvalidName) {
		test.Fatalf("want ErrInvalidName, got %v", err)
	}
}

func TestScaffoldDuplicate(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	if _, err := Scaffold(sampleSpec(), "t"); !errors.Is(err, ErrAlreadyExists) {
		test.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestListReturnsProjects(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	entries, err := List()
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "my-app" || entries[0].OS != goruntime.GOOS {
		test.Fatalf("list = %+v, want OS=%q", entries, goruntime.GOOS)
	}
	// §3.3: the row also carries the active agent CLIs and the workspace status
	// ("none" until a workspace is started).
	if len(entries[0].Agents) != 2 || entries[0].Agents[0] != "opencode" {
		test.Fatalf("list agents = %+v, want [opencode codex]", entries[0].Agents)
	}
	if entries[0].Status != "none" {
		test.Fatalf("list status = %q, want none (no workspace started)", entries[0].Status)
	}
}

// TestListReflectsLiveSession is a regression test: `ai list`'s status must
// come from a REAL running nono session for the project's root, matched by
// Workdir — not a stale saved-handle id guess (the id a workspace launch
// saves is the real nono session id, which never equals the old aip-<name>
// placeholder `workspaceHandle` used to compare against, so status was stuck
// at "none" forever even with a live session running).
func TestListReflectsLiveSession(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}

	original := listSessionsFn
	defer func() { listSessionsFn = original }()
	listSessionsFn = func() ([]nono.Session, error) {
		return []nono.Session{
			{SessionID: "sess-old", Workdir: root, Status: "exited", Started: "2026-01-01T00:00:00Z"},
			{SessionID: "sess-new", Workdir: root, Status: "running", Started: "2026-01-02T00:00:00Z"},
			{SessionID: "sess-other", Workdir: "/some/other/project", Status: "running", Started: "2026-01-03T00:00:00Z"},
		}, nil
	}

	entries, err := List()
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 {
		test.Fatalf("list = %+v, want 1 entry", entries)
	}
	if entries[0].Status != "running" {
		test.Errorf("status = %q, want running (live session for this project's workdir)", entries[0].Status)
	}
	if entries[0].ID != "sess-new" {
		test.Errorf("id = %q, want the most-recently-started matching session id sess-new", entries[0].ID)
	}
}

func TestDeleteRemovesPlatformDirKeepsOtherFiles(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	// A user file OUTSIDE .ai-platform (their actual code) must survive a plain delete.
	userFile := filepath.Join(root, "main.go")
	if err := os.WriteFile(userFile, []byte("package main"), 0o644); err != nil {
		test.Fatal(err)
	}
	// The per-CLI agent config dirs + venv the platform writes into the project folder
	// at workspace start (here only some exist — a missing one must be ignored).
	for _, dir := range []string{".opencode", ".claude", ".omp", ".venv-msb"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			test.Fatal(err)
		}
	}
	if err := Delete("my-app", false, true); err != nil {
		test.Fatal(err)
	}
	// The whole .ai-platform directory (the platform's footprint) is removed.
	if _, err := os.Stat(filepath.Join(root, ".ai-platform")); !os.IsNotExist(err) {
		test.Errorf(".ai-platform should be removed on a plain delete, got %v", err)
	}
	// Every per-CLI agent dir + venv is removed too (existing or not).
	for _, dir := range []string{".opencode", ".claude", ".codex", ".omp", ".gemini", ".venv-msb"} {
		if _, err := os.Stat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			test.Errorf("%s should be removed on a plain delete, got %v", dir, err)
		}
	}
	// The user's other files are kept (only --purge removes the whole directory).
	if _, err := os.Stat(userFile); err != nil {
		test.Errorf("the user's other files should be kept: %v", err)
	}
	index, _ := state.LoadIndex()
	if _, ok := index.Projects["my-app"]; ok {
		test.Error("project should be removed from index")
	}
}

// TestDeleteKeepsAgentDirsAndMaterializesSymlinks: when the agent config folders are
// KEPT (removeAgentDirs=false), their symlinks into the shared .ai-platform pool are
// converted to real files before .ai-platform is removed, so the kept content survives.
func TestDeleteKeepsAgentDirsAndMaterializesSymlinks(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	// Seed a shared pool with a skill, and a per-CLI dir symlinking into it (as
	// linkSharedResources does at workspace start).
	pool := filepath.Join(root, ".ai-platform", "skills", "caveman")
	if err := os.MkdirAll(pool, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool, "SKILL.md"), []byte("# caveman"), 0o644); err != nil {
		test.Fatal(err)
	}
	opencodeSkills := filepath.Join(root, ".opencode", "skills")
	if err := os.MkdirAll(filepath.Join(root, ".opencode"), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".ai-platform", "skills"), opencodeSkills); err != nil {
		test.Fatal(err)
	}

	if err := Delete("my-app", false, false); err != nil {
		test.Fatal(err)
	}
	// .ai-platform is gone, but .opencode is KEPT with the skill materialized as a REAL
	// file (not a now-dangling symlink).
	if _, err := os.Stat(filepath.Join(root, ".ai-platform")); !os.IsNotExist(err) {
		test.Errorf(".ai-platform should be removed, got %v", err)
	}
	info, err := os.Lstat(opencodeSkills)
	if err != nil {
		test.Fatalf(".opencode/skills should be kept: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		test.Error(".opencode/skills must be materialized to a real dir, not left a (dangling) symlink")
	}
	if data, err := os.ReadFile(filepath.Join(opencodeSkills, "caveman", "SKILL.md")); err != nil || string(data) != "# caveman" {
		test.Errorf("materialized skill content missing: %q err=%v", data, err)
	}
}

func TestDeletePurgeRemovesSource(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	if err := Delete("my-app", true, true); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		test.Errorf("--purge should remove the source tree, got %v", err)
	}
}

func TestDeleteUnknown(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Delete("ghost", false, true); !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}

// TestScaffoldWritesAuthModesAndOAuthEgress verifies Scaffold persists auth_modes for the
// selected OAuth-capable CLIs and allow-lists an oauth agent's provider egress domains.
func TestScaffoldWritesAuthModesAndOAuthEgress(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	spec := sampleSpec()
	spec.AgentCLIs = []string{"opencode", "claude-code", "gemini"}
	spec.AuthModes = map[string]string{"claude-code": "oauth", "gemini": "api-key"}

	root, err := Scaffold(spec, "t")
	if err != nil {
		test.Fatal(err)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	// claude-code oauth + gemini api-key persisted; opencode (not OAuth-capable) omitted.
	if got := projectConfig.Agent.AuthModes["claude-code"]; got != "oauth" {
		test.Errorf("auth_modes[claude-code] = %q, want oauth", got)
	}
	if got := projectConfig.Agent.AuthModes["gemini"]; got != "api-key" {
		test.Errorf("auth_modes[gemini] = %q, want api-key", got)
	}
	if _, present := projectConfig.Agent.AuthModes["opencode"]; present {
		test.Error("opencode is not OAuth-capable and must not appear in auth_modes")
	}
	// The oauth claude-code's provider domain is allow-listed; gemini (api-key) adds none.
	if !slicesContain(projectConfig.Network.AllowDomain, "api.anthropic.com") {
		test.Errorf("oauth claude-code must allow-list api.anthropic.com: %+v", projectConfig.Network.AllowDomain)
	}
	if slicesContain(projectConfig.Network.AllowDomain, "generativelanguage.googleapis.com") {
		test.Errorf("api-key gemini must NOT allow-list its provider domains: %+v", projectConfig.Network.AllowDomain)
	}
}

// TestScaffoldNoAuthModesWhenAllAPIKey verifies a default (all api-key) create writes no
// auth_modes and no oauth egress rules.
func TestScaffoldNoAuthModesWhenAllAPIKey(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	spec := sampleSpec()
	spec.AgentCLIs = []string{"opencode", "claude-code"}
	// No AuthModes → default api-key everywhere.
	root, err := Scaffold(spec, "t")
	if err != nil {
		test.Fatal(err)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if got := projectConfig.Agent.AuthMode("claude-code"); got != "api-key" {
		test.Errorf("default claude-code AuthMode = %q, want api-key", got)
	}
	if len(projectConfig.Network.AllowDomain) != 0 {
		test.Errorf("all-api-key create must add no egress allow rules: %+v", projectConfig.Network.AllowDomain)
	}
}

func slicesContain(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
