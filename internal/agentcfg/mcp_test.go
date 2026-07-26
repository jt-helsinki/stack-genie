package agentcfg

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEnabledMCPServers(test *testing.T) {
	servers := EnabledMCPServers(true, true, true, "/home/workspace/project/.venv-msb/bin/python")
	if len(servers) != 3 {
		test.Fatalf("want 3 servers, got %d: %+v", len(servers), servers)
	}
	byName := map[string]MCPServer{}
	for _, server := range servers {
		byName[server.Name] = server
	}
	if got := byName["code-review-graph"]; got.Command != "code-review-graph" || strings.Join(got.Args, " ") != "serve" {
		test.Errorf("code-review-graph server = %+v", got)
	}
	if got := byName["codebase-memory-mcp"]; got.Command != "codebase-memory-mcp" || len(got.Args) != 0 {
		test.Errorf("codebase-memory server = %+v", got)
	}
	graphify := byName["graphify"]
	if graphify.Command != "/home/workspace/project/.venv-msb/bin/python" ||
		strings.Join(graphify.Args, " ") != "-m graphify.serve graphify-out/graph.json" {
		test.Errorf("graphify server = %+v", graphify)
	}
}

func TestEnabledMCPServersOmitsDisabledAndGraphifyWithoutVenv(test *testing.T) {
	// Nothing enabled → empty.
	if servers := EnabledMCPServers(false, false, false, "/x/python"); len(servers) != 0 {
		test.Errorf("want no servers, got %+v", servers)
	}
	// graphify enabled but no venv path → graphify omitted (can't locate the server).
	servers := EnabledMCPServers(true, false, true, "")
	if len(servers) != 1 || servers[0].Name != "code-review-graph" {
		test.Errorf("graphify must be omitted without a venv python: %+v", servers)
	}
}

func TestOmpMcpConfig(test *testing.T) {
	servers := EnabledMCPServers(true, false, false, "")
	raw, err := OmpMcpConfig(servers)
	if err != nil {
		test.Fatalf("OmpMcpConfig: %v", err)
	}
	var document struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatalf("unmarshal omp mcp.json: %v\n%s", err, raw)
	}
	entry, ok := document.MCPServers["code-review-graph"]
	if !ok || entry.Command != "code-review-graph" || strings.Join(entry.Args, " ") != "serve" {
		test.Errorf("omp mcpServers = %+v", document.MCPServers)
	}
}

func TestInjectHermesMCP(test *testing.T) {
	base, err := HermesConfig("http://gw/v1", "", "")
	if err != nil {
		test.Fatalf("HermesConfig: %v", err)
	}
	if same, _ := InjectHermesMCP(base, nil); string(same) != string(base) {
		test.Error("empty servers must return the config unchanged")
	}
	out, err := InjectHermesMCP(base, EnabledMCPServers(true, false, false, ""))
	if err != nil {
		test.Fatalf("InjectHermesMCP: %v", err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(out, &document); err != nil {
		test.Fatalf("unmarshal hermes config: %v\n%s", err, out)
	}
	servers, ok := document["mcp_servers"].(map[string]any)
	if !ok || servers["code-review-graph"] == nil {
		test.Errorf("hermes mcp_servers = %v", document["mcp_servers"])
	}
	if document["providers"] == nil {
		test.Error("hermes provider block was lost during MCP injection")
	}
}

func TestAppendCodexMCP(test *testing.T) {
	base := CodexConfig("http://gw/v1", "")
	if same := AppendCodexMCP(base, nil); string(same) != string(base) {
		test.Error("empty servers must return the config unchanged")
	}
	out := string(AppendCodexMCP(base, EnabledMCPServers(true, true, false, "")))
	for _, want := range []string{
		"[mcp_servers.code-review-graph]",
		`command = "code-review-graph"`,
		`args = ["serve"]`,
		"[mcp_servers.codebase-memory-mcp]",
		`command = "codebase-memory-mcp"`,
	} {
		if !strings.Contains(out, want) {
			test.Errorf("codex config missing %q:\n%s", want, out)
		}
	}
	// The original gateway provider block must still be present (append, not replace).
	if !strings.Contains(out, "[model_providers.aip-gateway]") {
		test.Errorf("codex gateway provider block was lost:\n%s", out)
	}
}
