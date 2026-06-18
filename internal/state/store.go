package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// --- atomic JSON IO ---------------------------------------------------------

// writeJSONAtomic writes v as indented JSON to path by writing a temp file in
// the same directory and renaming over the target, so a crash mid-write never
// leaves a partial file (repo-layout §12 / arch §23).
func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// readJSON decodes path into v, rejecting unknown fields (§12).
func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func checkVersion(got int, path string) error {
	if got != SchemaVersion {
		return fmt.Errorf("%s: unsupported schema_version %d (want %d)", path, got, SchemaVersion)
	}
	return nil
}

// --- projects index (global) ------------------------------------------------

// IndexPath returns config/projects.json.
func IndexPath() (string, error) {
	c, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "projects.json"), nil
}

// LoadIndex reads the global projects index, returning an empty index if the
// file does not exist yet (a fresh install is not an error).
func LoadIndex() (*ProjectsIndex, error) {
	p, err := IndexPath()
	if err != nil {
		return nil, err
	}
	var idx ProjectsIndex
	if err := readJSON(p, &idx); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewProjectsIndex(), nil
		}
		return nil, err
	}
	if err := checkVersion(idx.SchemaVersion, p); err != nil {
		return nil, err
	}
	if idx.Projects == nil {
		idx.Projects = map[string]ProjectIndexEntry{}
	}
	return &idx, nil
}

// SaveIndex atomically writes the global projects index.
func SaveIndex(idx *ProjectsIndex) error {
	p, err := IndexPath()
	if err != nil {
		return err
	}
	if idx.SchemaVersion == 0 {
		idx.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(p, idx)
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
	root  string // project root (dir containing .ai-platform)
	aiDir string
}

// OpenStore returns a Store for the project rooted at root.
func OpenStore(root string) *Store {
	return &Store{root: root, aiDir: filepath.Join(root, ".ai-platform")}
}

// Root returns the project root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) projectFile() string   { return filepath.Join(s.aiDir, "project.json") }
func (s *Store) runDir() string        { return filepath.Join(s.aiDir, "run") }
func (s *Store) workspacesDir() string { return filepath.Join(s.runDir(), "workspaces") }
func (s *Store) agentsDir() string     { return filepath.Join(s.runDir(), "agents") }

// EnsureRunDirs creates the gitignored run/ subdirectories.
func (s *Store) EnsureRunDirs() error {
	for _, d := range []string{s.workspacesDir(), s.agentsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// LoadProject reads project.json (tracked, §12.1).
func (s *Store) LoadProject() (*Project, error) {
	var p Project
	if err := readJSON(s.projectFile(), &p); err != nil {
		return nil, err
	}
	if err := checkVersion(p.SchemaVersion, s.projectFile()); err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveProject atomically writes project.json.
func (s *Store) SaveProject(p *Project) error {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(s.projectFile(), p)
}

// SaveWorkspace atomically writes run/workspaces/<id>.json.
func (s *Store) SaveWorkspace(w *Workspace) error {
	if w.SchemaVersion == 0 {
		w.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(filepath.Join(s.workspacesDir(), w.ID+".json"), w)
}

// SaveAgent atomically writes run/agents/<name>.json.
func (s *Store) SaveAgent(a *Agent) error {
	if a.SchemaVersion == 0 {
		a.SchemaVersion = SchemaVersion
	}
	return writeJSONAtomic(filepath.Join(s.agentsDir(), a.Name+".json"), a)
}

// ListWorkspaces returns all workspace handles (empty if none).
func (s *Store) ListWorkspaces() ([]Workspace, error) {
	var out []Workspace
	err := eachJSON(s.workspacesDir(), func(path string) error {
		var w Workspace
		if err := readJSON(path, &w); err != nil {
			return err
		}
		if err := checkVersion(w.SchemaVersion, path); err != nil {
			return err
		}
		out = append(out, w)
		return nil
	})
	return out, err
}

// ListAgents returns all agent handles (empty if none).
func (s *Store) ListAgents() ([]Agent, error) {
	var out []Agent
	err := eachJSON(s.agentsDir(), func(path string) error {
		var a Agent
		if err := readJSON(path, &a); err != nil {
			return err
		}
		if err := checkVersion(a.SchemaVersion, path); err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

// eachJSON calls fn for every *.json file in dir, sorted by name. A missing
// dir is treated as empty.
func eachJSON(dir string, fn func(path string) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := fn(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}
