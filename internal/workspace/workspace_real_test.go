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
	// A host with only Podman (no Docker) must reach the deferred build seam,
	// not be rejected as "no runtime" (Slice 6 — never hardcode docker).
	builder := realBuilder{prober: fakeRuntimeProber{bins: map[string]bool{"podman": true}}}
	err := builder.Build("/projects/app", "aip-app:latest")
	if !errors.Is(err, ErrPending) {
		test.Fatalf("podman-only host should reach ErrPending, got %v", err)
	}
}

func TestRealBuilderNoContainerRuntime(test *testing.T) {
	builder := realBuilder{prober: fakeRuntimeProber{bins: map[string]bool{}}}
	err := builder.Build("/projects/app", "aip-app:latest")
	if !errors.Is(err, ErrContainerRuntimeMissing) {
		test.Fatalf("want ErrContainerRuntimeMissing, got %v", err)
	}
}
