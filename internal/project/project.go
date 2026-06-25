// Package project implements project scaffolding and teardown for
// `ai project create|delete|list` (CLI §3). Create's interactive wizard lives in
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

	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/envimage"
	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/templates"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
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
	// Apps are the opt-in in-VM AI applications to install (subset of apps.Keys()).
	// Empty by default — apps are opt-in.
	Apps []string
	// Root is the host source directory for the project — the directory `ai
	// project create` runs in. Empty falls back to ~/projects/<name> (RootPath).
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
// `ai project create` creates the project in the current directory and sets
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
	return nil
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
	// Dockerfile = OS base + selected stacks + selected agent CLIs (§25, §12).
	if err := envimage.Write(root, spec.OS, spec.Stacks, spec.AgentCLIs); err != nil {
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
	appEntries, err := apps.AllocateEntries(spec.Apps, reserved, nil)
	if err != nil {
		return "", err
	}
	projectConfig := &config.Config{
		OS:    spec.OS,
		Agent: config.AgentConfig{Tools: spec.AgentCLIs, DefaultTool: spec.DefaultTool},
		Apps:  appEntries,
	}
	if err := config.WriteProject(root, projectConfig); err != nil {
		return "", err
	}

	// profile.yaml — software stacks (§12.1a).
	if err := writeProfile(root, spec.Stacks); err != nil {
		return "", err
	}

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
// status, so `ai project list` reports the full row the spec promises (§3.3).
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

// Delete removes a project from the index, clears its host-local run/ state, and
// removes the project's persistent overlay. With purge it also removes the host
// source tree; host source is otherwise preserved (CLI §3.4). Destroying the
// workspace microVM is layered on by the CLI caller (workspace.DestroyIfPresent)
// before this runs, so a delete never leaves a running microVM orphaned.
func Delete(name string, purge bool) error {
	index, err := state.LoadIndex()
	if err != nil {
		return err
	}
	entry, ok := index.Projects[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProject, name)
	}

	if purge {
		if err := os.RemoveAll(entry.Path); err != nil {
			return err
		}
	} else {
		// Keep tracked source; clear only the gitignored run/ state.
		if err := os.RemoveAll(filepath.Join(entry.Path, ".ai-platform", "run")); err != nil {
			return err
		}
	}

	// Permanent removal: drop the project's persistent overlay (arch §26).
	// Unlike `ai workspace destroy`, deleting the project removes the overlay.
	if err := overlay.Remove(workspace.Name(name)); err != nil {
		return err
	}

	delete(index.Projects, name)
	return state.SaveIndex(index)
}
