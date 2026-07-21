//go:build integration

package integration

import (
	"os"
	"testing"
	"time"
)

// uninstallResult mirrors the `ai uninstall` data payload.
type uninstallResult struct {
	Purged            bool     `json:"purged"`
	RemovedContainers int      `json:"removed_containers,omitempty"`
	RemovedBinary     string   `json:"removed_binary,omitempty"`
	Aborted           bool     `json:"aborted,omitempty"`
	Plan              []string `json:"plan,omitempty"`
}

// Group 6: Uninstall.
//
// `ai uninstall --dry-run` always runs and asserts a non-empty plan (side-effect
// free). The FULL destructive uninstall runs ONLY when AIP_INTEGRATION_UNINSTALL=1
// is set — guarded so a normal run never nukes the machine.
func TestGroup06Uninstall(test *testing.T) {
	requireStack(test)

	test.Run("dry-run emits a plan, removes nothing", func(test *testing.T) {
		env, code, stderr := run(test, "", 60*time.Second, "uninstall", "--dry-run")
		if !assertOK(test, env, code, "uninstall") {
			test.Logf("uninstall --dry-run stderr:\n%s", stderr)
			return
		}
		var result uninstallResult
		env.dataInto(test, &result)
		if len(result.Plan) == 0 {
			test.Errorf("uninstall --dry-run returned an empty plan")
		}
		if result.RemovedContainers != 0 || result.RemovedBinary != "" {
			test.Errorf("uninstall --dry-run reported side effects: %+v", result)
		}
	})

	test.Run("destructive uninstall (gated)", func(test *testing.T) {
		if os.Getenv("AIP_INTEGRATION_UNINSTALL") != "1" {
			test.Skip("set AIP_INTEGRATION_UNINSTALL=1 to run the destructive uninstall (removes the stack + binary)")
		}
		env, code, stderr := run(test, "", 10*time.Minute, "uninstall", "--purge", "--yes")
		if !assertOK(test, env, code, "uninstall") {
			test.Fatalf("uninstall --purge --yes failed (exit %d):\nstderr:\n%s", code, stderr)
		}
		var result uninstallResult
		env.dataInto(test, &result)
		if !result.Purged {
			test.Errorf("uninstall --purge did not report purged=true: %+v", result)
		}
	})
}
