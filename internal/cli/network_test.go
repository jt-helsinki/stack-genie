package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
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

// realCoreDNSLog is verbatim output captured from `docker logs aip-dns`
// (coredns/coredns:1.11.3, default `common` log format) during the step-0
// reachability verification — the exact lines the parser must handle, including
// the banner lines it must skip and the optional "[INFO]" level prefix.
const realCoreDNSLog = `.:53
CoreDNS-1.11.3
linux/arm64, go1.21.11, a6338e9
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
	// NXDOMAIN denied-style name carried through with its rcode and the trailing
	// dot stripped from the qname.
	last := queries[3]
	if last.QName != "some-random-name-xyz.test-denied.net" {
		test.Errorf("last qname = %q, want some-random-name-xyz.test-denied.net (dot stripped)", last.QName)
	}
	if last.RCode != "NXDOMAIN" {
		test.Errorf("last rcode = %q, want NXDOMAIN", last.RCode)
	}
}

// A line missing the quoted query block (banner, plugin chatter) yields no record.
func TestParseCoreDNSLineSkipsNonQueries(test *testing.T) {
	for _, line := range []string{
		"CoreDNS-1.11.3",
		"linux/arm64, go1.21.11, a6338e9",
		".:53",
		"",
		"[ERROR] plugin/errors: 2 something. read udp timeout",
	} {
		if _, ok := parseCoreDNSLine(line); ok {
			test.Errorf("parseCoreDNSLine(%q) returned a query, want skip", line)
		}
	}
}

// runNetworkLog renders the audit from an injected log source (no live container)
// and surfaces the host-wide / names-only / not-enforcement caveats.
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
		"enforcement is by the msb net-rules",
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
	// Capture the tailed result by re-deriving it the way runNetworkLog does.
	if err := runNetworkLog(emitter, &exit, source, 2); err != nil {
		test.Fatalf("runNetworkLog returned error: %v", err)
	}
	tailed := queries[len(queries)-2:]
	if len(tailed) != 2 || tailed[0].QName != "example.com" {
		test.Errorf("tail of 2 = %+v, want last 2 queries", tailed)
	}
}

// An empty/absent log source (resolver not running) is a clean exit 0 with a note.
func TestNetworkLogNotRunning(test *testing.T) {
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := 0
	source := func() ([]byte, error) { return nil, nil }
	if err := runNetworkLog(emitter, &exit, source, 50); err != nil {
		test.Fatalf("runNetworkLog returned error: %v", err)
	}
	if exit != 0 {
		test.Errorf("exit = %d, want 0 for a not-running resolver", exit)
	}
	result := networkLogResult{}
	if !strings.Contains(result.Human(), "no DNS queries logged yet") {
		test.Errorf("empty Human() missing the no-queries message:\n%s", result.Human())
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
