// Package workspace manages per-project agent sessions sandboxed by nono
// (https://nono.sh) — the host-process capability sandbox (Landlock on Linux,
// Seatbelt on macOS) that replaced Microsandbox microVMs as the workspace
// runtime. There is no VM, no OS image, and no persistent boot: a "workspace"
// is a registered project directory (internal/project) with a generated nono
// profile (internal/nono); each agent invocation is its own nono session,
// started/stopped/attached independently via nono's own session lifecycle
// (nono run --detached / attach / stop / ps / inspect).
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

// Errors returned by Manager methods (exit-code mapping lives in internal/cli).
var (
	ErrUnknownProject  = errors.New("unknown workspace")
	ErrUnknownAgentCLI = errors.New("unknown agent CLI")
	ErrNoActiveSession = errors.New("no active session for this workspace")
	ErrNonoMissing     = errors.New("nono is not installed")
)

// Name returns the sandbox-facing identifier for a project. Kept for
// continuity with existing naming/display even though it no longer names a
// microVM — it's used as the nono session --name prefix and in log/UI text.
func Name(project string) string { return "aip-" + project }

// Session is a live or recent nono session for a project (CLI/TUI-facing alias
// so callers don't need to import internal/nono directly for this one type).
type Session = nono.Session

// KeyMinter mints/revokes the per-project scoped LiteLLM virtual key.
type KeyMinter interface {
	GenerateKey(scope litellm.KeyScope) (string, error)
	DeleteKeyByAlias(alias string) error
	KeyInfo(key string) (litellm.KeyDetails, error)
}

// ServedModels lists the models the gateway currently serves, for the agent
// CLI model picker.
type ServedModels interface {
	ServedModels() ([]agentcfg.Model, error)
}

// CredentialStore stores/removes a project's scoped gateway key in the OS
// keystore (internal/nono.StoreCredential/DeleteCredential), kept as an
// injectable seam — like Runner/Keys/Served — so Manager is unit-testable
// without touching the real OS keychain.
type CredentialStore interface {
	StoreCredential(name, value string) error
	DeleteCredential(name string) error
}

// Manager coordinates per-project agent sessions over a nono.Runner, minting
// LiteLLM keys via Keys, listing served models via Served, and storing the
// scoped gateway key via Credentials. Now stamps state with an RFC 3339 UTC
// timestamp.
type Manager struct {
	Runner      nono.Runner
	Keys        KeyMinter
	Served      ServedModels
	Credentials CredentialStore
	Now         func() string
}

// RealManager builds a production Manager: the real nono CLI, a real LiteLLM
// key manager over the detected container runtime, the real OS keychain, and
// now() = time.Now in UTC.
func RealManager(now func() string) Manager {
	prober := runtime.RealProber()
	keyManager := litellm.NewKeyManager(prober)
	return Manager{
		Runner:      nono.RealRunner{},
		Keys:        keyManager,
		Served:      servedModelsClient{manager: keyManager},
		Credentials: realCredentialStore{},
		Now:         now,
	}
}

// realCredentialStore is the production CredentialStore: the real OS keychain
// via internal/nono.
type realCredentialStore struct{}

func (realCredentialStore) StoreCredential(name, value string) error {
	return nono.StoreCredential(name, value)
}
func (realCredentialStore) DeleteCredential(name string) error {
	return nono.DeleteCredential(name)
}

// servedModelsClient adapts a *litellm.KeyManager to the ServedModels interface.
type servedModelsClient struct{ manager *litellm.KeyManager }

func (client servedModelsClient) ServedModels() ([]agentcfg.Model, error) {
	models, err := client.manager.ListModels()
	if err != nil {
		return nil, err
	}
	served := make([]agentcfg.Model, 0, len(models))
	for _, model := range models {
		if model.Name != "" {
			served = append(served, agentcfg.Model{Name: model.Name, Tools: model.SupportsTools})
		}
	}
	return served, nil
}

// resolveGateway derives the model-gateway host, port, and base URL every
// agent CLI on this machine routes through. It reads the configured
// AIPlatformHost from runtime.yaml (set machine-wide by `ai gateway set`) and
// applies runtime.ResolveGateway's rules. A missing/unreadable runtime.yaml
// falls back to plain loopback (runtime.DefaultGatewayHost) — nono-sandboxed
// agents run directly on the host, so there is no guest-network translation.
func resolveGateway() (host string, port int, url string) {
	info, err := runtime.Load()
	if err != nil || info == nil {
		return runtime.ResolveGateway("")
	}
	return runtime.ResolveGateway(info.HostAddress())
}

// resolveProjectRoot resolves a project name to its registered root directory.
func resolveProjectRoot(project string) (string, error) {
	index, err := state.LoadIndex()
	if err != nil {
		return "", err
	}
	entry, ok := index.Projects[project]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownProject, project)
	}
	return entry.Path, nil
}

// agentSpec describes how to launch one agent CLI: its PATH binary name and
// (for api-key mode) the destination env var nono injects the scoped gateway
// key under. Agent CLIs with no subscription login (opencode/omp/hermes) share
// AIP_GATEWAY_KEY (agentcfg.CodexKeyEnvVar) with codex; claude/gemini use their
// own provider-specific env var names.
type agentSpec struct {
	binary           string
	credentialEnvVar string
}

var agentValidCLIs = map[string]agentSpec{
	"opencode":    {binary: "opencode", credentialEnvVar: agentcfg.CodexKeyEnvVar},
	"omp":         {binary: "omp", credentialEnvVar: agentcfg.CodexKeyEnvVar},
	"claude-code": {binary: "claude", credentialEnvVar: agentcfg.ClaudeAuthEnvVar},
	"codex":       {binary: "codex", credentialEnvVar: agentcfg.CodexKeyEnvVar},
	"gemini":      {binary: "gemini", credentialEnvVar: agentcfg.GeminiKeyEnvVar},
	"hermes":      {binary: "hermes", credentialEnvVar: agentcfg.CodexKeyEnvVar},
}

// nonoPackForCLI names the nono registry pack (https://nono.sh) a project's
// generated profile extends, for the CLIs with an official pack providing
// battle-tested filesystem/OAuth-flow grants. A CLI with no pack falls back to
// nono's own built-in "default" profile.
var nonoPackForCLI = map[string]string{
	"opencode":    "nolabs-ai/opencode",
	"claude-code": "nolabs-ai/claude",
	"codex":       "nolabs-ai/codex",
}

// AgentCLINames returns the sorted set of agent CLI names the platform can
// launch (for shell completion and help text).
func AgentCLINames() []string {
	names := make([]string, 0, len(agentValidCLIs))
	for name := range agentValidCLIs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// credentialName is the OS-keystore entry name for a project's scoped gateway
// key (internal/nono.StoreCredential/DeleteCredential) — stable per project so
// re-minting overwrites in place rather than accumulating keychain entries.
func credentialName(project string) string {
	return "aip-gateway-" + project
}

// Agent launches (as a detached nono session) the given agent CLI sandboxed to
// project's root directory, wired to the LiteLLM gateway. It mints a fresh
// scoped virtual key (revoking any prior one), stores it ONLY in the OS
// keystore (never on disk/in a config file — internal/nono.StoreCredential),
// writes the selected CLI's keyless host-side config, and generates the
// project's nono profile before launching. Returns the recorded state handle.
func (manager Manager) Agent(project, cli string) (*state.Workspace, error) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return nil, err
	}
	projectConfig, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	spec, ok := agentValidCLIs[cli]
	if !ok {
		return nil, fmt.Errorf("%w %q (valid: %s)", ErrUnknownAgentCLI, cli, strings.Join(AgentCLINames(), ", "))
	}
	if _, err := exec.LookPath(spec.binary); err != nil {
		return nil, fmt.Errorf("%s is not installed on this host (run `ai setup` to install it): %w", cli, err)
	}

	_, _, gatewayURL := resolveGateway()
	oauthMode := config.IsOAuthEligible(cli) && projectConfig.Agent.AuthMode(cli) == "oauth"

	var envCredentialMap map[string]string
	if !oauthMode {
		key, err := manager.ensureScopedKey(project)
		if err != nil {
			return nil, err
		}
		envCredentialMap = map[string]string{credentialName(project): spec.credentialEnvVar}
		if err := manager.writeAgentConfig(root, cli, projectConfig, gatewayURL, key); err != nil {
			return nil, err
		}
	} else if err := manager.writeAgentConfig(root, cli, projectConfig, gatewayURL, ""); err != nil {
		return nil, err
	}

	manager.registerAITools(root, projectConfig)

	profilePath, err := ensureProfile(root, cli, projectConfig, gatewayURL)
	if err != nil {
		return nil, err
	}

	launch := []string{spec.binary}
	if wrapped, ok := agentcfg.HeadroomWrapName(cli); ok {
		if _, err := exec.LookPath("headroom"); err == nil {
			launch = []string{"headroom", "wrap", wrapped}
		}
	}

	sessionID, err := manager.Runner.RunDetached(context.Background(), nono.RunOptions{
		Profile:          profilePath,
		Name:             cli,
		AllowCWD:         true,
		Dir:              root,
		Command:          launch,
		EnvCredentialMap: envCredentialMap,
	})
	if err != nil {
		return nil, err
	}

	handle := &state.Workspace{
		SchemaVersion: state.SchemaVersion,
		ID:            sessionID,
		Project:       project,
		Status:        state.StatusStarted,
		Created:       manager.Now(),
		LastStarted:   manager.Now(),
	}
	if err := state.OpenStore(root).SaveWorkspace(handle); err != nil {
		return nil, err
	}
	return handle, nil
}

// ensureScopedKey mints a fresh LiteLLM virtual key scoped to project (revoking
// any prior one by alias) and stores it ONLY in the OS keystore, returning it
// so the caller can also write it into the config generated THIS call (the key
// itself is never retained on disk — only its keystore reference name is).
func (manager Manager) ensureScopedKey(project string) (string, error) {
	if manager.Keys == nil {
		return "", fmt.Errorf("no key manager configured")
	}
	_ = manager.Keys.DeleteKeyByAlias(project) // best-effort revoke of a prior key
	key, err := manager.Keys.GenerateKey(litellm.KeyScope{Alias: project})
	if err != nil {
		return "", fmt.Errorf("mint scoped gateway key: %w", err)
	}
	if err := manager.Credentials.StoreCredential(credentialName(project), key); err != nil {
		return "", fmt.Errorf("store gateway key in OS keystore: %w", err)
	}
	return key, nil
}

// Shell launches an interactive nono shell sandboxed to project's root
// directory (no specific agent CLI — just a shell inside the sandbox).
func (manager Manager) Shell(project string) error {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return err
	}
	projectConfig, err := config.Load(root)
	if err != nil {
		return err
	}
	manager.registerAITools(root, projectConfig)
	profilePath, err := ensureProfile(root, "", projectConfig, "")
	if err != nil {
		return err
	}
	shellBinary := projectConfig.Workspace.Shell
	if shellBinary == "" {
		shellBinary = "bash"
	}
	return manager.Runner.Shell(context.Background(), nono.RunOptions{
		Profile:  profilePath,
		AllowCWD: true,
		Dir:      root,
		Command:  []string{shellBinary},
	})
}

// Attach attaches to an existing nono session by id (or name/prefix).
func (manager Manager) Attach(sessionID string) error {
	if sessionID == "" {
		return ErrNoActiveSession
	}
	return manager.Runner.Attach(context.Background(), sessionID)
}

// ListSessions returns the live nono sessions launched for project (filtered
// by working directory, since nono's own `ps` is global across all projects).
func (manager Manager) ListSessions(project string) ([]nono.Session, error) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return nil, err
	}
	all, err := manager.Runner.ListSessions(context.Background(), false)
	if err != nil {
		return nil, err
	}
	filtered := make([]nono.Session, 0, len(all))
	for _, session := range all {
		if session.Workdir == root {
			filtered = append(filtered, session)
		}
	}
	return filtered, nil
}

// Stop stops a session by id. Use ListSessions to resolve a project's sessions
// first if the caller only has a project name.
func (manager Manager) Stop(sessionID string, force bool) error {
	if sessionID == "" {
		return ErrNoActiveSession
	}
	return manager.Runner.Stop(context.Background(), sessionID, force)
}

// StopAll stops every live session belonging to project.
func (manager Manager) StopAll(project string) error {
	sessions, err := manager.ListSessions(project)
	if err != nil {
		return err
	}
	var firstErr error
	for _, session := range sessions {
		if session.Status != "running" {
			continue
		}
		if err := manager.Stop(session.SessionID, false); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	root, err := resolveProjectRoot(project)
	if err == nil {
		_ = state.OpenStore(root).SaveWorkspace(&state.Workspace{
			SchemaVersion: state.SchemaVersion, Project: project,
			Status: state.StatusStopped, LastStarted: manager.Now(),
		})
	}
	return firstErr
}

// DeleteCredentials removes a project's scoped key from the OS keystore and
// revokes it at the gateway. Best-effort — called from `ai delete`.
func (manager Manager) DeleteCredentials(project string) error {
	if manager.Keys != nil {
		_ = manager.Keys.DeleteKeyByAlias(project)
	}
	return manager.Credentials.DeleteCredential(credentialName(project))
}
