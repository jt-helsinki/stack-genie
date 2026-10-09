package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

// fakeKeyMinter is an in-memory KeyMinter double.
type fakeKeyMinter struct {
	generated    []litellm.KeyScope
	deletedAlias []string
	generateErr  error
	generatedKey string
	deleteErr    error
}

func (fake *fakeKeyMinter) GenerateKey(scope litellm.KeyScope) (string, error) {
	fake.generated = append(fake.generated, scope)
	if fake.generateErr != nil {
		return "", fake.generateErr
	}
	if fake.generatedKey != "" {
		return fake.generatedKey, nil
	}
	return "sk-fake-key", nil
}
func (fake *fakeKeyMinter) DeleteKeyByAlias(alias string) error {
	fake.deletedAlias = append(fake.deletedAlias, alias)
	return fake.deleteErr
}
func (fake *fakeKeyMinter) KeyInfo(string) (litellm.KeyDetails, error) {
	return litellm.KeyDetails{}, nil
}

// fakeCredentialStore is an in-memory CredentialStore double — no real OS keychain
// is ever touched by tests.
type fakeCredentialStore struct {
	stored  map[string]string
	deleted []string
}

func newFakeCredentialStore() *fakeCredentialStore {
	return &fakeCredentialStore{stored: map[string]string{}}
}
func (fake *fakeCredentialStore) StoreCredential(name, value string) error {
	fake.stored[name] = value
	return nil
}
func (fake *fakeCredentialStore) DeleteCredential(name string) error {
	fake.deleted = append(fake.deleted, name)
	delete(fake.stored, name)
	return nil
}

// fakeServedModels is a fixed ServedModels double.
type fakeServedModels struct{ models []agentcfg.Model }

func (fake fakeServedModels) ServedModels() ([]agentcfg.Model, error) { return fake.models, nil }

// seedProject registers project in the global index (pointing at a fresh temp
// directory) and writes its config.yaml. Returns the project's root directory.
func seedProject(test *testing.T, project string, projectConfig *config.Config) string {
	test.Helper()
	root := filepath.Join(test.TempDir(), project)
	if err := os.MkdirAll(root, 0o755); err != nil {
		test.Fatal(err)
	}
	if projectConfig == nil {
		projectConfig = &config.Config{Agent: config.AgentConfig{Tools: []string{"opencode"}, DefaultTool: "opencode"}}
	}
	if err := config.WriteProject(root, projectConfig); err != nil {
		test.Fatal(err)
	}
	index, err := state.LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if index.Projects == nil {
		index.Projects = map[string]state.ProjectIndexEntry{}
	}
	index.Projects[project] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
	return root
}

func testManager(runner *nono.FakeRunner, keys *fakeKeyMinter, creds *fakeCredentialStore) Manager {
	return Manager{
		Runner:      runner,
		Keys:        keys,
		Served:      fakeServedModels{},
		Credentials: creds,
		Now:         func() string { return "2026-10-09T00:00:00Z" },
	}
}

func TestNameIsAipPrefixed(test *testing.T) {
	if got := Name("my-app"); got != "aip-my-app" {
		test.Errorf("Name(my-app) = %q, want aip-my-app", got)
	}
}

func TestAgentCLINamesIncludesEveryValidCLI(test *testing.T) {
	names := AgentCLINames()
	for _, want := range []string{"opencode", "omp", "claude-code", "codex", "gemini", "hermes"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			test.Errorf("AgentCLINames() missing %q: %v", want, names)
		}
	}
}

func TestAgentUnknownProjectAndCLI(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	seedProject(test, "demo", nil)
	runner := &nono.FakeRunner{}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())

	if _, err := manager.Agent("ghost-project", "opencode"); !errors.Is(err, ErrUnknownProject) {
		test.Errorf("unknown project: err = %v, want ErrUnknownProject", err)
	}
	if _, err := manager.Agent("demo", "not-a-cli"); !errors.Is(err, ErrUnknownAgentCLI) {
		test.Errorf("unknown agent CLI: err = %v, want ErrUnknownAgentCLI", err)
	}
}

// TestAgentOAuthModeSkipsKeyMinting verifies an OAuth-mode agent launches without
// minting/storing a scoped gateway key (its native login is used instead), and
// without an EnvCredentialMap entry on the nono run.
func TestAgentOAuthModeSkipsKeyMinting(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := seedProject(test, "demo", &config.Config{
		Agent: config.AgentConfig{
			Tools: []string{"claude-code"}, DefaultTool: "claude-code",
			AuthModes: map[string]string{"claude-code": "oauth"},
		},
	})
	// Put a fake "claude" binary on PATH so exec.LookPath succeeds.
	fakeBinOnPath(test, "claude")

	runner := &nono.FakeRunner{RunDetachedSessionID: "session-123"}
	keys := &fakeKeyMinter{}
	creds := newFakeCredentialStore()
	manager := testManager(runner, keys, creds)

	handle, err := manager.Agent("demo", "claude-code")
	if err != nil {
		test.Fatalf("Agent: %v", err)
	}
	if handle.ID != "session-123" {
		test.Errorf("handle.ID = %q, want session-123", handle.ID)
	}
	if len(keys.generated) != 0 {
		test.Errorf("oauth mode must not mint a scoped key: %v", keys.generated)
	}
	if len(creds.stored) != 0 {
		test.Errorf("oauth mode must not store any credential: %v", creds.stored)
	}
	if len(runner.RunDetachedCalls) != 1 {
		test.Fatalf("expected exactly one RunDetached call, got %d", len(runner.RunDetachedCalls))
	}
	if len(runner.RunDetachedCalls[0].EnvCredentialMap) != 0 {
		test.Errorf("oauth mode must not inject an env credential: %+v", runner.RunDetachedCalls[0].EnvCredentialMap)
	}

	// The claude settings file was written keyless at the project's default location.
	settingsPath := filepath.Join(root, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		test.Errorf("claude settings.json not written: %v", err)
	}
}

// TestAgentAPIKeyModeMintsAndStoresKey verifies the default (api-key) mode mints a
// scoped key, stores it ONLY via the injected CredentialStore (never touching
// EnvCredentialMap's destination env var directly as plaintext), and references it
// by name in the nono run's EnvCredentialMap.
func TestAgentAPIKeyModeMintsAndStoresKey(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	seedProject(test, "demo", &config.Config{
		Agent: config.AgentConfig{Tools: []string{"opencode"}, DefaultTool: "opencode"},
	})
	fakeBinOnPath(test, "opencode")

	runner := &nono.FakeRunner{RunDetachedSessionID: "session-abc"}
	keys := &fakeKeyMinter{generatedKey: "sk-scoped-xyz"}
	creds := newFakeCredentialStore()
	manager := testManager(runner, keys, creds)

	if _, err := manager.Agent("demo", "opencode"); err != nil {
		test.Fatalf("Agent: %v", err)
	}
	if len(keys.generated) != 1 || keys.generated[0].Alias != "demo" {
		test.Fatalf("expected one key minted with alias demo, got %+v", keys.generated)
	}
	// A prior key (if any) is revoked by alias before minting a fresh one.
	if len(keys.deletedAlias) != 1 || keys.deletedAlias[0] != "demo" {
		test.Errorf("expected a best-effort revoke by alias demo, got %v", keys.deletedAlias)
	}
	credentialKey := "aip-gateway-demo"
	if creds.stored[credentialKey] != "sk-scoped-xyz" {
		test.Errorf("scoped key not stored under %q: %v", credentialKey, creds.stored)
	}
	if len(runner.RunDetachedCalls) != 1 {
		test.Fatalf("expected exactly one RunDetached call, got %d", len(runner.RunDetachedCalls))
	}
	envMap := runner.RunDetachedCalls[0].EnvCredentialMap
	if envMap[credentialKey] != agentcfg.CodexKeyEnvVar {
		test.Errorf("EnvCredentialMap = %v, want %q -> %q", envMap, credentialKey, agentcfg.CodexKeyEnvVar)
	}
}

func TestShellUsesProjectConfiguredShell(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	seedProject(test, "demo", &config.Config{
		Agent:     config.AgentConfig{Tools: []string{"opencode"}, DefaultTool: "opencode"},
		Workspace: config.WorkspaceConfig{Shell: "zsh"},
	})
	runner := &nono.FakeRunner{}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())

	if err := manager.Shell("demo"); err != nil {
		test.Fatalf("Shell: %v", err)
	}
	if len(runner.ShellCalls) != 1 || len(runner.ShellCalls[0].Command) != 1 || runner.ShellCalls[0].Command[0] != "zsh" {
		test.Fatalf("Shell command = %+v, want [zsh]", runner.ShellCalls)
	}
}

func TestShellDefaultsToBash(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	seedProject(test, "demo", nil)
	runner := &nono.FakeRunner{}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())

	if err := manager.Shell("demo"); err != nil {
		test.Fatalf("Shell: %v", err)
	}
	if len(runner.ShellCalls) != 1 || runner.ShellCalls[0].Command[0] != "bash" {
		test.Fatalf("Shell command = %+v, want [bash]", runner.ShellCalls)
	}
}

func TestAttachRejectsEmptySessionID(test *testing.T) {
	runner := &nono.FakeRunner{}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())
	if err := manager.Attach(""); !errors.Is(err, ErrNoActiveSession) {
		test.Errorf("Attach(\"\") = %v, want ErrNoActiveSession", err)
	}
	if err := manager.Attach("session-1"); err != nil {
		test.Errorf("Attach: %v", err)
	}
	if len(runner.AttachCalls) != 1 || runner.AttachCalls[0] != "session-1" {
		test.Errorf("AttachCalls = %v, want [session-1]", runner.AttachCalls)
	}
}

// TestListSessionsFiltersByWorkdir verifies ListSessions returns only the sessions
// whose Workdir matches the project's root (nono's own `ps` is global across every
// project on the host).
func TestListSessionsFiltersByWorkdir(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := seedProject(test, "demo", nil)
	seedProject(test, "other", nil)

	runner := &nono.FakeRunner{Sessions: []nono.Session{
		{SessionID: "a", Workdir: root, Status: "running"},
		{SessionID: "b", Workdir: "/somewhere/else", Status: "running"},
		{SessionID: "c", Workdir: root, Status: "exited"},
	}}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())

	sessions, err := manager.ListSessions("demo")
	if err != nil {
		test.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		test.Fatalf("ListSessions = %+v, want 2 entries for root %q", sessions, root)
	}
}

func TestStopRejectsEmptySessionID(test *testing.T) {
	runner := &nono.FakeRunner{}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())
	if err := manager.Stop("", false); !errors.Is(err, ErrNoActiveSession) {
		test.Errorf("Stop(\"\", false) = %v, want ErrNoActiveSession", err)
	}
	if err := manager.Stop("session-1", true); err != nil {
		test.Errorf("Stop: %v", err)
	}
	if len(runner.StopCalls) != 1 || !runner.StopCalls[0].Force {
		test.Errorf("StopCalls = %+v, want one forced stop", runner.StopCalls)
	}
}

// TestStopAllStopsOnlyRunningSessions verifies StopAll stops every RUNNING session
// for the project (skipping already-exited ones) and records the workspace as
// stopped.
func TestStopAllStopsOnlyRunningSessions(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := seedProject(test, "demo", nil)
	runner := &nono.FakeRunner{Sessions: []nono.Session{
		{SessionID: "a", Workdir: root, Status: "running"},
		{SessionID: "b", Workdir: root, Status: "running"},
		{SessionID: "c", Workdir: root, Status: "exited"},
	}}
	manager := testManager(runner, &fakeKeyMinter{}, newFakeCredentialStore())

	if err := manager.StopAll("demo"); err != nil {
		test.Fatalf("StopAll: %v", err)
	}
	if len(runner.StopCalls) != 2 {
		test.Fatalf("StopCalls = %+v, want exactly 2 (only the running sessions)", runner.StopCalls)
	}
}

// TestDeleteCredentialsRevokesKeyAndCredential verifies DeleteCredentials revokes
// the gateway key by alias AND removes it from the (fake) OS keystore — never the
// real one.
func TestDeleteCredentialsRevokesKeyAndCredential(test *testing.T) {
	keys := &fakeKeyMinter{}
	creds := newFakeCredentialStore()
	creds.stored["aip-gateway-demo"] = "sk-whatever"
	manager := testManager(&nono.FakeRunner{}, keys, creds)

	if err := manager.DeleteCredentials("demo"); err != nil {
		test.Fatalf("DeleteCredentials: %v", err)
	}
	if len(keys.deletedAlias) != 1 || keys.deletedAlias[0] != "demo" {
		test.Errorf("expected key revoked by alias demo, got %v", keys.deletedAlias)
	}
	if _, present := creds.stored["aip-gateway-demo"]; present {
		test.Error("credential should be removed from the (fake) keystore")
	}
}

// fakeBinOnPath puts an executable fake binary named name on PATH for the
// duration of the test, so exec.LookPath(name) succeeds without needing the
// real agent CLI installed.
func fakeBinOnPath(test *testing.T, name string) {
	test.Helper()
	dir := test.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		test.Fatal(err)
	}
	test.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
