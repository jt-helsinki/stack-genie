package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// workspacesResult.Human() renders the table headers and a sample row.
func TestWorkspacesResultHuman(test *testing.T) {
	result := workspacesResult{Workspaces: []state.Workspace{
		{ID: "ws-1", Project: "alpha", Status: state.WorkspaceStatus("running"), Created: "2026-06-20T00:00:00Z", LastStarted: "2026-06-21T00:00:00Z"},
	}}
	rendered := result.Human()
	for _, want := range []string{"PROJECT", "ID", "STATUS", "CREATED", "LAST-STARTED", "ws-1", "alpha", "running", "2026-06-20T00:00:00Z"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("workspace list table missing %q:\n%s", want, rendered)
		}
	}
}

// The friendly empty case is shown when no workspaces exist.
func TestWorkspacesResultHumanEmpty(test *testing.T) {
	rendered := workspacesResult{}.Human()
	if !strings.Contains(rendered, "No workspaces yet") || !strings.Contains(rendered, "ai workspace start") {
		test.Errorf("empty workspace list hint missing:\n%s", rendered)
	}
}

// The JSON envelope still nests the entries under the `workspaces` key.
func TestWorkspacesResultJSONShape(test *testing.T) {
	out := &bytes.Buffer{}
	emitter := &output.Emitter{JSON: true, Out: out, Err: &bytes.Buffer{}}
	emitter.Success("workspace.list", workspacesResult{Workspaces: []state.Workspace{{ID: "ws-1", Project: "alpha"}}})

	var envelope struct {
		Data struct {
			Workspaces []struct {
				ID string `json:"id"`
			} `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		test.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(envelope.Data.Workspaces) != 1 || envelope.Data.Workspaces[0].ID != "ws-1" {
		test.Fatalf("data.workspaces shape changed: %+v", envelope.Data.Workspaces)
	}
}
