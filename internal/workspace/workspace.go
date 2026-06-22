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
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// headroomPort is the host port the in-workspace Headroom proxy reaches the
// model gateway on; the workspace egress policy always allows the host gateway
// on this port (egress.MsbNetworkArgs, arch §29.2).
const headroomPort = 18787

// gatewayURL is the base URL the in-workspace agent CLIs use to reach the host
// Headroom proxy from inside the microVM (arch §15). The /v1 suffix is required
// by both opencode and pi. host.microsandbox.internal resolves to the host from
// within a Microsandbox microVM.
const gatewayURL = "http://host.microsandbox.internal:18787/v1"

// Guest paths the agent provider configs are written to inside the microVM. The
// workspace image creates a `workspace` user; both files live under its home.
const (
	openCodeGuestPath = "/home/workspace/.config/opencode/opencode.json"
	piGuestPath       = "/home/workspace/.pi/agent/models.json"
)

// ErrUnknownProject is returned when a project name is not in the global index
// (→ exit 2).
var ErrUnknownProject = errors.New("unknown project")

// ErrNotStarted is returned when an operation needs an existing workspace handle
// but none has been created yet (→ exit 2): the caller should `ai workspace
// start` first.
var ErrNotStarted = errors.New("workspace was never started; run `ai workspace start` first")

// ErrAlreadyStopped lets a Sandbox.Stop signal that the microVM was already
// stopped. Restart treats this as a no-op (it only needs the microVM down before
// starting it again) rather than a failure.
var ErrAlreadyStopped = errors.New("workspace microVM is already stopped")

// Name derives the deterministic workspace/microVM name (arch §7, §19):
// aip-<project>. There is one workspace per project.
func Name(project string) string {
	return "aip-" + project
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
	// WriteFile writes content to guestPath inside the running microVM, creating
	// parent directories. name is the human label only used for error context.
	WriteFile(name, guestPath string, content []byte) error
	// InspectNetwork reads the egress policy in force on the named microVM via
	// `msb inspect`. It returns ErrNotRunning when no such sandbox exists (the
	// workspace is not running) and ErrMsbMissing when msb is not installed —
	// both of which callers treat as "show the declared policy only", not an error.
	InspectNetwork(name string) (NetworkPolicy, error)
}

// KeyMinter mints scoped LiteLLM virtual keys. It is the small surface
// Manager needs from litellm.KeyManager, defined locally so tests can supply a
// fake without a live gateway (the real impl is *litellm.KeyManager).
type KeyMinter interface {
	GenerateKey(scope litellm.KeyScope) (string, error)
}

// Manager coordinates the lifecycle over a Builder + Sandbox, stamping state with
// Now (RFC 3339 UTC). GOOS records the host OS for any host-specific behavior
// (supported hosts: macOS and Linux).
type Manager struct {
	Builder Builder
	Sandbox Sandbox
	Keys    KeyMinter
	Now     func() string
	GOOS    string
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
	// the host gateway is always allowed on the Headroom port (arch §29.2).
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		return nil, err
	}
	netArgs := egress.MsbNetworkArgs(projectConfig.Network, headroomPort)
	// The microVM mounts the host project path directly. Supported hosts are
	// macOS and Linux, so no path translation is needed (arch §7).
	if err := manager.Sandbox.Create(name, imageRef, root, overlayPath, netArgs); err != nil {
		return nil, err
	}
	if err := manager.Sandbox.Start(name); err != nil {
		return nil, err
	}
	// Register the agent CLIs' provider config so opencode/pi inside the microVM
	// talk to the host Headroom proxy through a per-workspace scoped LiteLLM
	// virtual key (arch §15, §17). The key flows host→VM only; it is never
	// written to platform disk.
	if err := manager.registerAgentProviders(name, project, projectConfig); err != nil {
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
	return handle, nil
}

// registerAgentProviders mints a scoped LiteLLM virtual key for the workspace
// and writes the opencode + pi provider configs into the running microVM so the
// agent CLIs reach the host Headroom proxy with that key. The per-project
// Headroom strategy maps to the two per-request knobs opencode bakes into each
// model's request body; pi cannot inject per-request fields and uses Headroom's
// server-side defaults (see internal/agentcfg).
func (manager Manager) registerAgentProviders(name, project string, projectConfig *config.Config) error {
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
	models := namedModels(routing)

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
	return manager.Sandbox.WriteFile(name, piGuestPath, piConfig)
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
