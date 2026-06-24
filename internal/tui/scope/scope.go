// Package scope resolves which project (if any) the `ai ui` TUI operates on.
//
// The UI is a global project switcher: it lists every project from the global
// index (~/.ai-platform/config/projects.yaml) and lets the user select any one
// as the current project. The current directory only decides the DEFAULT
// selection and whether to offer creating a new project:
//
//   - a project at the cwd or any ANCESTOR (state.FindProjectRoot) is the default
//     selection;
//   - when neither the cwd nor any ancestor is a project, the UI offers to create
//     one — in the cwd or a directory the user chooses — and otherwise falls back
//     to the server (services) scope.
//
// A new project may be created anywhere EXCEPT a directory that is already a
// project root; a child or parent of an existing project directory is allowed.
package scope

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// Kind is whether the UI is scoped to a project or to the server (services) view.
type Kind int

const (
	// Server shows the service tier + container status (no project selected).
	Server Kind = iota
	// Project shows a selected project and its workspace.
	Project
)

// ProjectRef is one selectable project from the global index.
type ProjectRef struct {
	Name string
	Path string
}

// Resolution is the startup scope decision for the UI.
type Resolution struct {
	// Projects is every project in the global index, sorted by name — the full
	// switcher list the user can pick from at any time.
	Projects []ProjectRef
	// DefaultProject is the project at the cwd or an ancestor (the pre-selected
	// entry), or "" when the cwd is not inside a project.
	DefaultProject string
	// OfferCreate is true when neither the cwd nor any ancestor is a project, so
	// the UI should surface the "create a new project" option.
	OfferCreate bool
}

// Resolve computes the startup scope from the current directory. It never fails
// on a missing index (a fresh host simply has no projects yet).
func Resolve(cwd string) (Resolution, error) {
	index, err := state.LoadIndex()
	if err != nil {
		return Resolution{}, fmt.Errorf("read projects index: %w", err)
	}
	projects := make([]ProjectRef, 0, len(index.Projects))
	for name, entry := range index.Projects {
		projects = append(projects, ProjectRef{Name: name, Path: entry.Path})
	}
	sort.Slice(projects, func(first, second int) bool { return projects[first].Name < projects[second].Name })

	resolution := Resolution{Projects: projects}
	if root, inProject := state.FindProjectRoot(cwd); inProject {
		// The default selection is the indexed project whose path matches the
		// detected root; fall back to the directory's base name if it is not
		// indexed (so the user still sees which project they are in).
		resolution.DefaultProject = projectNameForRoot(projects, root)
	} else {
		resolution.OfferCreate = true
	}
	return resolution, nil
}

// projectNameForRoot returns the indexed project name whose path is root, or the
// base name of root when it is not in the index.
func projectNameForRoot(projects []ProjectRef, root string) string {
	for _, project := range projects {
		if sameDir(project.Path, root) {
			return project.Name
		}
	}
	return filepath.Base(root)
}

// IsProjectRoot reports whether dir is itself a project root (it directly
// contains .ai-platform/project.yaml) — distinct from being inside one.
func IsProjectRoot(dir string) bool {
	root, ok := state.FindProjectRoot(dir)
	return ok && sameDir(root, dir)
}

// ValidateCreateTarget checks that a new project may be created in dir: the
// directory must exist (or be creatable) and must not ALREADY be a project root.
// A child or parent of an existing project directory is allowed.
func ValidateCreateTarget(dir string) error {
	if dir == "" {
		return fmt.Errorf("choose a directory for the new workspace")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", dir, err)
	}
	if info, statErr := os.Stat(absolute); statErr == nil && !info.IsDir() {
		return fmt.Errorf("%q is not a directory", absolute)
	}
	if IsProjectRoot(absolute) {
		return fmt.Errorf("%q is already a workspace — select it instead of creating a new one", absolute)
	}
	return nil
}

// sameDir compares two paths for directory equality, tolerating trailing
// separators and relative/clean differences.
func sameDir(first, second string) bool {
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	if firstErr != nil || secondErr != nil {
		return filepath.Clean(first) == filepath.Clean(second)
	}
	return firstAbs == secondAbs
}
