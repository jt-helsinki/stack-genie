package setup

import (
	"os/exec"
	"strings"
	"testing"
)

// statsProber fakes the container runtime for ServiceStats: it returns a canned
// inspect line and stats JSON per container, so the argv + JSON parsing are testable
// without a live engine (the round-trip is a hardware bring-up item).
type statsProber struct {
	calls     [][]string
	inspect   map[string]string // container → inspect --format output (tab-separated)
	stats     map[string]string // container → stats --format '{{json .}}' output
	failCont  map[string]bool   // container → inspect returns an error (absent)
	noRuntime bool
}

func (prober *statsProber) LookPath(file string) (string, error) {
	if prober.noRuntime {
		return "", exec.ErrNotFound
	}
	return "/usr/bin/" + file, nil
}
func (prober *statsProber) Exists(string) bool { return false }
func (prober *statsProber) Run(name string, args ...string) ([]byte, error) {
	prober.calls = append(prober.calls, append([]string{name}, args...))
	container := args[len(args)-1]
	switch args[0] {
	case "inspect":
		if prober.failCont[container] {
			return nil, exec.ErrNotFound
		}
		return []byte(prober.inspect[container]), nil
	case "stats":
		return []byte(prober.stats[container]), nil
	}
	return nil, nil
}

// TestServiceStatsSingleContainer parses inspect + stats for a running one-container
// service into a populated ContainerStats.
func TestServiceStatsSingleContainer(test *testing.T) {
	prober := &statsProber{
		inspect: map[string]string{
			"aip-ollama": "abcdef0123456789\trunning\t2026-07-03T10:00:00Z\t{\"11434/tcp\":null}",
		},
		stats: map[string]string{
			"aip-ollama": `{"CPUPerc":"3.20%","MemUsage":"180MiB / 512MiB","MemPerc":"35.20%","NetIO":"1.2kB / 0B","BlockIO":"10MB / 4MB","PIDs":"12"}`,
		},
	}
	deps := Deps{Prober: prober, Now: func() string { return "2026-07-03T11:30:00Z" }}

	stats, err := ServiceStats(deps, "ollama")
	if err != nil {
		test.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 1 {
		test.Fatalf("want 1 container, got %d", len(stats))
	}
	got := stats[0]
	if got.ID != "abcdef012345" {
		test.Errorf("id = %q, want short 12-char id", got.ID)
	}
	if got.State != "running" || !got.Found {
		test.Errorf("state/found = %q/%v, want running/true", got.State, got.Found)
	}
	if got.CPUPercent != "3.20%" || got.MemUsage != "180MiB / 512MiB" || got.NetIO != "1.2kB / 0B" || got.BlockIO != "10MB / 4MB" {
		test.Errorf("rate fields not parsed: %+v", got)
	}
	if got.Ports != "11434/tcp" {
		test.Errorf("ports = %q, want 11434/tcp", got.Ports)
	}
	if got.Uptime != "1h30m0s" {
		test.Errorf("uptime = %q, want 1h30m0s (11:30 - 10:00)", got.Uptime)
	}
	// The published-port form appends the host port.
	if p := parseContainerPorts(`{"4000/tcp":[{"HostIp":"127.0.0.1","HostPort":"4000"}]}`); p != "4000/tcp→4000" {
		test.Errorf("published port render = %q", p)
	}
}

// TestServiceStatsMultiContainer returns one entry per container (litellm + db) and
// does not fail when a companion is stopped.
func TestServiceStatsMultiContainer(test *testing.T) {
	prober := &statsProber{
		inspect: map[string]string{
			"aip-litellm": "id1\trunning\t2026-07-03T10:00:00Z\t{\"4000/tcp\":null}",
		},
		failCont: map[string]bool{"aip-litellm-db": true}, // db absent/stopped
		stats: map[string]string{
			"aip-litellm": `{"CPUPerc":"1.00%","MemUsage":"90MiB / 512MiB"}`,
		},
	}
	stats, err := ServiceStats(Deps{Prober: prober}, "litellm")
	if err != nil {
		test.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 2 {
		test.Fatalf("want 2 containers (litellm + db), got %d", len(stats))
	}
	if !stats[0].Found || stats[0].Container != "aip-litellm" {
		test.Errorf("first container should be a running aip-litellm: %+v", stats[0])
	}
	if stats[1].Found || stats[1].Container != "aip-litellm-db" {
		test.Errorf("absent companion should be Found=false: %+v", stats[1])
	}
}

// TestServiceStatsUnknownService is exit-2 invalid input.
func TestServiceStatsUnknownService(test *testing.T) {
	if _, err := ServiceStats(Deps{Prober: &statsProber{}}, "nope"); err == nil {
		test.Fatal("an unknown service must error")
	}
}

// TestServiceStatsNoRuntime: no container runtime → a missing-dependency error.
func TestServiceStatsNoRuntime(test *testing.T) {
	if _, err := ServiceStats(Deps{Prober: &statsProber{noRuntime: true}}, "ollama"); err == nil {
		test.Fatal("no container runtime must error")
	}
}

// TestServiceStatsArgv pins the inspect + stats argv.
func TestServiceStatsArgv(test *testing.T) {
	prober := &statsProber{
		inspect: map[string]string{"aip-ollama": "id\trunning\t\t{}"},
		stats:   map[string]string{"aip-ollama": "{}"},
	}
	if _, err := ServiceStats(Deps{Prober: prober}, "ollama"); err != nil {
		test.Fatal(err)
	}
	var inspectArgv, statsArgv string
	for _, call := range prober.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "inspect") {
			inspectArgv = joined
		}
		if strings.Contains(joined, "stats") {
			statsArgv = joined
		}
	}
	if !strings.Contains(inspectArgv, "inspect --format") || !strings.HasSuffix(inspectArgv, "aip-ollama") {
		test.Errorf("inspect argv = %q", inspectArgv)
	}
	if !strings.Contains(statsArgv, "stats --no-stream --format {{json .}} aip-ollama") {
		test.Errorf("stats argv = %q", statsArgv)
	}
}
