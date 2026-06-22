package cli

import (
	"strconv"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/spf13/cobra"
)

// dnsContainer is the name of the egress-audit resolver container whose `log`
// plugin output `ai network log` reads (mirrors setup.dnsContainer).
const dnsContainer = "aip-dns"

// defaultNetworkLogTail is how many of the most recent queries `ai network log`
// shows when --tail is not given.
const defaultNetworkLogTail = 50

// dnsQuery is one parsed CoreDNS `log`-plugin entry: the attempted resolution of
// a single name by some workspace.
type dnsQuery struct {
	// Time is the per-query duration string CoreDNS logs at the end of the line
	// (e.g. "0.063868875s"); CoreDNS's `log` plugin does NOT emit a wall-clock
	// timestamp by default, so this is the resolution latency, not a clock time.
	Time  string `json:"time"`
	QName string `json:"qname"`
	QType string `json:"qtype"`
	RCode string `json:"rcode"`
}

// networkLogResult is the `ai network log` payload: the attempted-egress-by-name
// audit read from the aip-dns resolver.
type networkLogResult struct {
	Queries []dnsQuery `json:"queries"`
	Note    string     `json:"note"`
}

// Human renders the audit: a header making the v1 scope explicit, then one line
// per attempted name. The header is deliberately verbose because the audit is
// host-wide and names-only (see the field comments / CLI §29.7).
func (result networkLogResult) Human() string {
	var builder strings.Builder
	builder.WriteString("attempted DNS names (host-wide, all workspaces) — names only, not connection verdicts\n")
	builder.WriteString("enforcement is by the msb net-rules shown in `ai network show`; a resolved name\n")
	builder.WriteString("may still have been blocked at the network layer\n")
	if result.Note != "" {
		builder.WriteString("(" + result.Note + ")\n")
	}
	builder.WriteString("\n")
	if len(result.Queries) == 0 {
		builder.WriteString("no DNS queries logged yet.")
		return builder.String()
	}
	for _, query := range result.Queries {
		// "<qtype> <qname> (<rcode>, <duration>)"
		builder.WriteString(query.QType + " " + query.QName)
		details := make([]string, 0, 2)
		if query.RCode != "" {
			details = append(details, query.RCode)
		}
		if query.Time != "" {
			details = append(details, query.Time)
		}
		if len(details) > 0 {
			builder.WriteString(" (" + strings.Join(details, ", ") + ")")
		}
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// dnsLogSource reads the raw resolver log (CoreDNS `log`-plugin lines). Injected
// so the command is testable without a live container.
type dnsLogSource func() ([]byte, error)

// realDNSLogSource reads `<runtime> logs aip-dns` via the detected container
// runtime. Returns (nil, nil) — an empty, non-error read — when no container
// runtime is present or the resolver container is not running, so the command
// renders a clean "not running" message rather than crashing.
func realDNSLogSource() ([]byte, error) {
	prober := runtime.RealProber()
	containerRuntime, err := runtime.ContainerRuntimeName(prober)
	if err != nil {
		return nil, nil // no runtime — treated as "resolver not available"
	}
	// `logs` writes to both stdout and stderr depending on the runtime; the
	// Prober.Run captures stdout. CoreDNS's `log` plugin writes to stdout, so
	// this is sufficient. A missing/stopped container surfaces as an error from
	// Run; we map that to an empty read (not a crash) below in the command.
	out, err := prober.Run(containerRuntime.Name, "logs", dnsContainer)
	if err != nil {
		return nil, nil // container absent/stopped — show "not running"
	}
	return out, nil
}

// newNetworkLogCmd builds `ai network log` (CLI §29.7): the attempted-egress-by-
// name audit read from the aip-dns resolver's query log.
func newNetworkLogCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var tail int
	cmd := &cobra.Command{
		Use:   "log [project]",
		Short: "Show attempted DNS names across all workspaces (the egress-by-name audit)",
		Long: "Show the attempted-egress-by-name audit: every DNS name workspaces tried to\n" +
			"resolve, read from the platform's aip-dns resolver.\n\n" +
			"v1 scope (read carefully):\n" +
			"  - HOST-WIDE: this is every workspace's DNS on this machine, not attributed\n" +
			"    per-project. The optional [project] argument is accepted for forward\n" +
			"    compatibility but does NOT filter the output in v1.\n" +
			"  - NAMES ONLY: it shows attempted resolutions, not connection verdicts and\n" +
			"    not direct-IP egress (traffic to a literal IP never hits DNS).\n" +
			"  - NOT ENFORCEMENT: a name appearing here was resolved, not necessarily\n" +
			"    reached. Enforcement is by the msb net-rules shown in `ai network show`.\n" +
			"    Under a default-deny posture msb may filter a denied name before it\n" +
			"    reaches the resolver, so denied names can be absent here.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runNetworkLog(emitter, exit, realDNSLogSource, tail)
		},
	}
	cmd.Flags().IntVar(&tail, "tail", defaultNetworkLogTail, "show only the most recent N queries")
	return cmd
}

// runNetworkLog is the command body with an injectable log source (so tests run
// without a live container). When the source yields no data, it reports a clean
// "resolver not running" note at exit 0.
func runNetworkLog(emitter *output.Emitter, exit *int, source dnsLogSource, tail int) error {
	raw, err := source()
	if err != nil {
		*exit = emitter.Failure("network.log", output.Errorf(output.ExitRuntimeFailure, "read aip-dns log: %s", err))
		return nil
	}
	queries := parseCoreDNSLog(raw)
	if tail > 0 && len(queries) > tail {
		queries = queries[len(queries)-tail:]
	}
	result := networkLogResult{Queries: queries}
	if len(queries) == 0 {
		result.Note = "the aip-dns resolver has no queries logged yet, or is not running " +
			"(start it with `ai services start dns` or `ai setup`)"
	}
	*exit = emitter.Success("network.log", result)
	return nil
}

// parseCoreDNSLog parses CoreDNS `log`-plugin lines into the attempted-name
// audit. A representative line (CoreDNS 1.11.3, default `common` format):
//
//	[INFO] 10.10.0.1:57319 - 28961 "A IN example.com. udp 40 false 4096" NOERROR qr,rd,ra,ad 83 0.063868875s
//
// The quoted field is `QTYPE QCLASS QNAME. PROTO SIZE DO BUFSIZE`; after the
// closing quote come RCODE, flags, response size, and the per-query duration.
// Lines without the quoted query block (CoreDNS banner, plugin chatter) are
// skipped. Robust to the optional leading "[LEVEL]" prefix.
func parseCoreDNSLog(raw []byte) []dnsQuery {
	var queries []dnsQuery
	for _, line := range strings.Split(string(raw), "\n") {
		query, ok := parseCoreDNSLine(line)
		if ok {
			queries = append(queries, query)
		}
	}
	return queries
}

// parseCoreDNSLine parses a single CoreDNS log line; ok is false for any line
// that is not a query record.
func parseCoreDNSLine(line string) (dnsQuery, bool) {
	line = strings.TrimSpace(line)
	openQuote := strings.IndexByte(line, '"')
	if openQuote < 0 {
		return dnsQuery{}, false
	}
	closeQuote := strings.IndexByte(line[openQuote+1:], '"')
	if closeQuote < 0 {
		return dnsQuery{}, false
	}
	inner := line[openQuote+1 : openQuote+1+closeQuote]
	after := strings.Fields(line[openQuote+1+closeQuote+1:])

	// inner: "QTYPE QCLASS QNAME. PROTO SIZE DO BUFSIZE"
	innerFields := strings.Fields(inner)
	if len(innerFields) < 3 {
		return dnsQuery{}, false
	}
	query := dnsQuery{
		QType: innerFields[0],
		QName: strings.TrimSuffix(innerFields[2], "."),
	}
	// after: RCODE flags responseSize duration. The duration is the last field
	// and ends in "s"; RCODE is the first.
	if len(after) >= 1 {
		query.RCode = after[0]
	}
	if len(after) >= 1 {
		last := after[len(after)-1]
		if strings.HasSuffix(last, "s") {
			// Validate it parses as a duration-ish number to avoid grabbing a flag.
			if _, err := strconv.ParseFloat(strings.TrimSuffix(last, "s"), 64); err == nil {
				query.Time = last
			}
		}
	}
	if query.QName == "" {
		return dnsQuery{}, false
	}
	return query, true
}
