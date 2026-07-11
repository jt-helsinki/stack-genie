package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// writeState and readState delegate to the shared conffile package (atomic YAML
// writes, strict reads that reject unknown fields).
func writeState(path string, value any) error { return conffile.WriteAtomic(path, value) }
func readState(path string, value any) error  { return conffile.Read(path, value) }

func checkVersion(got int, path string) error {
	if got != SchemaVersion {
		return fmt.Errorf("%s: unsupported schema_version %d (want %d)", path, got, SchemaVersion)
	}
	return nil
}

// --- projects index (global) ------------------------------------------------

// IndexPath returns config/projects.yaml.
func IndexPath() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "projects.yaml"), nil
}

// LoadIndex reads the global projects index, returning an empty index if the
// file does not exist yet (a fresh install is not an error).
func LoadIndex() (*ProjectsIndex, error) {
	indexPath, err := IndexPath()
	if err != nil {
		return nil, err
	}
	var index ProjectsIndex
	if err := readState(indexPath, &index); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewProjectsIndex(), nil
		}
		return nil, err
	}
	if err := checkVersion(index.SchemaVersion, indexPath); err != nil {
		return nil, err
	}
	if index.Projects == nil {
		index.Projects = map[string]ProjectIndexEntry{}
	}
	return &index, nil
}

// SaveIndex atomically writes the global projects index.
func SaveIndex(index *ProjectsIndex) error {
	indexPath, err := IndexPath()
	if err != nil {
		return err
	}
	if index.SchemaVersion == 0 {
		index.SchemaVersion = SchemaVersion
	}
	return writeState(indexPath, index)
}

// --- project discovery ------------------------------------------------------

// FindProjectRoot walks up from start looking for a directory that contains
// .ai-platform/project.yaml, returning that directory.
func FindProjectRoot(start string) (string, bool) {
	dir := start
	for {
		if fileExists(filepath.Join(dir, ".ai-platform", "project.yaml")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- per-project store ------------------------------------------------------

// Store reads and writes one project's state under <root>/.ai-platform.
type Store struct {
	root     string // project root (dir containing .ai-platform)
	platform string // <root>/.ai-platform
}

// OpenStore returns a Store for the project rooted at root.
func OpenStore(root string) *Store {
	return &Store{root: root, platform: filepath.Join(root, ".ai-platform")}
}

// Root returns the project root directory.
func (store *Store) Root() string { return store.root }

func (store *Store) projectFile() string   { return filepath.Join(store.platform, "project.yaml") }
func (store *Store) runDir() string        { return filepath.Join(store.platform, "run") }
func (store *Store) workspacesDir() string { return filepath.Join(store.runDir(), "workspaces") }

// EnsureRunDirs creates the gitignored run/ subdirectories.
func (store *Store) EnsureRunDirs() error {
	return os.MkdirAll(store.workspacesDir(), 0o755)
}

// LoadProject reads project.yaml (tracked, §12.1).
func (store *Store) LoadProject() (*Project, error) {
	var project Project
	if err := readState(store.projectFile(), &project); err != nil {
		return nil, err
	}
	if err := checkVersion(project.SchemaVersion, store.projectFile()); err != nil {
		return nil, err
	}
	return &project, nil
}

// SaveProject atomically writes project.yaml.
func (store *Store) SaveProject(project *Project) error {
	if project.SchemaVersion == 0 {
		project.SchemaVersion = SchemaVersion
	}
	return writeState(store.projectFile(), project)
}

// SaveWorkspace atomically writes run/workspaces/<id>.yaml.
func (store *Store) SaveWorkspace(workspace *Workspace) error {
	if workspace.SchemaVersion == 0 {
		workspace.SchemaVersion = SchemaVersion
	}
	return writeState(filepath.Join(store.workspacesDir(), workspace.ID+".yaml"), workspace)
}

// ListWorkspaces returns all workspace handles (empty if none).
func (store *Store) ListWorkspaces() ([]Workspace, error) {
	var workspaces []Workspace
	err := eachWorkspaceFile(store.workspacesDir(), func(path string) error {
		var workspace Workspace
		if err := readState(path, &workspace); err != nil {
			return err
		}
		if err := checkVersion(workspace.SchemaVersion, path); err != nil {
			return err
		}
		workspaces = append(workspaces, workspace)
		return nil
	})
	return workspaces, err
}

// eachWorkspaceFile calls visit for every *.yaml file in dir, sorted by name. A
// missing dir is treated as empty.
func eachWorkspaceFile(dir string, visit func(path string) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".yaml" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
