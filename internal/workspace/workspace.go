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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/egress"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/overlay"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/state"
	"github.com/jt-helsinki/stack-genie/internal/sysinfo"
	"github.com/jt-helsinki/stack-genie/internal/ui"
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

// Interactive-shell config written at start. The framework rc files (oh-my-bash owns
// ~/.bashrc, oh-my-zsh owns ~/.zshrc) are NEVER overwritten — a managed, marker-delimited
// block (agentcfg.ShellRCBlock) is appended idempotently to BOTH so either shell sources
// the agent gateway env + the Headroom-wrap aliases (agentcfg.ShellAliases, written whole
// to shellAliasesGuestPath). The chosen default shell (config.Workspace.Shell) is applied
// with chsh for zsh (best-effort); bash needs no chsh.
const (
	shellAliasesGuestPath = agentcfg.ShellAliasesFileGuestPath
	bashrcGuestPath       = "/home/workspace/.bashrc"
	zshrcGuestPath        = "/home/workspace/.zshrc"
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
// the agent CLI (opencode, omp, …).
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

// relayRetryDelay is the brief pause before retrying a launcher exec on a transiently
// wedged relay, giving the freshly-reset connection a moment to re-establish.
const relayRetryDelay = 750 * time.Millisecond

// launchDetachedRetry runs a SHORT detached-launcher exec (the tiny `setsid … &` that
// backgrounds a best-effort start step: app autostart, Caveman, code-review-graph,
// codebase-memory, graphify-MCP, dashboards) with ONE retry on a transiently wedged
// relay. The busy workspace-start exec sequence — containerd readiness polling plus
// back-to-back detached launchers — can momentarily saturate the single reused msb
// relay, so the next exec returns ErrWorkspaceUnresponsive even though the VM is fine.
// On that error it RESETS the relay (ReleaseConnection, so the next exec dials a fresh
// handle), waits briefly, and retries once. asRoot selects ExecRootContext (nerdctl app
// containers need root) vs ExecContext (workspace user). Best-effort: returns the final
// error for the caller to warn on; a non-unresponsive error is returned without retry.
func (manager Manager) launchDetachedRetry(name string, argv []string, asRoot bool, timeout time.Duration) error {
	run := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var err error
		if asRoot {
			_, err = manager.Sandbox.ExecRootContext(ctx, name, argv)
		} else {
			_, err = manager.Sandbox.ExecContext(ctx, name, argv)
		}
		return err
	}
	err := run()
	if errors.Is(err, ErrWorkspaceUnresponsive) {
		manager.ReleaseConnection(name)
		time.Sleep(relayRetryDelay)
		err = run()
	}
	return err
}

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
	// name — opencode, omp, … — for an agent session).
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
	// Disk is the writable rootfs (OCI overlay upper) size as "<GiB>" (e.g. "20"); it
	// backs containerd's in-VM image store so multi-GB app images have room. Empty falls
	// back to the platform default (microVMDisk).
	Disk string
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
	// KeyInfo returns LiteLLM's metadata for a virtual key. It errors when the key
	// is unknown (revoked / cleared from the token table) OR the gateway is
	// unreachable; the self-heal on attach uses it to detect an orphaned in-VM key.
	KeyInfo(key string) (litellm.KeyDetails, error)
}

// ServedModels lists the models the LiteLLM gateway currently SERVES — its
// DB-backed models (a keyed provider's catalog models + the registered Ollama
// models). It is the live source of the in-VM agent model picker in the
// catalog-driven model system, defined locally so tests can supply a fake without a
// live gateway (the real impl wraps litellm.KeyManager.ListModels). A nil source,
// or a ServedModels that errors (gateway down at start), degrades gracefully — the
// workspace still starts, with an empty picker, never failing over a model lookup.
type ServedModels interface {
	ServedModels() ([]agentcfg.Model, error)
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
// logStep prints a workspace-lifecycle progress line to stdout. When `ai start`/
// `ai create` runs (directly, or DETACHED from the TUI with stdout tee'd to
// run/<action>.log), these lines make the setup sequence — image build, microVM
// boot, agent-provider registration, containerd, venv, Graphify, Caveman —
// visible in the `ai ui` Logs view (which tails that build log during a lifecycle
// op) and on the terminal, so a stall or failure is no longer invisible.
func logStep(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, "▸ "+format+"\n", args...)
}

func (manager Manager) Start(project string) (*state.Workspace, error) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return nil, err
	}
	name := Name(project)
	imageRef := name + ":latest"
	logStep("building workspace image %s", imageRef)
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
		Disk:        projectConfig.Workspace.DiskLimit,
	}
	logStep("creating microVM")
	if err := manager.Sandbox.Create(name, imageRef, root, overlayPath, resources, netArgs); err != nil {
		return nil, err
	}
	logStep("booting microVM")
	if err := manager.Sandbox.Start(name); err != nil {
		return nil, err
	}
	// Correct the guest clock before anything time-sensitive runs (agent provider
	// TLS, in-VM image pulls): a fresh boot — or a recreate after the host slept —
	// can leave the VM's clock skewed by the sleep duration. Best-effort: a failure
	// must never fail the start.
	_ = manager.Sandbox.SyncClock(name)
	// Pin the host-gateway name to its IPv4 address inside the VM. msb seeds the
	// guest /etc/hosts with BOTH an A and AAAA record for host.microsandbox.internal,
	// and glibc getaddrinfo prefers the IPv6 one — but msb only forwards guest→host
	// traffic over IPv4 (the host nginx publish is IPv4-only), so an IPv6 connect to
	// the gateway reaches the msb gateway then RESETs (Node fetch surfaces this as
	// "socket connection was closed unexpectedly"; curl as "Recv failure: Connection
	// reset by peer"). Rewriting /etc/hosts to the IPv4-only entry makes every in-VM
	// client (Node agents, curl, Go, Rust) reach the gateway regardless of resolver
	// order — language-agnostic, unlike a gai.conf precedence tweak (which Node's
	// verbatim dns.lookup ignores). Only for the LOCAL msb gateway; a remote gateway
	// (client mode) is a real routable address and must not be touched. Best-effort:
	// a failure must never fail the start.
	if gatewayHost == runtime.DefaultGatewayHost {
		manager.pinHostGatewayIPv4(name, gatewayHost)
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
	// Register the agent CLIs' provider config so opencode inside the microVM
	// talk to the host Headroom proxy through a per-workspace scoped LiteLLM
	// virtual key (arch §15, §17). The key flows host→VM only; it is never
	// written to platform disk.
	logStep("registering agent providers (scoped gateway key + model configs)")
	if err := manager.registerAgentProviders(name, project, root, projectConfig, gatewayURL); err != nil {
		return nil, err
	}
	logStep("starting in-VM container runtime (containerd)")
	// Bring up the rootful in-VM container runtime (containerd) so nerdctl works
	// inside the workspace. BEST-EFFORT + bounded — a failure here must NOT fail the
	// workspace start.
	manager.ensureContainerd(name)
	// ensureContainerd polls `nerdctl info` in a tight loop until the daemon serves, which
	// can transiently saturate the single reused msb relay. Reset it here so the very first
	// app-autostart launcher below dials a FRESH handle instead of inheriting a wedged one
	// (launchDetachedRetry still retries if it wedges again mid-sequence).
	manager.ReleaseConnection(name)
	// Auto-start every installed in-VM app + agent-CLI dashboard so they are reachable
	// from the host browser by default (their host ports were already published above).
	// This is DETACHED + best-effort: a heavy first-start `nerdctl pull` is a long in-VM
	// exec that, run inline, would block the workspace start AND every other exec (shell,
	// session list) for the whole pull — so the app containers are launched in a setsid'd
	// background script that returns in milliseconds. Never fails or blocks the start.
	manager.autostartApps(name, projectConfig, gatewayURL)
	logStep("creating project virtualenv (.venv-msb)")
	// Create the per-project Python virtualenv (.venv-msb) using the guest's baked-in
	// Python. Best-effort — never fails the workspace start.
	manager.ensureVenv(name)
	logStep("linking shared agent resources (skills/agents/prompts)")
	// Wire the shared <project>/.ai-platform/{agents,skills,prompts} pool into each
	// installed CLI's real per-project dirs via relative symlinks, so one copy of a
	// skill/agent/prompt serves every client. Host-side + best-effort; MUST run BEFORE
	// registerGraphify + registerCaveman so their per-CLI skill files land in the shared
	// pool through the symlinks.
	if err := linkSharedResources(root, projectConfig.Agent.Tools); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not link shared agent resources in workspace %q: %v\n", name, err)
	}
	// Register Graphify with each selected agent CLI. Runs HERE (not at image build)
	// because `graphify install --project` writes into the project dir (~/project),
	// which is only bind-mounted at runtime. Best-effort — never fails the start.
	logStep("registering Graphify with the selected agent CLIs")
	manager.registerGraphify(name, projectConfig)
	// Install the Caveman output-compression toolkit into each detected CLI (native
	// skills/plugin/hooks/statusline/extension) and mirror its skills into the shared
	// pool for omp. Once-guarded, network-bound, best-effort — never fails the start.
	manager.registerCaveman(name, projectConfig)
	// Install the opt-in code-graph / code-memory tools and register each as an MCP server
	// with the installed agent CLIs. code-review-graph is DETACHED (its codebase `build`
	// can be long); codebase-memory-mcp is a bounded blocking config-only exec. Both are
	// once-guarded + best-effort — never fail the start, retry next start on failure.
	manager.registerCodeReviewGraph(name, projectConfig)
	manager.registerCodebaseMemory(name, projectConfig)
	logStep("workspace %q started", project)
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
// routes the gateway-capable agent CLIs through the host Headroom proxy with that key
// (copilot is gateway-incapable — forced-oauth, native GitHub auth — so it is skipped). It
// reads each CLI's KEYLESS host-side template from <project>/.ai-platform/agents/
// (scaffolding the default templates back if absent — without the key), merges in
// the dynamic values (the freshly-minted key, the served-model picker, the Headroom
// knobs), and writes the FINAL key-bearing config INTO the microVM (key host→VM
// only — never to platform disk):
//
//   - opencode: a merged JSON config file (the per-request Headroom knobs ride on it).
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

	// Per-agent auth mode: the OAuth-capable CLIs (claude-code/codex/gemini) set to
	// "oauth" use their OWN subscription login and talk DIRECTLY to the provider,
	// bypassing the gateway (and its firewall). Everything else (incl. api-key
	// claude-code/codex/gemini and always opencode/omp) routes through the gateway.
	// oauthList preserves tool order (for the deterministic shell aliases); oauthSet is
	// the lookup used to omit gateway env / pick the oauth config variants.
	oauthList := oauthAgentList(projectConfig)
	oauthSet := make(map[string]bool, len(oauthList))
	for _, cli := range oauthList {
		oauthSet[cli] = true
	}

	// Persist the agent CLIs' state dirs to the overlay so a CLI's per-project memory —
	// notably opencode's last-used model — survives microVM restarts (see below). For
	// oauth agents this ALSO persists their native-login credential dir (~/.claude,
	// ~/.codex, ~/.gemini) so the subscription login survives a restart.
	manager.linkAgentStateDirs(name, oauthSet)

	// SEED-THEN-REMEMBER default (chosen behavior): the model picked at setup (stored as
	// agent.graphify_model, registered in the gateway as ollama/<model>) is SEEDED as
	// every CLI's default on the FIRST start only. A host marker under .ai-platform (same
	// dir on host + in-VM) records that. On LATER starts we pass an empty default, which
	// makes MergeOpenCodeConfig actively DROP the pinned model so the
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

	// Write the KEYLESS per-CLI provider configs into each CLI's DEFAULT location. The
	// scoped virtual key is NEVER written here: opencode references it via {env:}
	// interpolation, codex via env_key, claude via the exported ANTHROPIC_AUTH_TOKEN — the
	// key lives ONLY in the in-VM agent env file below. opencode carries the served-
	// model LIST (writeModelListConfigs — the same helper used to refresh it on attach);
	// codex/gemini/claude carry no list (they name any served model per request).
	//
	// Enabled AI-tool MCP servers, injected into the configs the platform manages WHOLE
	// (codex/hermes/omp) so they survive each start's rewrite (the CLIs whose
	// configs the platform does NOT own get these tools via their native install instead).
	// graphify runs from the project venv (.venv-msb, where graphifyy[mcp] is installed).
	mcpServers := agentcfg.EnabledMCPServers(
		projectConfig.Context.CodeReviewGraphEnabledOrDefault(),
		projectConfig.Context.CodebaseMemoryEnabledOrDefault(),
		projectConfig.Context.GraphifyEnabledOrDefault(),
		workspaceWorkdir+"/.venv-msb/bin/python",
	)
	// opencode: written when selected — or when NO tools are configured at all, since
	// opencode is the platform default tool (a degenerate/legacy empty Tools list still
	// gets the default). It carries the refreshable served-model list.
	if len(projectConfig.Agent.Tools) == 0 || slices.Contains(projectConfig.Agent.Tools, "opencode") {
		if err := manager.writeModelListConfigs(name, root, gatewayURL, defaultModel, models, keepTurns, outputBufferTokens); err != nil {
			return err
		}
	}

	// claude-code: written only when selected. api-key mode gets the gateway base-URL env
	// block; oauth mode gets an EMPTY env block (no base URL) so its own subscription login
	// reaches Anthropic direct.
	if slices.Contains(projectConfig.Agent.Tools, "claude-code") {
		claudeExisting := readHostFileOrNil(projectConfigPath(root, ".claude", "settings.json"))
		var claudeConfig []byte
		if oauthSet["claude-code"] {
			claudeConfig, err = agentcfg.MergeClaudeSettingsOAuth(claudeExisting)
		} else {
			claudeConfig, err = agentcfg.MergeClaudeSettings(claudeExisting, gatewayURL)
		}
		if err != nil {
			return err
		}
		if err := writeHostFile(projectConfigPath(root, ".claude", "settings.json"), claudeConfig); err != nil {
			return err
		}
	}

	// codex: written only when selected. api-key mode gets the KEYLESS gateway provider
	// block (fully platform-managed, overwritten each start; the key is env-supplied via
	// env_key). oauth mode gets the ChatGPT-subscription config (no gateway provider —
	// direct to OpenAI). Either way a global in-VM trust entry (off host disk) is written so
	// codex loads the project config. The enabled tools' MCP servers are appended as
	// [mcp_servers.*] tables (local tools — they work regardless of auth mode).
	if slices.Contains(projectConfig.Agent.Tools, "codex") {
		codexConfig := agentcfg.CodexConfig(gatewayURL, defaultModel)
		if oauthSet["codex"] {
			codexConfig = agentcfg.CodexConfigOAuth()
		}
		codexConfig = agentcfg.AppendCodexMCP(codexConfig, mcpServers)
		if err := writeHostFile(projectConfigPath(root, ".codex", "config.toml"), codexConfig); err != nil {
			return err
		}
		if err := manager.Sandbox.WriteFile(name, agentcfg.CodexConfigGuestPath, agentcfg.CodexTrustConfig()); err != nil {
			return err
		}
	}

	// omp (Oh My Pi) — written only when selected. Its provider/models config is KEYLESS
	// YAML at the GLOBAL ~/.omp/agent/models.yml (the path omp reads; in-VM, off host
	// disk) with openai-models-list discovery so omp lists exactly the served models; the
	// project default + provider order live in <project>/.omp/config.yml (host, seeded per
	// seed-then-remember). ~/.omp is symlinked to /persist (linkAgentStateDirs) so omp's
	// last-used selection + hindsight memory survive restarts.
	if slices.Contains(projectConfig.Agent.Tools, "omp") {
		ompModels, err := agentcfg.OmpModelsConfig(gatewayURL, agentcfg.OmpAPIKeyRef)
		if err != nil {
			return err
		}
		if err := manager.Sandbox.WriteFile(name, agentcfg.OmpGlobalModelsGuest, ompModels); err != nil {
			return err
		}
		ompConfig, err := agentcfg.OmpConfig(defaultModel)
		if err != nil {
			return err
		}
		if err := writeHostFile(projectConfigPath(root, ".omp", "config.yml"), ompConfig); err != nil {
			return err
		}
		// omp reads stdio MCP servers from a SEPARATE .omp/mcp.json (project entries shadow
		// the user file) — not auto-detected by the tools' installers, so the platform writes
		// the enabled tools' servers here.
		if len(mcpServers) > 0 {
			ompMCP, err := agentcfg.OmpMcpConfig(mcpServers)
			if err != nil {
				return err
			}
			if err := writeHostFile(projectConfigPath(root, ".omp", "mcp.json"), ompMCP); err != nil {
				return err
			}
		}
	}

	// hermes — written only when selected. KEYLESS YAML at the GLOBAL ~/.hermes/config.yaml
	// (the path hermes reads; in-VM, off host disk): an aip-gateway provider whose key_env
	// NAMES AIP_GATEWAY_KEY, plus external_dirs pointing at the shared skills pool. Hermes
	// lists models by endpoint discovery, so — like omp — there is no served list to refresh
	// (nothing in writeModelListConfigs). ~/.hermes is symlinked to /persist so its last-used
	// selection + skills survive restarts.
	if slices.Contains(projectConfig.Agent.Tools, "hermes") {
		hermesConfig, err := agentcfg.HermesConfig(gatewayURL, defaultModel)
		if err != nil {
			return err
		}
		// The enabled tools' MCP servers ride in the SAME config.yaml (mcp_servers) the
		// platform rewrites each start.
		hermesConfig, err = agentcfg.InjectHermesMCP(hermesConfig, mcpServers)
		if err != nil {
			return err
		}
		if err := manager.Sandbox.WriteFile(name, agentcfg.HermesConfigGuest, hermesConfig); err != nil {
			return err
		}
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
	if err := manager.Sandbox.WriteFile(name, agentEnvGuestPath, agentcfg.AgentEnvScript(gatewayURL, apiKey, projectConfig.Agent.GraphifyModel, oauthSet)); err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, bashProfileGuestPath, agentcfg.BashProfile()); err != nil {
		return err
	}

	// Write the Headroom-wrap alias snippet and make the chosen interactive shell source
	// it + the agent env. Aliases are written ONLY for OAUTH-mode agents Headroom can wrap
	// (claude-code→claude, codex→codex): those bypass the gateway and go direct, so wrapping
	// them with `headroom wrap` restores input-compression in front of the CLI. api-key
	// agents already route through the gateway (where Headroom sits) and get NO alias;
	// gemini is not Headroom-wrappable so an oauth gemini gets no alias either.
	if err := manager.Sandbox.WriteFile(name, shellAliasesGuestPath, agentcfg.ShellAliases(oauthList)); err != nil {
		return err
	}
	manager.applyShellChoice(name, projectConfig.Workspace.Shell)

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

// applyShellChoice makes every interactive shell (bash + zsh) source the managed agent
// gateway env + Headroom-wrap aliases, and — when the project selected zsh — switches the
// workspace user's login shell to zsh. It appends agentcfg.ShellRCBlock idempotently to
// the framework-owned ~/.bashrc AND ~/.zshrc (oh-my-bash / oh-my-zsh own those files, so
// they are never overwritten — the block is stripped and re-appended between its markers).
// Both shells are configured regardless of the choice so a manually-launched shell still
// gets the aliases; only the login DEFAULT changes. All steps are BEST-EFFORT: a failure
// leaves a usable workspace, so it warns and continues rather than failing the start.
//
// hardware bring-up: the in-VM `chsh` taking effect for future sessions and both rc files
// sourcing the block (so `headroom wrap` aliases resolve in a real shell / tmux pane) are
// verified on a provisioned Apple Silicon host; the host-side orchestration (the append
// argv, the shell selection) is unit-tested against the fake sandbox.
func (manager Manager) applyShellChoice(name, shell string) {
	block := agentcfg.ShellRCBlock()
	for _, rcPath := range []string{bashrcGuestPath, zshrcGuestPath} {
		if err := manager.appendManagedBlock(name, rcPath, block); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: could not update %s in workspace %q (continuing): %v\n", rcPath, name, err)
		}
	}
	if shell == "zsh" {
		// Switch the workspace user's login shell to zsh. Best-effort — the rc block is
		// already in place for both shells, so a chsh failure just leaves bash as default.
		if _, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c", `chsh -s "$(command -v zsh)" workspace`}); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: could not set the zsh login shell in workspace %q (continuing): %v\n", name, err)
		}
	}
}

// appendManagedBlock appends a marker-delimited managed block to a framework-owned rc file
// inside the microVM, idempotently: it stages the block to a temp path via WriteFile
// (payload off argv, as the workspace user who owns the rc files), then runs an Exec that
// strips any prior copy between the markers and re-appends the fresh block, and removes the
// temp file. Re-running yields the same result, so restarts never duplicate the block.
func (manager Manager) appendManagedBlock(name, rcPath string, block []byte) error {
	stage := rcPath + ".aip-block"
	if err := manager.Sandbox.WriteFile(name, stage, block); err != nil {
		return err
	}
	// `sed '/begin/,/end/d'` drops any prior managed block (a no-op on the first start,
	// when the file has no markers); we then append the fresh staged block. `touch`
	// tolerates a not-yet-created rc file. The markers carry no sed metacharacters.
	script := fmt.Sprintf(
		"touch %[1]s && tmp=%[1]s.aip.$$ && "+
			"{ sed '/%[3]s/,/%[4]s/d' %[1]s 2>/dev/null || cat %[1]s; } > \"$tmp\" && "+
			"cat %[2]s >> \"$tmp\" && mv \"$tmp\" %[1]s && rm -f %[2]s",
		shellQuoteGuest(rcPath), shellQuoteGuest(stage),
		agentcfg.ShellRCMarkerBegin, agentcfg.ShellRCMarkerEnd)
	_, err := manager.Sandbox.Exec(name, []string{"sh", "-c", script})
	return err
}

// containerdLog is the in-VM path containerd's stdout/stderr is redirected to
// when ensureContainerd boots it, so the daemon's output is inspectable.
const containerdLog = "/var/log/containerd.log"

// fuseOverlayfsLog is the in-VM path the containerd-fuse-overlayfs-grpc snapshotter
// proxy's output is redirected to when ensureContainerd starts it.
const fuseOverlayfsLog = "/var/log/containerd-fuse-overlayfs.log"

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

// The Caveman install is a multi-minute network + node operation, so it runs
// DETACHED (setsid) rather than as a blocking exec — see registerCaveman. These
// bound/locate the DETACHED launch, not the install itself (which runs in the
// background for as long as it needs, once-guarded by its marker).
const (
	// cavemanLaunchTimeout bounds only the tiny launcher exec that setsid-backgrounds
	// the install and returns; the install runs on past it.
	cavemanLaunchTimeout = 30 * time.Second
	// cavemanScriptGuest is where the once-guarded install script is staged in-VM.
	cavemanScriptGuest = "/tmp/caveman-install.sh"
)

// The installed in-VM apps + agent dashboards are auto-started DETACHED at workspace
// start (see autostartApps): a heavy first-start `nerdctl pull` must never block the
// start or wedge the single msb relay, so the app-container launch is setsid'd into a
// background script exactly like the Caveman install.
const (
	// appsAutostartLaunchTimeout bounds only the tiny launcher exec that setsid-backgrounds
	// the app-autostart script and returns; the script itself runs on past it.
	appsAutostartLaunchTimeout = 30 * time.Second
	// appsAutostartScriptGuest is where the detached app-autostart script is staged in-VM.
	// It is a GUEST-ONLY path — deliberately NOT under <project>/.ai-platform/run (which is
	// bind-mounted to the HOST) — so the staged script never lands on host disk. (The script
	// only REFERENCES the scoped key via a shell variable, but keeping it off host disk
	// upholds the hard "key never on host disk" invariant with zero exposure.)
	appsAutostartScriptGuest = "/tmp/apps-autostart.sh"
)

// The code-review-graph registration runs its per-CLI `install` + a full-codebase
// `build` + a `visualize`, so — like the Caveman install — it is DETACHED (setsid) rather
// than a blocking exec: the `build` can be long on a large repo and must never block the
// workspace start or wedge the single msb agent-relay (see registerCodeReviewGraph).
const (
	// codeReviewGraphLaunchTimeout bounds only the tiny launcher exec that
	// setsid-backgrounds the install and returns; the install runs on past it.
	codeReviewGraphLaunchTimeout = 30 * time.Second
	// codeReviewGraphScriptGuest is where the once-guarded install script is staged in-VM.
	codeReviewGraphScriptGuest = "/tmp/code-review-graph-install.sh"
	// graphifyMCPScriptGuest is where the detached graphify-MCP setup script (venv install
	// of graphifyy[mcp] + offline graph build) is staged in-VM.
	graphifyMCPScriptGuest = "/tmp/graphify-mcp-setup.sh"
)

// codebaseMemoryInstallTimeout bounds the codebase-memory-mcp `install` exec. It is
// config-only (the binary is baked into the image; `install` just auto-detects the
// installed agent CLIs and writes their MCP config), so — like `graphify install` — it
// runs as a bounded BLOCKING exec, with an in-script `timeout` guard as defense-in-depth.
const codebaseMemoryInstallTimeout = 60 * time.Second

// linkAgentStateDirs points the agent CLIs' mutable STATE directories at the persistent
// overlay (/persist) so a CLI's per-project memory survives microVM restarts — the VM
// home does NOT persist, so without this opencode forgets the last-used /model on every
// restart. opencode persists under ~/.local/share/opencode (its XDG_DATA_HOME). /persist is root-owned, so
// the per-CLI dirs are created + handed to the workspace user as root, then symlinked in
// as the workspace user. Best-effort: a failure just falls back to the ephemeral home.
func (manager Manager) linkAgentStateDirs(name string, oauthAgents map[string]bool) {
	// stateLink pairs an in-VM home path with its /persist/agents/<key> target. The
	// always-present agent STATE dirs come first; oauth agents ALSO get their native-login
	// credential dir persisted so a subscription login survives a microVM restart.
	// seedMarker (optional): a subpath under the baked home that indicates a whole-install
	// living there (not just state). When set, the install is restored into /persist if the
	// marker is absent there (see the seed step below). hermes bakes its venv + binary under
	// ~/.hermes at image build, so it needs this; opencode/omp keep only state.
	type stateLink struct{ home, key, seedMarker string }
	links := []stateLink{
		{"~/.local/share/opencode", "opencode", ""},
		{"~/.omp", "omp", ""},
		// hermes keeps its global config + state (last-used model, memory, skills) AND its
		// entire install (venv + binary, baked at image build) in ~/.hermes; persist it so a
		// restart remembers, and seed the baked install so the symlink never shadows it away.
		{"~/.hermes", "hermes", "hermes-agent"},
	}
	// oauthCredDirs maps each OAuth-eligible CLI to its native-login credential dir. An
	// oauth agent's dir is persisted so the subscription login survives a microVM restart.
	// Iterated in a fixed order for a deterministic set of exec commands.
	oauthCredDirs := []struct{ cli, home, key string }{
		{"claude-code", "~/.claude", "claude"},
		{"codex", "~/.codex", "codex"},
		{"gemini", "~/.gemini", "gemini"},
		{"copilot", "~/.copilot", "copilot"},
	}
	for _, entry := range oauthCredDirs {
		if oauthAgents[entry.cli] {
			links = append(links, stateLink{entry.home, entry.key, ""})
		}
	}

	// Create + own every /persist target as root (/persist is root-owned).
	mkdir := "mkdir -p"
	for _, entry := range links {
		mkdir += " /persist/agents/" + entry.key
	}
	mkdir += " && chown -R workspace /persist/agents"
	if _, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c", mkdir}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not prepare persistent agent state in workspace %q (continuing): %v\n", name, err)
		return
	}
	// rm -rf on a symlink removes only the link (a re-start's existing symlink), not the
	// /persist target, so accumulated state is preserved across restarts. omp keeps its
	// last-used model + hindsight memory in ~/.omp (agent.db); an oauth agent's ~/.claude/
	// ~/.codex/~/.gemini holds its OAuth credentials.
	//
	// SEED FIRST (marker entries only): some CLIs bake their ENTIRE install under the home
	// path we persist — hermes's venv + binary live in ~/.hermes, written at image build.
	// A blind `rm -rf ~/.hermes` before the symlink deletes that install and points the
	// link at an overlay that lacks it, breaking `hermes dashboard`. Because msb rebuilds
	// the rootfs from the image on every start (--replace), the baked install is present
	// under the home path each start, so: if the baked home carries the install marker but
	// the persist target lacks it, no-clobber (`cp -an`) copy the baked tree into persist —
	// restoring the install while KEEPING any already-persisted state (config, last-used
	// model). Once seeded, later starts find the marker present and skip. Entries with no
	// marker (opencode/omp/oauth creds) keep only state and are never seeded.
	link := "set -e; mkdir -p ~/.local/share"
	for _, entry := range links {
		target := "/persist/agents/" + entry.key
		link += "; mkdir -p " + target
		if entry.seedMarker != "" {
			link += "; if [ -e " + entry.home + "/" + entry.seedMarker + " ] && [ ! -e " + target + "/" + entry.seedMarker + " ]; then cp -an " + entry.home + "/. " + target + "/ 2>/dev/null || true; fi"
		}
		link += "; rm -rf " + entry.home + "; ln -sfn " + target + " " + entry.home
	}
	if _, err := manager.Sandbox.Exec(name, []string{"bash", "-lc", link}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not link persistent agent state in workspace %q (continuing): %v\n", name, err)
	}
}

// oauthAgentList returns the installed CLIs configured for OAUTH (subscription) auth, in
// the project's tool order. This is every OAuth-ELIGIBLE CLI whose resolved auth mode is
// oauth: the OAuth-capable CLIs (claude-code/codex/gemini) set to oauth, plus the
// forced-oauth CLIs (copilot — always oauth). Everything else is always gateway/api-key
// and never appears here.
func oauthAgentList(projectConfig *config.Config) []string {
	var list []string
	for _, cli := range projectConfig.Agent.Tools {
		if config.IsOAuthEligible(cli) && projectConfig.Agent.AuthMode(cli) == "oauth" {
			list = append(list, cli)
		}
	}
	return list
}

// writeModelListConfigs writes the agent CLIs that carry a SERVED-MODEL LIST — currently
// only opencode (host project config: the list is REPLACED wholesale while all other user
// keys merge/survive). codex/gemini/claude carry NO list (they name any served model per
// request), so they are not touched here. A defaultModel of "" pins no model and drops any
// previously-seeded one (so a CLI's persisted last-used selection wins). When the served
// list is EMPTY (gateway unreachable), the existing list is LEFT UNTOUCHED — opencode's
// merge preserves it — so a transient outage never wipes a good list. Shared refresh used
// at start AND on attach.
func (manager Manager) writeModelListConfigs(name, root, gatewayURL, defaultModel string, models []agentcfg.Model, keepTurns, outputBufferTokens int) error {
	openCodeConfig, err := agentcfg.MergeOpenCodeConfig(
		readHostFileOrNil(projectConfigPath(root, ".opencode", "opencode.json")),
		gatewayURL, agentcfg.OpenCodeAPIKeyRef, defaultModel, models, keepTurns, outputBufferTokens)
	if err != nil {
		return err
	}
	return writeHostFile(projectConfigPath(root, ".opencode", "opencode.json"), openCodeConfig)
}

// refreshAgentModels re-writes the opencode served-model LIST against the LIVE
// gateway for an already-running workspace, so a model added/removed since the last
// start (via `ai models`/`ai keys`) becomes visible in the session about to open. It is
// LIST-ONLY: it passes an empty default (no model is pinned — each CLI's persisted
// last-used selection wins) and does NOT rotate the scoped key, rebuild the env files, or
// re-seed — that would invalidate a running agent's key. Best-effort: any error (gateway
// down, config load, VM write) is warned and swallowed so it never fails the attach.
func (manager Manager) refreshAgentModels(project string) {
	root, err := resolveProjectRoot(project)
	if err != nil {
		return
	}
	projectConfig, err := config.Load(root)
	if err != nil {
		return
	}
	_, _, gatewayURL := resolveGateway()
	name := Name(project)

	// Self-heal an orphaned scoped key. The gateway key is minted once at start and
	// baked into the VM; if the gateway's token table was cleared since (e.g. a
	// LiteLLM Postgres re-init), the VM holds a DEAD key and every agent request
	// 401s. Detect that here on the attach path and re-register — which re-mints the
	// key and rewrites the env + agent configs — healing WITHOUT a full workspace
	// restart. A healthy key skips this and falls through to the list-only refresh
	// (attach must never gratuitously rotate a valid key).
	if manager.agentKeyIsOrphaned(manager.currentAgentKey(name)) {
		if regErr := manager.registerAgentProviders(name, project, root, projectConfig, gatewayURL); regErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: workspace %q gateway key looked invalid but could not be re-minted (agent calls may 401 until `ai restart`): %v\n", name, regErr)
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "note: re-minted the gateway key for workspace %q (the previous key was no longer valid at the gateway)\n", name)
			return // registerAgentProviders already rewrote the model-list configs
		}
	}

	// The refreshable served-model list lives only in opencode's config; skip when
	// opencode isn't installed (nothing else carries a refreshable list). An empty Tools
	// list defaults to opencode, matching registerAgentProviders.
	if len(projectConfig.Agent.Tools) != 0 && !slices.Contains(projectConfig.Agent.Tools, "opencode") {
		return
	}
	keepTurns, outputBufferTokens := contextopt.HeadroomParams(projectConfig.Context.Strategy)
	models := manager.pickerModels()
	if err := manager.writeModelListConfigs(name, root, gatewayURL, "", models, keepTurns, outputBufferTokens); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not refresh agent model lists in workspace %q (continuing): %v\n", name, err)
	}
}

// agentKeyProbeTimeout bounds the in-VM read of the baked gateway key on attach so a
// wedged microVM cannot stall the self-heal probe.
const agentKeyProbeTimeout = 15 * time.Second

// currentAgentKey reads the scoped gateway key (AIP_GATEWAY_KEY) baked into the
// workspace's in-VM agent-env file. Returns "" when unreadable/unset. The key lives
// ONLY inside the VM (HARD invariant: never on host disk); this reads it into host
// MEMORY only, for the validity probe.
func (manager Manager) currentAgentKey(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), agentKeyProbeTimeout)
	defer cancel()
	script := ". " + shellQuoteGuest(agentEnvGuestPath) + " 2>/dev/null; printf %s \"$AIP_GATEWAY_KEY\""
	result, err := manager.Sandbox.ExecContext(ctx, name, []string{"bash", "-lc", script})
	if err != nil || result.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// agentKeyIsOrphaned reports whether the workspace's baked gateway key is no longer
// valid at the gateway and can be re-minted. It returns false when the key is still
// valid or when there is nothing to check (no minter, no key read from the VM), so a
// transient VM/gateway hiccup never churns a healthy workspace. A gateway that is
// merely unreachable also fails KeyInfo → the caller attempts a re-mint that likewise
// fails and is reported, leaving the VM untouched.
func (manager Manager) agentKeyIsOrphaned(key string) bool {
	if manager.Keys == nil || key == "" {
		return false
	}
	_, err := manager.Keys.KeyInfo(key)
	return err != nil
}

// graphifyPlatformFlag maps an agent CLI to its `graphify install --platform` value.
// claude-code is Graphify's DEFAULT platform, so it takes no --platform flag.
// A CLI absent from this map is not a Graphify platform and is skipped.
var graphifyPlatformFlag = map[string]string{
	"claude-code": "",
	"codex":       "codex",
	"gemini":      "gemini",
	"opencode":    "opencode",
	"copilot":     "copilot",
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
	// Graphify is an opt-in AI tool (context.graphify_enabled, default on when unset). When
	// disabled its binary is NOT baked into the image, so its install/hook steps are skipped —
	// but the git-init step still runs so every workspace stays git-backed regardless.
	graphifyOn := projectConfig.Context.GraphifyEnabledOrDefault()
	installs := make([]string, 0, len(projectConfig.Agent.Tools))
	if graphifyOn {
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
	}
	// The steps below run in ~/project in a single exec, best-effort:
	//   1. `git init` — ALWAYS (independent of Graphify): if the project is NOT already a
	//      valid git working tree, initialize one so every workspace is git-backed. Detection
	//      uses `git rev-parse --is-inside-work-tree`, NOT `[ -d .git ]`: a valid repo can be
	//      a `.git` DIRECTORY *or* a `.git` FILE (a gitlink — worktree/submodule), and a bare
	//      `-d .git` test misses the file form. When rev-parse rejects the tree AND a `.git`
	//      FILE is present, it is a DANGLING gitlink (points at an absent gitdir — e.g. a
	//      submodule checkout without its superproject); remove just that file (it references
	//      no reachable git data, so nothing is lost) so `git init` produces a real standalone
	//      repo instead of following the dead pointer. A `.git` DIRECTORY is never removed (a
	//      corrupt real repo is the user's to fix). This writes `.git` into the bind-mounted
	//      project dir, so the host project becomes a git repo too — intended.
	//   2. `graphify install --project` per CLI (Graphify ON only) — OVERWRITES its skill
	//      files each run, so a marker (.graphify-installed) guards re-runs, keeping user edits
	//      from being clobbered on every restart; the marker is touched only after all installs
	//      succeed (a failure retries next start). Skipped when no CLI needs it.
	//   3. `graphify hook install` (Graphify ON only) — installs Graphify's git hook. It runs
	//      on EVERY start when the tree is a valid repo (step 1 makes it one). `hook install`
	//      is idempotent (it rewrites the managed hook), so there is deliberately NO marker.
	// The install marker lives under the persistent .ai-platform dir.
	installMarker := workspaceWorkdir + "/.ai-platform/.graphify-installed"
	const gitPresent = "command -v git >/dev/null 2>&1"
	const isRepo = "git rev-parse --is-inside-work-tree >/dev/null 2>&1"
	const graphifyPresent = "command -v graphify >/dev/null 2>&1"
	clauses := []string{
		"cd " + workspaceWorkdir + " 2>/dev/null || exit 0",
		"mkdir -p " + workspaceWorkdir + "/.ai-platform",
		"if " + gitPresent + " && ! " + isRepo + "; then [ -f .git ] && rm -f .git; git init >/dev/null 2>&1; fi",
	}
	if graphifyOn {
		if len(installs) > 0 {
			clauses = append(clauses,
				"if "+graphifyPresent+" && [ ! -f "+installMarker+" ]; then "+strings.Join(installs, " && ")+" && touch "+installMarker+"; fi")
		}
		clauses = append(clauses, "if "+graphifyPresent+" && "+gitPresent+" && "+isRepo+"; then graphify hook install; fi")
	}
	script := strings.Join(clauses, "; ")
	ctx, cancel := context.WithTimeout(context.Background(), graphifyInstallTimeout)
	defer cancel()
	_, _ = manager.Sandbox.ExecContext(ctx, name, []string{"sh", "-lc", script})

	// When Graphify is enabled, also set up its stdio MCP server (registered into the
	// managed configs for codex/omp/hermes): install graphifyy[mcp] INTO the
	// project venv (.venv-msb — the uv-tool install is isolated and not importable via
	// `python -m`) and build the graph offline so the server has data on first use. This
	// is a multi-minute network op, so — like the Caveman install — it is DETACHED.
	if graphifyOn {
		manager.setupGraphifyMCP(name)
	}
}

// setupGraphifyMCP installs graphifyy[mcp] into the project venv and builds the graph,
// DETACHED and best-effort. The MCP server (agentcfg.GraphifyMCPArgs) runs
// `<venv>/bin/python -m graphify.serve graphify-out/graph.json`, so the package must be
// importable from that venv. `graphify update .` is AST-only (no API cost); the git hook
// keeps the graph fresh after. Failures never fail the start (the server just yields no
// tools until a graph exists).
func (manager Manager) setupGraphifyMCP(name string) {
	pool := workspaceWorkdir + "/.ai-platform"
	logPath := pool + "/run/graphify-mcp-setup.log"
	venvPython := venvPath + "/bin/python"
	// This script is launched via `setsid sh <file>` (a NON-login shell), so the
	// login-shell PATH additions are absent — `uv` and `graphify` live in the
	// workspace user's ~/.local/bin and would otherwise be "not found" (leaving
	// graphify-out/graph.json unbuilt, which keeps opencode's graphify plugin dark).
	// Put ~/.local/bin (and the venv bin) on PATH explicitly.
	script := "export PATH=\"$HOME/.local/bin:" + venvPath + "/bin:$PATH\"\n" +
		"cd " + workspaceWorkdir + " 2>/dev/null || exit 0\n" +
		"[ -x " + venvPython + " ] || exit 0\n" +
		"uv pip install --python " + venvPython + " --quiet \"graphifyy[mcp]\"\n" +
		"command -v graphify >/dev/null 2>&1 && graphify update . || true\n"
	if err := manager.Sandbox.WriteFile(name, graphifyMCPScriptGuest, []byte(script)); err != nil {
		return
	}
	launch := fmt.Sprintf("mkdir -p %s && setsid sh %s </dev/null >%s 2>&1 & exit 0",
		shellQuoteGuest(pool+"/run"), shellQuoteGuest(graphifyMCPScriptGuest), shellQuoteGuest(logPath))
	_ = manager.launchDetachedRetry(name, []string{"bash", "-lc", launch}, false, cavemanLaunchTimeout)
}

// cavemanOnlyAgent maps a selected agent CLI to Caveman's `install.sh --only <agent>`
// token. Caveman installs its native skills/agents/commands for these CLIs PLUS the
// CLI-native extras the shared pool cannot carry: the opencode plugin, claude hooks +
// statusline, and the gemini extension. copilot is a "soft probe" — Caveman won't
// auto-detect it, so it additionally needs `--with-init` (handled in registerCaveman).
// omp is absent — Caveman has no omp token, so it receives the skill via the shared pool.
var cavemanOnlyAgent = map[string]string{
	"claude-code": "claude",
	"gemini":      "gemini",
	"opencode":    "opencode",
	"codex":       "codex",
	"copilot":     "copilot",
	"hermes":      "hermes",
}

// registerCaveman installs the Caveman output-compression toolkit
// (https://github.com/JuliusBrussee/caveman) into the workspace at start, best-effort.
//
// It runs Caveman's OWN installer so each detected CLI gets caveman's CLI-NATIVE
// integration — skills, agents, commands, the opencode plugin, claude hooks + statusline,
// the gemini extension — which the platform's shared-pool symlinks cannot carry. It then
// mirrors the caveman skill/agent/command dirs opencode received (its GLOBAL
// ~/.config/opencode output — the richest plain-markdown copy) INTO the shared
// <project>/.ai-platform/{skills,agents,prompts} pool, where linkSharedResources' symlinks
// distribute them to omp (which reads skills ONLY from that pool and is NOT
// caveman-detectable). The same-content skills also re-appear for opencode/claude via the
// pool symlinks — a harmless duplicate of identical files.
//
// It is NETWORK-dependent (fetches the installer + the caveman package), so it only
// succeeds under an egress policy that allows outbound (the `public` default); under
// `deny` it is a no-op. ONCE-guarded by a marker under the persistent .ai-platform dir so
// a restart neither re-runs it nor clobbers user edits. Best-effort — never fails start.
func (manager Manager) registerCaveman(name string, projectConfig *config.Config) {
	if projectConfig == nil {
		return
	}
	// Honor the create-time choice (config.yaml context.caveman_enabled). Unset
	// defaults to enabled for pre-toggle projects; an explicit false skips it.
	if !projectConfig.Context.CavemanEnabledOrDefault() {
		return
	}
	// Explicitly-enabled (chosen at create) vs the nil back-compat default. We surface
	// warnings only for an EXPLICIT opt-in so pre-toggle omp-only projects stay quiet.
	explicit := projectConfig.Context.CavemanEnabled != nil && *projectConfig.Context.CavemanEnabled
	only := make([]string, 0, len(projectConfig.Agent.Tools))
	for _, cli := range projectConfig.Agent.Tools {
		// Skip the gemini adapter: Caveman's gemini step shells out to
		// `gemini extensions install`, and the Gemini CLI prompts for workspace-trust
		// ("trust this workspace? [Y/n]") that it reads from the controlling TTY and
		// which IGNORES Caveman's own --non-interactive. Under the detached, TTY-less
		// install it BLOCKS FOREVER; the accumulated hung installs then exhaust the msb
		// agent-relay (the "msb exec is not responding" wedge seen at the Caveman step).
		// gemini still receives Caveman's commands via the shared pool (prompts →
		// .gemini/commands), exactly like omp — so we lose only the native gemini
		// extension, not the skill itself. (Verified live: the hang is in gemini, not
		// Caveman.)
		if cli == "gemini" {
			continue
		}
		if agent, ok := cavemanOnlyAgent[cli]; ok {
			only = append(only, "--only "+agent)
		}
	}
	// Caveman's installer natively integrates opencode/claude-code/codex (gemini is
	// skipped above); omp/gemini receive its skills/agents/commands via the shared
	// pool. With none of the native CLIs selected there is nothing to install or mirror.
	if len(only) == 0 {
		if explicit {
			_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("Caveman is enabled but no compatible CLI "+
				"(opencode, claude-code, codex, or gemini) is selected — skipping install."))
		}
		return
	}
	pool := workspaceWorkdir + "/.ai-platform"
	installMarker := pool + "/.caveman-installed"
	// One guarded exec, run from $HOME — NEVER from ~/project: a bind-mounted
	// project that is a git SUBMODULE on the host has a `.git` FILE whose gitdir
	// points outside the mount, and git's repo discovery from that cwd dies
	// `fatal: not a git repository` (exit 128), killing any npm/npx invocation
	// there before it does anything (verified live).
	//   1. Clone Caveman and run its installer LOCALLY (`node bin/install.js`) —
	//      the curl|bash → npx path is NOT usable: Caveman's opencode NATIVE
	//      install (the skills/agents/commands drop into ~/.config/opencode)
	//      requires a local repo clone and fails under npx by design (its own
	//      installer says so). --with-hooks wires the claude hooks + statusline;
	//      the opencode plugin and gemini extension come from their adapters.
	//   2. Mirror opencode's GLOBAL caveman dirs into the shared pool
	//      (skills→skills, agents→agents, commands→prompts) so pi/omp pick them
	//      up via the symlinks.
	//   3. Require the caveman skill to actually be IN the pool before recording
	//      success — a "successful" install that dropped nothing must retry, not
	//      silently mark itself done.
	clone := "rm -rf /tmp/caveman-src && " +
		"git clone --depth 1 https://github.com/JuliusBrussee/caveman /tmp/caveman-src"
	// Defense-in-depth: bound the node installer with `timeout` (coreutils, present in
	// every base) so any future adapter that blocks on a TTY/network prompt self-kills
	// instead of hanging forever and exhausting the msb agent-relay (see the gemini
	// skip above). On timeout the installer exits non-zero, the marker is not touched,
	// and the next start retries. `--kill-after` SIGKILLs a child that ignores SIGTERM.
	// copilot is a Caveman "soft probe": it is not auto-detected even with `--only copilot`,
	// so it additionally needs `--with-init` to write its integration.
	withInit := ""
	if slices.Contains(projectConfig.Agent.Tools, "copilot") {
		withInit = " --with-init"
	}
	installCmd := "cd /tmp/caveman-src && timeout --kill-after=30s 600s node bin/install.js --non-interactive --with-hooks" +
		withInit + " " + strings.Join(only, " ")
	mirror := `og="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"; ` +
		`for pair in "skills:skills" "agents:agents" "commands:prompts"; do ` +
		`src="$og/${pair%%:*}"; dst="` + pool + `/${pair##*:}"; ` +
		`[ -d "$src" ] || continue; mkdir -p "$dst"; cp -a "$src/." "$dst/" 2>/dev/null || true; done`
	// The marker is touched ONLY after install + mirror succeeded AND the caveman
	// skill is verifiably in the pool (chained with &&), so a failed or empty
	// attempt (no network, adapter failure) retries on the next start rather than
	// being wrongly recorded as done. The clone is always cleaned up.
	// `mirror` contains an internal `;` (an assignment then a for-loop), so it MUST be
	// wrapped in a brace group here — otherwise that `;` would split the statement and
	// the `[ "$installed" -eq 0 ] &&` gate would guard only the assignment, letting the
	// loop + marker-touch run even when the install failed.
	script := "#!/usr/bin/env bash\n" +
		"cd \"$HOME\"; " +
		"mkdir -p " + pool + "; " +
		"[ -f " + installMarker + " ] && exit 0; " +
		"{ " + clone + " && " + installCmd + "; }; installed=$?; rm -rf /tmp/caveman-src; " +
		"[ \"$installed\" -eq 0 ] && " +
		"{ " + mirror + "; } && " +
		"[ -f " + pool + "/skills/caveman/SKILL.md ] && " +
		"touch " + installMarker + "\n"

	// The install is a multi-MINUTE network + node operation (git clone + the
	// caveman installer). Running it as a BLOCKING exec at workspace start stalled
	// the start AND saturated the single msb relay, so other execs — and the install
	// exec itself — timed out with "msb exec is not responding". So DETACH it, exactly
	// like the containerd boot: stage the script to a file, then `setsid` it into a new
	// session (fully detached from stdin/out and from the exec's process group, so it
	// survives msb tearing that group down when the launcher exec returns). The launcher
	// exec returns in milliseconds. The script itself is once-guarded (the marker is
	// touched ONLY on success), so a failed/killed background run simply retries on the
	// next start — no synchronous exit code to surface, which also removes the confusing
	// "did not complete" warning that fired whenever the relay was merely busy.
	if err := manager.Sandbox.WriteFile(name, cavemanScriptGuest, []byte(script)); err != nil {
		if explicit {
			_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("Caveman install could not be staged "+
				"(it will retry on the next workspace start): "+err.Error()))
		}
		return
	}
	// Write the log to the project run/ dir (bind-mounted, so it is readable on the
	// HOST at <project>/.ai-platform/run/caveman-install.log AND in-VM) — the detached
	// install's output does NOT appear in the create/start log stream (in-VM exec
	// output never does; only the image build is teed), so this file is where to look.
	logPath := pool + "/run/caveman-install.log"
	logStep("installing Caveman in the background (detached) → %s", logPath)
	launch := fmt.Sprintf("mkdir -p %s && setsid bash %s </dev/null >%s 2>&1 & exit 0",
		shellQuoteGuest(pool+"/run"), shellQuoteGuest(cavemanScriptGuest), shellQuoteGuest(logPath))
	if err := manager.launchDetachedRetry(name, []string{"bash", "-lc", launch}, false, cavemanLaunchTimeout); err != nil && explicit {
		_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("Caveman install could not be launched "+
			"(it will retry on the next workspace start): "+err.Error()))
	}
}

// codeReviewGraphPlatformFlag maps a selected agent CLI to code-review-graph's
// `install --platform <token>` value, used for the CLIs whose config the platform does
// NOT rewrite (so code-review-graph's native install is not clobbered). codex is
// deliberately ABSENT: its config.toml is platform-managed (rewritten each start), so a
// native install there would be wiped — codex instead gets code-review-graph's MCP server
// injected into config.toml by registerAgentProviders (AppendCodexMCP). omp/hermes
// are not code-review-graph platforms either and likewise get the MCP server via injection
// into their managed configs.
var codeReviewGraphPlatformFlag = map[string]string{
	"claude-code": "claude-code",
	"gemini":      "gemini-cli",
	"opencode":    "opencode",
	"copilot":     "copilot-cli",
}

// registerCodeReviewGraph installs code-review-graph (https://code-review-graph.com,
// baked into the image via `uv tool install code-review-graph`) and registers it as an
// MCP server with each SELECTED, supported agent CLI, at workspace start. Like Graphify,
// it MUST run here rather than at image-build time: `code-review-graph install --platform`
// writes project-scoped MCP config (e.g. `.opencode.json`, `.mcp.json`, `.gemini/settings.json`)
// into the project directory, only bind-mounted at runtime; and `build`/`visualize` parse
// the mounted codebase.
//
// It is DETACHED (setsid), exactly like registerCaveman: `code-review-graph build` parses
// the whole codebase, which can take minutes on a large repo — a blocking exec would stall
// the start AND saturate the single msb agent-relay. The launcher exec returns in
// milliseconds; the staged script is once-guarded (marker under the persistent .ai-platform
// dir, touched ONLY on success) so a failed/killed background run simply retries on the next
// start. No API key is used: code-review-graph defaults to LOCAL embeddings, so it never
// touches the gateway or a provider key. Best-effort — never fails the start.
func (manager Manager) registerCodeReviewGraph(name string, projectConfig *config.Config) {
	if projectConfig == nil {
		return
	}
	// Honor the create-time choice (config.yaml context.code_review_graph_enabled). It is
	// OPT-IN: unset (nil) defaults to disabled, so it is a no-op unless explicitly chosen.
	if !projectConfig.Context.CodeReviewGraphEnabledOrDefault() {
		return
	}
	installs := make([]string, 0, len(projectConfig.Agent.Tools))
	for _, cli := range projectConfig.Agent.Tools {
		platform, known := codeReviewGraphPlatformFlag[cli]
		if !known {
			continue
		}
		installs = append(installs, "code-review-graph install --platform "+platform)
	}
	if len(installs) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("code-review-graph is enabled but no supported CLI "+
			"(opencode, claude-code, codex, gemini, or copilot) is selected — skipping install."))
		return
	}
	pool := workspaceWorkdir + "/.ai-platform"
	installMarker := pool + "/.code-review-graph-installed"
	// The per-CLI MCP registration, then a full-codebase `build`, then `visualize` (which
	// writes a static D3 force-directed graph HTML at .code-review-graph/graph.html — that
	// IS the visualization "UI"; there is no server to launch). Run from ~/project so the
	// project-scoped MCP configs + the graph land in the bind-mounted project dir.
	steps := append(installs, "code-review-graph build", "code-review-graph visualize")
	// Bound the whole run with `timeout` (coreutils, present in every base) as
	// defense-in-depth so a wedged build self-kills instead of hanging the background
	// process; on timeout the marker is not touched and the next start retries.
	// `--kill-after` SIGKILLs a child that ignores SIGTERM. The joined command contains no
	// single quote, so single-quoting it for `bash -c` is safe.
	inner := strings.Join(steps, " && ")
	script := "#!/usr/bin/env bash\n" +
		"cd " + workspaceWorkdir + " 2>/dev/null || exit 0; " +
		"mkdir -p " + pool + "; " +
		"[ -f " + installMarker + " ] && exit 0; " +
		"command -v code-review-graph >/dev/null 2>&1 || exit 0; " +
		"timeout --kill-after=30s 600s bash -c '" + inner + "'; installed=$?; " +
		"[ \"$installed\" -eq 0 ] && touch " + installMarker + "\n"
	if err := manager.Sandbox.WriteFile(name, codeReviewGraphScriptGuest, []byte(script)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("code-review-graph install could not be staged "+
			"(it will retry on the next workspace start): "+err.Error()))
		return
	}
	// The detached install's output is written to the project run/ dir (bind-mounted, so it
	// is readable on the HOST at <project>/.ai-platform/run/code-review-graph-install.log).
	logPath := pool + "/run/code-review-graph-install.log"
	logStep("installing code-review-graph in the background (detached) → %s", logPath)
	launch := fmt.Sprintf("mkdir -p %s && setsid bash %s </dev/null >%s 2>&1 & exit 0",
		shellQuoteGuest(pool+"/run"), shellQuoteGuest(codeReviewGraphScriptGuest), shellQuoteGuest(logPath))
	if err := manager.launchDetachedRetry(name, []string{"bash", "-lc", launch}, false, codeReviewGraphLaunchTimeout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("code-review-graph install could not be launched "+
			"(it will retry on the next workspace start): "+err.Error()))
	}
}

// registerCodebaseMemory registers codebase-memory-mcp
// (https://github.com/DeusData/codebase-memory-mcp, baked into the image via its install.sh
// with the `--ui` variant + `--skip-config`) as an MCP server with each installed agent CLI,
// at workspace start. `codebase-memory-mcp install` AUTO-DETECTS the installed agent CLIs and
// writes their MCP config (project-scoped where the CLI uses a project config — `.mcp.json`,
// `.gemini/settings.json`, `.opencode.json` — global otherwise), so it MUST run here, after
// the CLIs are set up and the project is bind-mounted at ~/project. Unlike Graphify /
// code-review-graph there is no per-platform flag — the single `install` configures whatever
// it detects.
//
// It is config-only (the binary is baked; `install` writes config, no network and no build),
// so — like `graphify install` — it runs as a bounded BLOCKING exec, with an in-script
// `timeout` guard as defense-in-depth so any hang self-kills instead of wedging the msb
// agent-relay. No API key is used (codebase-memory-mcp needs none), so it never touches the
// gateway or a provider key. The optional 3D graph UI ships in the baked `--ui` variant and is
// launched ON DEMAND with `codebase-memory-mcp --ui=true --port=9749` (not auto-started —
// nothing publishes :9749). ONCE-guarded by a marker under the persistent .ai-platform dir.
// Best-effort — never fails the start.
func (manager Manager) registerCodebaseMemory(name string, projectConfig *config.Config) {
	if projectConfig == nil {
		return
	}
	// Honor the create-time choice (config.yaml context.codebase_memory_enabled). It is
	// OPT-IN: unset (nil) defaults to disabled, so it is a no-op unless explicitly chosen.
	if !projectConfig.Context.CodebaseMemoryEnabledOrDefault() {
		return
	}
	pool := workspaceWorkdir + "/.ai-platform"
	installMarker := pool + "/.codebase-memory-installed"
	// `codebase-memory-mcp install` OVERWRITES the agent MCP entries each run, so a marker
	// guards re-runs (keeping user edits from being clobbered on every restart); it is
	// touched only after install succeeds (a failure retries next start). Run from ~/project
	// so the project-scoped MCP configs land in the bind-mounted project dir.
	script := "cd " + workspaceWorkdir + " 2>/dev/null || exit 0; " +
		"mkdir -p " + pool + "; " +
		"[ -f " + installMarker + " ] && exit 0; " +
		"command -v codebase-memory-mcp >/dev/null 2>&1 || exit 0; " +
		"timeout --kill-after=15s 45s codebase-memory-mcp install && touch " + installMarker
	if err := manager.launchDetachedRetry(name, []string{"sh", "-lc", script}, false, codebaseMemoryInstallTimeout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, ui.Warn.Render("codebase-memory-mcp registration could not "+
			"complete (it will retry on the next workspace start): "+err.Error()))
	}
}

// pinHostGatewayIPv4 rewrites the guest /etc/hosts so gatewayHost resolves to its
// IPv4 address ONLY. msb seeds /etc/hosts with both an A and an AAAA record for the
// host-gateway name; glibc getaddrinfo (and therefore Node's dns.lookup, curl, Go,
// Rust) prefers the IPv6 record, but msb forwards guest→host traffic only over IPv4
// (the host nginx publish is IPv4-only), so an IPv6 connect reaches the msb gateway
// then RESETs — the agent CLIs report it as "socket connection was closed
// unexpectedly". Stripping the AAAA record and pinning the resolved IPv4 makes every
// in-VM client reach the gateway regardless of resolver order. Runs as ROOT (the
// workspace user cannot edit /etc/hosts) and is BEST-EFFORT: any failure is logged to
// stderr and the start proceeds — a stale-but-present hosts file is no worse than the
// state before this step. Called only for the LOCAL msb gateway (see the caller's
// runtime.DefaultGatewayHost guard); a remote/client-mode gateway is a real routable
// address and is left untouched.
func (manager Manager) pinHostGatewayIPv4(name, gatewayHost string) {
	// getent ahostsv4 returns only A records; take the first. Escape dots in the sed
	// address so the delete pattern matches the literal name, not any char.
	quotedHost := shellQuoteGuest(gatewayHost)
	sedPattern := strings.ReplaceAll(gatewayHost, ".", `\.`)
	cmd := fmt.Sprintf(
		"V4=$(getent ahostsv4 %s | awk '{print $1; exit}'); "+
			"[ -n \"$V4\" ] || exit 3; "+
			"sed -i '/%s/d' /etc/hosts; "+
			"printf '%%s\\t%s\\n' \"$V4\" >> /etc/hosts",
		quotedHost, sedPattern, gatewayHost)
	result, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c", cmd})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not pin the host-gateway IPv4 in workspace %q: %v\n", name, err)
		return
	}
	if result.ExitCode != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not resolve the host-gateway IPv4 in workspace %q (agents may fail to reach the gateway over IPv6)\n", name)
	}
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
	// Do NOT use containerd's native overlayfs snapshotter: the VM root (`/`) is ITSELF
	// an overlay (the msb OCI upper), and the kernel refuses to stack a second overlay on
	// it — a container's rootfs mount fails with "invalid argument", so images pull but
	// containers never START ("installed (stopped)"). Prefer the **fuse-overlayfs**
	// snapshotter (userspace overlay via /dev/fuse — works on top of the overlay root and
	// keeps layer sharing); it needs the bundled `containerd-fuse-overlayfs-grpc` proxy
	// (registered as a containerd proxy_plugin) AND the `fuse3` package's `mount.fuse3`
	// helper (baked into the base image). Start the grpc proxy and WAIT for its socket; if
	// it comes up, default nerdctl to fuse-overlayfs, else FALL BACK to the **native**
	// snapshotter (a full-copy snapshotter that needs no mount and always works on an
	// overlay root — costs disk, but reliable). containerd loads both, so nerdctl's
	// default (nerdctl.toml) selects which is used by every pull/run.
	//
	// CRITICAL (containerd 2.x): the config MUST be `version = 3` AND declare a transfer
	// `unpack_config` entry for the chosen (platform, snapshotter) pair. containerd 2.x
	// pulls through the transfer service, whose unpacker refuses to extract into a
	// snapshotter it has no unpack_config for — the image pulls but extraction dies with
	// `unable to initialize unpacker: no unpack platforms defined: invalid argument`, so
	// the app "installs" but never runs. The default overlayfs snapshotter is implicitly
	// unpackable, but our fuse-overlayfs/native snapshotters are NOT, so we list the one
	// we actually use. The platform is derived from `uname -m` (linux/arm64 on Apple
	// Silicon, linux/amd64 on x86). Verified live: with this entry, image pulls extract.
	//
	// The fuse-overlayfs grpc socket is `rm -f`'d before (re)starting the daemon: a stale
	// socket FILE left by a prior boot whose daemon is gone would satisfy the `-S` wait
	// yet refuse connections at unpack time (`connection refused`), so we always recreate
	// it fresh.
	bootCmd := fmt.Sprintf(
		"mkdir -p /etc/containerd /etc/nerdctl /var/lib/containerd-fuse-overlayfs; "+
			"rm -f /run/containerd-fuse-overlayfs.sock; "+
			"setsid sh -c 'containerd-fuse-overlayfs-grpc /run/containerd-fuse-overlayfs.sock /var/lib/containerd-fuse-overlayfs >%s 2>&1 &'; "+
			"s=0; while [ $s -lt 20 ] && [ ! -S /run/containerd-fuse-overlayfs.sock ]; do s=$((s+1)); sleep 0.5; done; "+
			"if [ -S /run/containerd-fuse-overlayfs.sock ]; then snap=fuse-overlayfs; else snap=native; fi; "+
			"arch=$(uname -m); case \"$arch\" in aarch64|arm64) plat=linux/arm64;; x86_64|amd64) plat=linux/amd64;; *) plat=linux/$arch;; esac; "+
			"printf 'version = 3\\n[proxy_plugins]\\n  [proxy_plugins.\"fuse-overlayfs\"]\\n    type = \"snapshot\"\\n    address = \"/run/containerd-fuse-overlayfs.sock\"\\n[plugins.\"io.containerd.transfer.v1.local\"]\\n  [[plugins.\"io.containerd.transfer.v1.local\".unpack_config]]\\n    platform = \"%%s\"\\n    snapshotter = \"%%s\"\\n' \"$plat\" \"$snap\" > /etc/containerd/config.toml; "+
			"printf 'snapshotter = \"%%s\"\\n' \"$snap\" > /etc/nerdctl/nerdctl.toml; "+
			"setsid sh -c 'containerd >%s 2>&1 &'; "+
			"iters=0; while [ $iters -lt 150 ]; do timeout 5 nerdctl info >/dev/null 2>&1 && exit 0; "+
			"iters=$((iters+1)); sleep 0.2; done; exit 1",
		shellQuoteGuest(fuseOverlayfsLog), shellQuoteGuest(containerdLog))
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

// autostartApps starts every INSTALLED in-VM app container AND every installed agent-CLI
// dashboard at workspace start so they are reachable from the host browser by default
// (their host ports were already published by the microVM create). It is DETACHED +
// best-effort: it never fails or blocks the workspace start.
//
//   - App CONTAINERS run as ROOT (nerdctl needs root). Because a first-start image pull is
//     a long in-VM exec that would block the start AND every other exec (the single msb
//     relay), the container launch is staged to a guest-only script and setsid'd into the
//     background — exactly like registerCaveman — so the launcher exec returns in ms. The
//     script is idempotent (`nerdctl rm -f` then `run`), so it runs every start (only the
//     FIRST pulls; later starts reuse the cached image). Output goes to the bind-mounted
//     run/apps-autostart.log so it is readable on the host.
//   - DASHBOARDS (e.g. hermes) run as the WORKSPACE USER and are a process, not a container.
//     Each is pgrep-guarded so a second start does not double-launch, then setsid'd with the
//     server bound to 0.0.0.0 so the published host port reaches it.
func (manager Manager) autostartApps(name string, projectConfig *config.Config, gatewayURL string) {
	if projectConfig == nil {
		return
	}
	logPath := workspaceWorkdir + "/.ai-platform/run/apps-autostart.log"
	// App containers (root). The gateway URL is baked (non-secret); the scoped key stays a
	// shell reference resolved in-VM, so no key value is staged.
	if script := apps.AppsAutostartScript(projectConfig, gatewayURL); script != "" {
		if err := manager.Sandbox.WriteFile(name, appsAutostartScriptGuest, []byte(script)); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: could not stage in-VM app autostart in workspace %q: %v\n", name, err)
		} else {
			logStep("auto-starting installed in-VM apps in the background (detached) → %s", logPath)
			launch := fmt.Sprintf("mkdir -p %s && setsid bash %s </dev/null >%s 2>&1 & exit 0",
				shellQuoteGuest(workspaceWorkdir+"/.ai-platform/run"), shellQuoteGuest(appsAutostartScriptGuest), shellQuoteGuest(logPath))
			if err := manager.launchDetachedRetry(name, []string{"bash", "-lc", launch}, true, appsAutostartLaunchTimeout); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: could not launch in-VM app autostart in workspace %q: %v\n", name, err)
			}
		}
	}
	// Agent-CLI dashboards (workspace user).
	if command := dashboardAutostartCommand(projectConfig, logPath); command != "" {
		logStep("auto-starting installed agent dashboards in the background (detached)")
		if err := manager.launchDetachedRetry(name, []string{"bash", "-lc", command}, false, appsAutostartLaunchTimeout); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: could not launch in-VM agent dashboards in workspace %q: %v\n", name, err)
		}
	}
}

// dashboardAutostartCommand builds the (workspace-user) shell command that pgrep-guards
// and setsid-launches every installed agent-CLI dashboard, or "" when none are installed.
// It sources the agent env first (dashboards like hermes read AIP_GATEWAY_KEY from it) and
// binds each server to 0.0.0.0:<port> so the published host port reaches it.
func dashboardAutostartCommand(projectConfig *config.Config, logPath string) string {
	var launches []string
	for _, entry := range projectConfig.AgentDashboards {
		if !apps.IsDashboardAgent(entry.Key) {
			continue
		}
		launches = append(launches, fmt.Sprintf(
			"pgrep -f %s >/dev/null 2>&1 || setsid %s dashboard --host 0.0.0.0 --port %d </dev/null >>%s 2>&1 &",
			shellQuoteGuest(entry.Key+" dashboard"), entry.Key, entry.Port, shellQuoteGuest(logPath)))
	}
	if len(launches) == 0 {
		return ""
	}
	// Ensure the uv-tool bin dir is on PATH: the dashboard binary (e.g. hermes) is a
	// wrapper in ~/.local/bin, and this command may run under a shell that hasn't
	// picked it up. Explicit + idempotent so the launch never fails "command not found".
	header := `case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac; ` +
		"set -a; [ -f " + shellQuoteGuest(agentEnvGuestPath) + " ] && . " + shellQuoteGuest(agentEnvGuestPath) + "; set +a; " +
		"mkdir -p " + shellQuoteGuest(workspaceWorkdir+"/.ai-platform/run") + "; "
	return header + strings.Join(launches, " ") + " exit 0"
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
		"omp":         ".omp/skills",
		// hermes is a gateway agent like omp; it reads skills from its config's
		// external_dirs (HermesConfig points one at .hermes/skills). hardware bring-up:
		// confirm the dir resolves in-VM.
		"hermes": ".hermes/skills",
	}},
	{pool: "agents", perCLI: map[string]string{
		"opencode":    ".opencode/agents",
		"claude-code": ".claude/agents",
		// omp reads its OWN native .omp/agents dir (it deliberately SKIPS .claude/agents —
		// schema differs), so it needs its own symlink to receive the shared pool
		// (including Caveman's agents).
		"omp": ".omp/agents",
	}},
	{pool: "prompts", perCLI: map[string]string{
		"opencode":    ".opencode/commands",
		"claude-code": ".claude/commands",
		"gemini":      ".gemini/commands",
		"omp":         ".omp/commands",
		// hermes derives commands from skills (no separate dir) so it gets NO prompts link
		// — a CLI a kind lacks is skipped, like codex/gemini.
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
func (manager Manager) pickerModels() []agentcfg.Model {
	if manager.Served == nil {
		return []agentcfg.Model{}
	}
	served, err := manager.Served.ServedModels()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not list the gateway's served models for the agent picker (continuing with an empty list): %v\n", err)
		return []agentcfg.Model{}
	}
	seen := make(map[string]struct{}, len(served))
	models := make([]agentcfg.Model, 0, len(served))
	for _, model := range served {
		if model.Name == "" {
			continue
		}
		if _, ok := seen[model.Name]; ok {
			continue
		}
		seen[model.Name] = struct{}{}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
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
	// Refresh the served-model LIST for opencode against the LIVE gateway before
	// handing over the session, so a model added/removed since the last start (via
	// `ai models`/`ai keys`) is immediately visible here — the "refresh when a shell is
	// attached" requirement. Also self-heals an orphaned gateway key (see
	// refreshAgentModels). Best-effort; never fails the attach. Runs BEFORE the tmux
	// warm-up so that warm-up stays the LAST preflight exec right before the PTY attach.
	manager.refreshAgentModels(project)
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
// CLI in ~/project. The session is named after the CLI (opencode, omp, …) so each
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
	"omp":         {"omp"},
	"claude-code": {"claude"},
	"codex":       {"codex"},
	"gemini":      {"gemini"},
	"hermes":      {"hermes"},
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
// scoped virtual key. opencode takes its config from a file and ignores the env,
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
