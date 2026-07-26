package apps

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// ExecResult is the outcome of running a command inside the workspace microVM
// (the subset the app Manager needs). It mirrors workspace.ExecResult so the
// apps package stays free of a workspace import (workspace imports apps at start,
// so the dependency must point one way). The real ExecRunner adapts the workspace
// Sandbox's ExecRoot to this shape.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// guestAppDataRoot is the in-VM directory (on the persistent overlay, mounted at
// /persist, arch §26) under which each app's data volume lives. An app's data is
// bind-mounted from /persist/apps/<key> into the container's DataDir so it
// survives microVM restart/recreation.
const guestAppDataRoot = "/persist/apps"

// Status is the runtime state of one installed app in a workspace (for `ai apps
// list` + the TUI).
type Status struct {
	// Key/Name identify the app (from the manifest).
	Key  string `json:"key"`
	Name string `json:"name"`
	// Installed is true when the app is recorded in the workspace config.
	Installed bool `json:"installed"`
	// Running is true when the app's container is up inside the running microVM.
	// It is only meaningful when the workspace is running; otherwise it is false.
	Running bool `json:"running"`
	// Port is the unique host port the app is published on (0 when uninstalled).
	Port int `json:"port,omitempty"`
	// URL is the host URL the app is reachable at (empty when uninstalled).
	URL string `json:"url,omitempty"`
	// Kind distinguishes a managed in-VM container app ("app", the default/empty)
	// from an agent-CLI web dashboard ("dashboard") the user launches in-VM (e.g.
	// `hermes dashboard`) — the platform only publishes its host port, it does not
	// run a container for it, so it is not startable via `ai apps start`.
	Kind string `json:"kind,omitempty"`
}

// KindDashboard marks a Status as an agent-CLI web dashboard rather than a
// managed in-VM container app.
const KindDashboard = "dashboard"

// ExecRunner runs a command inside the workspace microVM as root (the surface
// Manager needs from workspace.Sandbox.ExecRoot). Injected so the host-side
// orchestration is unit-tested with a fake; the real impl is the workspace
// Sandbox bound to the project's microVM.
type ExecRunner func(argv []string) (ExecResult, error)

// Deps are the injectable collaborators the Manager needs, so every lifecycle
// method is unit-testable without a live microVM, gateway, or filesystem.
type Deps struct {
	// LoadConfig / SaveConfig read and persist the project's config.yaml (the
	// installed-apps + port records live there).
	LoadConfig func() (*config.Config, error)
	SaveConfig func(*config.Config) error
	// ReservedPorts returns the host ports already allocated to apps across ALL
	// workspaces on this host, so an allocation is unique machine-wide and two
	// running microVMs never publish the same host port.
	ReservedPorts func() (map[int]bool, error)
	// PortFree reports whether a host TCP port is currently bindable. Defaults to
	// a real loopback bind probe when nil.
	PortFree portChecker
	// Exec runs nerdctl (and probes) inside the workspace microVM as root. It is
	// nil when the workspace is not running; lifecycle methods that need the VM
	// (start/stop/run/pull) return ErrWorkspaceNotRunning then. It is intentionally
	// UNBOUNDED so a legitimately long op (a heavy `nerdctl pull`) is not killed.
	Exec ExecRunner
	// ProbeExec runs a SHORT, time-bounded command inside the microVM for the
	// running-status probe in List (`nerdctl ps`), so listing fails fast (and the
	// caller can classify a stale/overloaded VM) instead of hanging behind a busy
	// microVM. It is wired alongside Exec when the workspace is running; nil falls
	// back to Exec (so existing callers/tests keep working).
	ProbeExec ExecRunner
	// EnsureRuntime makes the in-VM container runtime (containerd) ready before an
	// app container is run, and recovers it if it became unreachable (e.g. it
	// crashed/restarted mid-pull). Optional; nil skips the check.
	EnsureRuntime func() error
	// Gateway supplies the resolved model-gateway base URL (".../v1"), the
	// workspace's scoped LiteLLM virtual key, and the model preference the app
	// containers are configured with — EMPTY in the catalog-driven system (no
	// built-in default model; the app/user picks a served model). Called only when
	// (re)starting an app.
	Gateway func() (gatewayURL, apiKey, defaultModel string, err error)
}

// Manager orchestrates a single workspace's in-VM apps over the injected Deps.
type Manager struct {
	deps Deps
}

// NewManager builds an app Manager over the injected dependencies.
func NewManager(deps Deps) *Manager {
	if deps.PortFree == nil {
		deps.PortFree = realPortFree
	}
	return &Manager{deps: deps}
}

// ErrWorkspaceNotRunning is returned by lifecycle methods that need a running
// microVM (start/stop/restart/update, and the running-status probe in List) when
// none is available (→ exit 3).
var ErrWorkspaceNotRunning = fmt.Errorf("workspace is not running — run `ai start` first")

// ErrNotInstalled is returned when an action targets an app that is not installed
// in the workspace (→ exit 2).
var ErrNotInstalled = fmt.Errorf("app is not installed in this workspace")

// ErrAlreadyInstalled is returned when Install targets an app that is already
// installed (→ exit 2). Re-installing would re-allocate a port; use update/restart.
var ErrAlreadyInstalled = fmt.Errorf("app is already installed in this workspace")

// PublishedPorts returns the guest→host port mappings for every installed app, so
// the workspace start can hand them to egress.MsbNetworkArgs (which publishes
// host:<port> → VM:<port>). The host and guest sides use the SAME number: the
// host reaches the app on <port>, msb forwards it to the VM on <port>, and nerdctl
// maps VM:<port> → container:<containerPort>. Nothing is published for an app that
// is not installed. The mappings are sorted by host port for determinism.
//
// The port chain: host:<port> --(msb -p <port>:<port>)--> VM:<port>
// --(nerdctl -p <port>:<containerPort>)--> container:<containerPort>.
func PublishedPorts(projectConfig *config.Config) []config.PortMapping {
	mappings := make([]config.PortMapping, 0, len(projectConfig.Apps)+len(projectConfig.AgentDashboards))
	for _, entry := range projectConfig.Apps {
		if _, ok := Lookup(entry.Key); !ok {
			continue
		}
		mappings = append(mappings, config.PortMapping{Guest: entry.Port, Host: entry.Port})
	}
	// Agent-CLI web dashboards (e.g. hermes) publish the SAME way — host==guest, forwarded
	// by msb into the VM where the agent launches its dashboard server on that port.
	for _, entry := range projectConfig.AgentDashboards {
		if !IsDashboardAgent(entry.Key) {
			continue
		}
		mappings = append(mappings, config.PortMapping{Guest: entry.Port, Host: entry.Port})
	}
	sort.Slice(mappings, func(left, right int) bool {
		return mappings[left].Host < mappings[right].Host
	})
	return mappings
}

// ErrPortUnavailable is returned when a user-requested app host port is out of range,
// already reserved by another app/workspace, or in use on the host (→ exit 2).
var ErrPortUnavailable = errors.New("requested app host port is unavailable")

// SuggestedHostPort proposes a default host port to expose an app on, for seeding the
// create prompt: the app's familiar container port (e.g. 8080 Open WebUI)
// when it is free and unreserved, otherwise the next auto-allocated free port. Returns 0
// for an unknown key. isFree defaults to a real loopback probe when nil.
func SuggestedHostPort(key string, reserved map[int]bool, isFree portChecker) int {
	manifest, ok := Lookup(key)
	if !ok {
		return 0
	}
	if isFree == nil {
		isFree = realPortFree
	}
	if reserved == nil {
		reserved = map[int]bool{}
	}
	if !reserved[manifest.ContainerPort] && isFree(manifest.ContainerPort) {
		return manifest.ContainerPort
	}
	if port, err := allocatePort(reserved, isFree); err == nil {
		return port
	}
	return manifest.ContainerPort
}

// AllocateEntries assigns each requested app key a host port, returning the AppEntry
// records to persist into a workspace config. It is used at create time to seed the
// wizard-selected apps. Unknown keys are skipped.
//
// requested carries a user-chosen host port per app key (from the create prompt/flag); a
// key with a requested port > 0 is validated (valid range, host-free, not already reserved
// by another app or workspace, no collision within this call) and used as-is — an
// unavailable port is an error (ErrPortUnavailable). A key with no request (0/absent) is
// auto-allocated from the platform's port window, as before. isFree defaults to a real
// loopback probe when nil.
func AllocateEntries(keys []string, requested map[string]int, reserved map[int]bool, isFree portChecker) ([]config.AppEntry, error) {
	return allocatePortsForKeys(keys, requested, reserved, isFree, func(key string) bool {
		_, ok := Lookup(key)
		return ok
	})
}

// allocatePortsForKeys is the shared allocator behind AllocateEntries (in-VM apps) and
// AllocateDashboardEntries (agent-CLI web dashboards): for each KNOWN key it honors a
// requested port (validating range + host-free + no collision within this call or the
// reserved set) or auto-allocates a free window port. Unknown keys are skipped.
func allocatePortsForKeys(keys []string, requested map[string]int, reserved map[int]bool, isFree portChecker, known func(string) bool) ([]config.AppEntry, error) {
	if isFree == nil {
		isFree = realPortFree
	}
	if reserved == nil {
		reserved = map[int]bool{}
	}
	// Copy so we don't mutate the caller's reserved set as we allocate.
	taken := make(map[int]bool, len(reserved))
	for port := range reserved {
		taken[port] = true
	}
	entries := make([]config.AppEntry, 0, len(keys))
	for _, key := range keys {
		if !known(key) {
			continue
		}
		port := requested[key]
		if port > 0 {
			if port > 65535 {
				return nil, fmt.Errorf("%w: %q port %d is out of range (1-65535)", ErrPortUnavailable, key, port)
			}
			if taken[port] {
				return nil, fmt.Errorf("%w: port %d is already in use by another app or workspace", ErrPortUnavailable, port)
			}
			if !isFree(port) {
				return nil, fmt.Errorf("%w: port %d is in use on the host", ErrPortUnavailable, port)
			}
		} else {
			allocated, err := allocatePort(taken, isFree)
			if err != nil {
				return nil, err
			}
			port = allocated
		}
		taken[port] = true
		entries = append(entries, config.AppEntry{Key: key, Port: port})
	}
	return entries, nil
}

// installedEntry returns the recorded AppEntry for key (and whether it exists).
func installedEntry(projectConfig *config.Config, key string) (config.AppEntry, bool) {
	for _, entry := range projectConfig.Apps {
		if entry.Key == key {
			return entry, true
		}
	}
	return config.AppEntry{}, false
}

// Install records the app in the workspace config and allocates its unique host
// port. It does NOT publish the port or start the container by itself — adding an
// app changes the microVM's published-port set, which msb only applies at create,
// so the workspace must be (re)started to take effect. When the workspace is
// running, Install best-effort starts the container immediately so it is usable
// without a restart for the container itself (the published port still needs the
// restart). It returns the allocated port and whether a restart is required.
func (manager *Manager) Install(key string) (port int, restartRequired bool, err error) {
	manifest, ok := Lookup(key)
	if !ok {
		return 0, false, Validate(key)
	}
	projectConfig, err := manager.deps.LoadConfig()
	if err != nil {
		return 0, false, err
	}
	if _, exists := installedEntry(projectConfig, key); exists {
		return 0, false, fmt.Errorf("%w: %q", ErrAlreadyInstalled, key)
	}
	reserved, err := manager.deps.ReservedPorts()
	if err != nil {
		return 0, false, err
	}
	if reserved == nil {
		reserved = map[int]bool{}
	}
	// Also reserve this workspace's own already-allocated ports (defensive — they
	// should already be in the machine-wide set, but never collide locally).
	for _, entry := range projectConfig.Apps {
		reserved[entry.Port] = true
	}
	allocated, err := allocatePort(reserved, manager.deps.PortFree)
	if err != nil {
		return 0, false, err
	}
	projectConfig.Apps = append(projectConfig.Apps, config.AppEntry{Key: key, Port: allocated})
	if err := manager.deps.SaveConfig(projectConfig); err != nil {
		return 0, false, err
	}
	// If the microVM is running, start the container now (best-effort) so the app
	// is immediately usable in-VM. The host-published port still needs a restart
	// for msb to publish it, so always report restartRequired.
	if manager.deps.Exec != nil {
		_ = manager.runContainer(manifest, allocated)
	}
	return allocated, true, nil
}

// Remove stops/removes the app's container (best-effort, when the VM is running),
// unrecords it from the workspace config, and frees its host port. Removing an app
// changes the published-port set, so the workspace must be restarted for msb to
// stop publishing the freed port; that is reported as restartRequired.
func (manager *Manager) Remove(key string) (restartRequired bool, err error) {
	manifest, ok := Lookup(key)
	if !ok {
		return false, Validate(key)
	}
	projectConfig, err := manager.deps.LoadConfig()
	if err != nil {
		return false, err
	}
	if _, exists := installedEntry(projectConfig, key); !exists {
		return false, fmt.Errorf("%w: %q", ErrNotInstalled, key)
	}
	// Tear down the container if the VM is up (best-effort: a missing container is
	// fine — the goal is no leftover running container for a removed app).
	if manager.deps.Exec != nil {
		_, _ = manager.deps.Exec([]string{"nerdctl", "rm", "-f", manifest.ContainerName()})
	}
	kept := projectConfig.Apps[:0]
	for _, entry := range projectConfig.Apps {
		if entry.Key == key {
			continue
		}
		kept = append(kept, entry)
	}
	projectConfig.Apps = kept
	if err := manager.deps.SaveConfig(projectConfig); err != nil {
		return false, err
	}
	return true, nil
}

// Update pulls the app's latest image and recreates its container (rm -f then
// run) so the running app is upgraded in place. Needs a running workspace.
func (manager *Manager) Update(key string) error {
	manifest, entry, err := manager.requireInstalled(key)
	if err != nil {
		return err
	}
	if manager.deps.Exec == nil {
		return ErrWorkspaceNotRunning
	}
	// hardware bring-up: the live `nerdctl pull` runs only inside a booted microVM.
	if result, err := manager.deps.Exec([]string{"nerdctl", "pull", manifest.Image}); err != nil {
		return err
	} else if result.ExitCode != 0 {
		return fmt.Errorf("nerdctl pull %s exited %d: %s", manifest.Image, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return manager.runContainer(manifest, entry.Port)
}

// Start (re)starts the app's container. If the container exists it is started;
// otherwise it is run fresh. Needs a running workspace.
func (manager *Manager) Start(key string) error {
	manifest, entry, err := manager.requireInstalled(key)
	if err != nil {
		return err
	}
	if manager.deps.Exec == nil {
		return ErrWorkspaceNotRunning
	}
	// Prefer a plain start of the existing container; fall back to a fresh run when
	// it does not exist yet (e.g. installed while the VM was down).
	if result, err := manager.deps.Exec([]string{"nerdctl", "start", manifest.ContainerName()}); err == nil && result.ExitCode == 0 {
		return nil
	}
	return manager.runContainer(manifest, entry.Port)
}

// Stop stops the app's container (without removing it). Needs a running workspace.
func (manager *Manager) Stop(key string) error {
	manifest, _, err := manager.requireInstalled(key)
	if err != nil {
		return err
	}
	if manager.deps.Exec == nil {
		return ErrWorkspaceNotRunning
	}
	result, err := manager.deps.Exec([]string{"nerdctl", "stop", manifest.ContainerName()})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("nerdctl stop %s exited %d: %s", manifest.ContainerName(), result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// Restart restarts the app's container. Needs a running workspace.
func (manager *Manager) Restart(key string) error {
	manifest, entry, err := manager.requireInstalled(key)
	if err != nil {
		return err
	}
	if manager.deps.Exec == nil {
		return ErrWorkspaceNotRunning
	}
	if result, err := manager.deps.Exec([]string{"nerdctl", "restart", manifest.ContainerName()}); err == nil && result.ExitCode == 0 {
		return nil
	}
	// Container missing — run it fresh.
	return manager.runContainer(manifest, entry.Port)
}

// List returns the status of every supported app: installed (from config),
// running (probed in the VM when it is up), the allocated port, and the host URL.
// It never fails on a stopped workspace — running simply reports false.
func (manager *Manager) List() ([]Status, error) {
	projectConfig, err := manager.deps.LoadConfig()
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	if manager.deps.Exec != nil {
		probed, err := manager.runningContainers()
		if err != nil {
			// The in-VM running-status probe failed/timed out (busy or wedged VM). Surface
			// it so the caller (CLI/TUI) can classify it into a precise, actionable error
			// (stale vs. overloaded), rather than misreporting every app as stopped.
			return nil, err
		}
		running = probed
	}
	statuses := make([]Status, 0, len(catalogue))
	for _, manifest := range catalogue {
		status := Status{Key: manifest.Key, Name: manifest.Name}
		if entry, ok := installedEntry(projectConfig, manifest.Key); ok {
			status.Installed = true
			status.Port = entry.Port
			status.URL = fmt.Sprintf("http://localhost:%d", entry.Port)
			status.Running = running[manifest.ContainerName()]
		}
		statuses = append(statuses, status)
	}
	// Agent-CLI web dashboards (e.g. hermes) are not container apps — the platform only
	// publishes their host port; the user launches the server in-VM (`hermes dashboard`).
	// Surface them in the same list so their reserved port + URL are discoverable, with a
	// best-effort running probe (the launched `<cli> dashboard` process).
	for _, entry := range projectConfig.AgentDashboards {
		if !IsDashboardAgent(entry.Key) {
			continue
		}
		status := Status{
			Key:       entry.Key,
			Name:      dashboardDisplayName(entry.Key),
			Kind:      KindDashboard,
			Installed: true,
			Port:      entry.Port,
			URL:       fmt.Sprintf("http://localhost:%d", entry.Port),
		}
		if manager.deps.Exec != nil {
			status.Running = manager.dashboardRunning(entry.Key)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// dashboardDisplayName renders an agent CLI's dashboard label for listings, e.g.
// "hermes" -> "Hermes Dashboard".
func dashboardDisplayName(cli string) string {
	if cli == "" {
		return "Dashboard"
	}
	return strings.ToUpper(cli[:1]) + cli[1:] + " Dashboard"
}

// dashboardRunning best-effort reports whether an agent CLI's in-VM dashboard server
// (`<cli> dashboard`) is currently running. A probe failure reports false (not fatal —
// the dashboard is a user-launched process, absence just means "not running yet").
func (manager *Manager) dashboardRunning(cli string) bool {
	probe := manager.deps.ProbeExec
	if probe == nil {
		probe = manager.deps.Exec
	}
	if probe == nil {
		return false
	}
	result, err := probe([]string{"sh", "-c", fmt.Sprintf("pgrep -f '%s dashboard' >/dev/null 2>&1 && echo up", cli)})
	if err != nil {
		return false
	}
	return strings.TrimSpace(result.Stdout) == "up"
}

// StartInstalled (re)runs every installed app's container, used at workspace
// start once the microVM and gateway are up. It is best-effort PER app: one app
// failing to start is collected as a warning and does not abort the others or the
// workspace start. The returned warnings are human strings for the caller to
// surface. Exec must be set (the workspace is running by the time this is called).
func (manager *Manager) StartInstalled() (warnings []string, err error) {
	projectConfig, err := manager.deps.LoadConfig()
	if err != nil {
		return nil, err
	}
	for _, entry := range projectConfig.Apps {
		manifest, ok := Lookup(entry.Key)
		if !ok {
			continue
		}
		if err := manager.runContainer(manifest, entry.Port); err != nil {
			warnings = append(warnings, fmt.Sprintf("app %q did not start: %v", entry.Key, err))
		}
	}
	return warnings, nil
}

// requireInstalled resolves an installed app's manifest + entry, mapping an
// unknown key to ErrUnknownApp and a not-installed app to ErrNotInstalled.
func (manager *Manager) requireInstalled(key string) (Manifest, config.AppEntry, error) {
	manifest, ok := Lookup(key)
	if !ok {
		return Manifest{}, config.AppEntry{}, Validate(key)
	}
	projectConfig, err := manager.deps.LoadConfig()
	if err != nil {
		return Manifest{}, config.AppEntry{}, err
	}
	entry, exists := installedEntry(projectConfig, key)
	if !exists {
		return Manifest{}, config.AppEntry{}, fmt.Errorf("%w: %q", ErrNotInstalled, key)
	}
	return manifest, entry, nil
}

// runContainer runs the app's container idempotently inside the microVM: it
// removes any prior container of the same name, then `nerdctl run -d` with the
// published port, the persistent data volume, an optional /workspace mount, and
// the gateway-pointing env. The host and guest port are the SAME number (the msb
// publish handles host:<port> → VM:<port>); nerdctl maps VM:<port> →
// container:<ContainerPort>.
//
// hardware bring-up: the live `nerdctl run` executes only inside a booted microVM
// with the rootful containerd up; the argv construction is unit-tested here.
func (manager *Manager) runContainer(manifest Manifest, port int) error {
	gatewayURL, apiKey, defaultModel, err := manager.deps.Gateway()
	if err != nil {
		return err
	}
	// Ensure the in-VM container runtime is up before talking to it.
	if manager.deps.EnsureRuntime != nil {
		if err := manager.deps.EnsureRuntime(); err != nil {
			return err
		}
	}
	// The persistent data dir is a host bind source that nerdctl auto-creates ROOT-owned
	// 0755; an app whose container runs as a NON-root user then cannot
	// write it and crashes on startup. Create it world-writable first (see AppsAutostartScript).
	dataDir := fmt.Sprintf("%s/%s", guestAppDataRoot, manifest.Key)
	_, _ = manager.deps.Exec([]string{"sh", "-c", "mkdir -p " + dataDir + " && chmod 0777 " + dataDir})
	argv := runArgs(manifest, port, gatewayURL, apiKey, defaultModel)
	run := func() (ExecResult, error) {
		// Idempotent recreate: drop any existing container first (ignore its result —
		// a missing container is fine), then run fresh.
		_, _ = manager.deps.Exec([]string{"nerdctl", "rm", "-f", manifest.ContainerName()})
		return manager.deps.Exec(argv)
	}
	result, err := run()
	if err != nil {
		return err
	}
	// If the runtime became unreachable mid-op (it crashed/restarted — e.g. memory
	// pressure during a large image pull), re-ensure it and retry once.
	if result.ExitCode != 0 && runtimeUnreachable(result.Stderr) && manager.deps.EnsureRuntime != nil {
		if err := manager.deps.EnsureRuntime(); err != nil {
			return err
		}
		if result, err = run(); err != nil {
			return err
		}
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("nerdctl run %s exited %d: %s", manifest.ContainerName(), result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// runtimeUnreachable reports whether a nerdctl stderr indicates the in-VM container
// runtime (containerd) was not reachable (socket missing or refusing connections),
// as opposed to an ordinary container error.
func runtimeUnreachable(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "containerd.sock") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "cannot access containerd")
}

// runArgs builds the `nerdctl run -d` argv for an app container. The env is
// rendered in sorted key order so the argv is deterministic (testable).
func runArgs(manifest Manifest, port int, gatewayURL, apiKey, defaultModel string) []string {
	dataVolume := fmt.Sprintf("%s/%s", guestAppDataRoot, manifest.Key)
	argv := []string{
		"nerdctl", "run", "-d",
		"--name", manifest.ContainerName(),
		"--restart", "always",
		"-p", fmt.Sprintf("%d:%d", port, manifest.ContainerPort),
		"-v", fmt.Sprintf("%s:%s", dataVolume, manifest.DataDir),
	}
	if manifest.MountWorkspace {
		// Mount the in-VM project dir (workspace.workspaceWorkdir = ~/project; can't
		// import that package here — cycle) into the app container at /workspace.
		argv = append(argv, "-v", "/home/workspace/project:/workspace")
	}
	if manifest.Memory != "" {
		argv = append(argv, "--memory", manifest.Memory)
	}
	env := manifest.Env(gatewayURL, apiKey, defaultModel)
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		argv = append(argv, "-e", fmt.Sprintf("%s=%s", key, env[key]))
	}
	argv = append(argv, manifest.Image)
	return argv
}

// runningContainers returns the set of app container names currently running in
// the microVM, by name (nerdctl ps name filter), using the bounded ProbeExec so it
// can't hang behind a busy VM. An INFRASTRUCTURE error (the bounded exec failed —
// VM gone, msb missing, or the probe timed out) is returned so List can classify
// it; only a non-zero nerdctl exit (e.g. containerd not up yet — an empty set) is
// treated as "nothing running", never an error.
func (manager *Manager) runningContainers() (map[string]bool, error) {
	running := map[string]bool{}
	probe := manager.deps.ProbeExec
	if probe == nil {
		probe = manager.deps.Exec
	}
	result, err := probe([]string{"sh", "-c", "nerdctl ps --format '{{.Names}}' 2>/dev/null"})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return running, nil
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			running[name] = true
		}
	}
	return running, nil
}
