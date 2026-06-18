// Package project implements project scaffolding and teardown for
// `ai project create|delete|list` (CLI §3). Create's interactive wizard lives in
// the CLI layer; this package owns the deterministic work: validate the choices,
// write the project's tracked .ai-platform/ files (Dockerfile via envimage,
// config.yaml, profile.yaml, project.json, .gitignore) and the global index.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/envimage"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"gopkg.in/yaml.v3"
)

// namePattern validates project (and agent) names (arch §19): lowercase
// alphanumeric and hyphens, 2–40 chars, no leading/trailing hyphen.
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
	Agents      int
	Clone       string
}

// ValidateName reports whether a name is well-formed (arch §19).
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: %q (lowercase alphanumeric + hyphens, 2-40 chars, no leading/trailing hyphen)", ErrInvalidName, name)
	}
	return nil
}

// RootPath returns the host source path for a project (~/projects/<name>).
func RootPath(name string) (string, error) {
	projectsDir, err := paths.ProjectsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(projectsDir, name), nil
}

// profileFile is <project>/.ai-platform/profile.yaml (repo-layout §12.1a).
type profileFile struct {
	SchemaVersion int      `yaml:"schema_version"`
	Stacks        []string `yaml:"stacks"`
}

// EnsureCreatable validates the name and confirms no project with that name is
// already registered or already on disk (so the caller can clone/init the dir
// before scaffolding).
func EnsureCreatable(name string) error {
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
	root, err := RootPath(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "project.json")); err == nil {
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
// the global index. It tolerates an existing directory (a freshly cloned or
// git-init'd dir) but refuses to overwrite an existing project. It does NOT run
// git or start a workspace — those are layered on by the caller.
func Scaffold(spec Spec, createdAt string) (string, error) {
	if err := EnsureCreatable(spec.Name); err != nil {
		return "", err
	}
	root, err := RootPath(spec.Name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(root, ".ai-platform"), 0o755); err != nil {
		return "", err
	}
	index, err := state.LoadIndex()
	if err != nil {
		return "", err
	}

	// Dockerfile = OS base + selected stacks + selected agent CLIs (§25, §12).
	if err := envimage.Write(root, spec.OS, spec.Stacks, spec.AgentCLIs); err != nil {
		return "", err
	}

	// config.yaml — project config with the installed agent CLIs + default.
	projectConfig := &config.Config{
		OS:    spec.OS,
		Agent: config.AgentConfig{Tools: spec.AgentCLIs, DefaultTool: spec.DefaultTool},
	}
	if err := config.WriteProject(root, projectConfig); err != nil {
		return "", err
	}

	// profile.yaml — software stacks (§12.1a).
	if err := writeProfile(root, spec.Stacks); err != nil {
		return "", err
	}

	// project.json (tracked).
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
	encoded, err := yaml.Marshal(profileFile{SchemaVersion: 1, Stacks: stacks})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, ".ai-platform", "profile.yaml"), encoded, 0o644)
}

// Entry is one row of `ai project list`.
type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	OS   string `json:"os,omitempty"`
}

// List returns the registered projects (from the global index, enriched with the
// OS from each project.json when readable).
func List() ([]Entry, error) {
	index, err := state.LoadIndex()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(index.Projects))
	for name, indexEntry := range index.Projects {
		entry := Entry{Name: name, Path: indexEntry.Path}
		if project, err := state.OpenStore(indexEntry.Path).LoadProject(); err == nil {
			entry.OS = project.OS
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Delete removes a project from the index and clears its host-local run/ state.
// With purge it also removes the host source tree. Host source is otherwise
// preserved (CLI §3.4). Workspace/overlay teardown is layered on by the caller.
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

	delete(index.Projects, name)
	return state.SaveIndex(index)
}
