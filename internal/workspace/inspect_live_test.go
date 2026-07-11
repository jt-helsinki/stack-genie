//go:build manual

package workspace

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// TestInspectNetworkLive drives the REAL realSandbox.InspectNetwork against a
// microVM that must already be running (boot it with:
//
//	msb create --name aip-livecheck --memory 512 \
//	  --net-default-egress deny \
//	  --net-rule "allow:egress@example.com:tcp:443" \
//	  --net-rule "allow:egress@10.0.0.0/8:tcp:5432" alpine
//
// Run with: go test -tags manual -run TestInspectNetworkLive ./internal/workspace/
func TestInspectNetworkLive(test *testing.T) {
	sandbox := realSandbox{prober: runtime.RealProber()}
	policy, err := sandbox.InspectNetwork("aip-livecheck")
	if err != nil {
		test.Fatalf("InspectNetwork(live): %v", err)
	}
	test.Logf("default egress: %q", policy.DefaultEgress)
	test.Logf("on violation:   %q", policy.OnViolation)
	for _, rule := range policy.Rules {
		test.Logf("rule: %s", rule)
	}
	if policy.DefaultEgress != "deny" {
		test.Fatalf("default egress = %q, want deny", policy.DefaultEgress)
	}
	if len(policy.Rules) != 2 {
		test.Fatalf("rules = %v, want 2", policy.Rules)
	}

	// A name that does not resolve to a sandbox must surface ErrNotRunning, not a
	// hard failure (so the CLI shows the declared policy only).
	if _, err := sandbox.InspectNetwork("aip-does-not-exist-xyz"); err != ErrNotRunning {
		test.Fatalf("missing sandbox: got %v, want ErrNotRunning", err)
	}
}
