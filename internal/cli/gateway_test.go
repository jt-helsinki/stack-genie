package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// runGateway drives a single `ai gateway <sub> [args...]` invocation against a
// fresh command tree, returning the exit code the command set.
func runGateway(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newGatewayCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("gateway %v returned error: %v", args, err)
	}
	return exit
}

func seedRuntime(test *testing.T, aiPlatformHost string) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, AIPlatformHost: aiPlatformHost}); err != nil {
		test.Fatal(err)
	}
}

// gateway set writes AIPlatformHost machine-wide; show reflects it and the
// resolved URL; clear empties it back to local.
func TestGatewaySetShowClearRoundTrip(test *testing.T) {
	seedRuntime(test, "")

	// Initially local (standalone) — no AIPlatformHost.
	if exit := runGateway(test, "set", "demo-server:18787"); exit != output.ExitOK {
		test.Fatalf("set exit = %d, want 0", exit)
	}
	info, err := runtime.Load()
	if err != nil || info == nil {
		test.Fatalf("load after set: info=%+v err=%v", info, err)
	}
	if info.AIPlatformHost != "demo-server:18787" {
		test.Fatalf("AIPlatformHost = %q, want demo-server:18787", info.AIPlatformHost)
	}

	// show resolves the configured address.
	result := resolveGatewayResult(info.AIPlatformHost)
	if result.Local || result.Host != "demo-server" || result.Port != 18787 {
		test.Fatalf("show result = %+v, want host demo-server:18787", result)
	}
	if !strings.Contains(result.Human(), "http://demo-server:18787/v1") {
		test.Fatalf("Human() missing resolved URL:\n%s", result.Human())
	}

	// clear restores local.
	if exit := runGateway(test, "clear"); exit != output.ExitOK {
		test.Fatalf("clear exit = %d, want 0", exit)
	}
	info, err = runtime.Load()
	if err != nil || info == nil || info.AIPlatformHost != "" {
		test.Fatalf("after clear: info=%+v err=%v", info, err)
	}
	if !resolveGatewayResult(info.AIPlatformHost).Local {
		test.Fatal("after clear the gateway must be local (standalone)")
	}
}

// Missing runtime.yaml → friendly missing-dependency error (exit 3), not a crash.
func TestGatewayMissingRuntime(test *testing.T) {
	test.Setenv("HOME", test.TempDir()) // no runtime.yaml seeded
	if exit := runGateway(test, "show"); exit != output.ExitMissingDep {
		test.Fatalf("show with no runtime: exit = %d, want %d", exit, output.ExitMissingDep)
	}
	if exit := runGateway(test, "set", "srv"); exit != output.ExitMissingDep {
		test.Fatalf("set with no runtime: exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

// Invalid addresses → exit 2 (invalid input), and runtime.yaml is unchanged.
func TestGatewaySetRejectsInvalidAddress(test *testing.T) {
	for _, bad := range []string{"http://srv", "srv/path", "srv:bad", "", "two words"} {
		seedRuntime(test, "")
		if exit := runGateway(test, "set", bad); exit != output.ExitInvalidInput {
			test.Fatalf("set %q: exit = %d, want %d", bad, exit, output.ExitInvalidInput)
		}
		info, err := runtime.Load()
		if err != nil || info == nil || info.AIPlatformHost != "" {
			test.Fatalf("set %q must not mutate runtime.yaml: info=%+v err=%v", bad, info, err)
		}
	}
}

// show on a local (standalone) host names the local gateway and its URL.
func TestGatewayShowLocal(test *testing.T) {
	seedRuntime(test, "")
	result := resolveGatewayResult("")
	if !result.Local {
		test.Fatal("empty AIPlatformHost must resolve to local")
	}
	human := result.Human()
	for _, want := range []string{"local (standalone)", "host.microsandbox.internal", "http://host.microsandbox.internal:18787/v1"} {
		if !strings.Contains(human, want) {
			test.Fatalf("Human() missing %q:\n%s", want, human)
		}
	}
}
