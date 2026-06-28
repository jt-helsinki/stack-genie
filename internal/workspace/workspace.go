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
	"os"
	"sort"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/agentcfg"
	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
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
// every tmux session opens (matches the microVM --workdir in Create).
const workspaceWorkdir = "/workspace"

// ErrUnknownProject is returned when a project name is not in the global index
// (→ exit 2).
var ErrUnknownProject = errors.New("unknown project")

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
// read-only image, the host project source, and the persistent overlay (arch
// §26) as a volume, and applies the project's egress policy via netArgs (the
// `msb create` network-rule fragment from egress.MsbNetworkArgs).
type Sandbox interface {
	Create(name, imageRef, projectMount, overlayPath string, netArgs []string) error
	Start(name string) error
	Stop(name string) error
	Destroy(name string) error
	Exec(name string, argv []string) (ExecResult, error)
	// ExecRoot runs argv inside the running microVM as the image's ROOT user
	// (`msb exec -u root` — msb's no-`-u` default is the unprivileged `workspace`
	// user, NOT root), for privileged operations the unprivileged workspace user
	// cannot perform — notably booting the rootful in-VM containerd. A non-zero
	// inner exit is carried in ExecResult; only an infrastructure failure (microVM
	// down, msb missing) is a Go error.
	ExecRoot(name string, argv []string) (ExecResult, error)
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
	// the full captured log. Backs the TUI "Sandbox Log" tab. ErrMsbMissing when msb
	// is absent; a non-running/unknown sandbox surfaces as a Go error carrying msb's
	// message.
	LogTail(name string, lines int) (string, error)
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
	// Bring up the rootful in-VM container runtime (containerd) so nerdctl works
	// inside the workspace. BEST-EFFORT: a failure here must NOT fail the workspace
	// start — a non-container workspace is still fully usable, and Phase 1's app
	// start surfaces a clear error if the runtime is down. We only log a warning.
	manager.ensureContainerd(name)
	// Start the installed in-VM apps (best-effort PER app: one failing must not
	// fail the workspace or the other apps — see startInstalledApps).
	manager.startInstalledApps(name, project, root, gatewayURL)
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
	// The in-VM picker is the models the gateway currently SERVES (its DB-backed
	// models). There is NO built-in default model in the catalog-driven system, so
	// the empty default is passed through (the agent CLIs fall back to their own
	// default selection).
	const defaultModel = ""
	models := manager.pickerModels()

	openCodeConfig, err := agentcfg.OpenCodeConfig(gatewayURL, apiKey, defaultModel, models, keepTurns, outputBufferTokens)
	if err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, openCodeGuestPath, openCodeConfig); err != nil {
		return err
	}

	piConfig, err := agentcfg.PiConfig(gatewayURL, apiKey, defaultModel, models)
	if err != nil {
		return err
	}
	if err := manager.Sandbox.WriteFile(name, piGuestPath, piConfig); err != nil {
		return err
	}

	// Install the in-VM `refresh-models` command so the user can re-pull the model
	// picker (after adding a provider key with `ai keys` or pulling/removing an
	// Ollama model on the host) WITHOUT restarting the workspace. It bakes the SAME
	// gateway URL, scoped key, default, and Headroom knobs as the configs above; at
	// run time it re-fetches the served models from the gateway's /v1/models endpoint
	// and rewrites the configs, reproducing pickerModels' result. The minted key
	// flows host→VM only.
	if err := manager.installRefreshScript(name, gatewayURL, apiKey, defaultModel, keepTurns, outputBufferTokens); err != nil {
		return err
	}

	// Write the managed tmux.conf so the workspace session model is transparent
	// (mouse scroll, hidden status bar) — the user never types a tmux command.
	return manager.Sandbox.WriteFile(name, tmuxConfGuestPath, agentcfg.TmuxConfig())
}

// containerdLog is the in-VM path containerd's stdout/stderr is redirected to
// when ensureContainerd boots it, so the daemon's output is inspectable.
const containerdLog = "/var/log/containerd.log"

// ensureContainerd makes the rootful in-VM container runtime (containerd)
// available so nerdctl works inside the workspace (arch §7). It probes whether
// containerd is already up (a `nerdctl info` that talks to the daemon) and, if
// not, boots it DETACHED via `setsid` so the daemon outlives the exec that
// started it and runs for the VM's life. Both run as ROOT (ExecRoot) because the
// runtime is rootful and the unprivileged workspace user cannot start it.
//
// It is BEST-EFFORT: any failure (probe error, boot error, non-zero exit) is
// logged as a warning and swallowed — a workspace with no running runtime is
// still fully usable, and Phase 1's app start surfaces a clear error if the
// runtime is down. It never returns an error and never fails the workspace start.
//
// hardware bring-up: the daemon-persistence of `msb exec` + setsid and the real
// containerd boot are verified on a provisioned Apple Silicon host. The host-side
// orchestration (the probe, the boot argv, the best-effort swallowing) is
// unit-tested here against the fake sandbox.
func (manager Manager) ensureContainerd(name string) {
	// Probe: if `nerdctl info` reaches the daemon, containerd is already up.
	if result, err := manager.Sandbox.ExecRoot(name, []string{"nerdctl", "info"}); err == nil && result.ExitCode == 0 {
		return
	}
	// Boot containerd detached so it survives this exec returning. setsid +
	// background keeps the daemon running for the VM's life; output is redirected
	// to a log for later inspection.
	//
	// CRITICAL: after backgrounding, WAIT for the control socket to appear before
	// returning. The boot exec returns the instant the outer sh backgrounds the
	// daemon (the `&`), and msb tears down the exec's process group on return — so
	// without the wait the just-forked containerd is killed before it establishes
	// (verified live: the log file is created but stays 0 bytes). Polling the socket
	// keeps this exec alive until the daemon is fully up (it boots in ~10ms, so this
	// returns in 1-2 iterations normally) and bounds the wait to ~10s so a genuinely
	// broken runtime never hangs the workspace start.
	bootCmd := fmt.Sprintf(
		"setsid sh -c 'containerd >%s 2>&1 &'; "+
			"iters=0; while [ $iters -lt 100 ]; do [ -S /run/containerd/containerd.sock ] && break; "+
			"iters=$((iters+1)); sleep 0.1; done",
		shellQuoteGuest(containerdLog))
	result, err := manager.Sandbox.ExecRoot(name, []string{"sh", "-c", bootCmd})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not start the in-VM container runtime in workspace %q: %v\n", name, err)
		return
	}
	if result.ExitCode != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "warning: in-VM container runtime did not start cleanly in workspace %q (exit %d): %s\n",
			name, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
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
// system — no built-in default model). It is used by `ai apps` and by
// startInstalledApps. exec is nil when the workspace is not running, so the
// lifecycle methods that need the VM report ErrWorkspaceNotRunning.
func (manager Manager) AppManager(name, project, root, gatewayURL string) *apps.Manager {
	return manager.buildAppManager(name, project, root, gatewayURL, manager.requireRunning(project) == nil)
}

// buildAppManager wires an apps.Manager for a workspace. forceExec=true wires the
// in-VM exec surface unconditionally (used at workspace start, when the microVM is
// running but its handle is not yet saved so requireRunning would say "no");
// otherwise Exec is wired only when the workspace is recorded running.
func (manager Manager) buildAppManager(name, project, root, gatewayURL string, forceExec bool) *apps.Manager {
	var exec apps.ExecRunner
	if forceExec {
		exec = func(argv []string) (apps.ExecResult, error) {
			result, err := manager.Sandbox.ExecRoot(name, argv)
			return apps.ExecResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
		}
	}
	return apps.NewManager(apps.Deps{
		LoadConfig:    func() (*config.Config, error) { return config.LoadProjectConfig(root) },
		SaveConfig:    func(updated *config.Config) error { return config.WriteProject(root, updated) },
		ReservedPorts: apps.ReservedPortsAcrossWorkspaces,
		Exec:          exec,
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

// startInstalledApps brings up every installed in-VM app at workspace start. It is
// BEST-EFFORT and never fails the workspace start: if the gateway key cannot be
// minted, or an individual app fails to start, it logs a warning and continues. A
// workspace with a degraded app is still fully usable.
//
// hardware bring-up: the live `nerdctl run` for each app executes only inside a
// booted microVM with containerd up; the orchestration is unit-tested with a fake.
func (manager Manager) startInstalledApps(name, project, root, gatewayURL string) {
	// The microVM is running here even though the lifecycle handle is not saved
	// yet, so wire Exec directly (AppManager's requireRunning gate would see no
	// handle and leave Exec nil). buildAppManager(..., true) forces Exec on.
	appManager := manager.buildAppManager(name, project, root, gatewayURL, true)
	warnings, err := appManager.StartInstalled()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: could not start in-VM apps in workspace %q: %v\n", name, err)
		return
	}
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
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
	installCmd := fmt.Sprintf("sudo install -m 0755 %s %s && rm -f %s",
		shellQuoteGuest(refreshScriptStagePath), shellQuoteGuest(refreshScriptBinPath), shellQuoteGuest(refreshScriptStagePath))
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
// success), then attaches the PTY. tmux is a hard requirement: every base image
// installs it, so a missing tmux means a stale project Dockerfile (recreate /
// rebuild), not a case to silently degrade.
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

// SandboxLogTail returns the last lines of the project's workspace microVM
// captured output (`msb logs`). It backs the TUI "Sandbox Log" tab. A not-yet-
// started workspace reports cleanly via requireRunning rather than surfacing a raw
// msb "sandbox not found" error.
func (manager Manager) SandboxLogTail(project string, lines int) (string, error) {
	if _, err := resolveProjectRoot(project); err != nil {
		return "", err
	}
	if err := manager.requireRunning(project); err != nil {
		return "", err
	}
	return manager.Sandbox.LogTail(Name(project), lines)
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

// isNoTmuxServer reports whether a tmux stderr indicates simply that no tmux server
// is running yet (no sessions), across tmux builds that word it differently.
func isNoTmuxServer(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting to") ||
		strings.Contains(lower, "no such file or directory")
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
