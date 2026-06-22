package workspace

import (
	"errors"
	"strings"
	"testing"
)

// realInspectSample is a realistic `msb inspect <name> --format json` document
// captured live from msb 0.5.7. The applied egress policy is nested at
// config.network.policy (default_egress + rules) and the secrets violation
// posture at config.network.secrets.on_violation. It mixes the destination
// kinds msb emits: a literal domain, a "*.suffix" wildcard (domain_suffix), and
// a CIDR — plus a single-port and a port-range rule.
const realInspectSample = `{
  "config": {
    "memory_mib": 512,
    "name": "aip-app",
    "network": {
      "enabled": true,
      "policy": {
        "default_egress": "deny",
        "default_ingress": "allow",
        "rules": [
          {
            "action": "allow",
            "destination": { "domain": "example.com" },
            "direction": "egress",
            "ports": [ { "end": 443, "start": 443 } ],
            "protocols": [ "tcp" ]
          },
          {
            "action": "allow",
            "destination": { "domain_suffix": "npmjs.org" },
            "direction": "egress",
            "ports": [ { "end": 443, "start": 443 } ],
            "protocols": [ "tcp" ]
          },
          {
            "action": "allow",
            "destination": { "cidr": "10.0.0.0/8" },
            "direction": "egress",
            "ports": [ { "end": 5500, "start": 5432 } ],
            "protocols": [ "tcp" ]
          }
        ]
      },
      "secrets": { "on_violation": "block-and-log", "secrets": [] }
    }
  },
  "name": "aip-app",
  "status": "Running"
}`

func TestParseInspectNetworkRealShape(test *testing.T) {
	policy, err := parseInspectNetwork([]byte(realInspectSample))
	if err != nil {
		test.Fatalf("parse: %v", err)
	}
	if policy.DefaultEgress != "deny" {
		test.Errorf("default egress = %q, want deny", policy.DefaultEgress)
	}
	if policy.OnViolation != "block-and-log" {
		test.Errorf("on violation = %q, want block-and-log", policy.OnViolation)
	}
	if len(policy.Rules) != 3 {
		test.Fatalf("rules = %v, want 3", policy.Rules)
	}
	// Literal domain, single port.
	if policy.Rules[0] != "allow egress example.com tcp 443" {
		test.Errorf("rule[0] = %q", policy.Rules[0])
	}
	// domain_suffix renders as a "*.suffix" wildcard.
	if policy.Rules[1] != "allow egress *.npmjs.org tcp 443" {
		test.Errorf("rule[1] = %q", policy.Rules[1])
	}
	// CIDR destination, port range.
	if policy.Rules[2] != "allow egress 10.0.0.0/8 tcp 5432-5500" {
		test.Errorf("rule[2] = %q", policy.Rules[2])
	}
}

func TestParseInspectNetworkEmptyPolicy(test *testing.T) {
	policy, err := parseInspectNetwork([]byte(`{"config":{"network":{"policy":{"default_egress":"allow","rules":[]}}}}`))
	if err != nil {
		test.Fatalf("parse: %v", err)
	}
	if policy.DefaultEgress != "allow" || len(policy.Rules) != 0 || policy.OnViolation != "" {
		test.Fatalf("unexpected policy: %+v", policy)
	}
}

func TestParseInspectNetworkBadJSON(test *testing.T) {
	if _, err := parseInspectNetwork([]byte("not json")); err == nil {
		test.Fatal("expected a parse error on malformed json")
	}
}

func TestManagerInspectNetworkReturnsSandboxPolicy(test *testing.T) {
	seedProject(test, "app")
	want := NetworkPolicy{DefaultEgress: "deny", Rules: []string{"allow egress example.com tcp 443"}}
	sandbox := &fakeSandbox{inspectPolicy: want}
	got, err := newManager(&fakeBuilder{}, sandbox).InspectNetwork("app")
	if err != nil {
		test.Fatalf("InspectNetwork: %v", err)
	}
	if got.DefaultEgress != "deny" || len(got.Rules) != 1 {
		test.Fatalf("policy = %+v", got)
	}
}

func TestManagerInspectNetworkNotRunning(test *testing.T) {
	seedProject(test, "app")
	sandbox := &fakeSandbox{inspectErr: ErrNotRunning}
	_, err := newManager(&fakeBuilder{}, sandbox).InspectNetwork("app")
	if !errors.Is(err, ErrNotRunning) {
		test.Fatalf("want ErrNotRunning, got %v", err)
	}
}

func TestManagerInspectNetworkUnknownProject(test *testing.T) {
	seedProject(test, "app")
	_, err := newManager(&fakeBuilder{}, &fakeSandbox{}).InspectNetwork("nope")
	if !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}

// renderInspectRule must keep the destination match-kinds readable even when msb
// reports an IP destination (no special prefix, unlike domain_suffix).
func TestRenderInspectRuleIPDestination(test *testing.T) {
	line := renderInspectRule(msbInspectRule{
		Action:      "allow",
		Direction:   "egress",
		Destination: map[string]string{"ip": "192.168.1.10"},
		Protocols:   []string{"tcp"},
		Ports: []struct {
			Start int `json:"start"`
			End   int `json:"end"`
		}{{Start: 5432, End: 5432}},
	})
	if !strings.Contains(line, "192.168.1.10") || !strings.Contains(line, "5432") {
		test.Fatalf("rule line = %q", line)
	}
}
