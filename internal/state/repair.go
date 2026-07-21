package state

import "path/filepath"

// Snapshot is the state view rendered by `ai state show` (CLI §14.1).
type Snapshot struct {
	Projects map[string]ProjectIndexEntry `json:"projects"`
	Project  *ProjectState                `json:"project"` // nil when cwd is not inside a project
}

// ProjectState is the per-project portion of a Snapshot.
type ProjectState struct {
	Root       string      `json:"root"`
	Project    Project     `json:"project"`
	Workspaces []Workspace `json:"workspaces"`
}

// Show builds a state snapshot: the global projects index plus, if cwd is inside
// a known project, that project's runtime state.
func Show(cwd string) (*Snapshot, error) {
	idx, err := LoadIndex()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Projects: idx.Projects}

	root, ok := FindProjectRoot(cwd)
	if !ok {
		return snap, nil
	}
	st := OpenStore(root)
	proj, err := st.LoadProject()
	if err != nil {
		return nil, err
	}
	ws, err := st.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	snap.Project = &ProjectState{Root: root, Project: *proj, Workspaces: ws}
	return snap, nil
}

// RepairReport summarizes what `ai state repair` changed (CLI §14.2).
type RepairReport struct {
	IndexCreated    bool     `json:"index_created"`
	RemovedProjects []string `json:"removed_projects"`
	RunDirsEnsured  []string `json:"run_dirs_ensured"`
}

// Repair reconciles host-local state from the filesystem (arch §23, CLI §14.2):
// it drops projects-index entries whose project no longer exists on disk and
// ensures the run/ shard directories exist for the ones that remain. (Git and
// Microsandbox reconciliation of individual workspace handles arrives with the
// slices that introduce them.)
func Repair() (*RepairReport, error) {
	rep := &RepairReport{RemovedProjects: []string{}, RunDirsEnsured: []string{}}

	indexPath, err := IndexPath()
	if err != nil {
		return nil, err
	}
	rep.IndexCreated = !fileExists(indexPath)

	idx, err := LoadIndex()
	if err != nil {
		return nil, err
	}

	changed := rep.IndexCreated
	for name, entry := range idx.Projects {
		if !fileExists(filepath.Join(entry.Path, ".ai-platform", "project.yaml")) {
			delete(idx.Projects, name)
			rep.RemovedProjects = append(rep.RemovedProjects, name)
			changed = true
			continue
		}
		if err := OpenStore(entry.Path).EnsureRunDirs(); err != nil {
			return nil, err
		}
		rep.RunDirsEnsured = append(rep.RunDirsEnsured, name)
	}

	if changed {
		if err := SaveIndex(idx); err != nil {
			return nil, err
		}
	}
	return rep, nil
}
