// Package project implements project scaffolding and teardown for
// `ai create|delete|list` (CLI §3). Create's interactive wizard lives in
// the CLI layer; this package owns the deterministic work: validate the choices,
// write the project's tracked .ai-platform/ files (Dockerfile via envimage,
// config.yaml, profile.yaml, project.yaml, .gitignore) and the global index.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/envimage"
	"github.com/jt-helsinki/stack-genie/internal/overlay"
	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/state"
	"github.com/jt-helsinki/stack-genie/internal/templates"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// namePattern validates project names (arch §19): lowercase alphanumeric and
// hyphens, 2–40 chars, no leading/trailing hyphen.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

var (
	ErrInvalidName    = errors.New("invalid project name")
	ErrAlreadyExists  = errors.New("project already exists")
	ErrUnknownProject = errors.New("unknown project")
)

// Spec is the resolved set of choices from the create wizard (CLI §3.1).
type Spec struct {
	Name        string
	OS          string
	Stacks      []string
	AgentCLIs   []string
	DefaultTool string
	// AuthModes records the per-CLI auth mode ("api-key"|"oauth") for the OAuth-capable
	// CLIs (claude-code/codex/gemini). Written to config.yaml agent.auth_modes for the
	// selected + OAuth-capable CLIs only; an absent CLI defaults to "api-key".
	AuthModes map[string]string
	// GraphifyModel is the local omlx-served model Graphify uses, written to
	// config.yaml agent.graphify_model — just the model NAME, no download (model
	// management lives in omlx's own admin panel). Empty leaves Graphify's backend
	// unconfigured.
	GraphifyModel string
	// Apps are the opt-in in-VM AI applications to install (subset of apps.Keys()).
	// Empty by default — apps are opt-in.
	Apps []string
	// AppPorts is the user-chosen HOST port to expose each selected app's web UI on,
	// keyed by app key (from the create prompt / --app-port flag). A key with no entry
	// (or 0) is auto-allocated. Validated + honored by apps.AllocateEntries at Scaffold.
	AppPorts map[string]int
	// IdleTimeout is written to config.yaml microsandbox.idle_timeout and passed to
	// `msb create --idle-timeout` at every workspace start/restart. Empty defaults to
	// config.DefaultMicrosandboxIdleTimeout.
	IdleTimeout string
	// CPUs is the workspace vCPU limit written to config.yaml workspace.cpu_limit and
	// applied to the microVM at start. 0 uses the platform default.
	CPUs int
	// Memory is the workspace memory limit (e.g. "8G") written to
	// config.yaml workspace.memory_limit. Empty uses the platform default.
	Memory string
	// Disk is the workspace writable-rootfs size in GiB (e.g. "20") written to
	// config.yaml workspace.disk_limit, sizing the in-VM containerd image store so
	// multi-GB app images fit. Empty uses the platform default.
	Disk string
	// Shell is the workspace's default interactive shell ("bash" or "zsh"), written to
	// config.yaml workspace.shell. Empty defaults to "bash" (today's behavior).
	Shell string
	// CavemanEnabled records whether the Caveman output-compression toolkit is
	// installed into the workspace at start, written to config.yaml
	// context.caveman_enabled. Chosen at `ai create` from the unified AI-tools list
	// (default on).
	CavemanEnabled bool
	// GraphifyEnabled records whether the Graphify knowledge-graph toolkit is baked
	// into the workspace image (conditional Dockerfile snippet) and registered with
	// each installed agent CLI at start, written to config.yaml
	// context.graphify_enabled. Chosen at `ai create` from the unified AI-tools list
	// (default on).
	GraphifyEnabled bool
	// CodeReviewGraphEnabled records whether the code-review-graph toolkit is
	// installed into the workspace at start and registered as an MCP server with each
	// installed agent CLI, written to config.yaml context.code_review_graph_enabled.
	// Chosen at `ai create` (OPT-IN, default false).
	CodeReviewGraphEnabled bool
	// CodebaseMemoryEnabled records whether the codebase-memory-mcp server is
	// installed into the workspace at start and registered as an MCP server with each
	// installed agent CLI, written to config.yaml context.codebase_memory_enabled.
	// Chosen at `ai create` (OPT-IN, default false).
	CodebaseMemoryEnabled bool
	// PublishPorts are the host↔guest ports to open into the sandbox, written to
	// config.yaml network.publish_ports.
	PublishPorts []config.PortMapping
	// Root is the host source directory for the workspace. Empty falls back to
	// ~/projects/<name> (RootPath); `ai create` sets it explicitly to the chosen
	// location.
	Root string
}

// ValidateName reports whether a name is well-formed (arch §19).
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q (lowercase alphanumeric + hyphens, 2-40 chars, no leading/trailing hyphen)", ErrInvalidName, name)
	}
	return nil
}

// RootPath returns the default host source path for a project (~/projects/<name>).
// `ai create` creates the project in the current directory and sets
// Spec.Root explicitly; RootPath remains the fallback when Root is unset.
func RootPath(name string) (string, error) {
	projectsDir, err := paths.ProjectsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(projectsDir, name), nil
}

// resolvedRoot returns the spec's host source dir: spec.Root when set, else the
// default location for the name.
func (spec Spec) resolvedRoot() (string, error) {
	if spec.Root != "" {
		return spec.Root, nil
	}
	return RootPath(spec.Name)
}

// profileFile is <project>/.ai-platform/profile.yaml (repo-layout §12.1a).
type profileFile struct {
	SchemaVersion int      `yaml:"schema_version"`
	Stacks        []string `yaml:"stacks"`
}

// EnsureCreatable validates the name and confirms no project with that name is
// already registered, and that root is not already a project (so the caller can
// clone/init the dir before scaffolding). root is the resolved host source dir.
func EnsureCreatable(name, root string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	index, err := state.LoadIndex()
	if err != nil {
		return err
	}
	if _, exists := index.Projects[name]; exists {
		return fmt.Errorf("%w: %q", ErrAlreadyExists, name)
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "project.yaml")); err == nil {
		return fmt.Errorf("%w: %s is already a project", ErrAlreadyExists, root)
	}
	// Forbid nesting: a workspace may not be created inside another workspace.
	if ancestor, found, err := nearestAncestorProject(root); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %s is inside an existing workspace at %s", ErrAlreadyExists, root, ancestor)
	}
	return nil
}

// ValidateNewLocation reports whether dir is a valid location for a NEW workspace:
// it must not itself be a workspace, nor be nested inside one (no ancestor with a
// .ai-platform/project.yaml). It does not require the directory to exist (create
// makes it). Used for early feedback in the create wizard.
func ValidateNewLocation(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, ".ai-platform", "project.yaml")); err == nil {
		return fmt.Errorf("%w: %s is already a workspace", ErrAlreadyExists, abs)
	}
	if ancestor, found, err := nearestAncestorProject(abs); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %s is inside an existing workspace at %s", ErrAlreadyExists, abs, ancestor)
	}
	return nil
}

// nearestAncestorProject walks up from dir's PARENT looking for a directory that holds
// .ai-platform/project.yaml (an existing workspace). It returns that ancestor and true
// if found — used to forbid creating a workspace nested inside another. The directory
// itself is not checked (callers handle the self case).
func nearestAncestorProject(dir string) (string, bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	current := filepath.Dir(abs)
	for {
		if _, err := os.Stat(filepath.Join(current, ".ai-platform", "project.yaml")); err == nil {
			return current, true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil // reached the filesystem root
		}
		current = parent
	}
}

// Path returns a registered project's root and whether it exists in the index.
func Path(name string) (string, bool, error) {
	index, err := state.LoadIndex()
	if err != nil {
		return "", false, err
	}
	entry, ok := index.Projects[name]
	return entry.Path, ok, nil
}

// Scaffold creates a project's tracked .ai-platform/ files and registers it in
// the global index. It tolerates a directory that already has other content
// (the files are left untouched) but refuses to overwrite an existing project.
// It does NOT touch version control or start a workspace.
func Scaffold(spec Spec, createdAt string) (string, error) {
	root, err := spec.resolvedRoot()
	if err != nil {
		return "", err
	}
	if err := EnsureCreatable(spec.Name, root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(root, ".ai-platform"), 0o755); err != nil {
		return "", err
	}
	index, err := state.LoadIndex()
	if err != nil {
		return "", err
	}

	// Refresh the on-disk templates (~/.ai-platform/templates) from THIS binary's
	// embedded copies before composing. envimage reads the on-disk copies, so an
	// install left by an OLDER `ai` (e.g. before tmux was added to the base image)
	// would otherwise silently seed a stale Dockerfile — a binary upgrade alone
	// wouldn't fix it without re-running `ai setup`. Install overwrites and is
	// idempotent (repo-layout §1.5), so create always reflects the running binary.
	if err := templates.Install(); err != nil {
		return "", err
	}
	// Dockerfile = OS base + selected stacks + selected agent CLIs + selected opt-in
	// tools (§25, §12). The opt-in tools are baked ONLY when selected, so an
	// unselected tool's installer never runs at build.
	var tools []string
	if spec.GraphifyEnabled {
		tools = append(tools, "graphify")
	}
	if spec.CodeReviewGraphEnabled {
		tools = append(tools, "code-review-graph")
	}
	if spec.CodebaseMemoryEnabled {
		tools = append(tools, "codebase-memory-mcp")
	}
	if err := envimage.Write(root, spec.OS, spec.Stacks, spec.AgentCLIs, tools); err != nil {
		return "", err
	}

	// config.yaml — project config with the installed agent CLIs + default, and
	// the opt-in in-VM apps each allocated a unique machine-wide host port (so two
	// running workspaces never publish the same port). Allocation avoids ports
	// already in use by other workspaces' apps.
	reserved, err := apps.ReservedPortsAcrossWorkspaces()
	if err != nil {
		return "", err
	}
	appEntries, err := apps.AllocateEntries(spec.Apps, spec.AppPorts, reserved, nil)
	if err != nil {
		return "", err
	}
	// Agent-CLI web dashboards (e.g. hermes) get a host port too, published from the microVM
	// the same way as apps. Reserve the app entries just allocated so a dashboard can't
	// collide with them (or with another workspace). spec.AppPorts carries the user's
	// requested dashboard ports too (keyed by the agent CLI).
	dashboardReserved := make(map[int]bool, len(reserved)+len(appEntries))
	for port := range reserved {
		dashboardReserved[port] = true
	}
	for _, entry := range appEntries {
		dashboardReserved[entry.Port] = true
	}
	dashboardEntries, err := apps.AllocateDashboardEntries(apps.SelectedDashboardAgents(spec.AgentCLIs), spec.AppPorts, dashboardReserved, nil)
	if err != nil {
		return "", err
	}
	idleTimeout := spec.IdleTimeout
	if idleTimeout == "" {
		idleTimeout = config.DefaultMicrosandboxIdleTimeout
	}
	// Resource limits: an unset CPU/memory falls back to the platform default so the
	// written config is self-describing (diagnostics + the Sandbox Configuration tab).
	cpus := spec.CPUs
	if cpus <= 0 {
		cpus = config.Default().Workspace.CPULimit
	}
	memory := spec.Memory
	if memory == "" {
		memory = config.Default().Workspace.MemoryLimit
	}
	disk := spec.Disk
	if disk == "" {
		disk = config.Default().Workspace.DiskLimit
	}
	// Default interactive shell: bash unless the create wizard/flag chose zsh.
	shell := spec.Shell
	if shell == "" {
		shell = "bash"
	}
	// Per-agent auth modes: record ONLY the CLIs that are both selected AND OAuth-eligible.
	// The OAuth-CAPABLE CLIs (claude-code/codex/gemini) take the user's chosen mode
	// (default api-key); a FORCED-oauth CLI (currently none) is always "oauth" — it has no
	// api-key mode. Everything else is always gateway/api-key and is omitted. For each
	// agent in oauth, allow-list its provider egress domains so its native login can reach
	// the provider even under `deny` (explicit under `public`).
	authModes := map[string]string{}
	var oauthAllow []config.HostService
	allowOAuthDomains := func(cli string) {
		for _, domain := range agentcfg.OAuthProviderDomains(cli) {
			oauthAllow = appendUniqueHostService(oauthAllow, config.HostService{Host: domain, Port: 443})
		}
	}
	for _, cli := range config.OAuthCapableCLIs() {
		if !slices.Contains(spec.AgentCLIs, cli) {
			continue
		}
		mode := spec.AuthModes[cli]
		if mode == "" {
			mode = "api-key"
		}
		authModes[cli] = mode
		if mode == "oauth" {
			allowOAuthDomains(cli)
		}
	}
	for _, cli := range config.ForcedOAuthCLIs() {
		if !slices.Contains(spec.AgentCLIs, cli) {
			continue
		}
		authModes[cli] = "oauth"
		allowOAuthDomains(cli)
	}
	if len(authModes) == 0 {
		authModes = nil
	}
	// Persist the AI-tools on/off choices (caveman + graphify + code-review-graph +
	// codebase-memory-mcp) here; create.Execute's later SetStrategy/SetCavemanLevel calls
	// are read-modify-write and preserve them.
	cavemanEnabled := spec.CavemanEnabled
	graphifyEnabled := spec.GraphifyEnabled
	codeReviewGraphEnabled := spec.CodeReviewGraphEnabled
	codebaseMemoryEnabled := spec.CodebaseMemoryEnabled
	projectConfig := &config.Config{
		OS:              spec.OS,
		Agent:           config.AgentConfig{Tools: spec.AgentCLIs, DefaultTool: spec.DefaultTool, GraphifyModel: spec.GraphifyModel, AuthModes: authModes},
		Workspace:       config.WorkspaceConfig{CPULimit: cpus, MemoryLimit: memory, DiskLimit: disk, Shell: shell},
		Context:         config.ContextConfig{CavemanEnabled: &cavemanEnabled, GraphifyEnabled: &graphifyEnabled, CodeReviewGraphEnabled: &codeReviewGraphEnabled, CodebaseMemoryEnabled: &codebaseMemoryEnabled},
		Microsandbox:    config.MicrosandboxConfig{IdleTimeout: idleTimeout},
		Network:         config.NetworkConfig{PublishPorts: spec.PublishPorts, AllowHostServices: oauthAllow},
		Apps:            appEntries,
		AgentDashboards: dashboardEntries,
	}
	if err := config.WriteProject(root, projectConfig); err != nil {
		return "", err
	}

	// profile.yaml — software stacks (§12.1a).
	if err := writeProfile(root, spec.Stacks); err != nil {
		return "", err
	}

	// The per-CLI gateway configs are no longer scaffolded here as `.ai-platform/agents/`
	// templates — at workspace start the platform writes each CLI's KEYLESS config into
	// its own DEFAULT project location (<project>/.opencode/, .claude/, .codex/;
	// gemini is env-only) with the scoped key supplied via env vars, so the key is never
	// on host disk. The shared <project>/.ai-platform/{agents,skills,prompts,projects}
	// pool + per-CLI symlinks are set up at start too.

	// project.yaml (tracked).
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: spec.Name, OS: spec.OS, Created: createdAt}); err != nil {
		return "", err
	}

	// .gitignore — ignore the host-local run/ directory.
	if err := os.WriteFile(filepath.Join(root, ".ai-platform", ".gitignore"), []byte("run/\n"), 0o644); err != nil {
		return "", err
	}

	// Register in the global index.
	index.Projects[spec.Name] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		return "", err
	}
	return root, nil
}

// appendUniqueHostService appends service to list unless an identical host:port entry is
// already present (de-duplicated allow-list, e.g. two oauth agents sharing a domain).
func appendUniqueHostService(list []config.HostService, service config.HostService) []config.HostService {
	for _, existing := range list {
		if existing.Host == service.Host && existing.Port == service.Port {
			return list
		}
	}
	return append(list, service)
}

func writeProfile(root string, stacks []string) error {
	if stacks == nil {
		stacks = []string{}
	}
	return conffile.WriteAtomic(filepath.Join(root, ".ai-platform", "profile.yaml"),
		profileFile{SchemaVersion: 1, Stacks: stacks})
}

// Entry is one row of `ai list` (CLI §3.3): name, os, the active agent CLIs, the
// workspace status, and the microVM handle details (id + lifecycle timestamps).
type Entry struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	OS     string   `json:"os,omitempty"`
	Agents []string `json:"agents,omitempty"`
	// Status is the project's workspace lifecycle state (started/stopped/…), or
	// "none" when no workspace has been started for it yet.
	Status string `json:"status"`
	// ID is the workspace microVM id (aip-<name>); Created / LastStarted are the
	// lifecycle timestamps from the saved handle (empty when never started).
	ID          string `json:"id,omitempty"`
	Created     string `json:"created,omitempty"`
	LastStarted string `json:"last_started,omitempty"`
}

// List returns the registered projects (from the global index), each enriched
// with the OS + active agent CLIs from its config and the current workspace
// status, so `ai list` reports the full row the spec promises (§3.3).
func List() ([]Entry, error) {
	index, err := state.LoadIndex()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(index.Projects))
	for name, indexEntry := range index.Projects {
		entry := Entry{Name: name, Path: indexEntry.Path}
		entry.ID, entry.Status, entry.Created, entry.LastStarted = workspaceHandle(name, indexEntry.Path)
		if project, err := state.OpenStore(indexEntry.Path).LoadProject(); err == nil {
			entry.OS = project.OS
		}
		if projectConfig, err := config.LoadProjectConfig(indexEntry.Path); err == nil {
			entry.Agents = projectConfig.Agent.Tools
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// workspaceHandle reports a project's workspace microVM id and lifecycle handle
// details: the id is deterministic (aip-<name>) even before a first start; the
// status is "none" and the timestamps empty when no handle exists yet. An
// unreadable store is reported as "none" rather than failing the whole listing.
func workspaceHandle(name, root string) (id, status, created, lastStarted string) {
	id = workspace.Name(name)
	status = "none"
	workspaces, err := state.OpenStore(root).ListWorkspaces()
	if err != nil {
		return id, status, "", ""
	}
	for index := range workspaces {
		if workspaces[index].ID == id {
			return id, string(workspaces[index].Status), workspaces[index].Created, workspaces[index].LastStarted
		}
	}
	return id, status, "", ""
}

// agentArtifactDirs are the per-CLI agent config dirs and the Python venv the platform
// writes into the PROJECT folder (outside .ai-platform) at workspace start. A plain
// `ai delete` removes them too, best-effort — a missing one is ignored.
var agentArtifactDirs = []string{".opencode", ".claude", ".codex", ".omp", ".gemini", ".hermes", ".venv-msb"}

// Delete removes a project from the index and removes its persistent overlay. A plain
// delete removes the whole .ai-platform tree (config + run state) and — when
// removeAgentDirs is set — the per-CLI agent config folders + venv; the user's OTHER
// files are kept. With purge it removes the ENTIRE project directory. Destroying the
// workspace microVM is layered on by the CLI caller (workspace.DestroyIfPresent)
// before this runs, so a delete never leaves a running microVM orphaned.
func Delete(name string, purge, removeAgentDirs bool) error {
	index, err := state.LoadIndex()
	if err != nil {
		return err
	}
	entry, ok := index.Projects[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProject, name)
	}

	// The per-CLI agent config dirs + the Python venv the platform writes into the
	// PROJECT folder at workspace start (outside .ai-platform). On a non-purge delete the
	// caller chooses whether to remove them; when KEPT, their symlinks into the shared
	// .ai-platform pool must be MATERIALIZED first (the pool is about to be removed).
	var removeErr error
	if !purge {
		if removeAgentDirs {
			for _, dir := range agentArtifactDirs {
				if err := forceRemoveAll(filepath.Join(entry.Path, dir)); err != nil && removeErr == nil {
					removeErr = err
				}
			}
		} else {
			for _, dir := range agentArtifactDirs {
				if err := materializeSymlinks(filepath.Join(entry.Path, dir)); err != nil && removeErr == nil {
					removeErr = err
				}
			}
		}
	}

	// Choose the removal target: --purge removes the ENTIRE project directory (the
	// user's files too); a normal delete de-platforms the directory by removing only the
	// .ai-platform tree (config + run state — the platform's footprint), keeping the
	// user's OTHER files. (Symlinks into it were materialized above if the agent dirs are
	// kept, so nothing dangles.)
	target := filepath.Join(entry.Path, ".ai-platform")
	if purge {
		target = entry.Path
	}
	if err := forceRemoveAll(target); err != nil && removeErr == nil {
		removeErr = err
	}

	// Permanent removal: drop the project's persistent overlay (arch §26).
	// Unlike `ai destroy`, deleting the project removes the overlay. Best-effort
	// — an overlay-removal failure must NOT keep the project registered (and the
	// .ai-platform dir is already gone), so we record it but still de-register.
	if err := overlay.Remove(workspace.Name(name)); err != nil && removeErr == nil {
		removeErr = err
	}

	// Always de-register the project, even if a removal above failed, so a
	// half-removed workspace never lingers in `ai list` pointing at a gone dir.
	delete(index.Projects, name)
	if err := state.SaveIndex(index); err != nil {
		return err
	}
	return removeErr
}

// forceRemoveAll removes path, retrying once after clearing restrictive
// permissions if the first attempt is blocked by a read-only/owned file
// somewhere in the tree — so `ai delete` reliably removes the whole footprint.
func forceRemoveAll(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	// Best-effort widen permissions on everything under path, then retry.
	_ = filepath.Walk(path, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil {
			return nil //nolint:nilerr // keep walking; the retry surfaces real failures
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		}
		_ = os.Chmod(name, mode)
		return nil
	})
	return os.RemoveAll(path)
}

// materializeSymlinks replaces every direct SYMLINK child of dir with a real copy of
// its target's contents, so a KEPT per-CLI dir survives the removal of the shared
// .ai-platform pool its symlinks point into (the original is deleted). A missing dir is
// a no-op; a dangling/unresolvable symlink is simply dropped (nothing to preserve).
func materializeSymlinks(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		linkPath := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(linkPath)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue // not a symlink — leave it as-is
		}
		realTarget, err := filepath.EvalSymlinks(linkPath)
		if err != nil {
			_ = os.Remove(linkPath) // dangling — drop it
			continue
		}
		if err := os.Remove(linkPath); err != nil {
			return err
		}
		if err := copyTree(realTarget, linkPath); err != nil {
			return err
		}
	}
	return nil
}

// copyTree recursively copies the file or directory at src to dst, preserving file
// permission bits — used to materialize a symlinked directory into real files.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyTree(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}
