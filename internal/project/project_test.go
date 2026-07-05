package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/templates"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

func withTemplates(test *testing.T) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	if err := templates.Install(); err != nil {
		test.Fatal(err)
	}
}

func sampleSpec() Spec {
	return Spec{
		Name:        "my-app",
		OS:          "debian-trixie",
		Stacks:      []string{"go"},
		AgentCLIs:   []string{"opencode", "codex"},
		DefaultTool: "opencode",
	}
}

func TestScaffoldWritesArtifactsAndIndex(test *testing.T) {
	withTemplates(test)

	root, err := Scaffold(sampleSpec(), "2026-06-18T00:00:00Z")
	if err != nil {
		test.Fatal(err)
	}

	dockerfile, err := os.ReadFile(filepath.Join(root, ".ai-platform", "Dockerfile"))
	if err != nil {
		test.Fatal(err)
	}
	for _, fragment := range []string{"FROM debian:trixie-slim", "# stack: go", "# agent CLI: opencode", "# agent CLI: codex"} {
		if !strings.Contains(string(dockerfile), fragment) {
			test.Errorf("Dockerfile missing %q", fragment)
		}
	}

	for _, file := range []string{"config.yaml", "profile.yaml", "project.yaml", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, ".ai-platform", file)); err != nil {
			test.Errorf("missing %s: %v", file, err)
		}
	}

	profile, _ := os.ReadFile(filepath.Join(root, ".ai-platform", "profile.yaml"))
	if !strings.Contains(string(profile), "go") {
		test.Errorf("profile.yaml missing stack: %s", profile)
	}

	index, err := state.LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if _, ok := index.Projects["my-app"]; !ok {
		test.Fatal("project not registered in index")
	}
}

func TestScaffoldWritesDefaultMicrosandboxIdleTimeout(test *testing.T) {
	withTemplates(test)
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if got := projectConfig.Microsandbox.IdleTimeout; got != config.DefaultMicrosandboxIdleTimeout {
		test.Fatalf("microsandbox.idle_timeout = %q, want %q", got, config.DefaultMicrosandboxIdleTimeout)
	}
}

func TestScaffoldWritesConfiguredMicrosandboxIdleTimeout(test *testing.T) {
	withTemplates(test)
	spec := sampleSpec()
	spec.IdleTimeout = "2h"
	root, err := Scaffold(spec, "t")
	if err != nil {
		test.Fatal(err)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if got := projectConfig.Microsandbox.IdleTimeout; got != "2h" {
		test.Fatalf("microsandbox.idle_timeout = %q, want 2h", got)
	}
}

// TestScaffoldRefreshesStaleTemplates: create must overwrite an on-disk template
// left by an OLDER ai (here a tmux-less base) with THIS binary's embedded copy, so
// a binary upgrade alone fixes the generated Dockerfile without re-running setup.
func TestScaffoldRefreshesStaleTemplates(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// Seed a stale on-disk base template (no tmux), as an old `ai setup` would have.
	root, err := templates.InstalledRoot()
	if err != nil {
		test.Fatal(err)
	}
	stalePath := filepath.Join(root, "dockerfiles", "debian-trixie", "Dockerfile")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("FROM debian:trixie-slim\nRUN apt-get install -y curl git\n"), 0o644); err != nil {
		test.Fatal(err)
	}

	projectRoot, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(projectRoot, ".ai-platform", "Dockerfile"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "tmux") {
		test.Errorf("create must refresh stale on-disk templates from the embed (expected tmux in base):\n%s", dockerfile)
	}
}

func TestScaffoldAtExplicitRoot(test *testing.T) {
	withTemplates(test)
	home, _ := os.UserHomeDir()

	// A directory entirely outside ~/projects — `ai project create` sets
	// Spec.Root to the current directory, which can be anywhere.
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
	withTemplates(test)
	spec := sampleSpec()
	spec.Name = "-bad-"
	if _, err := Scaffold(spec, "t"); !errors.Is(err, ErrInvalidName) {
		test.Fatalf("want ErrInvalidName, got %v", err)
	}
}

func TestScaffoldDuplicate(test *testing.T) {
	withTemplates(test)
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	if _, err := Scaffold(sampleSpec(), "t"); !errors.Is(err, ErrAlreadyExists) {
		test.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestListReturnsProjects(test *testing.T) {
	withTemplates(test)
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	entries, err := List()
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "my-app" || entries[0].OS != "debian-trixie" {
		test.Fatalf("list = %+v", entries)
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

func TestDeleteRemovesPlatformDirKeepsOtherFiles(test *testing.T) {
	withTemplates(test)
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
	for _, dir := range []string{".opencode", ".claude", ".pi", ".venv-msb"} {
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
	for _, dir := range []string{".opencode", ".claude", ".codex", ".pi", ".gemini", ".venv-msb"} {
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
	withTemplates(test)
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
	withTemplates(test)
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

func TestDeleteRemovesOverlay(test *testing.T) {
	withTemplates(test)
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	// Simulate a provisioned workspace's persistent overlay.
	workspaceID := workspace.Name("my-app")
	if _, err := overlay.Ensure(workspaceID); err != nil {
		test.Fatal(err)
	}
	if err := Delete("my-app", false, true); err != nil {
		test.Fatal(err)
	}
	// Deleting the project is permanent removal (arch §26): the overlay goes too.
	present, err := overlay.Exists(workspaceID)
	if err != nil || present {
		test.Fatalf("delete should remove the overlay: present=%v err=%v", present, err)
	}
}

func TestDeleteUnknown(test *testing.T) {
	withTemplates(test)
	if err := Delete("ghost", false, true); !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}

func TestScaffoldAllocatesAppPorts(test *testing.T) {
	withTemplates(test)
	spec := sampleSpec()
	spec.Apps = []string{"openwebui", "anythingllm"}
	root, err := Scaffold(spec, "t")
	if err != nil {
		test.Fatal(err)
	}
	cfg, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if len(cfg.Apps) != 2 {
		test.Fatalf("config has %d apps, want 2: %+v", len(cfg.Apps), cfg.Apps)
	}
	if cfg.Apps[0].Port == 0 || cfg.Apps[0].Port == cfg.Apps[1].Port {
		test.Fatalf("app ports not allocated uniquely: %+v", cfg.Apps)
	}
}

func TestScaffoldNoAppsByDefault(test *testing.T) {
	withTemplates(test)
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	cfg, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if len(cfg.Apps) != 0 {
		test.Fatalf("apps default to %v, want empty", cfg.Apps)
	}
}

// TestScaffoldWritesAuthModesAndOAuthEgress verifies Scaffold persists auth_modes for the
// selected OAuth-capable CLIs and allow-lists an oauth agent's provider egress domains.
func TestScaffoldWritesAuthModesAndOAuthEgress(test *testing.T) {
	withTemplates(test)
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
	if !hasAllowedHost(projectConfig.Network.AllowHostServices, "api.anthropic.com") {
		test.Errorf("oauth claude-code must allow-list api.anthropic.com: %+v", projectConfig.Network.AllowHostServices)
	}
	if hasAllowedHost(projectConfig.Network.AllowHostServices, "generativelanguage.googleapis.com") {
		test.Errorf("api-key gemini must NOT allow-list its provider domains: %+v", projectConfig.Network.AllowHostServices)
	}
}

// TestScaffoldForcesCopilotOAuth verifies selecting copilot (forced-oauth) persists
// auth_modes["copilot"]="oauth" automatically and allow-lists its provider egress domains,
// with no auth-mode choice required.
func TestScaffoldForcesCopilotOAuth(test *testing.T) {
	withTemplates(test)
	spec := sampleSpec()
	spec.AgentCLIs = []string{"opencode", "copilot"}
	// No AuthModes provided — copilot must still become oauth.
	root, err := Scaffold(spec, "t")
	if err != nil {
		test.Fatal(err)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if got := projectConfig.Agent.AuthModes["copilot"]; got != "oauth" {
		test.Errorf("auth_modes[copilot] = %q, want oauth (forced)", got)
	}
	if got := projectConfig.Agent.AuthMode("copilot"); got != "oauth" {
		test.Errorf("copilot AuthMode = %q, want oauth", got)
	}
	if !hasAllowedHost(projectConfig.Network.AllowHostServices, "api.githubcopilot.com") {
		test.Errorf("copilot must allow-list api.githubcopilot.com: %+v", projectConfig.Network.AllowHostServices)
	}
}

// TestScaffoldNoAuthModesWhenAllAPIKey verifies a default (all api-key) create writes no
// auth_modes and no oauth egress rules.
func TestScaffoldNoAuthModesWhenAllAPIKey(test *testing.T) {
	withTemplates(test)
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
	if len(projectConfig.Network.AllowHostServices) != 0 {
		test.Errorf("all-api-key create must add no egress allow rules: %+v", projectConfig.Network.AllowHostServices)
	}
}

func hasAllowedHost(services []config.HostService, host string) bool {
	for _, service := range services {
		if service.Host == host {
			return true
		}
	}
	return false
}
