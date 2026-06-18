package setup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// --- fakes ------------------------------------------------------------------

type fakeProber struct {
	bins      map[string]bool
	files     map[string]bool
	dockerOut string
}

func (f fakeProber) LookPath(file string) (string, error) {
	if f.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", exec.ErrNotFound
}
func (f fakeProber) Run(name string, _ ...string) ([]byte, error) {
	if name == "docker" {
		return []byte(f.dockerOut), nil
	}
	return nil, exec.ErrNotFound
}
func (f fakeProber) Exists(path string) bool { return f.files[path] }

type fakeServices struct {
	reconciled bool
	provider   string
}

func (s *fakeServices) Reconcile(pc string) ([]ServiceStatus, error) {
	s.reconciled = true
	s.provider = pc
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (s *fakeServices) Status() ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}

type fakeCA struct{ ensured bool }

func (c *fakeCA) Ensure() error { c.ensured = true; return nil }

// healthyDeps returns Deps that pass preflight (Apple Silicon, rootless docker,
// msb installed) with fresh fakes.
func healthyDeps() (Deps, *fakeServices, *fakeCA) {
	svc := &fakeServices{}
	ca := &fakeCA{}
	return Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{
			bins:      map[string]bool{"docker": true, "msb": true},
			dockerOut: "[name=seccomp name=rootless]",
		},
		Now:      func() string { return "2026-06-18T00:00:00Z" },
		Services: svc,
		CA:       ca,
	}, svc, ca
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("error is not *output.Error: %v", err)
	}
	return oe.Code
}

// --- tests ------------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	d, svc, ca := healthyDeps()

	rep, err := Run(Options{ProviderConfig: "prov.yaml"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.reconciled || svc.provider != "prov.yaml" {
		t.Fatalf("services not reconciled with provider config: %+v", svc)
	}
	if !ca.ensured {
		t.Fatal("CA.Ensure was not called")
	}
	if !rep.ConfigCreated || !rep.VersionsCreated || !rep.CAReady {
		t.Fatalf("report flags: %+v", rep)
	}
	// Layout + persisted artifacts exist.
	for _, p := range []string{
		filepath.Join(home, ".ai-platform", "config", "runtime.json"),
		filepath.Join(home, ".ai-platform", "config", "config.yaml"),
		filepath.Join(home, ".ai-platform", "config", "versions.json"),
		filepath.Join(home, ".ai-platform", "logs"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}
}

func TestRunPreflightMissingDep(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := healthyDeps()
	d.Prober = fakeProber{bins: map[string]bool{"msb": true}} // no docker

	_, err := Run(Options{}, d)
	if got := exitCode(t, err); got != output.ExitMissingDep {
		t.Fatalf("exit = %d, want %d", got, output.ExitMissingDep)
	}
}

func TestRunPreflightRootlessUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := healthyDeps()
	d.Prober = fakeProber{
		bins:      map[string]bool{"docker": true, "msb": true},
		dockerOut: "[name=seccomp]", // not rootless
	}
	_, err := Run(Options{}, d)
	if got := exitCode(t, err); got != output.ExitRuntimeFailure {
		t.Fatalf("exit = %d, want %d", got, output.ExitRuntimeFailure)
	}
}

func TestRunIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := healthyDeps()

	first, err := Run(Options{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ConfigCreated || !first.VersionsCreated {
		t.Fatalf("first run should create defaults: %+v", first)
	}
	second, err := Run(Options{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if second.ConfigCreated || second.VersionsCreated {
		t.Fatalf("second run must not recreate defaults: %+v", second)
	}
}

func TestServicesStatusIncludesMicrosandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _, _ := healthyDeps()
	if _, err := Run(Options{}, d); err != nil {
		t.Fatal(err)
	}

	st, err := ServicesStatus(d)
	if err != nil {
		t.Fatal(err)
	}
	var foundMSB bool
	for _, s := range st {
		if s.Name == "microsandbox" {
			foundMSB = true
			if s.Mode != "runtime" || !s.Healthy || s.Detail != "hvf" {
				t.Fatalf("microsandbox status: %+v", s)
			}
		}
	}
	if !foundMSB {
		t.Fatal("microsandbox runtime missing from services status")
	}
}
