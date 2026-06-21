package workspace

import (
	"errors"
	"os/exec"
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

func TestRealBuilderAcceptsPodmanOnlyHost(test *testing.T) {
	// A host with only Podman (no Docker) must get PAST runtime detection — not
	// be rejected as "no runtime" (Slice 6 — never hardcode docker). With msb
	// absent in this fake host, Build then fails on the missing microVM tool, so
	// reaching ErrMsbMissing proves the Podman runtime was accepted.
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
