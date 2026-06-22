// Package state owns the platform's on-disk state: per-project sharded runtime
// handles under <project>/.ai-platform/run/ and the global projects index
// (repo-layout spec §12). All writes are atomic (temp file + rename) so a crash
// mid-write never corrupts state, and all reads reject unknown fields.
package state

// SchemaVersion is the current version stamped on every state file (§12).
const SchemaVersion = 1

// Project is <project>/.ai-platform/project.yaml (tracked, §12.1). The selected
// software stacks live in profile.yaml (§12.1a), not here.
type Project struct {
	SchemaVersion int    `yaml:"schema_version"`
	Name          string `yaml:"name"`
	OS            string `yaml:"os"`
	Created       string `yaml:"created"`
}

// WorkspaceStatus is the lifecycle state of a workspace (§12.2, arch §7).
type WorkspaceStatus string

const (
	StatusCreated   WorkspaceStatus = "created"
	StatusStarted   WorkspaceStatus = "started"
	StatusStopped   WorkspaceStatus = "stopped"
	StatusArchived  WorkspaceStatus = "archived"
	StatusDestroyed WorkspaceStatus = "destroyed"
)

// Workspace is run/workspaces/<id>.yaml (gitignored, §12.2). There is one
// workspace per project; multi-agent work happens inside it and is the
// in-workspace agent CLI's concern, not the platform's. It carries both yaml and
// json tags: yaml for the on-disk handle, json for the --json output envelope
// (`ai workspace list` emits it directly).
type Workspace struct {
	SchemaVersion  int             `json:"schema_version" yaml:"schema_version"`
	ID             string          `json:"id" yaml:"id"`
	Project        string          `json:"project" yaml:"project"`
	MicrosandboxID string          `json:"microsandbox_id" yaml:"microsandbox_id"`
	Status         WorkspaceStatus `json:"status" yaml:"status"`
	Created        string          `json:"created" yaml:"created"`
	LastStarted    string          `json:"last_started" yaml:"last_started"`
}

// ProjectsIndex is config/projects.yaml (global, §12.7): name → path.
type ProjectsIndex struct {
	SchemaVersion int                          `yaml:"schema_version"`
	Projects      map[string]ProjectIndexEntry `yaml:"projects"`
}

// ProjectIndexEntry maps a project to its absolute path on this host.
type ProjectIndexEntry struct {
	Path string `yaml:"path"`
}

// NewProjectsIndex returns an empty, schema-stamped index.
func NewProjectsIndex() *ProjectsIndex {
	return &ProjectsIndex{SchemaVersion: SchemaVersion, Projects: map[string]ProjectIndexEntry{}}
}
