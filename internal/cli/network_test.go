package cli

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

// When the live policy is available, Human() renders BOTH a declared section and
// a clearly-labeled in-force (live) section, and makes explicit that the live
// view is the applied policy (what would be blocked), not a denial log.
func TestNetworkResultHumanWithInForce(test *testing.T) {
	result := networkResult{
		Egress:            "deny",
		AllowHostServices: []config.HostService{{Host: "gateway", Port: 5432}},
		PublishPorts:      []config.PortMapping{{Guest: 3000, Host: 3000}},
		InForce: &workspace.NetworkPolicy{
			DefaultEgress: "deny",
			Rules:         []string{"allow egress example.com tcp 443"},
			OnViolation:   "block-and-log",
		},
	}
	output := result.Human()
	for _, want := range []string{
		"declared (config.yaml):",
		"gateway:5432",
		"3000→3000",
		"in force (live",
		"what WOULD be blocked",
		"default egress: deny",
		"allow egress example.com tcp 443",
		"on violation: block-and-log",
	} {
		if !strings.Contains(output, want) {
			test.Errorf("Human() missing %q\n--- got ---\n%s", want, output)
		}
	}
}

// With no live policy (workspace not running), Human() shows the declared policy
// and an explicit "not running" note — and no in-force rules.
func TestNetworkResultHumanDeclaredOnly(test *testing.T) {
	result := networkResult{Egress: "deny"}
	output := result.Human()
	if !strings.Contains(output, "declared (config.yaml):") {
		test.Errorf("missing declared section:\n%s", output)
	}
	if !strings.Contains(output, "workspace not running — showing declared policy only") {
		test.Errorf("missing not-running note:\n%s", output)
	}
	if strings.Contains(output, "in force (live, on the running microVM)") {
		test.Errorf("declared-only output must not render a live policy section:\n%s", output)
	}
}

func TestSplitHostPort(test *testing.T) {
	cases := []struct {
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		// Bare host -> default HTTPS port.
		{"api.github.com", "api.github.com", 443, false},
		{"registry.npmjs.org", "registry.npmjs.org", 443, false},
		{"gateway", "gateway", 443, false},
		{"10.0.0.5", "10.0.0.5", 443, false},
		// Bare wildcard -> default HTTPS port.
		{"*.npmjs.org", "*.npmjs.org", 443, false},
		// Explicit host:port, split on the last colon.
		{"db.internal:5432", "db.internal", 5432, false},
		{"*.npmjs.org:8443", "*.npmjs.org", 8443, false},
		{"gateway:5442", "gateway", 5442, false},
		// Errors: leading colon, trailing colon, non-numeric port.
		{":443", "", 0, true},
		{"host:", "", 0, true},
		{"host:notaport", "", 0, true},
	}
	for _, testCase := range cases {
		host, port, err := splitHostPort(testCase.input)
		if testCase.wantErr {
			if err == nil {
				test.Errorf("splitHostPort(%q) = (%q,%d,nil), want error", testCase.input, host, port)
			}
			continue
		}
		if err != nil {
			test.Errorf("splitHostPort(%q) unexpected error: %v", testCase.input, err)
			continue
		}
		if host != testCase.wantHost || port != testCase.wantPort {
			test.Errorf("splitHostPort(%q) = (%q,%d), want (%q,%d)",
				testCase.input, host, port, testCase.wantHost, testCase.wantPort)
		}
	}
}
