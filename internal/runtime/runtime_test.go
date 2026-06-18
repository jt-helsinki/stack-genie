package runtime

import (
	"errors"
	"os/exec"
	"testing"
)

type fakeProber struct {
	bins      map[string]bool
	files     map[string]bool
	dockerOut string // stdout for `docker info ...`
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

func appleSiliconDocker(rootless bool) fakeProber {
	out := "[name=seccomp]"
	if rootless {
		out = "[name=seccomp name=rootless]"
	}
	return fakeProber{
		bins:      map[string]bool{"docker": true, "msb": true},
		dockerOut: out,
	}
}

func TestDetectHappyPath(t *testing.T) {
	info, err := Detect("darwin", "arm64", appleSiliconDocker(true), "2026-06-18T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if info.Detected != "docker" || !info.Rootless {
		t.Fatalf("container: %+v", info)
	}
	if !info.Microsandbox.Available || info.Microsandbox.Virtualization != "hvf" {
		t.Fatalf("microsandbox: %+v", info)
	}
	if err := Verify(info); err != nil {
		t.Fatalf("verify should pass: %v", err)
	}
}

func TestDetectNoContainerRuntime(t *testing.T) {
	_, err := Detect("darwin", "arm64", fakeProber{bins: map[string]bool{"msb": true}}, "t")
	if !errors.Is(err, ErrNoContainerRuntime) {
		t.Fatalf("want ErrNoContainerRuntime, got %v", err)
	}
}

func TestDetectMsbMissing(t *testing.T) {
	p := fakeProber{bins: map[string]bool{"docker": true}, dockerOut: "[name=rootless]"}
	_, err := Detect("darwin", "arm64", p, "t")
	if !errors.Is(err, ErrMsbMissing) {
		t.Fatalf("want ErrMsbMissing, got %v", err)
	}
}

func TestVerifyRootlessRequired(t *testing.T) {
	info, err := Detect("darwin", "arm64", appleSiliconDocker(false), "t")
	if err != nil {
		t.Fatal(err)
	}
	if info.Rootless {
		t.Fatal("expected non-rootless")
	}
	if !errors.Is(Verify(info), ErrRootlessUnavailable) {
		t.Fatalf("verify should fail rootless, got %v", Verify(info))
	}
}

func TestVerifyVirtualizationRequired(t *testing.T) {
	// Rootless docker, but Linux without /dev/kvm → virtualization unavailable.
	p := fakeProber{bins: map[string]bool{"docker": true, "msb": true}, dockerOut: "[name=rootless]"}
	info, err := Detect("linux", "amd64", p, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(Verify(info), ErrVirtualizationUnavailable) {
		t.Fatalf("verify should fail virtualization, got %v", Verify(info))
	}
}

func TestPodmanDetectedRootless(t *testing.T) {
	p := fakeProber{bins: map[string]bool{"podman": true, "msb": true}, files: map[string]bool{"/dev/kvm": true}}
	info, err := Detect("linux", "arm64", p, "t")
	if err != nil {
		t.Fatal(err)
	}
	if info.Detected != "podman" || !info.Rootless {
		t.Fatalf("podman: %+v", info)
	}
}

func TestPersistLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in, err := Detect("darwin", "arm64", appleSiliconDocker(true), "2026-06-18T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := Persist(in); err != nil {
		t.Fatal(err)
	}
	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || out.Detected != "docker" || out.Microsandbox.Virtualization != "hvf" {
		t.Fatalf("round-trip: %+v", out)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out, err := Load()
	if err != nil || out != nil {
		t.Fatalf("missing runtime.json should be (nil,nil), got (%+v,%v)", out, err)
	}
}
