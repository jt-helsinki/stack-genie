package workspace

import (
	"errors"
	"path/filepath"

	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// Sentinel errors for the deferred real impls. Missing tools → exit 3; the
// not-yet-wired operations → exit 4 (mapped by the CLI).
var (
	ErrContainerRuntimeMissing = errors.New("no container runtime (docker or podman) found")
	ErrMsbMissing              = errors.New("microsandbox (msb) is not installed")
	ErrPending                 = errors.New("workspace build / microVM operations are wired during hardware bring-up")
)

// realBuilder builds the OCI image with the detected container runtime (Docker or
// Podman, Slice 6 — never hardcoded). Intended command (run on a provisioned
// host), where <rt> is `docker` or `podman`:
//
//	<rt> build -t <imageRef> -f <root>/.ai-platform/Dockerfile <root>
type realBuilder struct{ prober runtime.Prober }

func (builder realBuilder) Build(projectRoot, imageRef string) error {
	containerRuntime, err := runtime.ContainerRuntimeName(builder.prober)
	if err != nil {
		return ErrContainerRuntimeMissing
	}
	// The exact build invocation, shared with the service tier via the runtime
	// abstraction; executed on a provisioned host during hardware bring-up.
	dockerfile := filepath.Join(projectRoot, ".ai-platform", "Dockerfile")
	_ = containerRuntime.BuildArgs(imageRef, dockerfile, projectRoot)
	return ErrPending
}

// realSandbox drives Microsandbox microVMs via the Go SDK / `msb`. The concrete
// SDK calls are wired on a provisioned host; here it verifies the runtime then
// reports ErrPending.
type realSandbox struct{ prober runtime.Prober }

func (sandbox realSandbox) ensureInstalled() error {
	if _, err := sandbox.prober.LookPath("msb"); err != nil {
		return ErrMsbMissing
	}
	return nil
}

func (sandbox realSandbox) Create(string, string, string, string) error { return sandbox.pending() }
func (sandbox realSandbox) Start(string) error                          { return sandbox.pending() }
func (sandbox realSandbox) Stop(string) error                           { return sandbox.pending() }
func (sandbox realSandbox) Destroy(string) error                        { return sandbox.pending() }

func (sandbox realSandbox) Exec(string, []string) (ExecResult, error) {
	if err := sandbox.ensureInstalled(); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{}, ErrPending
}

func (sandbox realSandbox) pending() error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	return ErrPending
}

// RealManager builds a Manager wired to the actual host (used by the CLI).
func RealManager(goos string, now func() string) Manager {
	prober := runtime.RealProber()
	return Manager{
		Builder: realBuilder{prober: prober},
		Sandbox: realSandbox{prober: prober},
		Now:     now,
		GOOS:    goos,
	}
}
