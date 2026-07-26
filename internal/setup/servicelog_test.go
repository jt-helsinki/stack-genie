package setup

import (
	"os/exec"
	"strings"
	"testing"
)

// logTailProber records every `<runtime> logs` argv and returns a per-container
// canned output, so ServiceLogTail's argv + multi-container layout are unit-testable
// against a fake (the live `docker logs` round-trip is a hardware bring-up item).
type logTailProber struct {
	calls     [][]string
	perCont   map[string]string // container → canned log text
	failCont  map[string]bool   // container → return an error (stopped/absent)
	noRuntime bool              // LookPath fails → no container runtime
}

func (prober *logTailProber) LookPath(file string) (string, error) {
	if prober.noRuntime {
		return "", exec.ErrNotFound
	}
	return "/usr/bin/" + file, nil
}
func (prober *logTailProber) Exists(string) bool { return false }
func (prober *logTailProber) Run(name string, args ...string) ([]byte, error) {
	prober.calls = append(prober.calls, append([]string{name}, args...))
	// args: logs --tail N <container>
	if len(args) >= 1 && args[0] == "logs" {
		container := args[len(args)-1]
		if prober.failCont[container] {
			return nil, exec.ErrNotFound
		}
		return []byte(prober.perCont[container]), nil
	}
	return nil, nil
}

// TestServiceLogTailSingleContainerArgv pins the docker-logs argv for a
// single-container service: `<runtime> logs --tail <n> <container>`, with the raw
// output returned (no heading).
func TestServiceLogTailSingleContainerArgv(test *testing.T) {
	prober := &logTailProber{perCont: map[string]string{"aip-headroom": "headroom line 1\nheadroom line 2\n"}}
	deps := Deps{Prober: prober}

	out, err := ServiceLogTail(deps, "headroom", 50)
	if err != nil {
		test.Fatalf("unexpected error: %v", err)
	}
	if out != "headroom line 1\nheadroom line 2\n" {
		test.Errorf("single-container output should be the raw logs, got:\n%s", out)
	}
	if len(prober.calls) != 1 {
		test.Fatalf("expected one logs call, got %d: %v", len(prober.calls), prober.calls)
	}
	got := strings.Join(prober.calls[0], " ")
	if got != "docker logs --tail 50 aip-headroom" {
		test.Errorf("argv = %q, want \"docker logs --tail 50 aip-headroom\"", got)
	}
}

// TestServiceLogTailDefaultTail uses ServiceLogTailLines when tail <= 0.
func TestServiceLogTailDefaultTail(test *testing.T) {
	prober := &logTailProber{perCont: map[string]string{"aip-headroom": "x"}}
	if _, err := ServiceLogTail(Deps{Prober: prober}, "headroom", 0); err != nil {
		test.Fatalf("unexpected error: %v", err)
	}
	got := strings.Join(prober.calls[0], " ")
	if !strings.Contains(got, "--tail 200") {
		test.Errorf("tail<=0 must default to ServiceLogTailLines (200), got %q", got)
	}
}

// TestServiceLogTailMultiContainerSections concatenates per-container sections under
// a heading for a multi-container service (presidio = analyzer + anonymizer).
func TestServiceLogTailMultiContainerSections(test *testing.T) {
	prober := &logTailProber{perCont: map[string]string{
		"aip-presidio-analyzer":   "analyzer ready\n",
		"aip-presidio-anonymizer": "anonymizer ready\n",
	}}
	out, err := ServiceLogTail(Deps{Prober: prober}, "presidio", 10)
	if err != nil {
		test.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"── aip-presidio-analyzer ──", "analyzer ready",
		"── aip-presidio-anonymizer ──", "anonymizer ready",
	} {
		if !strings.Contains(out, want) {
			test.Errorf("multi-container output should contain %q, got:\n%s", want, out)
		}
	}
	if len(prober.calls) != 2 {
		test.Fatalf("expected two logs calls (one per container), got %d", len(prober.calls))
	}
}

// TestServiceLogTailStoppedContainerTolerated: a not-running/absent container is not
// an error for the viewer — it just yields an empty section (single) / empty body
// under its heading (multi).
func TestServiceLogTailStoppedContainerTolerated(test *testing.T) {
	prober := &logTailProber{
		perCont:  map[string]string{"aip-headroom": ""},
		failCont: map[string]bool{"aip-headroom": true},
	}
	out, err := ServiceLogTail(Deps{Prober: prober}, "headroom", 10)
	if err != nil {
		test.Fatalf("a stopped container must not be an error, got: %v", err)
	}
	if out != "" {
		test.Errorf("a stopped single-container service should yield empty output, got %q", out)
	}
}

// TestServiceLogTailUnknownService is exit-2 invalid input.
func TestServiceLogTailUnknownService(test *testing.T) {
	prober := &logTailProber{}
	if _, err := ServiceLogTail(Deps{Prober: prober}, "nope", 10); err == nil {
		test.Fatal("an unknown service must error")
	}
}

// TestServiceLogTailNoRuntime: no container runtime → a missing-dependency error.
func TestServiceLogTailNoRuntime(test *testing.T) {
	prober := &logTailProber{noRuntime: true}
	if _, err := ServiceLogTail(Deps{Prober: prober}, "headroom", 10); err == nil {
		test.Fatal("no container runtime must error")
	}
}
