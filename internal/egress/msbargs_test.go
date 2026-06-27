package egress

import (
	"reflect"
	"slices"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

const (
	testGatewayHost = "host.microsandbox.internal"
	testGatewayPort = 18787
)

// TestHostGatewayTargetMatchesRuntime guards against the egress host-gateway
// target drifting from the single source of truth, runtime.DefaultGatewayHost
// (which runtime.HostGateway pins). hostGatewayTarget derives from it directly,
// so this is a belt-and-braces check that the consolidation holds.
func TestHostGatewayTargetMatchesRuntime(t *testing.T) {
	if hostGatewayTarget != runtime.DefaultGatewayHost {
		t.Fatalf("hostGatewayTarget = %q, want runtime.DefaultGatewayHost %q",
			hostGatewayTarget, runtime.DefaultGatewayHost)
	}
}

// gatewayPrefix is the always-on rule prefix every invocation emits: the
// model-gateway tcp allow followed by the two DNS allow rules (udp/53 + tcp/53 to
// the `host` group). msb filters DNS under default-deny, and the `host` group is
// the only target that re-opens it (see MsbNetworkArgs (1b)).
func gatewayPrefix() []string {
	return []string{
		"--net-rule", "allow:egress@host:tcp:18787",
		"--net-rule", "allow:egress@host:udp:53",
		"--net-rule", "allow:egress@host:tcp:53",
	}
}

func TestMsbNetworkArgsPublicDefault(t *testing.T) {
	// Empty Egress now resolves to "public" (allow-outbound) — a zero NetworkConfig
	// emits the broad public allow rule on top of the default-deny fallthrough and
	// the always-on gateway + DNS allows.
	got := MsbNetworkArgs(config.NetworkConfig{}, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(), "--net-default-egress", "deny", "--net-rule", "allow:egress@public")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public default:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsDenyWithAllowList(t *testing.T) {
	network := config.NetworkConfig{
		Egress: "deny",
		AllowHostServices: []config.HostService{
			{Host: "gateway", Port: 5442}, // gateway token -> host machine
			{Host: "", Port: 6379},        // empty -> host machine
			{Host: "db.internal", Port: 5432},
		},
	}
	got := MsbNetworkArgs(network, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(),
		"--net-default-egress", "deny",
		"--net-rule", "allow:egress@host:tcp:5442",
		"--net-rule", "allow:egress@host:tcp:6379",
		"--net-rule", "allow:egress@db.internal:tcp:5432",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deny+allowlist:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsPublic(t *testing.T) {
	network := config.NetworkConfig{
		Egress:            "public",
		AllowHostServices: []config.HostService{{Host: "gateway", Port: 5442}},
	}
	got := MsbNetworkArgs(network, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(),
		"--net-default-egress", "deny",
		"--net-rule", "allow:egress@public",
		"--net-rule", "allow:egress@host:tcp:5442",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsUnrestricted(t *testing.T) {
	network := config.NetworkConfig{Egress: "unrestricted"}
	got := MsbNetworkArgs(network, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(), "--net-default-egress", "allow")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unrestricted:\n got %#v\nwant %#v", got, want)
	}
	// No broad public rule needed: default is allow.
	if slices.Contains(got, "allow:egress@public") {
		t.Fatalf("unrestricted should not emit allow:egress@public: %#v", got)
	}
}

func TestMsbNetworkArgsPublishPorts(t *testing.T) {
	network := config.NetworkConfig{
		Egress: "deny",
		PublishPorts: []config.PortMapping{
			{Host: 8080, Guest: 80},
			{Host: 9090, Guest: 9000},
		},
	}
	got := MsbNetworkArgs(network, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(),
		"--net-default-egress", "deny",
		"-p", "8080:80",
		"-p", "9090:9000",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publish ports:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsPublishPortsAllModes(t *testing.T) {
	for _, mode := range []string{"deny", "public", "unrestricted"} {
		network := config.NetworkConfig{
			Egress:       mode,
			PublishPorts: []config.PortMapping{{Host: 3000, Guest: 3000}},
		}
		got := MsbNetworkArgs(network, testGatewayHost, testGatewayPort)
		if !slices.Contains(got, "-p") {
			t.Fatalf("mode %q: expected publish port flag, got %#v", mode, got)
		}
		// The publish flag and its value are adjacent and last.
		if got[len(got)-2] != "-p" || got[len(got)-1] != "3000:3000" {
			t.Fatalf("mode %q: publish args not at tail: %#v", mode, got)
		}
	}
}

func TestMsbNetworkArgsAlwaysHasGatewayRuleFirst(t *testing.T) {
	cases := []config.NetworkConfig{
		{},
		{Egress: "deny"},
		{Egress: "public"},
		{Egress: "unrestricted"},
		{Egress: "public", AllowHostServices: []config.HostService{{Host: "x", Port: 1}}},
	}
	for _, network := range cases {
		got := MsbNetworkArgs(network, testGatewayHost, 9999)
		// Gateway tcp rule first, then the DNS allow pair (host udp/53 + tcp/53).
		if len(got) < 6 ||
			got[0] != "--net-rule" || got[1] != "allow:egress@host:tcp:9999" ||
			got[2] != "--net-rule" || got[3] != "allow:egress@host:udp:53" ||
			got[4] != "--net-rule" || got[5] != "allow:egress@host:tcp:53" {
			t.Fatalf("egress %q: gateway+dns prefix not first: %#v", network.ResolvedEgress(), got)
		}
	}
}

func TestMsbNetworkArgsDomainAndWildcard(t *testing.T) {
	network := config.NetworkConfig{
		Egress: "deny",
		AllowHostServices: []config.HostService{
			{Host: "api.github.com", Port: 443},
			{Host: "*.npmjs.org", Port: 443},
		},
	}
	got := MsbNetworkArgs(network, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(),
		"--net-default-egress", "deny",
		"--net-rule", "allow:egress@api.github.com:tcp:443",
		"--net-rule", "allow:egress@*.npmjs.org:tcp:443",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domain+wildcard:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsGatewayPortRespected(t *testing.T) {
	got := MsbNetworkArgs(config.NetworkConfig{Egress: "deny"}, testGatewayHost, 8123)
	want := []string{
		"--net-rule", "allow:egress@host:tcp:8123",
		"--net-rule", "allow:egress@host:udp:53",
		"--net-rule", "allow:egress@host:tcp:53",
		"--net-default-egress", "deny",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("custom gateway port:\n got %#v\nwant %#v", got, want)
	}
}

// In client mode the always-on gateway allow rule targets the remote server
// (the resolved AIPlatformHost), but the DNS allow still targets the LOCAL `host`
// group — the DNS forwarder is local even when the model gateway is remote.
func TestMsbNetworkArgsRemoteGatewayHost(t *testing.T) {
	got := MsbNetworkArgs(config.NetworkConfig{Egress: "deny"}, "demo-server", 9999)
	want := []string{
		"--net-rule", "allow:egress@demo-server:tcp:9999",
		"--net-rule", "allow:egress@host:udp:53",
		"--net-rule", "allow:egress@host:tcp:53",
		"--net-default-egress", "deny",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote gateway host:\n got %#v\nwant %#v", got, want)
	}
}

// The local gateway host still emits the local allow rule (standalone fallback).
func TestMsbNetworkArgsLocalGatewayHost(t *testing.T) {
	got := MsbNetworkArgs(config.NetworkConfig{Egress: "deny"}, testGatewayHost, testGatewayPort)
	want := append(gatewayPrefix(), "--net-default-egress", "deny")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("local gateway host:\n got %#v\nwant %#v", got, want)
	}
}
