package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jt-helsinki/ideal-robot/internal/jsonfile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// writeJSONAtomic and readJSON delegate to the shared jsonfile package.
func writeJSONAtomic(path string, value any) error { return jsonfile.WriteAtomic(path, value) }
func readJSON(path string, value any) error        { return jsonfile.Read(path, value) }

func checkVersion(got int, path string) error {
	if got != SchemaVersion {
		return fmt.Errorf("%s: unsupported schema_version %d (want %d)", path, got, SchemaVersion)
	}
	return nil
}

// --- projects index (global) ------------------------------------------------

// IndexPath returns config/projects.json.
func IndexPath() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "projects.json"), nil
}

// LoadIndex reads the global projects index, returning an empty index if the
// file does not exist yet (a fresh install is not an error).
func LoadIndex() (*ProjectsIndex, error) {
	indexPath, err := IndexPath()
	if err != nil {
		return nil, err
	}
	var index ProjectsIndex
	if err := readJSON(indexPath, &index); err != nil {
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
	return writeJSONAtomic(indexPath, index)
}

// --- project discovery ------------------------------------------------------

// FindProjectRoot walks up from start looking for a directory that contains
// .ai-platform/project.json, returning that directory.
func FindProjectRoot(start string) (string, bool) {
	dir := start
	for {
		if fileExists(filepath.Join(dir, ".ai-platform", "project.json")) {
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

func (store *Store) projectFile() string   { return filepath.Join(store.platform, "project.json") }
func (store *Store) runDir() string        { return filepath.Join(store.platform, "run") }
func (store *Store) workspacesDir() string { return filepath.Join(store.runDir(), "workspaces") }
func (store *Store) agentsDir() string     { return filepath.Join(store.runDir(), "agents") }

// EnsureRunDirs creates the gitignored run/ subdirectories.
func (store *Store) EnsureRunDirs() error {
	for _, dir := range []string{store.workspacesDir(), store.agentsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// LoadProject reads project.json (tracked, §12.1).
func (store *Store) LoadProject() (*Project, error) {
	var project Project
	if err := readJSON(store.projectFile(), &project); err != nil {
		return nil, err
	}
	if err := checkVersion(project.SchemaVersion, store.projectFile()); err != nil {
		return nil, err
	}
	return &project, nil
}

// SaveProject atomically writes project.json.
func (store *Store) SaveProject(project *Project) error {
	if project.SchemaVersion == 0 {
		project.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(store.projectFile(), project)
}

// SaveWorkspace atomically writes run/workspaces/<id>.json.
func (store *Store) SaveWorkspace(workspace *Workspace) error {
	if workspace.SchemaVersion == 0 {
		workspace.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(filepath.Join(store.workspacesDir(), workspace.ID+".json"), workspace)
}

// SaveAgent atomically writes run/agents/<name>.json.
func (store *Store) SaveAgent(agent *Agent) error {
	if agent.SchemaVersion == 0 {
		agent.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(filepath.Join(store.agentsDir(), agent.Name+".json"), agent)
}

// ListWorkspaces returns all workspace handles (empty if none).
func (store *Store) ListWorkspaces() ([]Workspace, error) {
	var workspaces []Workspace
	err := eachJSON(store.workspacesDir(), func(path string) error {
		var workspace Workspace
		if err := readJSON(path, &workspace); err != nil {
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

// ListAgents returns all agent handles (empty if none).
func (store *Store) ListAgents() ([]Agent, error) {
	var agents []Agent
	err := eachJSON(store.agentsDir(), func(path string) error {
		var agent Agent
		if err := readJSON(path, &agent); err != nil {
			return err
		}
		if err := checkVersion(agent.SchemaVersion, path); err != nil {
			return err
		}
		agents = append(agents, agent)
		return nil
	})
	return agents, err
}

// eachJSON calls visit for every *.json file in dir, sorted by name. A missing
// dir is treated as empty.
func eachJSON(dir string, visit func(path string) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
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
