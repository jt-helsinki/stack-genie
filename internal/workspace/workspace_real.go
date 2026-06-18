package workspace

import (
	"errors"

	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// Sentinel errors for the deferred real impls. Missing tools → exit 3; the
// not-yet-wired operations → exit 4 (mapped by the CLI).
var (
	ErrDockerMissing = errors.New("docker is not installed")
	ErrMsbMissing    = errors.New("microsandbox (msb) is not installed")
	ErrPending       = errors.New("workspace build / microVM operations are wired during hardware bring-up")
)

// realBuilder builds the OCI image with the container runtime.
// Intended command (run on a provisioned host):
//
//	docker build -t <imageRef> -f <root>/.ai-platform/Dockerfile <root>
type realBuilder struct{ prober runtime.Prober }

func (builder realBuilder) Build(string, string) error {
	if _, err := builder.prober.LookPath("docker"); err != nil {
		return ErrDockerMissing
	}
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

func (sandbox realSandbox) Create(string, string, string) error { return sandbox.pending() }
func (sandbox realSandbox) Start(string) error                  { return sandbox.pending() }
func (sandbox realSandbox) Stop(string) error                   { return sandbox.pending() }
func (sandbox realSandbox) Destroy(string) error                { return sandbox.pending() }

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
func RealManager(now func() string) Manager {
	prober := runtime.RealProber()
	return Manager{
		Builder: realBuilder{prober: prober},
		Sandbox: realSandbox{prober: prober},
		Now:     now,
	}
}
