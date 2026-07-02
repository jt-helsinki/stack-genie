// Package workspace coordinates the lifecycle of a project's workspace microVM
// (arch §7): build the OCI image from the project's Dockerfile, create/start the
// Microsandbox microVM, exec into it, and stop/destroy it — recording each
// transition in the project's run state. The OCI build and microVM operations
// are behind the Builder and Sandbox interfaces so the orchestration is
// unit-tested with fakes and the real impls run on a provisioned host.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/agentcfg"
	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/sysinfo"
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

// In-VM (KEYED) files written under the `workspace` user's home. The per-CLI provider
// configs no longer live here — they are written KEYLESS into each CLI's default
// PROJECT location (see registerAgentProviders). These three are in-VM only:
//
//   - agentEnv: the gateway env vars the CLIs read, incl. the scoped virtual key and
//     OPENCODE_CONFIG — sourced by every session (key in-VM only, off host disk).
//   - bashProfile / tmuxConf: the managed login profile + transparent tmux config.
const (
	agentEnvGuestPath    = agentcfg.AgentEnvFileGuestPath
	bashProfileGuestPath = "/home/workspace/.bash_profile"
	tmuxConfGuestPath    = "/home/workspace/.tmux.conf"
)

// refresh-models guest paths. The generated script is first written to a home
// staging path (WriteFile runs as the `workspace` user and keeps the payload off
// argv), then installed onto PATH at /usr/local/bin via passwordless sudo so the
// user can run `refresh-models` from any session. The staging file is removed
// after install.
const (
	refreshScriptStagePath = "/home/workspace/.cache/aip/refresh-models"
	refreshScriptBinPath   = "/usr/local/bin/refresh-models"
)

// shellSessionName is the tmux session that backs the default interactive shell
// (`ai shell` / `ai attach` with no session). Per-agent sessions are named after
// the agent CLI (opencode, pi, …).
const shellSessionName = "shell"

// workspaceWorkdir is the guest path the project source is mounted at and where
// every tmux session opens (matches the microVM --workdir in Create). It is a
// SUBDIRECTORY of the workspace user's home (~/project) — deliberately NOT the home
// itself and NOT a top-level /workspace: mounting under ~ keeps the project beside
// the user's tooling while the bind mount does NOT shadow the baked-in ~/.local/bin
// (uv/Graphify) or ~/.config (the agent configs, which hold the scoped key and must
// stay OFF the host — a bind at ~ would write them to host disk).
const workspaceWorkdir = "/home/workspace/project"

// ErrUnknownProject is returned when a project name is not in the global index
// (→ exit 2).
var ErrUnknownProject = errors.New("unknown project")

// ErrWorkspaceUnresponsive is returned when an in-VM probe (tmux/session listing)
// times out OR fails but a liveness probe confirms the microVM IS present: the VM
// is up but not answering execs in time (overloaded — e.g. behind a long image
// pull, or wedged). The message stays generic (it does NOT assume an image pull —
// that was misleading) and points at the actionable fix. Mapped to exit 4 (runtime
// failure).
var ErrWorkspaceUnresponsive = errors.New("workspace microVM is running, but msb exec is not responding — logs may still be available; try `ai restart`")

// ErrWorkspaceStale is returned when the platform's lifecycle handle says the
// workspace is "started" but a bounded liveness probe finds NO running microVM for
// it (the VM is gone, was never fully booted, or msb lost it) — i.e. the saved
// state is stale. It is distinct from ErrNotStarted (handle not started) and
// ErrWorkspaceUnresponsive (VM present but slow): here the fix is to recreate the
// VM, so the message points at `ai restart`. Mapped to exit 4 (runtime failure).
var ErrWorkspaceStale = errors.New("workspace is marked started but its microVM isn't running (stale state) — run `ai restart`")

// inVMProbeTimeout bounds the short buffered in-VM probes (tmux presence, session
// listing, apps `nerdctl ps`) so the CLI/TUI fail fast instead of hanging when the
// workspace is busy. After such a probe times out OR fails, the manager runs the
// much shorter livenessProbeTimeout-bounded VM-liveness check to classify the
// error precisely (stale VM vs. unresponsive VM) — the liveness probe is NOT on
// the happy path, so a healthy workspace pays no extra latency.
//
// Timeout budget (must stay coherent with the TUI view backstop): the worst-case
// manager classification is inVMProbeTimeout*inVMProbeAttempts + livenessProbeTimeout
// (in-VM exec gives up after its retries, then the liveness probe classifies). The
// TUI's fetch backstop (views.viewFetchTimeout) is set LARGER than that sum so the
// manager's PRECISE classified error always wins over the view's generic timeout.
const inVMProbeTimeout = 4 * time.Second

// inVMProbeAttempts is how many times a bounded in-VM probe is tried before the
// manager gives up and classifies the failure. The retry is the SLEEP-RECOVERY
// path: the FIRST `msb exec` after the host wakes from sleep often hangs while the
// VM's vsock connection re-establishes; killing it (the per-attempt timeout) and
// reconnecting on a fresh `msb exec` typically succeeds — so the Apps/Shell views
// self-heal after a sleep instead of forcing a manual `ai restart`. Only timeouts
// are retried (a definite error like msb-missing is not).
const inVMProbeAttempts = 2

// livenessProbeTimeout bounds the metadata-only VM-liveness probe (`msb inspect`),
// run ONLY to classify an in-VM exec that already failed/timed out. It is short so
// the classification itself can never hang: the user sees a specific, actionable
// message within a couple of seconds of the in-VM probe giving up.
const livenessProbeTimeout = 3 * time.Second

// ErrNotStarted is returned when an operation needs a RUNNING workspace but it is
// stopped or was never started (→ exit 2). The message is the user-facing nudge to
// start it; it covers both cases (the platform tracks the running state in the
// lifecycle handle, set by start/stop).
var ErrNotStarted = errors.New("workspace is not running — run `ai start` first")

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
// VMResources is the per-workspace microVM runtime allocation/options passed to
// `msb create`, sourced from the project config. CPUs ≤ 0 omits `--cpus` (msb's
// default vCPU count); an empty Memory falls back to the platform default (see
// microVMMemory); an empty IdleTimeout falls back to config.DefaultMicrosandboxIdleTimeout.
type VMResources struct {
	CPUs        int
	Memory      string
	IdleTimeout string
}

// read-only image, the host project source, and the persistent overlay (arch
// §26) as a volume, and applies the project's egress policy via netArgs (the
// `msb create` network-rule fragment from egress.MsbNetworkArgs).
type Sandbox interface {
	Create(name, imageRef, projectMount, overlayPath string, resources VMResources, netArgs []string) error
	Start(name string) error
	Stop(name string) error
	Destroy(name string) error
	Exec(name string, argv []string) (ExecResult, error)
	// ExecContext is Exec bounded by ctx — used for the short in-VM probes so they
	// fail fast (killing the hung msb exec) instead of hanging when the workspace is
	// busy or wedged.
	ExecContext(ctx context.Context, name string, argv []string) (ExecResult, error)
	// ExecRoot runs argv inside the running microVM as the image's ROOT user
	// (`msb exec -u root` — msb's no-`-u` default is the unprivileged `workspace`
	// user, NOT root), for privileged operations the unprivileged workspace user
	// cannot perform — notably booting the rootful in-VM containerd. A non-zero
	// inner exit is carried in ExecResult; only an infrastructure failure (microVM
	// down, msb missing) is a Go error.
	ExecRoot(name string, argv []string) (ExecResult, error)
	// ExecRootContext is ExecRoot bounded by ctx — used for the short root probes
	// (the apps `nerdctl ps` listing) so they fail fast (killing the hung msb exec
	// with ErrWorkspaceUnresponsive) instead of hanging when the workspace is busy or
	// wedged. The unbounded ExecRoot stays for the deliberately-long containerd boot.
	ExecRootContext(ctx context.Context, name string, argv []string) (ExecResult, error)
	// ExecInteractive runs argv inside the running microVM with the CALLER'S
	// terminal attached — a real PTY via `msb exec -t`, with stdin/stdout/stderr
	// wired straight through — for interactive shells and agent CLIs. Only an
	// infrastructure failure (microVM down, msb missing) is returned; the inner
	// program's own exit (the user ending the session) is not an error.
	ExecInteractive(name string, argv []string) error
	// WriteFile writes content to guestPath inside the running microVM, creating
	// parent directories. name is the human label only used for error context.
	WriteFile(name, guestPath string, content []byte) error
	// LogTail returns the microVM's captured output (`msb logs <name> --tail
	// <lines>` — the merged stdout/stderr the sandbox produced). lines ≤ 0 returns
	// the full captured log. Backs the TUI "Workspace Log" tab. ErrMsbMissing when msb
	// is absent; a non-running/unknown sandbox surfaces as a Go error carrying msb's
	// message.
	LogTail(name string, lines int) (string, error)
	// LogTailContext is LogTail bounded by ctx, used for live health diagnostics so a
	// stuck log command cannot hang doctor.
	LogTailContext(ctx context.Context, name string, lines int) (string, error)
	// InspectNetwork reads the egress policy in force on the named microVM via
	// `msb inspect`. It returns ErrNotRunning when no such sandbox exists (the
	// workspace is not running) and ErrMsbMissing when msb is not installed —
	// both of which callers treat as "show the declared policy only", not an error.
	InspectNetwork(name string) (NetworkPolicy, error)
	// IsRunning reports whether a microVM named name actually exists/runs, via a
	// bounded metadata query (`msb inspect`, bounded by ctx). It is a fast liveness
	// probe — NOT an in-VM exec — so it cannot wedge the way `msb exec` can against a
	// gone/booting VM. (false, nil) means the VM is not present (stale handle);
	// (true, nil) means it is present. A timeout/cancel or msb error is returned as
	// the error so the caller can decide (an unknown liveness is treated as "present
	// but slow" → unresponsive, never as a confident "stale").
	IsRunning(ctx context.Context, name string) (bool, error)
	// SyncClock sets the guest clock to the host's current time (best-effort,
	// bounded, as root). A microVM's clock FREEZES while the host sleeps and jumps
	// backward on wake; left uncorrected the drift breaks in-VM TLS (e.g. an image
	// `nerdctl pull` failing cert validation). Called at workspace start/restart.
	// ErrMsbMissing when msb is absent.
	SyncClock(name string) error
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

// ServedModels lists the models the LiteLLM gateway currently SERVES — its
// DB-backed models (a keyed provider's catalog models + the registered Ollama
// models). It is the live source of the in-VM agent model picker in the
// catalog-driven model system, defined locally so tests can supply a fake without a
// live gateway (the real impl wraps litellm.KeyManager.ListModels). A nil source,
// or a ServedModels that errors (gateway down at start), degrades gracefully — the
// workspace still starts, with an empty picker, never failing over a model lookup.
type ServedModels interface {
	ServedModels() ([]string, error)
}

// Manager coordinates the lifecycle over a Builder + Sandbox, stamping state with
// Now (RFC 3339 UTC). GOOS records the host OS for any host-specific behavior
// (supported hosts: macOS and Linux).
type Manager struct {
	Builder Builder
	Sandbox Sandbox
	Keys    KeyMinter
	// Served lists the models the gateway currently serves, for the in-VM agent
	// model picker. It is optional: a nil source (or a ServedModels error) yields an
	// empty picker without failing the workspace start.
	Served ServedModels
	Now    func() string
	GOOS   string
	// Sleep is an OPTIONAL lifecycle hook for callers/tests that want to hold an
	// external assertion while a workspace is running. The real manager leaves it nil:
	// workspace idle reaping is handled by the msb create-time idle timeout, not by
	// preventing host sleep or running a background heartbeat.
	Sleep SleepInhibitor
}

// SleepInhibitor is an optional start/stop hook. It is deliberately generic (not
// necessarily a host power assertion): Inhibit is called at start and Release at
// stop/destroy. Implementations are best-effort — a failure must never block the
// lifecycle. RealManager does not install one by default, so the platform does not
// keep the host awake just because a workspace is running.
type SleepInhibitor interface {
	// Inhibit starts (or re-uses) the power assertion for the project. root is the
	// project's host source dir (the assertion's pid is tracked under its run/ dir).
	Inhibit(project, root string) error
	// Release drops the project's power assertion (no-op if none is held).
	Release(project, root string) error
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
	// Translate the effective egress policy into the msb network argv fragment;
	// the configured model gateway is always allowed on its Headroom port (arch
	// §29.2). In standalone/local mode this is host.microsandbox.internal:18787;
	// in client mode it is the remote server `ai gateway set` configured.
	projectConfig, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateIdleTimeout(projectConfig.Microsandbox.IdleTimeout); err != nil {
		return nil, err
	}
	gatewayHost, gatewayPort, gatewayURL := resolveGateway()
	// Publish the installed in-VM apps' unique host ports so msb forwards
	// host:<port> → VM:<port> (nerdctl then maps VM:<port> → container). The app
	// ports are merged into the project's publish set for this start only — they
	// live in the apps config, not the network block, so adding/removing an app and
	// restarting re-derives the published set automatically.
	networkForStart := projectConfig.Network
	networkForStart.PublishPorts = mergePublishPorts(networkForStart.PublishPorts, apps.PublishedPorts(projectConfig))
	netArgs := egress.MsbNetworkArgs(networkForStart, gatewayHost, gatewayPort)
	// The microVM mounts the host project path directly. Supported hosts are
	// macOS and Linux, so no path translation is needed (arch §7).
	// Apply the effective microVM resource limits (merged config.yaml
	// `workspace.cpu_limit`/`memory_limit`) to `msb create`. Empty/zero values fall
	// back to msb's default vCPU count and the platform default memory.
	resources := VMResources{
		CPUs:        projectConfig.Workspace.CPULimit,
		Memory:      projectConfig.Workspace.MemoryLimit,
		IdleTimeout: projectConfig.Microsandbox.ResolvedIdleTimeout(),
	}
	if err := manager.Sandbox.Create(name, imageRef, root, overlayPath, resources, netArgs); err != nil {
		return nil, err
	}
	if err := manager.Sandbox.Start(name); err != nil {
		return nil, err
	}
	// Correct the guest clock before anything time-sensitive runs (agent provider
	// TLS, in-VM image pulls): a fresh boot — or a recreate after the host slept —
	// can leave the VM's clock skewed by the sleep duration. Best-effort: a failure
	// must never fail the start.
	_ = manager.Sandbox.SyncClock(name)
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
	if err := manager.registerAgentProviders(name, project, root, projectConfig, gatewayURL); err != nil {
		return nil, err
	}
	// Bring up the rootful in-VM container runtime (containerd) so nerdctl works
	// inside the workspace. BEST-EFFORT + bounded — a failure here must NOT fail the
	// workspace start. The installed in-VM apps are NOT auto-started here: pulling a
	// heavy app image (Open WebUI etc.) is slow and, because in-VM execs contend,
	// would block the workspace start AND every other exec (shell, session list) for
	// the whole pull, leaving the workspace unresponsive. Apps are started ON DEMAND
	// via `ai apps start` (which brings containerd up if needed and shows progress).
	manager.ensureContainerd(name)
	// Create the per-project Python virtualenv (.venv-msb) using the guest's baked-in
	// Python. Best-effort — never fails the workspace start.
	manager.ensureVenv(name)
	// Wire the shared <project>/.ai-platform/{agents,skills,prompts} pool into each
	// installed CLI's real per-project dirs via relative symlinks, so one copy of a
	// skill/agent/prompt (incl. Caveman) serves every client. Host-side + best-effort;
	// MUST run BEFORE registerGraphify so graphify's per-CLI skill files land in the
	// shared pool through the symlinks.
	if err := linkSharedResources(root, projectConfig.Agent.Tools); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not link shared agent resources in workspace %q: %v\n", name, err)
	}
	// Register Graphify with each selected agent CLI. Runs HERE (not at image build)
	// because `graphify install --project` writes into the project dir (~/project),
	// which is only bind-mounted at runtime. Best-effort — never fails the start.
	manager.registerGraphify(name, projectConfig)
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
	// Run any optional lifecycle hook (RealManager leaves this nil; tests/future
	// callers may inject one). Best-effort — never fail start.
	if manager.Sleep != nil {
		_ = manager.Sleep.Inhibit(project, root)
	}
	return handle, nil
}

// registerAgentProviders mints a scoped LiteLLM virtual key for the workspace and
// routes ALL FIVE agent CLIs through the host Headroom proxy with that key. It
// reads each CLI's KEYLESS host-side template from <project>/.ai-platform/agents/
// (scaffolding the default templates back if absent — without the key), merges in
// the dynamic values (the freshly-minted key, the served-model picker, the Headroom
// knobs), and writes the FINAL key-bearing config INTO the microVM (key host→VM
// only — never to platform disk):
//
//   - opencode / pi: a merged JSON config file (the per-request Headroom knobs ride
//     on opencode only; pi cannot inject per-request fields → Headroom defaults).
//   - codex: a keyless ~/.codex/config.toml provider block (key via env_key).
//   - claude-code / codex / gemini: the gateway env vars in the in-VM agent env
//     file, sourced by every shell + agent session.
//
// User edits to the host templates survive across restarts: a present template is
// read and merged (only its absence triggers re-scaffolding), and the key-bearing
// version is never written back to the host.
func (manager Manager) registerAgentProviders(name, project, root string, projectConfig *config.Config, gatewayURL string) error {
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
	models := manager.pickerModels()

	// Persist the agent CLIs' state dirs to the overlay so a CLI's per-project memory —
	// notably opencode's last-used model — survives microVM restarts (see below).
	manager.linkAgentStateDirs(name)

	// SEED-THEN-REMEMBER default (chosen behavior): the model picked at setup (stored as
	// agent.graphify_model, registered in the gateway as ollama/<model>) is SEEDED as
	// every CLI's default on the FIRST start only. A host marker under .ai-platform (same
	// dir on host + in-VM) records that. On LATER starts we pass an empty default, which
	// makes MergeOpenCodeConfig/MergePiSettings actively DROP the pinned model so the
	// user's persisted /model choice wins (opencode ranks config "model" above last-used,
	// so a stale pin would defeat remembering). No setup model → never seed.
	setupModel := ""
	if projectConfig.Agent.GraphifyModel != "" {
		setupModel = "ollama/" + projectConfig.Agent.GraphifyModel
	}
	seedMarker := filepath.Join(root, ".ai-platform", ".agent-default-seeded")
	defaultModel := ""
	seedingNow := false
	if setupModel != "" {
		if _, statErr := os.Stat(seedMarker); os.IsNotExist(statErr) {
			defaultModel = setupModel
			seedingNow = true
		}
	}

	// Write the KEYLESS per-CLI provider configs into each CLI's DEFAULT PROJECT
	// location (under the bind-mounted project dir = host disk, so the project is
	// self-describing and portable). The scoped virtual key is NEVER written here:
	// opencode/pi reference it via {env:}/$VAR interpolation, codex via env_key, claude
	// via the exported ANTHROPIC_AUTH_TOKEN — the key lives ONLY in the in-VM agent env
	// file below. Existing files are deep-merged so user edits survive across restarts.
	openCodeConfig, err := agentcfg.MergeOpenCodeConfig(
		readHostFileOrNil(projectConfigPath(root, ".opencode", "opencode.json")),
		gatewayURL, agentcfg.OpenCodeAPIKeyRef, defaultModel, models, keepTurns, outputBufferTokens)
	if err != nil {
		return err
	}
	if err := writeHostFile(projectConfigPath(root, ".opencode", "opencode.json"), openCodeConfig); err != nil {
		return err
	}

	// pi reads the GLOBAL ~/.pi/agent/models.json (NOT a project .pi/models.json), so
	// write the served-model provider config there — pi then lists the SAME gateway
	// models as opencode. In-VM home (off host disk); keyless via $AIP_GATEWAY_KEY.
	piModels, err := agentcfg.PiConfig(gatewayURL, agentcfg.PiAPIKeyRef, defaultModel, models)
	if err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, agentcfg.PiGlobalModelsGuest, piModels); err != nil {
		return err
	}
	// pi settings (project-scoped, which pi DOES read): the gateway as default provider,
	// the workspace default model, and skills/prompts resource paths pointing at the
	// symlinked shared pools (linkSharedResources creates .pi/skills, .pi/prompts).
	piSettings, err := agentcfg.MergePiSettings(readHostFileOrNil(projectConfigPath(root, ".pi", "settings.json")), defaultModel)
	if err != nil {
		return err
	}
	if err := writeHostFile(projectConfigPath(root, ".pi", "settings.json"), piSettings); err != nil {
		return err
	}

	claudeConfig, err := agentcfg.MergeClaudeSettings(
		readHostFileOrNil(projectConfigPath(root, ".claude", "settings.json")), gatewayURL)
	if err != nil {
		return err
	}
	if err := writeHostFile(projectConfigPath(root, ".claude", "settings.json"), claudeConfig); err != nil {
		return err
	}

	// codex: the KEYLESS per-project provider block (fully platform-managed, so it is
	// overwritten each start; the key is env-supplied via env_key). Plus a global in-VM
	// trust entry (off host disk) so codex loads the per-project config.
	if err := writeHostFile(projectConfigPath(root, ".codex", "config.toml"), agentcfg.CodexConfig(gatewayURL, defaultModel)); err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, agentcfg.CodexConfigGuestPath, agentcfg.CodexTrustConfig()); err != nil {
		return err
	}

	// Record that the setup model has been seeded, so subsequent starts stop pinning it
	// and defer to each CLI's persisted last-used selection. Best-effort: if the marker
	// can't be written we simply re-seed next start (harmless — opencode records the same
	// model as last-used anyway).
	if seedingNow {
		if err := os.WriteFile(seedMarker, []byte(setupModel+"\n"), 0o644); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: could not record the agent default-seed marker in workspace %q (continuing): %v\n", name, err)
		}
	}

	// The KEYED files stay IN-VM only (off host disk): the gateway env vars — the
	// scoped virtual key (ANTHROPIC_AUTH_TOKEN/AIP_GATEWAY_KEY/GEMINI_API_KEY), the
	// gemini/claude base URLs, and OPENCODE_CONFIG (pointing opencode at its project
	// config) — sourced by every shell + agent session. gemini is env-only (no config
	// file supports a base URL).
	if err := manager.Sandbox.WriteFile(name, agentEnvGuestPath, agentcfg.AgentEnvScript(gatewayURL, apiKey, projectConfig.Agent.GraphifyModel)); err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, bashProfileGuestPath, agentcfg.BashProfile()); err != nil {
		return err
	}

	// Install the in-VM `refresh-models` command so the user can re-pull the model
	// picker (after adding a provider key with `ai keys` or pulling/removing an
	// Ollama model on the host) WITHOUT restarting the workspace. It bakes the SAME
	// gateway URL, scoped key, default, and Headroom knobs as the configs above; at
	// run time it re-fetches the served models from the gateway's /v1/models endpoint
	// and rewrites the configs, reproducing pickerModels' result. The minted key
	// flows host→VM only. BEST-EFFORT: a convenience helper must never fail the whole
	// workspace start (the msb rootfs persists across starts, so a prior copy can
	// already be present) — warn and continue.
	// refresh-models NEVER pins a default (it passes an empty default, which drops the
	// "model" key) — it refreshes the served-model LIST on demand and must not clobber
	// the user's persisted last-used selection.
	if err := manager.installRefreshScript(name, gatewayURL, apiKey, "", keepTurns, outputBufferTokens); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not install the in-VM refresh-models helper in workspace %q (continuing): %v\n", name, err)
	}

	// Write the managed tmux.conf so the workspace session model is transparent
	// (mouse scroll, hidden status bar) — the user never types a tmux command.
	return manager.Sandbox.WriteFile(name, tmuxConfGuestPath, agentcfg.TmuxConfig())
}

// containerdLog is the in-VM path containerd's stdout/stderr is redirected to
// when ensureContainerd boots it, so the daemon's output is inspectable.
const containerdLog = "/var/log/containerd.log"

// venvPath is the per-project Python virtualenv created inside the workspace. It lives
// in the bind-mounted project dir (workspaceWorkdir), so it is ONE directory visible
// on both the host and the guest — but it is a LINUX venv, usable only INSIDE the
// sandbox (a venv hard-codes its interpreter path + carries platform-specific
// binaries, so it is not portable across the macOS host and the Linux guest; see
// docs/MSB-SDK-MIGRATION.md / the venv note). The sandbox IS the dev environment, so
// this single venv is all in-sandbox Python work needs.
const venvPath = workspaceWorkdir + "/.venv-msb"

// venvCreateTimeout bounds the one-time `python3 -m venv` (which bootstraps pip).
const venvCreateTimeout = 90 * time.Second

// ensureVenv creates the per-project virtualenv (.venv-msb) inside the running microVM
// with the guest's baked-in Python, if it does not already exist. Best-effort and
// bounded — a failure never fails the workspace start, and an existing venv (guarded
// by its pyvenv.cfg) is left untouched. Runs as the workspace user, which owns the
// bind-mounted project dir.
func (manager Manager) ensureVenv(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), venvCreateTimeout)
	defer cancel()
	script := "command -v python3 >/dev/null 2>&1 || exit 0; " +
		"test -f " + venvPath + "/pyvenv.cfg || python3 -m venv " + venvPath
	_, _ = manager.Sandbox.ExecContext(ctx, name, []string{"sh", "-lc", script})
}

// graphifyInstallTimeout bounds each per-CLI `graphify install` (config-only, no
// network — it just writes skill/plugin/hook files into the project).
const graphifyInstallTimeout = 60 * time.Second

// linkAgentStateDirs points the agent CLIs' mutable STATE directories at the persistent
// overlay (/persist) so a CLI's per-project memory survives microVM restarts — the VM
// home does NOT persist, so without this opencode forgets the last-used /model on every
// restart. opencode persists under ~/.local/share/opencode (its XDG_DATA_HOME); pi under
// ~/.pi (into which the served-models config is then written). /persist is root-owned, so
// the per-CLI dirs are created + handed to the workspace user as root, then symlinked in
// as the workspace user. Best-effort: a failure just falls back to the ephemeral home.
func (manager Manager) linkAgentStateDirs(name string) {
	if _, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c",
		"mkdir -p /persist/agents/opencode /persist/agents/pi && chown -R workspace /persist/agents"}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not prepare persistent agent state in workspace %q (continuing): %v\n", name, err)
		return
	}
	// rm -rf on a symlink removes only the link (a re-start's existing symlink), not the
	// /persist target, so accumulated state is preserved across restarts.
	link := "set -e; mkdir -p ~/.local/share; " +
		"rm -rf ~/.local/share/opencode; ln -sfn /persist/agents/opencode ~/.local/share/opencode; " +
		"rm -rf ~/.pi; ln -sfn /persist/agents/pi ~/.pi"
	if _, err := manager.Sandbox.Exec(name, []string{"bash", "-lc", link}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not link persistent agent state in workspace %q (continuing): %v\n", name, err)
	}
}

// graphifyPlatformFlag maps an agent CLI to its `graphify install --platform` value.
// claude-code is Graphify's DEFAULT platform, so it takes no --platform flag.
// A CLI absent from this map is not a Graphify platform and is skipped.
var graphifyPlatformFlag = map[string]string{
	"claude-code": "",
	"codex":       "codex",
	"gemini":      "gemini",
	"opencode":    "opencode",
	"pi":          "pi",
}

// registerGraphify registers Graphify (baked into the image via `uv tool install`)
// with each SELECTED agent CLI, at workspace start. It MUST run here rather than at
// image-build time: `graphify install --project` writes project-scoped skill/plugin/
// hook files (e.g. opencode's `.opencode/plugins/graphify.js`, `AGENTS.md`) into the
// project directory, which is only bind-mounted (at ~/project = workspaceWorkdir) at
// runtime. Those files are DISTINCT from the platform's global agent configs under
// ~/.config, so there is no clobber. Each install is config-only (no network) and
// best-effort — a failure never fails the workspace start.
func (manager Manager) registerGraphify(name string, projectConfig *config.Config) {
	if projectConfig == nil {
		return
	}
	installs := make([]string, 0, len(projectConfig.Agent.Tools))
	for _, cli := range projectConfig.Agent.Tools {
		platform, known := graphifyPlatformFlag[cli]
		if !known {
			continue
		}
		install := "graphify install --project"
		if platform != "" {
			install += " --platform " + platform
		}
		installs = append(installs, install)
	}
	if len(installs) == 0 {
		return
	}
	// Run ONCE per project: `graphify install` OVERWRITES its skill files each run,
	// so a marker under the (persistent) .ai-platform dir guards re-runs — this keeps
	// user edits to graphify's project files from being clobbered on every restart.
	// All installs run in ~/project so --project writes there; the marker is touched
	// only after they all succeed (a failure retries next start). Best-effort.
	marker := workspaceWorkdir + "/.ai-platform/.graphify-installed"
	script := "command -v graphify >/dev/null 2>&1 || exit 0; " +
		"test -f " + marker + " && exit 0; " +
		"mkdir -p " + workspaceWorkdir + "/.ai-platform && cd " + workspaceWorkdir + " && " +
		strings.Join(installs, " && ") + " && touch " + marker
	ctx, cancel := context.WithTimeout(context.Background(), graphifyInstallTimeout)
	defer cancel()
	_, _ = manager.Sandbox.ExecContext(ctx, name, []string{"sh", "-lc", script})
}

// ensureContainerd makes the rootful in-VM container runtime (containerd)
// available so nerdctl works inside the workspace (arch §7). It probes whether
// containerd is already up (a `nerdctl info` that talks to the daemon) and, if
// not, boots it DETACHED via `setsid` so the daemon outlives the exec that
// started it and runs for the VM's life. Both run as ROOT (ExecRoot) because the
// runtime is rootful and the unprivileged workspace user cannot start it.
//
// It is BEST-EFFORT: it returns whether containerd is READY (nerdctl can reach it)
// so the caller can skip the in-VM apps cleanly when it is not, rather than letting
// each app fatal on a dead socket. It never returns an error and never fails the
// workspace start — a workspace with no running runtime is still fully usable.
//
// hardware bring-up: the daemon-persistence of `msb exec` + setsid and the real
// containerd boot are verified on a provisioned Apple Silicon host. The host-side
// orchestration (the probe, the boot argv, the readiness poll) is unit-tested here
// against the fake sandbox.
func (manager Manager) ensureContainerd(name string) bool {
	// Probe: if `nerdctl info` reaches the daemon, containerd is already up. Bound it
	// with `timeout` so a wedged daemon (socket present but not responding) can't hang
	// the probe — and therefore the workspace start — indefinitely. Its OUTPUT is
	// discarded (`>/dev/null 2>&1`): on a fresh VM containerd is legitimately not up
	// yet, so nerdctl emits a `level=fatal "cannot access containerd socket"` line —
	// that is EXPECTED probe noise (we boot containerd next), not a failure to show
	// the user; we only consume the exit code.
	probeCmd := "timeout 5 nerdctl info >/dev/null 2>&1"
	if result, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c", probeCmd}); err == nil && result.ExitCode == 0 {
		return true
	}
	// Boot containerd detached so it survives this exec returning. setsid +
	// background keeps the daemon running for the VM's life; output is redirected
	// to a log for later inspection.
	//
	// CRITICAL: after backgrounding, WAIT for the runtime to be READY before
	// returning — poll `nerdctl info` (which talks to the daemon), not merely the
	// socket file: the socket can exist a moment before containerd serves requests,
	// and on first boot (or while the VM is busy building/pulling) it can take a few
	// seconds. The boot exec also returns the instant the outer sh backgrounds the
	// daemon (the `&`) and msb tears down the exec's process group on return, so the
	// poll doubles as keeping this exec alive until the setsid'd daemon establishes.
	// Bounded to ~30s (150 × 0.2s) so a genuinely broken runtime never hangs forever;
	// exit 0 = ready, exit 1 = gave up.
	bootCmd := fmt.Sprintf(
		"setsid sh -c 'containerd >%s 2>&1 &'; "+
			"iters=0; while [ $iters -lt 150 ]; do timeout 5 nerdctl info >/dev/null 2>&1 && exit 0; "+
			"iters=$((iters+1)); sleep 0.2; done; exit 1",
		shellQuoteGuest(containerdLog))
	result, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c", bootCmd})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not start the in-VM container runtime in workspace %q: %v\n", name, err)
		return false
	}
	if result.ExitCode != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "warning: in-VM container runtime did not become ready in workspace %q (see %s in the VM)\n",
			name, containerdLog)
		return false
	}
	return true
}

// mergePublishPorts overlays the app-derived publish mappings onto the project's
// declared ones, with app mappings winning on a host-port clash (the network
// allow-list never publishes an app's reserved port). Deterministic order is left
// to MsbNetworkArgs/the caller; this only dedupes by host port.
func mergePublishPorts(declared, appPorts []config.PortMapping) []config.PortMapping {
	byHost := make(map[int]config.PortMapping, len(declared)+len(appPorts))
	order := make([]int, 0, len(declared)+len(appPorts))
	add := func(mapping config.PortMapping) {
		if _, seen := byHost[mapping.Host]; !seen {
			order = append(order, mapping.Host)
		}
		byHost[mapping.Host] = mapping
	}
	for _, mapping := range declared {
		add(mapping)
	}
	for _, mapping := range appPorts {
		add(mapping)
	}
	merged := make([]config.PortMapping, 0, len(order))
	for _, host := range order {
		merged = append(merged, byHost[host])
	}
	return merged
}

// AppManager builds an apps.Manager bound to a running workspace microVM: its
// Exec runs nerdctl as root in the VM (Sandbox.ExecRoot), config is the project's
// config.yaml, port reservations span every workspace, and the gateway env is the
// resolved gateway URL + a freshly-minted scoped virtual key + the model
// preference (litellm.DefaultRouting().Default, which is EMPTY in the catalog-driven
// system — no built-in default model). It backs `ai apps` and the TUI Apps tab. exec
// is nil when the workspace is not running, so the lifecycle methods that need the VM
// report ErrWorkspaceNotRunning (and List degrades to installed-but-not-running).
func (manager Manager) AppManager(name, project, root, gatewayURL string) *apps.Manager {
	return manager.buildAppManager(name, project, root, gatewayURL, manager.requireRunning(project) == nil)
}

// buildAppManager wires an apps.Manager for a workspace. forceExec=true wires the
// in-VM exec surface unconditionally (used at workspace start, when the microVM is
// running but its handle is not yet saved so requireRunning would say "no");
// otherwise Exec is wired only when the workspace is recorded running.
func (manager Manager) buildAppManager(name, project, root, gatewayURL string, forceExec bool) *apps.Manager {
	var exec apps.ExecRunner
	var probeExec apps.ExecRunner
	if forceExec {
		exec = func(argv []string) (apps.ExecResult, error) {
			result, err := manager.Sandbox.ExecRoot(name, argv)
			return apps.ExecResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
		}
		// The running-status probe (`nerdctl ps`) is bounded so a busy/wedged VM fails
		// fast; on failure it is classified into a precise error (stale vs. overloaded
		// VM), matching the sessions path. The unbounded exec above still serves the
		// legitimately-long lifecycle ops (image pulls).
		probeExec = func(argv []string) (apps.ExecResult, error) {
			result, err := manager.probeInVM(project, func(ctx context.Context) (ExecResult, error) {
				return manager.Sandbox.ExecRootContext(ctx, name, argv)
			})
			if err != nil {
				return apps.ExecResult{}, err // classified (stale / unresponsive), retried for sleep recovery
			}
			// msb reporting the sandbox is gone (stale handle) → stale, not an app error.
			if result.ExitCode != 0 && stderrSandboxNotFound(result.Stderr) {
				return apps.ExecResult{}, ErrWorkspaceStale
			}
			return apps.ExecResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, nil
		}
	}
	return apps.NewManager(apps.Deps{
		LoadConfig:    func() (*config.Config, error) { return config.LoadProjectConfig(root) },
		SaveConfig:    func(updated *config.Config) error { return config.WriteProject(root, updated) },
		ReservedPorts: apps.ReservedPortsAcrossWorkspaces,
		Exec:          exec,
		ProbeExec:     probeExec,
		// Make containerd ready before running an app, and recover it if it became
		// unreachable (crashed/restarted under load). Only wired when Exec is.
		EnsureRuntime: func() error {
			if exec == nil {
				return nil
			}
			if manager.ensureContainerd(name) {
				return nil
			}
			return fmt.Errorf("in-VM container runtime (containerd) is not ready")
		},
		Gateway: func() (string, string, string, error) {
			apiKey, err := manager.appGatewayKey(name, project)
			if err != nil {
				return "", "", "", err
			}
			return gatewayURL, apiKey, litellm.DefaultRouting().Default, nil
		},
	})
}

// AppManagerFor resolves a project's root + gateway and returns an apps.Manager
// bound to its workspace microVM. It is the entry the `ai apps` CLI uses: it does
// not require the workspace to be running (the returned Manager reports
// ErrWorkspaceNotRunning from the methods that need the VM). An unknown project
// returns ErrUnknownProject (→ exit 2).
func (manager Manager) AppManagerFor(project string) (*apps.Manager, error) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return nil, err
	}
	_, _, gatewayURL := resolveGateway()
	return manager.AppManager(Name(project), project, root, gatewayURL), nil
}

// appGatewayKey mints a scoped LiteLLM virtual key for the workspace's apps,
// aliased per workspace+apps so it does not collide with the agent-CLI key. It
// rotates (delete-then-mint) so a restart always has a fresh key.
func (manager Manager) appGatewayKey(name, project string) (string, error) {
	alias := project + "-apps"
	_ = manager.Keys.DeleteKeyByAlias(alias)
	return manager.Keys.GenerateKey(litellm.KeyScope{
		Alias:    alias,
		Metadata: map[string]any{"workspace": name, "purpose": "apps"},
	})
}

// installRefreshScript generates the per-workspace `refresh-models` script and
// installs it on PATH inside the running microVM at /usr/local/bin/refresh-models
// (executable). The script fetches the served models LIVE from the gateway's
// /v1/models endpoint at run time — nothing about the model list is baked in. It is
// staged to a home path via WriteFile (payload off argv) then moved into place with
// `sudo install -m 0755`, matching how the workspace user gains PATH commands
// (passwordless sudo per the base image).
//
// hardware bring-up: the staging+install Exec and the script's own /v1/models fetch
// run only inside a live microVM; the host-side generation and the script logic
// are unit-tested (internal/agentcfg) and the install wiring is unit-tested here
// against the fake sandbox.
func (manager Manager) installRefreshScript(name, gatewayURL, apiKey, defaultModel string, keepTurns, outputBufferTokens int) error {
	script, err := agentcfg.RefreshScript(gatewayURL, apiKey, defaultModel, keepTurns, outputBufferTokens)
	if err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, refreshScriptStagePath, script); err != nil {
		return err
	}
	// Move the staged script onto PATH, executable, then drop the staging copy. A
	// single shell keeps it one exec; sudo is passwordless for the workspace user.
	// REMOVE the destination first so this is idempotent: the msb rootfs persists
	// across workspace starts, so a prior /usr/local/bin/refresh-models is already
	// present and `install` can fail with "File exists" on it.
	installCmd := fmt.Sprintf("sudo rm -f %s && sudo install -m 0755 %s %s && rm -f %s",
		shellQuoteGuest(refreshScriptBinPath),
		shellQuoteGuest(refreshScriptStagePath), shellQuoteGuest(refreshScriptBinPath),
		shellQuoteGuest(refreshScriptStagePath))
	result, err := manager.Sandbox.Exec(name, []string{"sh", "-c", installCmd})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("could not install refresh-models in workspace %q (exit %d): %s",
			name, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// shellQuoteGuest single-quotes a guest path for safe embedding in a `sh -c`
// command run inside the microVM (POSIX single-quote escaping).
func shellQuoteGuest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// projectConfigPath returns the host path of a per-CLI project config file under the
// project root (e.g. <root>/.opencode/opencode.json). These live in the bind-mounted
// project dir so they are visible in-VM at /home/workspace/project/… too.
func projectConfigPath(root string, parts ...string) string {
	return filepath.Join(append([]string{root}, parts...)...)
}

// readHostFileOrNil reads a host file, returning nil when it is absent or unreadable
// (the merge generators degrade to a freshly-generated config on a nil input, so a
// missing/corrupt project config never fails the start).
func readHostFileOrNil(path string) []byte {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return content
}

// writeHostFile writes content to a host path, creating parent dirs. Used for the
// KEYLESS per-CLI project configs — safe on host disk because the scoped virtual key
// is never in them (it is referenced via env interpolation / supplied via env vars).
func writeHostFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}

// clampWorkspaceMemoryMiB caps the requested microVM memory at the usable host
// ceiling (config.UsableHostMemoryMiB), leaving headroom for the host OS, the Docker
// service tier, and the hypervisor. This is a HARD safety clamp applied at VM
// creation: a microVM handed the whole host RAM boots its agent relay but cannot be
// backed, so the sandbox wedges and msb stops it. It also self-heals projects whose
// config.yaml requests too much (e.g. `memory_limit: 24` on a 24 GB host). It warns on
// stderr when it clamps so the effective value is visible. Host RAM unknown → no clamp.
func clampWorkspaceMemoryMiB(requested uint64) uint64 {
	hostMiB, ok := sysinfo.MemoryMiB()
	if !ok {
		return requested
	}
	usable := config.UsableHostMemoryMiB(hostMiB)
	if requested > usable {
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: capping workspace memory at %d MiB (requested %d MiB; host has %d MiB — reserving headroom for the host + service tier + hypervisor). A microVM given all host RAM cannot boot.\n",
			usable, requested, hostMiB)
		return usable
	}
	return requested
}

// sharedResourceKinds are the shared pools under <project>/.ai-platform/ that hold ONE
// copy of the project's agents / skills / prompts (and a reserved `projects` dir),
// symlinked into each CLI's own dirs so a single copy serves every client.
var sharedResourceKinds = []string{"agents", "skills", "prompts", "projects"}

// sharedResourceLink maps a shared pool dir to each CLI's REAL per-project directory
// for that kind. A CLI absent from a kind's map has no concept for it and is skipped
// (codex/gemini have no skills/agents; gemini only has commands). The CLI dirs are one
// level under the project root, so the symlink target is always ../.ai-platform/<pool>.
type sharedResourceLink struct {
	pool   string            // .ai-platform/<pool>
	perCLI map[string]string // agent CLI -> project-relative dir
}

var sharedResourceLinks = []sharedResourceLink{
	{pool: "skills", perCLI: map[string]string{
		"opencode":    ".opencode/skills",
		"claude-code": ".claude/skills",
		"pi":          ".pi/skills",
	}},
	{pool: "agents", perCLI: map[string]string{
		"opencode":    ".opencode/agents",
		"claude-code": ".claude/agents",
	}},
	{pool: "prompts", perCLI: map[string]string{
		"opencode":    ".opencode/commands",
		"claude-code": ".claude/commands",
		"gemini":      ".gemini/commands",
		"pi":          ".pi/prompts",
	}},
}

// linkSharedResources ensures the shared .ai-platform pools exist and symlinks each
// into the installed CLIs' real per-project dirs (relative symlinks, so they resolve
// identically on host and in the guest — the project dir is one directory shared
// host↔guest). Idempotent: a correct symlink is left as-is, a wrong one is replaced,
// and a real (non-symlink) dir a user created is left untouched.
func linkSharedResources(root string, tools []string) error {
	for _, kind := range sharedResourceKinds {
		if err := os.MkdirAll(filepath.Join(root, ".ai-platform", kind), 0o755); err != nil {
			return err
		}
	}
	installed := make(map[string]bool, len(tools))
	for _, tool := range tools {
		installed[tool] = true
	}
	for _, link := range sharedResourceLinks {
		for cli, relDir := range link.perCLI {
			if !installed[cli] {
				continue
			}
			linkPath := filepath.Join(root, relDir)
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				return err
			}
			// The CLI dir is one level under root, so the pool is ../.ai-platform/<pool>.
			if err := ensureRelSymlink(linkPath, filepath.Join("..", ".ai-platform", link.pool)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureRelSymlink makes linkPath a symlink to target (relative), idempotently: an
// existing symlink pointing at target is left as-is, a symlink pointing elsewhere is
// replaced, and a real file/dir at linkPath (e.g. user content) is left untouched so
// nothing is destroyed.
func ensureRelSymlink(linkPath, target string) error {
	if current, err := os.Readlink(linkPath); err == nil {
		if current == target {
			return nil
		}
		if err := os.Remove(linkPath); err != nil {
			return err
		}
	} else if _, statErr := os.Lstat(linkPath); statErr == nil {
		return nil // a real dir/file — do not clobber user content
	}
	return os.Symlink(target, linkPath)
}

// pickerModels builds the concrete model list the in-VM agent CLIs offer in their
// picker: exactly the models the LiteLLM gateway currently SERVES (its DB-backed
// models — a keyed provider's catalog models + the registered Ollama models),
// deduped and sorted for a deterministic config.
//
// The served-models lookup DEGRADES GRACEFULLY: a nil source or a ServedModels error
// (gateway down at workspace start, or no models registered yet) yields an EMPTY
// picker — the workspace still starts, never failing over a model lookup. The user
// adds provider keys (`ai keys`) / pulls Ollama models and the served set grows; the
// in-VM `refresh-models` command re-pulls it without a restart.
func (manager Manager) pickerModels() []string {
	if manager.Served == nil {
		return []string{}
	}
	served, err := manager.Served.ServedModels()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not list the gateway's served models for the agent picker (continuing with an empty list): %v\n", err)
		return []string{}
	}
	seen := make(map[string]struct{}, len(served))
	models := make([]string, 0, len(served))
	for _, model := range served {
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
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
	// The workspace is no longer running — drop any optional lifecycle assertion.
	if manager.Sleep != nil {
		_ = manager.Sleep.Release(project, root)
	}
	return manager.updateStatus(root, name, project, state.StatusStopped)
}

// Restart restarts the EXISTING workspace: it stops the microVM (tolerating an
// already-stopped microVM) then runs the FULL start path again, which rebuilds
// the OCI image and recreates the microVM via Sandbox.Create so config changes —
// notably a newly added/removed in-VM app's host port — are picked up (see the
// inline note below). It refreshes the handle to a started state with a new
// LastStarted timestamp. It requires a previously-created handle; if the workspace
// was never started it returns ErrNotStarted (→ exit 2) so the caller runs
// `start` first.
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
	// Stop the existing microVM (an already-stopped one is fine for a restart), then
	// run the FULL start path again. Start recreates the microVM via Sandbox.Create,
	// re-deriving the network/published-port set, so a restart picks up config changes
	// — notably a newly added/removed in-VM app's host port (the apps publish set is
	// merged into netArgs only at Start). A bare Sandbox.Stop+Start (no Create) would
	// leave the new port unpublished. Start has no "already running" short-circuit, so
	// it rebuilds + recreates unconditionally and re-runs provider/containerd/app setup.
	if err := manager.Sandbox.Stop(name); err != nil && !errors.Is(err, ErrAlreadyStopped) {
		return nil, err
	}
	return manager.Start(project)
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
// teardown `ai delete` layers on before removing project state (CLI
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
	// The microVM is gone — drop any optional lifecycle assertion.
	if manager.Sleep != nil {
		_ = manager.Sleep.Release(project, root)
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
// message tells the user to run `ai start`.
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

// IsRunning reports whether the project's workspace microVM is currently RUNNING
// (its lifecycle handle is "started"). It reads the platform's own handle, not msb,
// so it never blocks. Any resolution error means "not running" (false).
func (manager Manager) IsRunning(project string) bool {
	return manager.requireRunning(project) == nil
}

// classifyInVMFailure turns a failed/timed-out in-VM exec into a PRECISE,
// actionable error by running the bounded VM-liveness probe AFTER the fact (so the
// happy path pays no probe latency). The handle is already known "started" by the
// time an in-VM op is attempted, so the only two remaining cases are:
//
//   - the microVM is NOT actually present (liveness probe says false) → the saved
//     handle is stale → ErrWorkspaceStale ("run `ai restart`").
//   - the microVM IS present (or its liveness can't be determined fast) but the
//     exec didn't answer in time → ErrWorkspaceUnresponsive ("it may be
//     overloaded; try `ai restart`").
//
// It is only called once an in-VM exec has already failed, so it never masks a
// healthy path. cause is the original exec error/timeout, returned unchanged if it
// is already a classified sentinel.
// probeInVM runs a bounded in-VM exec with retry-on-timeout, returning the first
// successful ExecResult or — once the attempts are exhausted — a CLASSIFIED error
// (stale VM vs. unresponsive VM). The retry is the sleep-recovery path: a stale
// post-wake vsock connection makes the first `msb exec` hang, but a fresh one
// usually succeeds (see inVMProbeAttempts). `run` is given a fresh bounded context
// per attempt. A non-zero INNER exit (e.g. tmux "no server") comes back as
// (result, nil) and is the caller's to interpret — only a transport timeout/error
// is retried/classified here.
func (manager Manager) probeInVM(project string, run func(context.Context) (ExecResult, error)) (ExecResult, error) {
	var lastErr error
	for attempt := 0; attempt < inVMProbeAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), inVMProbeTimeout)
		result, err := run(ctx)
		cancel()
		if err == nil {
			return result, nil
		}
		lastErr = err
		// A definite, non-transient failure won't be cured by reconnecting.
		if errors.Is(err, ErrMsbMissing) {
			break
		}
	}
	return ExecResult{}, manager.classifyInVMFailure(project, lastErr)
}

func (manager Manager) classifyInVMFailure(project string, cause error) error {
	// Already a precise sentinel (e.g. ErrTmuxMissing, ErrMsbMissing) — keep it.
	if errors.Is(cause, ErrMsbMissing) || errors.Is(cause, ErrTmuxMissing) || errors.Is(cause, ErrNotStarted) {
		return cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), livenessProbeTimeout)
	defer cancel()
	running, probeErr := manager.Sandbox.IsRunning(ctx, Name(project))
	if probeErr != nil {
		// Couldn't confirm liveness fast (probe timed out or msb errored). Don't
		// claim "stale" on uncertainty — treat it as present-but-slow.
		if errors.Is(probeErr, ErrMsbMissing) {
			return probeErr
		}
		return ErrWorkspaceUnresponsive
	}
	if !running {
		return ErrWorkspaceStale
	}
	return ErrWorkspaceUnresponsive
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
// named "shell", created detached then attached, so the shell (and anything left
// running in it) survives DETACHING and is reattachable. The session opens in
// ~/project with a login shell.
func (manager Manager) Shell(project string) error {
	return manager.launchTmuxSession(project, shellSessionName, projectLoginShell())
}

// projectLoginShell is the command for the default interactive workspace shell: a
// login shell that starts in the project directory (~/project = workspaceWorkdir).
// tmux's `-c` already sets the start dir for a FRESHLY-created session, but the
// explicit `cd` guarantees the shell opens in ~/project even when a login profile
// would otherwise leave it in $HOME. `exec bash -l` then hands over to a clean
// interactive login shell whose cwd is ~/project. (`tmux new-session -A` reuses an
// existing session and ignores this command, so it only affects new sessions.)
func projectLoginShell() []string {
	return []string{"bash", "-lc", "cd " + shellQuoteGuest(workspaceWorkdir) + " 2>/dev/null; exec bash -l"}
}

// launchTmuxSession is the shared entry for the tmux-backed interactive sessions
// (shell / attach / agent). It verifies the microVM is up (clean ErrNotStarted if
// not) and that tmux is present in the image (ErrTmuxMissing with remediation if
// not — rather than msb's raw "failed to exec tmux" leak, which also misreports
// success), then attaches the PTY. tmux is a hard requirement: every base image
// installs it, so a missing tmux means a stale project Dockerfile (recreate /
// rebuild), not a case to silently degrade.
func (manager Manager) launchTmuxSession(project, session string, command []string) error {
	if _, err := resolveProjectRoot(project); err != nil {
		return err
	}
	if err := manager.requireRunning(project); err != nil {
		return err
	}
	if err := manager.requireTmux(project); err != nil {
		return err
	}
	if err := manager.ensureTmuxReady(project); err != nil {
		return err
	}
	// Create-or-attach in a SINGLE interactive exec: `tmux new-session -A` creates the
	// session on first use and reattaches on later calls. The tmux server daemonizes,
	// so the session persists after the client DETACHES (and `ai sessions` lists it);
	// it ends only when its shell EXITS. Doing this as ONE interactive command — rather
	// than a separate detached create then attach — avoids a race where msb tears down
	// the create exec's process group before the attach connects, which left the attach
	// with no session and bounced the user straight back to the TUI.
	return manager.Sandbox.ExecInteractive(Name(project), tmuxNewSessionAttach(session, command))
}

// ensureTmuxReady warms up the tmux server path with a bounded, NON-interactive exec
// before the user's real terminal is handed to `msb exec -t`. Live msb can be flaky on
// the first interactive PTY shortly after VM start: tmux/server setup or the vsock PTY
// path can race, leaving the host terminal on a blank/frozen client. Creating and
// killing a short probe session proves tmux can create a server/session and exercises
// the first msb exec/tmux startup path while output is buffered and recoverable. The
// actual user session still uses the atomic `tmux new-session -A` below.
func (manager Manager) ensureTmuxReady(project string) error {
	probeName := "aip-probe-" + strings.ReplaceAll(Name(project), "-", "_")
	command := "tmux new-session -d -s " + shellQuote(probeName) + " -c " + shellQuote(workspaceWorkdir) +
		" sleep 1 >/dev/null 2>&1 || exit $?; tmux has-session -t " + shellQuote(probeName) + " >/dev/null 2>&1; tmux kill-session -t " + shellQuote(probeName) + " >/dev/null 2>&1 || true"
	result, err := manager.probeInVM(project, func(ctx context.Context) (ExecResult, error) {
		return manager.Sandbox.ExecContext(ctx, Name(project), []string{"sh", "-c", command})
	})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		if stderrSandboxNotFound(result.Stderr) {
			return ErrWorkspaceStale
		}
		return ErrWorkspaceUnresponsive
	}
	return nil
}

// requireTmux verifies tmux is on PATH inside the running microVM before a tmux
// session is launched. It runs a buffered probe (not the interactive PTY), so a
// missing tmux surfaces as a clear ErrTmuxMissing instead of msb failing to exec
// tmux through the PTY (which returns a misleading success).
func (manager Manager) requireTmux(project string) error {
	// Probe with retry so the interactive entry points (shell/agent/attach) recover
	// from a transient post-sleep stale connection instead of erroring; a real
	// failure is classified (stale VM vs. overloaded VM).
	result, err := manager.probeInVM(project, func(ctx context.Context) (ExecResult, error) {
		return manager.Sandbox.ExecContext(ctx, Name(project), []string{"sh", "-c", "command -v tmux >/dev/null 2>&1"})
	})
	if err != nil {
		return err // classified
	}
	if result.ExitCode != 0 {
		// msb reporting the sandbox is gone (stale handle) is NOT "tmux missing".
		if stderrSandboxNotFound(result.Stderr) {
			return ErrWorkspaceStale
		}
		return ErrTmuxMissing
	}
	return nil
}

// Attach opens (creating it if needed) the named tmux session in the project's
// workspace microVM. A missing/empty session name attaches the default "shell"
// session. When the session is created fresh it opens a login shell that starts in
// the project directory (~/project); an existing session is reattached as-is. This
// backs `ai attach [session]`, the `ai shell` picker's create path, and the TUI
// Sessions view.
func (manager Manager) Attach(project, session string) error {
	if session == "" {
		session = shellSessionName
	}
	return manager.launchTmuxSession(project, session, projectLoginShell())
}

// Agent starts (or reattaches to) a per-CLI tmux session running the named agent
// CLI in ~/project. The session is named after the CLI (opencode, pi, …) so each
// agent has one persistent, reattachable session and multiple agents can run
// concurrently. An unknown CLI returns ErrUnknownAgentCLI (→ exit 2).
func (manager Manager) Agent(project, cli string) error {
	launch, err := agentLaunchCommand(cli)
	if err != nil {
		return err
	}
	return manager.launchTmuxSession(project, cli, launch)
}

// WorkspaceLogTail returns the last lines of the project's workspace microVM
// captured output (`msb logs`). It backs the TUI "Workspace Log" tab. It does NOT gate
// on the platform's "started" state handle: `msb logs` works for any sandbox that
// exists in msb (created/starting/running), and the log is most useful DURING startup
// (build + image-pull progress) — before the handle flips to "started". A sandbox
// that does not exist yet (never created) surfaces msb's "sandbox not found", which is
// reported as an EMPTY log (not an error), so the tab reads "no output yet" instead
// of a false "not running".
func (manager Manager) WorkspaceLogTail(project string, lines int) (string, error) {
	if _, err := resolveProjectRoot(project); err != nil {
		return "", err
	}
	out, err := manager.Sandbox.LogTail(Name(project), lines)
	if err != nil {
		if isSandboxNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// isSandboxNotFound reports whether an error is msb's "sandbox not found" (the
// microVM has not been created yet), as opposed to a real read failure.
func isSandboxNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
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
	// Delimit fields with '|', NOT a tab: `msb exec` MANGLES tab bytes in argv (a
	// '\t' in the format string arrives in the guest as '_'), which collapsed every
	// row into a single field so parseSessions skipped them all and `ai sessions`
	// ALWAYS reported zero sessions (verified against a live VM). '|' survives the
	// transport intact and can never occur in a field (session names are validated to
	// letters/digits/'-'/'_', attached is 0/1, activity is a Unix epoch). The probe
	// retries on a timeout so a wedged-after-sleep VM self-heals (see probeInVM).
	result, err := manager.probeInVM(project, func(ctx context.Context) (ExecResult, error) {
		return manager.Sandbox.ExecContext(ctx, Name(project), []string{"sh", "-c", tmuxListSessionsCommand()})
	})
	if err != nil {
		return nil, err // already classified (stale / unresponsive)
	}
	if result.ExitCode != 0 {
		// msb itself reporting the sandbox is gone (the handle is stale, e.g. after a
		// sleep/teardown) surfaces as a non-zero exit with "sandbox not found" — that
		// is NOT a tmux failure; report it as stale so the user runs `ai restart`.
		if stderrSandboxNotFound(result.Stderr) {
			return nil, ErrWorkspaceStale
		}
		// `tmux list-sessions` with no running tmux server yet is a non-zero exit —
		// treat it as ZERO sessions, not a failure. The message varies by tmux build:
		// "no server running on …" OR "error connecting to /tmp/tmux-…/default (No such
		// file or directory)" (the socket doesn't exist until the first session). Exec
		// returns the inner non-zero exit as data (not a Go error), so the check is on
		// the ExecResult (§4.5).
		if isNoTmuxServer(result.Stderr) {
			return []Session{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return parseSessions(result.Stdout), nil
}

// stderrSandboxNotFound reports whether msb stderr says the sandbox does not exist
// (a stale lifecycle handle: the workspace is marked started but its microVM is
// gone). It is distinct from isSandboxNotFound, which inspects a Go error.
func stderrSandboxNotFound(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "sandbox not found")
}

// isNoTmuxServer reports whether a tmux stderr indicates simply that no tmux server
// is running yet (no sessions), across tmux builds that word it differently.
func isNoTmuxServer(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting to") ||
		strings.Contains(lower, "no such file or directory")
}

func tmuxListSessionsCommand() string {
	format := "#{session_name}|#{session_attached}|#{session_activity}"
	return "err=$(mktemp); " +
		"tmux list-sessions -F " + shellQuote(format) + " 2>\"$err\"; code=$?; " +
		"if [ $code -eq 0 ]; then rm -f \"$err\"; exit 0; fi; " +
		"if grep -Eqi 'no server running|error connecting|no such file or directory' \"$err\"; then rm -f \"$err\"; exit 0; fi; " +
		"cat \"$err\" >&2; rm -f \"$err\"; exit $code"
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

// tmuxNewSessionAttach builds an attach-or-create tmux invocation opening in
// ~/project: `tmux new-session -A -s <session> -c ~/project [command]`. `-A` makes
// it idempotent and reattachable — it creates the session named session on first use
// and reattaches on every later call, in ONE interactive command (so there is no
// window between a separate detached create and the attach for msb to tear down).
// The tmux server daemonizes, so the session survives a DETACH (and is listed by
// `ai sessions`); it ends only when its shell exits. When command is non-empty it is
// the session's program (an agent CLI or a login shell); nil opens the default shell.
func tmuxNewSessionAttach(session string, command []string) []string {
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
//
// The launch is wrapped in a LOGIN shell that first sources the in-VM agent env
// file (the gateway env vars for claude-code/codex/gemini, key in-VM only) and then
// execs the CLI — so the env-routed CLIs reach the gateway with the workspace's
// scoped virtual key. opencode/pi take their config from a file and ignore the env,
// but wrapping them uniformly is harmless (they still get a normal login env).
func agentLaunchCommand(cli string) ([]string, error) {
	launch, ok := agentValidCLIs[cli]
	if !ok {
		valid := make([]string, 0, len(agentValidCLIs))
		for name := range agentValidCLIs {
			valid = append(valid, name)
		}
		sort.Strings(valid)
		return nil, fmt.Errorf("%w %q (valid: %s)", ErrUnknownAgentCLI, cli, strings.Join(valid, ", "))
	}
	return wrapWithAgentEnv(launch), nil
}

// wrapWithAgentEnv wraps an in-VM command so it runs in a login shell that first
// sources the agent env file (if present) and then execs the command. The command
// argv is passed as positional parameters ($1, $2, …) so values never need
// re-quoting. A missing env file is tolerated (the test `-f` guard), so a
// not-yet-provisioned VM still launches the CLI.
func wrapWithAgentEnv(command []string) []string {
	script := "[ -f " + shellQuoteGuest(agentEnvGuestPath) + " ] && . " + shellQuoteGuest(agentEnvGuestPath) + "; exec \"$@\""
	argv := []string{"bash", "-lc", script, "bash"}
	return append(argv, command...)
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
		// Fields are '|'-delimited (see ListSessions — a tab does not survive
		// `msb exec`). SplitN keeps any stray '|' out of the first two fields.
		fields := strings.SplitN(line, "|", 3)
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
