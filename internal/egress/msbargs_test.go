package egress

import (
	"reflect"
	"slices"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
)

const testGatewayPort = 18787

func gatewayRule() []string {
	return []string{"--net-rule", "allow:egress@host.microsandbox.internal:tcp:18787"}
}

func TestMsbNetworkArgsDenyDefault(t *testing.T) {
	// Empty Egress resolves to "deny".
	got := MsbNetworkArgs(config.NetworkConfig{}, testGatewayPort)
	want := append(gatewayRule(), "--net-default-egress", "deny")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deny default:\n got %#v\nwant %#v", got, want)
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
	got := MsbNetworkArgs(network, testGatewayPort)
	want := []string{
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:18787",
		"--net-default-egress", "deny",
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:5442",
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:6379",
		"--net-rule", "allow:egress@db.internal:tcp:5432",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deny+allowlist:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsPublic(t *testing.T) {
	network := config.NetworkConfig{
		Egress:            "public",
		AllowHostServices: []config.HostService{{Host: "gateway", Port: 5442}},
	}
	got := MsbNetworkArgs(network, testGatewayPort)
	want := []string{
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:18787",
		"--net-default-egress", "deny",
		"--net-rule", "allow:egress@public",
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:5442",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsUnrestricted(t *testing.T) {
	network := config.NetworkConfig{Egress: "unrestricted"}
	got := MsbNetworkArgs(network, testGatewayPort)
	want := []string{
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:18787",
		"--net-default-egress", "allow",
	}
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
	got := MsbNetworkArgs(network, testGatewayPort)
	want := []string{
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:18787",
		"--net-default-egress", "deny",
		"-p", "8080:80",
		"-p", "9090:9000",
	}
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
		got := MsbNetworkArgs(network, testGatewayPort)
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
		got := MsbNetworkArgs(network, 9999)
		if len(got) < 2 || got[0] != "--net-rule" || got[1] != "allow:egress@host.microsandbox.internal:tcp:9999" {
			t.Fatalf("egress %q: gateway rule not first: %#v", network.ResolvedEgress(), got)
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
	got := MsbNetworkArgs(network, testGatewayPort)
	want := []string{
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:18787",
		"--net-default-egress", "deny",
		"--net-rule", "allow:egress@api.github.com:tcp:443",
		"--net-rule", "allow:egress@*.npmjs.org:tcp:443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domain+wildcard:\n got %#v\nwant %#v", got, want)
	}
}

func TestMsbNetworkArgsGatewayPortRespected(t *testing.T) {
	got := MsbNetworkArgs(config.NetworkConfig{Egress: "deny"}, 8123)
	want := []string{
		"--net-rule", "allow:egress@host.microsandbox.internal:tcp:8123",
		"--net-default-egress", "deny",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("custom gateway port:\n got %#v\nwant %#v", got, want)
	}
}
