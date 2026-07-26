package workspace

import (
	"errors"
	"os/exec"
	"slices"
	"testing"
)

// fakeRuntimeProber is a runtime.Prober that reports a fixed set of binaries.
type fakeRuntimeProber struct{ bins map[string]bool }

func (prober fakeRuntimeProber) LookPath(file string) (string, error) {
	if prober.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", exec.ErrNotFound
}
func (fakeRuntimeProber) Run(string, ...string) ([]byte, error) { return nil, exec.ErrNotFound }
func (fakeRuntimeProber) Exists(string) bool                    { return false }

// stubMsbBinary overrides the msb resolver for a test (so unit tests never download the
// real binary) and restores it afterward.
func stubMsbBinary(test *testing.T, path string, err error) {
	test.Helper()
	previous := msbBinaryFn
	msbBinaryFn = func() (string, error) { return path, err }
	test.Cleanup(func() { msbBinaryFn = previous })
}

func TestRealBuilderAcceptsPodmanOnlyHost(test *testing.T) {
	// A host with only Podman (no Docker) must get PAST runtime detection — not
	// be rejected as "no runtime" (Slice 6 — never hardcode docker). With the msb
	// resolver reporting the tool unavailable, Build then fails on the missing microVM
	// tool, so reaching ErrMsbMissing proves the Podman runtime was accepted.
	stubMsbBinary(test, "", errors.New("no pinned msb binary"))
	builder := realBuilder{prober: fakeRuntimeProber{bins: map[string]bool{"podman": true}}}
	err := builder.Build("/projects/app", "aip-app:latest")
	if !errors.Is(err, ErrMsbMissing) {
		test.Fatalf("podman-only host should pass runtime detection and reach ErrMsbMissing, got %v", err)
	}
}

func TestRealBuilderNoContainerRuntime(test *testing.T) {
	builder := realBuilder{prober: fakeRuntimeProber{bins: map[string]bool{}}}
	err := builder.Build("/projects/app", "aip-app:latest")
	if !errors.Is(err, ErrContainerRuntimeMissing) {
		test.Fatalf("want ErrContainerRuntimeMissing, got %v", err)
	}
}

// Workspace microVMs must opt out of msb's short default idle reaping with an
// explicit, long idle timeout. This is intentionally a create-time policy (no
// heartbeat loop), so it does not wake the CPU periodically and does not block host
// sleep.
func TestSandboxCreateArgsIncludesLongIdleTimeout(t *testing.T) {
	args := sandboxCreateArgs(
		"aip-app", "aip-app:latest", "/projects/app", "/overlays/aip-app",
		VMResources{CPUs: 2, Memory: "8G", IdleTimeout: "2h"}, []string{"--net-rule", "allow@public"},
	)
	if !containsArgPair(args, "--idle-timeout", "2h") {
		t.Fatalf("create args must include configured --idle-timeout 2h, got %v", args)
	}
	if slices.Contains(args, "--max-duration") {
		t.Fatalf("create args must not impose a max duration on dev workspaces: %v", args)
	}
	if !containsArgPair(args, "--cpus", "2") || !containsArgPair(args, "--memory", "8G") {
		t.Fatalf("create args should preserve resources, got %v", args)
	}
	if !containsArgPair(args, "--net-rule", "allow@public") {
		t.Fatalf("create args should append network rules, got %v", args)
	}
}

func TestSandboxCreateArgsDefaultsIdleTimeout(t *testing.T) {
	args := sandboxCreateArgs("aip-app", "aip-app:latest", "/projects/app", "/overlays/aip-app", VMResources{}, nil)
	if !containsArgPair(args, "--idle-timeout", "24h") {
		t.Fatalf("create args must default --idle-timeout to 24h, got %v", args)
	}
}

// The real manager must not install a host sleep inhibitor by default: explicit or
// idle host sleep should remain a real sleep, and the idle fix must not burn battery
// by keeping the machine awake.
func TestRealManagerDoesNotPreventHostSleepByDefault(t *testing.T) {
	manager := RealManager("darwin", func() string { return "now" })
	if manager.Sleep != nil {
		t.Fatalf("RealManager should not wire host sleep prevention by default; got %#v", manager.Sleep)
	}
}

func containsArgPair(args []string, key, value string) bool {
	for index := 0; index < len(args)-1; index++ {
		if args[index] == key && args[index+1] == value {
			return true
		}
	}
	return false
}
