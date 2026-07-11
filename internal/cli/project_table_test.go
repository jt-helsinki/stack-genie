package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

// projectsResult.Human() renders the table headers and a sample row.
func TestProjectsResultHuman(test *testing.T) {
	result := projectsResult{Projects: []project.Entry{
		{Name: "alpha", OS: "debian-trixie", Agents: []string{"opencode", "pi"}, Status: "running",
			ID: "aip-alpha", Created: "2026-06-20T00:00:00Z", LastStarted: "2026-06-21T00:00:00Z"},
		{Name: "beta", Status: "none", ID: "aip-beta"},
	}}
	rendered := result.Human()
	for _, want := range []string{
		"NAME", "OS", "AGENTS", "STATUS", "ID", "CREATED", "LAST-STARTED",
		"alpha", "debian-trixie", "opencode, pi", "running", "aip-alpha", "2026-06-20T00:00:00Z", "2026-06-21T00:00:00Z",
		"beta", "aip-beta",
	} {
		if !strings.Contains(rendered, want) {
			test.Errorf("project list table missing %q:\n%s", want, rendered)
		}
	}
	// Empty OS/agents render as an em dash.
	if !strings.Contains(rendered, "—") {
		test.Errorf("empty cell should render an em dash:\n%s", rendered)
	}
}

// The friendly empty case is shown when no workspaces exist.
func TestProjectsResultHumanEmpty(test *testing.T) {
	rendered := projectsResult{}.Human()
	if !strings.Contains(rendered, "No workspaces yet") || !strings.Contains(rendered, "ai create") {
		test.Errorf("empty workspace list hint missing:\n%s", rendered)
	}
}

// The JSON envelope still nests the entries under the `projects` key.
func TestProjectsResultJSONShape(test *testing.T) {
	out := &bytes.Buffer{}
	emitter := &output.Emitter{JSON: true, Out: out, Err: &bytes.Buffer{}}
	emitter.Success("project.list", projectsResult{Projects: []project.Entry{{Name: "alpha", Status: "none"}}})

	var envelope struct {
		Data struct {
			Projects []struct {
				Name string `json:"name"`
			} `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		test.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(envelope.Data.Projects) != 1 || envelope.Data.Projects[0].Name != "alpha" {
		test.Fatalf("data.projects shape changed: %+v", envelope.Data.Projects)
	}
}
