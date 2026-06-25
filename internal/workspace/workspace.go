// Package workspace coordinates the lifecycle of a project's workspace microVM
// (arch §7): build the OCI image from the project's Dockerfile, create/start the
// Microsandbox microVM, exec into it, and stop/destroy it — recording each
// transition in the project's run state. The OCI build and microVM operations
// are behind the Builder and Sandbox interfaces so the orchestration is
// unit-tested with fakes and the real impls run on a provisioned host.
package workspace

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/agentcfg"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// resolveGateway derives the model-gateway host, port, and base URL every
// workspace microVM on this machine routes through (arch §29.2). It reads the
// configured AIPlatformHost from runtime.yaml (set machine-wide by `ai gateway
// set`) and applies runtime.ResolveGateway's rules. A missing or unreadable
// runtime.yaml falls back to the local standalone gateway
// (host.microsandbox.internal:18787), so a freshly-set-up host always resolves.
func resolveGateway() (host string, port int, url string) {
	info, err := runtime.Load()
	if err != nil || info == nil {
		return runtime.ResolveGateway("")
	}
	return runtime.ResolveGateway(info.HostAddress())
}

// Guest paths the agent provider configs are written to inside the microVM. The
// workspace image creates a `workspace` user; both files live under its home.
const (
	openCodeGuestPath = "/home/workspace/.config/opencode/opencode.json"
	piGuestPath       = "/home/workspace/.pi/agent/models.json"
	tmuxConfGuestPath = "/home/workspace/.tmux.conf"
)

// shellSessionName is the tmux session that backs the default interactive shell
// (`ai shell` / `ai attach` with no session). Per-agent sessions are named after
// the agent CLI (opencode, pi, …).
const shellSessionName = "shell"

// workspaceWorkdir is the guest path the project source is mounted at and where
// every tmux session opens (matches the microVM --workdir in Create).
const workspaceWorkdir = "/workspace"

// ErrUnknownProject is returned when a project name is not in the global index
// (→ exit 2).
var ErrUnknownProject = errors.New("unknown project")

// ErrNotStarted is returned when an operation needs a RUNNING workspace but it is
// stopped or was never started (→ exit 2). The message is the user-facing nudge to
// start it; it covers both cases (the platform tracks the running state in the
// lifecycle handle, set by start/stop).
var ErrNotStarted = errors.New("workspace is not running — run `ai workspace start` first")

// ErrAlreadyStopped lets a Sandbox.Stop signal that the microVM was already
// stopped. Restart treats this as a no-op (it only needs the microVM down before
// starting it again) rather than a failure.
var ErrAlreadyStopped = errors.New("workspace microVM is already stopped")

// ErrTmuxMissing is returned when the workspace image has no tmux, which the
// persistent-session model (shell/agent/attach) requires. It carries the
// remediation so the user is not left with msb's raw "failed to exec tmux" leak
// (which also misreports as a successful exit). Mapped to exit 3 (missing dep).
var ErrTmuxMissing = errors.New("tmux is not installed in the workspace image — add `tmux` to <project>/.ai-platform/Dockerfile and restart the workspace (rebuild), or recreate the project with an up-to-date `ai`")

// Name derives the deterministic workspace/microVM name (arch §7, §19):
// aip-<project>. There is one workspace per project.
func Name(project string) string {
	return "aip-" + project
}

// ErrUnknownAgentCLI is returned when `ai agent <cli>` names a CLI the platform
// does not know how to launch (→ exit 2). Its message lists the valid set.
var ErrUnknownAgentCLI = errors.New("unknown agent CLI")

// Session is one tmux session inside the workspace microVM — a persistent,
// reattachable shell or agent CLI. The platform exposes these via `ai sessions`.
type Session struct {
	// Name is the tmux session name ("shell" for the default shell; the agent CLI
	// name — opencode, pi, … — for an agent session).
	Name string `json:"name"`
	// Attached is true when a client is currently attached to the session.
	Attached bool `json:"attached"`
	// Activity is tmux's raw last-activity value (a Unix epoch string).
	Activity string `json:"activity"`
}

// ExecResult is the outcome of running a command inside a workspace (§4.5). The
// inner command's exit code is reported separately from the platform's.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// NetworkPolicy is the egress policy actually IN FORCE on a running microVM, read
// back from `msb inspect`. It is the applied POLICY (what would be blocked), not a
// record of blocked connections — msb 0.5.7 does not expose per-connection denials.
type NetworkPolicy struct {
	// DefaultEgress is the default outbound posture in force (e.g. "deny", "allow").
	DefaultEgress string `json:"default_egress"`
	// Rules are the applied egress allow/deny rules, each rendered as a single
	// human line (e.g. "allow egress example.com tcp 443").
	Rules []string `json:"rules,omitempty"`
	// OnViolation is the secrets-broker violation posture (e.g. "block-and-log"),
	// empty if msb did not report one.
	OnViolation string `json:"on_violation,omitempty"`
}

// Builder builds the workspace OCI image from <projectRoot>/.ai-platform/Dockerfile.
type Builder interface {
	Build(projectRoot, imageRef string) error
}

// Sandbox drives Microsandbox microVMs (Go SDK / msb). Create mounts the
// read-only image, the host project source, and the persistent overlay (arch
// §26) as a volume, and applies the project's egress policy via netArgs (the
// `msb create` network-rule fragment from egress.MsbNetworkArgs).
type Sandbox interface {
	Create(name, imageRef, projectMount, overlayPath string, netArgs []string) error
	Start(name string) error
	Stop(name string) error
	Destroy(name string) error
	Exec(name string, argv []string) (ExecResult, error)
	// ExecInteractive runs argv inside the running microVM with the CALLER'S
	// terminal attached — a real PTY via `msb exec -t`, with stdin/stdout/stderr
	// wired straight through — for interactive shells and agent CLIs. Only an
	// infrastructure failure (microVM down, msb missing) is returned; the inner
	// program's own exit (the user ending the session) is not an error.
	ExecInteractive(name string, argv []string) error
	// WriteFile writes content to guestPath inside the running microVM, creating
	// parent directories. name is the human label only used for error context.
	WriteFile(name, guestPath string, content []byte) error
	// InspectNetwork reads the egress policy in force on the named microVM via
	// `msb inspect`. It returns ErrNotRunning when no such sandbox exists (the
	// workspace is not running) and ErrMsbMissing when msb is not installed —
	// both of which callers treat as "show the declared policy only", not an error.
	InspectNetwork(name string) (NetworkPolicy, error)
}

// KeyMinter mints (and rotates) scoped LiteLLM virtual keys. It is the small
// surface Manager needs from litellm.KeyManager, defined locally so tests can
// supply a fake without a live gateway (the real impl is *litellm.KeyManager).
// DeleteKeyByAlias lets a re-start revoke the previous key for a project before
// minting a fresh one — LiteLLM requires key aliases to be unique, so without the
// revoke a second start fails ("alias already exists").
type KeyMinter interface {
	GenerateKey(scope litellm.KeyScope) (string, error)
	DeleteKeyByAlias(alias string) error
}

// ModelLister lists the locally-installed Ollama models so the in-VM agent model
// picker can offer them alongside the named aliases and the curated cloud seed. It
// is the small surface Manager needs from ollama.Client, defined locally so tests
// can supply a fake without a live Ollama service (the real impl is
// ollama.RealClient()). A nil lister, or a List that errors (Ollama down at start),
// degrades gracefully — the workspace still starts with aliases + the cloud seed.
type ModelLister interface {
	List() ([]ollama.Model, error)
}

// Manager coordinates the lifecycle over a Builder + Sandbox, stamping state with
// Now (RFC 3339 UTC). GOOS records the host OS for any host-specific behavior
// (supported hosts: macOS and Linux).
type Manager struct {
	Builder Builder
	Sandbox Sandbox
	Keys    KeyMinter
	// Ollama lists the installed local models for the in-VM agent model picker. It
	// is optional: a nil lister (or a List error) skips the local models without
	// failing the workspace start.
	Ollama ModelLister
	Now    func() string
	GOOS   string
}

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

// Start builds the image and creates+starts the project's workspace microVM,
// recording a started handle. Idempotent enough to re-run (recreates the handle).
func (manager Manager) Start(project string) (*state.Workspace, error) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return nil, err
	}
	name := Name(project)
	imageRef := name + ":latest"
	if err := manager.Builder.Build(root, imageRef); err != nil {
		return nil, err
	}
	// Ensure the persistent overlay before create so installs + agent state
	// survive restart/recreation (arch §26). Re-ensuring re-uses the same
	// directory, so a recreated workspace keeps its prior contents.
	overlayPath, err := overlay.Ensure(name)
	if err != nil {
		return nil, err
	}
	// Translate the project's egress policy into the msb network argv fragment;
	// the configured model gateway is always allowed on its Headroom port (arch
	// §29.2). In standalone/local mode this is host.microsandbox.internal:18787;
	// in client mode it is the remote server `ai gateway set` configured.
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		return nil, err
	}
	gatewayHost, gatewayPort, gatewayURL := resolveGateway()
	netArgs := egress.MsbNetworkArgs(projectConfig.Network, gatewayHost, gatewayPort)
	// The microVM mounts the host project path directly. Supported hosts are
	// macOS and Linux, so no path translation is needed (arch §7).
	if err := manager.Sandbox.Create(name, imageRef, root, overlayPath, netArgs); err != nil {
		return nil, err
	}
	if err := manager.Sandbox.Start(name); err != nil {
		return nil, err
	}
	// The microVM is now running but no state handle is saved yet. Arm a
	// best-effort rollback so that ANY failure on the remaining post-start steps
	// (agent-provider registration AND the state-handle write) tears the microVM
	// down rather than leaking an untracked running VM — one that no saved handle
	// could later reap (DestroyIfPresent keys off the handle). It is disarmed only
	// once the handle is saved, and never masks the original error.
	started := true
	defer func() {
		if started {
			_ = manager.Sandbox.Destroy(name)
		}
	}()
	// Register the agent CLIs' provider config so opencode/pi inside the microVM
	// talk to the host Headroom proxy through a per-workspace scoped LiteLLM
	// virtual key (arch §15, §17). The key flows host→VM only; it is never
	// written to platform disk.
	if err := manager.registerAgentProviders(name, project, projectConfig, gatewayURL); err != nil {
		return nil, err
	}
	now := manager.Now()
	handle := &state.Workspace{
		ID:          name,
		Project:     project,
		Status:      state.StatusStarted,
		Created:     now,
		LastStarted: now,
	}
	if err := state.OpenStore(root).SaveWorkspace(handle); err != nil {
		return nil, err
	}
	started = false // handle saved — disarm the rollback, keep the running microVM
	return handle, nil
}

// registerAgentProviders mints a scoped LiteLLM virtual key for the workspace
// and writes the opencode + pi provider configs into the running microVM so the
// agent CLIs reach the host Headroom proxy with that key. The per-project
// Headroom strategy maps to the two per-request knobs opencode bakes into each
// model's request body; pi cannot inject per-request fields and uses Headroom's
// server-side defaults (see internal/agentcfg).
func (manager Manager) registerAgentProviders(name, project string, projectConfig *config.Config, gatewayURL string) error {
	// Rotate: revoke any key left from a previous start of this project before
	// minting a new one. LiteLLM requires unique key aliases, so re-using the
	// project name as the alias would otherwise fail the second start with "alias
	// already exists". Best-effort — a missing alias (first start) is not an error,
	// and a real gateway problem surfaces on GenerateKey below.
	_ = manager.Keys.DeleteKeyByAlias(project)

	// Empty Models = all models allowed (the workspace agent names any model and
	// LiteLLM routes it). The metadata ties the key back to this workspace.
	apiKey, err := manager.Keys.GenerateKey(litellm.KeyScope{
		Alias:    project,
		Metadata: map[string]any{"workspace": name},
	})
	if err != nil {
		return err
	}

	keepTurns, outputBufferTokens := contextopt.HeadroomParams(projectConfig.Context.Strategy)
	routing := litellm.DefaultRouting()
	models := manager.pickerModels(routing)

	openCodeConfig, err := agentcfg.OpenCodeConfig(gatewayURL, apiKey, routing.Default, models, keepTurns, outputBufferTokens)
	if err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, openCodeGuestPath, openCodeConfig); err != nil {
		return err
	}

	piConfig, err := agentcfg.PiConfig(gatewayURL, apiKey, routing.Default, models)
	if err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, piGuestPath, piConfig); err != nil {
		return err
	}

	// Write the managed tmux.conf so the workspace session model is transparent
	// (mouse scroll, hidden status bar) — the user never types a tmux command.
	return manager.Sandbox.WriteFile(name, tmuxConfGuestPath, agentcfg.TmuxConfig())
}

// pickerModels builds the concrete model list the in-VM agent CLIs offer in their
// picker: the UNION of the named aliases (namedModels), the INSTALLED local Ollama
// models (rendered as "ollama/<Name>"), and the maintainer-curated cloud seed
// (agentcfg.CloudModels). The set is deduped and sorted for a deterministic config.
//
// The local-model lookup DEGRADES GRACEFULLY: a nil lister or a List error (Ollama
// down at workspace start) simply skips the local models — the picker still offers
// the aliases + cloud seed, and the workspace start never fails over a model-list
// lookup. LiteLLM's per-provider wildcards still route any model the agent names by
// hand regardless of what the picker lists.
func (manager Manager) pickerModels(routing litellm.Routing) []string {
	seen := make(map[string]struct{})
	models := make([]string, 0)
	add := func(model string) {
		if model == "" {
			return
		}
		if _, ok := seen[model]; ok {
			return
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}

	for _, alias := range namedModels(routing) {
		add(alias)
	}
	if manager.Ollama != nil {
		if installed, err := manager.Ollama.List(); err == nil {
			for _, model := range installed {
				add("ollama/" + model.Name)
			}
		}
	}
	for _, model := range agentcfg.CloudModels() {
		add(model)
	}

	sort.Strings(models)
	return models
}

// namedModels enumerates the non-wildcard named aliases from the routing (the
// model handles both agent CLIs can address), sorted for a stable config. The
// per-provider "*" wildcards are skipped — neither opencode nor pi can address
// them as concrete model ids.
func namedModels(routing litellm.Routing) []string {
	models := make([]string, 0, len(routing.Aliases))
	for alias := range routing.Aliases {
		if strings.Contains(alias, "*") {
			continue
		}
		models = append(models, alias)
	}
	sort.Strings(models)
	return models
}

// Stop stops the workspace microVM and marks the handle stopped (state preserved).
func (manager Manager) Stop(project string) error {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return err
	}
	name := Name(project)
	if err := manager.Sandbox.Stop(name); err != nil {
		return err
	}
	return manager.updateStatus(root, name, project, state.StatusStopped)
}

// Restart restarts the EXISTING workspace microVM without rebuilding the OCI
// image: it stops the microVM (tolerating an already-stopped microVM), starts it
// again, and refreshes the handle to a started state with a new LastStarted
// timestamp. It requires a previously-created handle; if the workspace was never
// started it returns ErrNotStarted (→ exit 2) so the caller runs `start` first.
func (manager Manager) Restart(project string) (*state.Workspace, error) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return nil, err
	}
	name := Name(project)
	existing, err := manager.findHandle(root, name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotStarted, project)
	}
	// Stop the existing microVM; an already-stopped microVM is fine for a
	// restart, so tolerate that case and proceed to start.
	if err := manager.Sandbox.Stop(name); err != nil && !errors.Is(err, ErrAlreadyStopped) {
		return nil, err
	}
	if err := manager.Sandbox.Start(name); err != nil {
		return nil, err
	}
	now := manager.Now()
	handle := &state.Workspace{
		ID:             name,
		Project:        project,
		MicrosandboxID: existing.MicrosandboxID,
		Status:         state.StatusStarted,
		Created:        existing.Created,
		LastStarted:    now,
	}
	if err := state.OpenStore(root).SaveWorkspace(handle); err != nil {
		return nil, err
	}
	return handle, nil
}

// findHandle returns the workspace handle for name, or nil if no handle exists
// yet (workspace never started).
func (manager Manager) findHandle(root, name string) (*state.Workspace, error) {
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil {
		return nil, err
	}
	for index := range workspaces {
		if workspaces[index].ID == name {
			return &workspaces[index], nil
		}
	}
	return nil, nil
}

// DestroyIfPresent tears down the project's workspace microVM only when a
// workspace handle exists and is not already destroyed. It is the idempotent
// teardown `ai project delete` layers on before removing project state (CLI
// §3.4): a project that was never started (no handle) is a no-op, so deleting a
// project with no workspace still works. A still-existing handle is destroyed via
// the normal Destroy path (so state is stamped destroyed). Returns nil when there
// is nothing to do.
func (manager Manager) DestroyIfPresent(project string) error {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return err
	}
	handle, err := manager.findHandle(root, Name(project))
	if err != nil {
		return err
	}
	if handle == nil || handle.Status == state.StatusDestroyed {
		return nil // never started, or already torn down
	}
	return manager.Destroy(project)
}

// Destroy removes the microVM/runtime handle only. It is NON-destructive (§4.4):
// the overlay and host source remain, so `start` fully recovers the workspace.
func (manager Manager) Destroy(project string) error {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return err
	}
	name := Name(project)
	if err := manager.Sandbox.Destroy(name); err != nil {
		return err
	}
	return manager.updateStatus(root, name, project, state.StatusDestroyed)
}

// Exec runs argv inside the project workspace and returns the inner result. A
// non-zero inner exit is carried in ExecResult, not as a Go error; only platform
// failures (microVM down, etc.) are returned as errors (§4.5).
func (manager Manager) Exec(project string, argv []string) (ExecResult, error) {
	if _, err := resolveProjectRoot(project); err != nil {
		return ExecResult{}, err
	}
	return manager.Sandbox.Exec(Name(project), argv)
}

// requireRunning verifies the project's workspace microVM is RUNNING before an
// operation that needs it. It reads the platform's own lifecycle handle (set by
// start/stop) rather than poking msb, because `msb exec` against a STOPPED microVM
// hangs (and `msb exec -t` against a missing one can leave the terminal in raw
// mode). A stopped or never-started workspace fails fast with ErrNotStarted, whose
// message tells the user to run `ai workspace start`.
func (manager Manager) requireRunning(project string) error {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return err
	}
	handle, err := manager.findHandle(root, Name(project))
	if err != nil {
		return err
	}
	if handle == nil || handle.Status != state.StatusStarted {
		return ErrNotStarted
	}
	return nil
}

// ExecInteractive runs argv inside the project's running workspace microVM with
// the caller's terminal attached (a real PTY), for interactive shells and agent
// CLIs. It checks the microVM is up first so a not-yet-started workspace fails
// cleanly (ErrNotStarted) instead of attaching a PTY to a missing VM. Only
// infrastructure failures are returned (§4.5).
func (manager Manager) ExecInteractive(project string, argv []string) error {
	if _, err := resolveProjectRoot(project); err != nil {
		return err
	}
	if err := manager.requireRunning(project); err != nil {
		return err
	}
	return manager.Sandbox.ExecInteractive(Name(project), argv)
}

// Shell opens the project's PERSISTENT default shell session — a tmux session
// named "shell". `tmux new-session -A` creates the session on first use and
// re-attaches to it on every later call, so the shell (and anything left running
// in it) survives detaching and is reattachable. The session opens in /workspace
// with a login shell.
func (manager Manager) Shell(project string) error {
	return manager.launchTmuxSession(project, tmuxNewSession(shellSessionName, []string{"bash", "-l"}))
}

// launchTmuxSession is the shared entry for the tmux-backed interactive sessions
// (shell / attach / agent). It verifies the microVM is up (clean ErrNotStarted if
// not) and that tmux is present in the image (ErrTmuxMissing with remediation if
// not — rather than msb's raw "failed to exec tmux" leak, which also misreports
// success), then attaches the PTY.
func (manager Manager) launchTmuxSession(project string, argv []string) error {
	if _, err := resolveProjectRoot(project); err != nil {
		return err
	}
	if err := manager.requireRunning(project); err != nil {
		return err
	}
	if err := manager.requireTmux(project); err != nil {
		return err
	}
	return manager.Sandbox.ExecInteractive(Name(project), argv)
}

// requireTmux verifies tmux is on PATH inside the running microVM before a tmux
// session is launched. It runs a buffered probe (not the interactive PTY), so a
// missing tmux surfaces as a clear ErrTmuxMissing instead of msb failing to exec
// tmux through the PTY (which returns a misleading success).
func (manager Manager) requireTmux(project string) error {
	result, err := manager.Sandbox.Exec(Name(project), []string{"sh", "-c", "command -v tmux >/dev/null 2>&1"})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return ErrTmuxMissing
	}
	return nil
}

// Attach opens (creating it if needed) the named tmux session in the project's
// workspace microVM. A missing/empty session name attaches the default "shell"
// session. When the session is created fresh it has no command, so it opens the
// image's default login shell in /workspace; an existing session is reattached
// as-is. This backs `ai attach [session]` and the TUI Sessions view.
func (manager Manager) Attach(project, session string) error {
	if session == "" {
		session = shellSessionName
	}
	return manager.launchTmuxSession(project, tmuxNewSession(session, nil))
}

// Agent starts (or reattaches to) a per-CLI tmux session running the named agent
// CLI in /workspace. The session is named after the CLI (opencode, pi, …) so each
// agent has one persistent, reattachable session and multiple agents can run
// concurrently. An unknown CLI returns ErrUnknownAgentCLI (→ exit 2).
func (manager Manager) Agent(project, cli string) error {
	launch, err := agentLaunchCommand(cli)
	if err != nil {
		return err
	}
	return manager.launchTmuxSession(project, tmuxNewSession(cli, launch))
}

// ListSessions returns the tmux sessions running in the project's workspace
// microVM. When no tmux server is running yet (no sessions have been opened) tmux
// exits non-zero with "no server running" — that is ZERO sessions, not an error.
func (manager Manager) ListSessions(project string) ([]Session, error) {
	if _, err := resolveProjectRoot(project); err != nil {
		return nil, err
	}
	// A not-yet-started workspace has no microVM to query — report that cleanly
	// rather than surfacing a raw msb "sandbox not found" error.
	if err := manager.requireRunning(project); err != nil {
		return nil, err
	}
	result, err := manager.Sandbox.Exec(Name(project), []string{
		"tmux", "list-sessions", "-F", "#{session_name}\t#{session_attached}\t#{session_activity}",
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		// `tmux list-sessions` with no running server is a non-zero exit carrying
		// "no server running" — treat it as an empty session list, not a failure.
		// Exec returns the inner non-zero exit as data (not a Go error), so the
		// check is on the ExecResult, not err (§4.5).
		if strings.Contains(result.Stderr, "no server running") {
			return []Session{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return parseSessions(result.Stdout), nil
}

// KillSession kills the named tmux session in the project's workspace microVM,
// ending the shell/agent running in it.
func (manager Manager) KillSession(project, session string) error {
	if _, err := resolveProjectRoot(project); err != nil {
		return err
	}
	result, err := manager.Sandbox.Exec(Name(project), []string{"tmux", "kill-session", "-t", session})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("tmux kill-session %s exited %d: %s", session, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// tmuxNewSession builds the argv for an attach-or-create tmux session opening in
// /workspace. `-A` makes it idempotent and reattachable: it creates the session
// named session on first use and re-attaches on every later call. When command is
// non-empty it is the session's program (an agent CLI or a login shell); a nil
// command lets tmux open the image's default shell.
func tmuxNewSession(session string, command []string) []string {
	argv := []string{"tmux", "new-session", "-A", "-s", session, "-c", workspaceWorkdir}
	return append(argv, command...)
}

// agentValidCLIs is the set of agent CLIs the platform knows how to launch in a
// session, mapped to the in-VM launch command. The keys match `ai project
// create`'s agent choices.
var agentValidCLIs = map[string][]string{
	"opencode":    {"opencode"},
	"pi":          {"pi"},
	"claude-code": {"claude"},
	"codex":       {"codex"},
	"gemini":      {"gemini"},
}

// AgentCLINames returns the sorted set of agent CLI names the platform can launch
// in a session (for shell completion and help text).
func AgentCLINames() []string {
	names := make([]string, 0, len(agentValidCLIs))
	for name := range agentValidCLIs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// agentLaunchCommand maps an agent CLI name to its in-VM launch command. An
// unknown CLI returns ErrUnknownAgentCLI with the valid set listed (→ exit 2).
func agentLaunchCommand(cli string) ([]string, error) {
	if launch, ok := agentValidCLIs[cli]; ok {
		return launch, nil
	}
	valid := make([]string, 0, len(agentValidCLIs))
	for name := range agentValidCLIs {
		valid = append(valid, name)
	}
	sort.Strings(valid)
	return nil, fmt.Errorf("%w %q (valid: %s)", ErrUnknownAgentCLI, cli, strings.Join(valid, ", "))
}

// parseSessions turns tmux's tab-separated list-sessions output (one session per
// line: name<TAB>attached<TAB>activity) into Session values. Blank/short lines are
// skipped; attached is "1" when a client is attached.
func parseSessions(stdout string) []Session {
	sessions := []Session{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		sessions = append(sessions, Session{
			Name:     fields[0],
			Attached: fields[1] == "1",
			Activity: fields[2],
		})
	}
	return sessions
}

// InspectNetwork returns the egress policy in force on the project's running
// workspace microVM. A not-running workspace (or missing msb) is signalled with
// ErrNotRunning / ErrMsbMissing so the caller can fall back to the declared
// policy rather than treating it as a failure.
func (manager Manager) InspectNetwork(project string) (NetworkPolicy, error) {
	if _, err := resolveProjectRoot(project); err != nil {
		return NetworkPolicy{}, err
	}
	return manager.Sandbox.InspectNetwork(Name(project))
}

func (manager Manager) updateStatus(root, name, project string, status state.WorkspaceStatus) error {
	handle := &state.Workspace{
		ID:      name,
		Project: project,
		Status:  status,
		Created: manager.Now(),
	}
	return state.OpenStore(root).SaveWorkspace(handle)
}
