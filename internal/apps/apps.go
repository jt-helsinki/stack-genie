package apps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/config"
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
}

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
	// (start/stop/run/pull) return ErrWorkspaceNotRunning then.
	Exec ExecRunner
	// Gateway supplies the resolved model-gateway base URL (".../v1"), the
	// workspace's scoped LiteLLM virtual key, and the default model handle that the
	// app containers are configured with. Called only when (re)starting an app.
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
	mappings := make([]config.PortMapping, 0, len(projectConfig.Apps))
	for _, entry := range projectConfig.Apps {
		if _, ok := Lookup(entry.Key); !ok {
			continue
		}
		mappings = append(mappings, config.PortMapping{Guest: entry.Port, Host: entry.Port})
	}
	sort.Slice(mappings, func(left, right int) bool {
		return mappings[left].Host < mappings[right].Host
	})
	return mappings
}

// AllocateEntries allocates a unique host port for each requested app key,
// avoiding the reserved set (and the ports allocated to earlier keys in the same
// call), returning the AppEntry records to persist into a workspace config. It is
// used at create time to seed the wizard-selected apps. Unknown keys are skipped.
// isFree defaults to a real loopback probe when nil.
func AllocateEntries(keys []string, reserved map[int]bool, isFree portChecker) ([]config.AppEntry, error) {
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
		if _, ok := Lookup(key); !ok {
			continue
		}
		port, err := allocatePort(taken, isFree)
		if err != nil {
			return nil, err
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
		running = manager.runningContainers()
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
	return statuses, nil
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
	// Idempotent recreate: drop any existing container first (ignore its result —
	// a missing container is fine), then run fresh.
	_, _ = manager.deps.Exec([]string{"nerdctl", "rm", "-f", manifest.ContainerName()})
	argv := runArgs(manifest, port, gatewayURL, apiKey, defaultModel)
	result, err := manager.deps.Exec(argv)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("nerdctl run %s exited %d: %s", manifest.ContainerName(), result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
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
		argv = append(argv, "-v", "/workspace:/workspace")
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
// the microVM, by name (nerdctl ps name filter). A probe error yields an empty
// set (treated as "nothing running"), never an error — List must not fail on a
// transient probe issue.
func (manager *Manager) runningContainers() map[string]bool {
	running := map[string]bool{}
	result, err := manager.deps.Exec([]string{"nerdctl", "ps", "--format", "{{.Names}}"})
	if err != nil || result.ExitCode != 0 {
		return running
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			running[name] = true
		}
	}
	return running
}
