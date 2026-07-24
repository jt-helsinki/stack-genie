package create

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/egress"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

// TestAppPortEndToEnd is the integration guard for the in-VM app host-port feature: a
// chosen port must flow create.Execute → project.Scaffold → apps.AllocateEntries →
// persisted config.yaml → apps.PublishedPorts → egress.MsbNetworkArgs (the `-p` publish
// flag that exposes the app's web UI on the host from the microVM). An un-chosen app is
// auto-allocated. This ties together every package in the chain so a regression anywhere
// breaks the test.
func TestAppPortEndToEnd(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := filepath.Join(test.TempDir(), "app-ws")

	spec := project.Spec{
		Name:        "app-ws",
		OS:          SupportedOSes()[0],
		AgentCLIs:   []string{"opencode"},
		DefaultTool: "opencode",
		Root:        root,
		Apps:        []string{"openwebui", "anythingllm"},
		AppPorts:    map[string]int{"openwebui": 21500},
	}
	if _, _, err := Execute(spec, "2026-07-20T00:00:00Z", nil); err != nil {
		test.Fatalf("Execute: %v", err)
	}

	// 1) The chosen port is honored + persisted; the un-chosen app is auto-allocated.
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatalf("LoadProjectConfig: %v", err)
	}
	ports := map[string]int{}
	for _, entry := range projectConfig.Apps {
		ports[entry.Key] = entry.Port
	}
	if ports["openwebui"] != 21500 {
		test.Errorf("openwebui port = %d, want the chosen 21500", ports["openwebui"])
	}
	if ports["anythingllm"] == 0 || ports["anythingllm"] == 21500 {
		test.Errorf("anythingllm port = %d, want an auto-allocated port distinct from 21500", ports["anythingllm"])
	}

	// 2) PublishedPorts exposes each installed app on host==guest.
	published := apps.PublishedPorts(projectConfig)
	if len(published) != 2 {
		test.Fatalf("PublishedPorts = %v, want 2 mappings", published)
	}
	foundOpenWebUI := false
	for _, mapping := range published {
		if mapping.Host != mapping.Guest {
			test.Errorf("published mapping %+v must use host==guest", mapping)
		}
		if mapping.Host == 21500 {
			foundOpenWebUI = true
		}
	}
	if !foundOpenWebUI {
		test.Errorf("published ports %v missing the chosen 21500", published)
	}

	// 3) The published port becomes an msb `-p` publish flag (exposed from the microVM).
	network := config.NetworkConfig{PublishPorts: published}
	args := egress.MsbNetworkArgs(network, "host.microsandbox.internal", 18787)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-p 21500:21500") {
		test.Errorf("msb args must publish the app port (-p 21500:21500), got: %s", joined)
	}
}

// TestHermesDashboardPortEndToEnd is the integration guard for exposing the hermes web
// dashboard on a host port, mirroring the in-VM app chain: a chosen port flows
// create.Execute → project.Scaffold → apps.AllocateDashboardEntries → persisted
// config.yaml (agent_dashboards) → apps.PublishedPorts → egress.MsbNetworkArgs (-p flag).
// Only fires when hermes is a selected agent CLI.
func TestHermesDashboardPortEndToEnd(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := filepath.Join(test.TempDir(), "hermes-ws")

	spec := project.Spec{
		Name:        "hermes-ws",
		OS:          SupportedOSes()[0],
		AgentCLIs:   []string{"opencode", "hermes"},
		DefaultTool: "opencode",
		Root:        root,
		// A high, unusual port (NOT hermes's default 9119) so the real free-port check in
		// AllocateDashboardEntries passes even when a live workspace has the hermes
		// dashboard bound to its default 9119 on this host.
		AppPorts: map[string]int{"hermes": 29119},
	}
	if _, _, err := Execute(spec, "2026-07-21T00:00:00Z", nil); err != nil {
		test.Fatalf("Execute: %v", err)
	}

	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatalf("LoadProjectConfig: %v", err)
	}
	if len(projectConfig.AgentDashboards) != 1 ||
		projectConfig.AgentDashboards[0].Key != "hermes" || projectConfig.AgentDashboards[0].Port != 29119 {
		test.Fatalf("agent_dashboards = %+v, want [hermes:29119]", projectConfig.AgentDashboards)
	}

	network := config.NetworkConfig{PublishPorts: apps.PublishedPorts(projectConfig)}
	joined := strings.Join(egress.MsbNetworkArgs(network, "host.microsandbox.internal", 18787), " ")
	if !strings.Contains(joined, "-p 29119:29119") {
		test.Errorf("msb args must publish the hermes dashboard port (-p 29119:29119), got: %s", joined)
	}
}

// TestAppPortRejectedUnavailableFailsCreate guards that an unavailable requested port fails
// the create (rather than silently ignoring the user's choice).
func TestAppPortRejectedUnavailableFailsCreate(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// Occupy a port so the requested one is not host-free.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		test.Skipf("could not open a probe listener: %v", err)
	}
	defer func() { _ = listener.Close() }()
	busyPort := listener.Addr().(*net.TCPAddr).Port

	spec := project.Spec{
		Name:        "busy-ws",
		OS:          SupportedOSes()[0],
		AgentCLIs:   []string{"opencode"},
		DefaultTool: "opencode",
		Root:        filepath.Join(test.TempDir(), "busy-ws"),
		Apps:        []string{"openwebui"},
		AppPorts:    map[string]int{"openwebui": busyPort},
	}
	if _, _, err := Execute(spec, "2026-07-20T00:00:00Z", nil); err == nil {
		test.Error("create must fail when the requested app port is in use on the host")
	}
	// The half-created project dir must not have persisted a bad app port.
	if _, statErr := os.Stat(config.ProjectPath(spec.Root)); statErr == nil {
		if projectConfig, loadErr := config.LoadProjectConfig(spec.Root); loadErr == nil {
			for _, entry := range projectConfig.Apps {
				if entry.Port == busyPort {
					test.Errorf("a busy app port %d must not be persisted", busyPort)
				}
			}
		}
	}
}
