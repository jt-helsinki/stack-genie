package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/egress"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// Human() renders the declared egress mode and the allow/deny domain lists.
func TestNetworkResultHuman(test *testing.T) {
	result := networkResult{
		Egress:      "deny",
		AllowDomain: []string{"api.github.com"},
		DenyDomain:  []string{"evil.example.com"},
	}
	human := result.Human()
	for _, want := range []string{"egress:", "deny", "allow:", "api.github.com", "deny:", "evil.example.com"} {
		if !strings.Contains(human, want) {
			test.Errorf("Human() missing %q\n--- got ---\n%s", want, human)
		}
	}
}

// With no allow list, Human() says so explicitly rather than showing a blank list.
func TestNetworkResultHumanNoAllow(test *testing.T) {
	result := networkResult{Egress: "deny"}
	human := result.Human()
	if !strings.Contains(human, "none") {
		test.Errorf("Human() should note no allowed domains\n--- got ---\n%s", human)
	}
}

var realCoreDNSLog = `.:53
CoreDNS-1.11.3
linux/arm64, go1.21.11,
[INFO] 10.10.0.1:57319 - 28961 "A IN example.com. udp 40 false 4096" NOERROR qr,rd,ra,ad 83 0.063868875s
[INFO] 10.10.0.1:63478 - 52961 "A IN example.com. udp 29 false 512" NOERROR qr,rd,ra,ad 83 0.03897s
[INFO] 10.10.0.1:57070 - 52962 "AAAA IN example.com. udp 29 false 512" NOERROR qr,rd,ra,ad 107 0.049090458s
[INFO] 10.10.0.1:58462 - 11268 "A IN some-random-name-xyz.test-denied.net. udp 54 false 512" NXDOMAIN qr,rd,ra 133 0.040828667s
`

func TestParseCoreDNSLog(test *testing.T) {
	queries := parseCoreDNSLog([]byte(realCoreDNSLog))
	if len(queries) != 4 {
		test.Fatalf("expected 4 query records (banner lines skipped), got %d: %+v", len(queries), queries)
	}
	first := queries[0]
	if first.QType != "A" || first.QName != "example.com" {
		test.Errorf("first query = %+v, want qtype=A qname=example.com", first)
	}
	if first.RCode != "NOERROR" {
		test.Errorf("first query rcode = %q, want NOERROR", first.RCode)
	}
	if first.Time != "0.063868875s" {
		test.Errorf("first query time = %q, want 0.063868875s", first.Time)
	}
	// AAAA record parsed (qtype carried through).
	if queries[2].QType != "AAAA" || queries[2].QName != "example.com" {
		test.Errorf("third query = %+v, want qtype=AAAA qname=example.com", queries[2])
	}
	// NXDOMAIN denied-style name carried through with its rcode and trailing
	// dot stripped from qname.
	last := queries[3]
	if last.QName != "some-random-name-xyz.test-denied.net" {
		test.Errorf("last query qname = %q, want some-random-name-xyz.test-denied.net", last.QName)
	}
	if last.RCode != "NXDOMAIN" {
		test.Errorf("last query rcode = %q, want NXDOMAIN", last.RCode)
	}
}

func TestParseCoreDNSLineSkipsNonQueries(test *testing.T) {
	for _, line := range []string{
		"CoreDNS-1.11.3",
		"linux/arm64, go1.21.11, ",
		".:53",
		"[ERROR] plugin/errors: 2 example.com. A: read udp: i/o timeout",
	} {
		if _, ok := parseCoreDNSLine(line); ok {
			test.Errorf("parseCoreDNSLine(%q) should not parse as a query", line)
		}
	}
}

// host-wide, names-only, not-enforcement header text renders.
func TestNetworkLogRenderingWithFakeSource(test *testing.T) {
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := 0
	source := func() ([]byte, error) { return []byte(realCoreDNSLog), nil }
	if err := runNetworkLog(emitter, &exit, source, 50); err != nil {
		test.Fatalf("runNetworkLog returned error: %v", err)
	}
	if exit != 0 {
		test.Errorf("exit = %d, want 0", exit)
	}
	result := networkLogResult{Queries: parseCoreDNSLog([]byte(realCoreDNSLog))}
	human := result.Human()
	for _, want := range []string{
		"attempted DNS names (host-wide, all workspaces)",
		"names only",
		"enforcement is by the project's nono network policy",
		"A example.com",
		"some-random-name-xyz.test-denied.net",
		"NXDOMAIN",
	} {
		if !strings.Contains(human, want) {
			test.Errorf("Human() missing %q\n--- got ---\n%s", want, human)
		}
	}
}

// --tail keeps only the most recent N queries.
func TestNetworkLogTail(test *testing.T) {
	queries := parseCoreDNSLog([]byte(realCoreDNSLog))
	result := networkLogResult{Queries: queries}
	if len(result.Queries) != 4 {
		test.Fatalf("setup: expected 4 queries, got %d", len(result.Queries))
	}
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := 0
	source := func() ([]byte, error) { return []byte(realCoreDNSLog), nil }
	if err := runNetworkLog(emitter, &exit, source, 2); err != nil {
		test.Fatalf("runNetworkLog returned error: %v", err)
	}
	tailed := queries[len(queries)-2:]
	if len(tailed) != 2 || tailed[0].QName != "example.com" {
		test.Errorf("tail of 2 = %+v, want last 2 queries", tailed)
	}
}

// An empty/absent log source (resolver not running) reports a clean note at exit 0.
func TestNetworkLogNotRunning(test *testing.T) {
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := 0
	source := func() ([]byte, error) { return nil, nil }
	if err := runNetworkLog(emitter, &exit, source, 50); err != nil {
		test.Fatalf("runNetworkLog returned error: %v", err)
	}
	if exit != 0 {
		test.Errorf("exit = %d, want 0", exit)
	}
}

// runNetwork drives `ai network <args...>` with a JSON emitter (so interactive()
// is false), exercising the non-interactive path — no prompt attempted.
func runNetwork(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newNetworkCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("network %v returned error: %v", args, err)
	}
	return exit
}

// A missing value on a non-TTY (JSON emitter) is exit 2 for every value-taking
// subcommand, resolving the project from cwd.
func TestNetworkValueRequiredNonInteractive(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runNetwork(test, "egress"); exit != output.ExitInvalidInput {
		test.Fatalf("egress no mode: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if exit := runNetwork(test, "allow"); exit != output.ExitInvalidInput {
		test.Fatalf("allow no host: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// disallow with no value (off TTY) stays exit 2 — it needs an explicit target.
func TestNetworkDisallowRequiresValue(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runNetwork(test, "disallow"); exit != output.ExitInvalidInput {
		test.Fatalf("disallow no host: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// Values passed on the command line are used as-is (no prompt) and applied.
func TestNetworkValuesFromArgs(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runNetwork(test, "egress", "public"); exit != output.ExitOK {
		test.Fatalf("egress public: exit = %d, want 0", exit)
	}
	if exit := runNetwork(test, "allow", "api.github.com"); exit != output.ExitOK {
		test.Fatalf("allow api.github.com: exit = %d, want 0", exit)
	}

	network, err := egress.Get(root)
	if err != nil {
		test.Fatalf("egress.Get: %v", err)
	}
	if network.ResolvedEgress() != "public" {
		test.Fatalf("egress = %q, want public", network.ResolvedEgress())
	}
	if len(network.AllowDomain) != 1 || network.AllowDomain[0] != "api.github.com" {
		test.Fatalf("allow list = %+v, want [api.github.com]", network.AllowDomain)
	}
}

// Invalid values on the command line are exit 2.
func TestNetworkInvalidValueArgs(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runNetwork(test, "egress", "nonsense"); exit != output.ExitInvalidInput {
		test.Fatalf("egress nonsense: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if exit := runNetwork(test, "allow", "http://bad"); exit != output.ExitInvalidInput {
		test.Fatalf("allow http://bad: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}
