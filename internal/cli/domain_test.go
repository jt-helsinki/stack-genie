package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// runDomain drives a single `ai domain [args...]` invocation against a fresh
// command tree, returning the exit code the command set.
func runDomain(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newDomainCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("domain %v returned error: %v", args, err)
	}
	return exit
}

func seedRuntimeDomain(test *testing.T, domain string) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Domain: domain}); err != nil {
		test.Fatal(err)
	}
}

// show with no arg reports the resolved domain; set persists a valid domain.
func TestDomainSetShowRoundTrip(test *testing.T) {
	seedRuntimeDomain(test, "")

	// Default before any set.
	result := domainResultFor(&runtime.Info{})
	if !result.Default || result.Domain != runtime.DefaultDomain {
		test.Fatalf("default domain result = %+v", result)
	}
	if !strings.Contains(result.Human(), "litellm."+runtime.DefaultDomain) {
		test.Fatalf("Human() missing subdomain:\n%s", result.Human())
	}

	if exit := runDomain(test, "aip.example.com"); exit != output.ExitOK {
		test.Fatalf("set exit = %d, want 0", exit)
	}
	info, err := runtime.Load()
	if err != nil || info == nil {
		test.Fatalf("load after set: info=%+v err=%v", info, err)
	}
	if info.Domain != "aip.example.com" {
		test.Fatalf("Domain = %q, want aip.example.com", info.Domain)
	}

	// show reflects it.
	if exit := runDomain(test); exit != output.ExitOK {
		test.Fatalf("show exit = %d, want 0", exit)
	}
}

// Missing runtime.yaml → friendly missing-dependency error (exit 3).
func TestDomainMissingRuntime(test *testing.T) {
	test.Setenv("HOME", test.TempDir()) // no runtime.yaml seeded
	if exit := runDomain(test); exit != output.ExitMissingDep {
		test.Fatalf("show with no runtime: exit = %d, want %d", exit, output.ExitMissingDep)
	}
	if exit := runDomain(test, "aip.example.com"); exit != output.ExitMissingDep {
		test.Fatalf("set with no runtime: exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

// Invalid domains → exit 2 (invalid input), and runtime.yaml is unchanged.
func TestDomainSetRejectsInvalid(test *testing.T) {
	for _, bad := range []string{"http://aip.local", "aip.local/x", "AIP.local", ".aip.local", "aip.local.", "two words", "aip..local", "aip-.local"} {
		seedRuntimeDomain(test, "")
		if exit := runDomain(test, bad); exit != output.ExitInvalidInput {
			test.Fatalf("set %q: exit = %d, want %d", bad, exit, output.ExitInvalidInput)
		}
		info, err := runtime.Load()
		if err != nil || info == nil || info.Domain != "" {
			test.Fatalf("set %q must not mutate runtime.yaml: info=%+v err=%v", bad, info, err)
		}
	}
}

// Valid domains are accepted: dotted and bare-label hostnames.
func TestDomainSetAcceptsValid(test *testing.T) {
	for _, good := range []string{"aip.local", "aip.example.com", "aip", "ai-platform.internal"} {
		seedRuntimeDomain(test, "")
		if exit := runDomain(test, good); exit != output.ExitOK {
			test.Fatalf("set %q: exit = %d, want 0", good, exit)
		}
		info, err := runtime.Load()
		if err != nil || info == nil || info.Domain != good {
			test.Fatalf("set %q did not persist: info=%+v err=%v", good, info, err)
		}
	}
}
