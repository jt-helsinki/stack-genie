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

func (prober fakeProber) LookPath(file string) (string, error) {
	if prober.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", exec.ErrNotFound
}
func (prober fakeProber) Run(name string, _ ...string) ([]byte, error) {
	if name == "docker" {
		return []byte(prober.dockerOut), nil
	}
	return nil, exec.ErrNotFound
}
func (prober fakeProber) Exists(path string) bool { return prober.files[path] }

func appleSiliconDocker(rootless bool) fakeProber {
	securityOptions := "[name=seccomp]"
	if rootless {
		securityOptions = "[name=seccomp name=rootless]"
	}
	return fakeProber{
		bins:      map[string]bool{"docker": true, "msb": true},
		dockerOut: securityOptions,
	}
}

func TestDetectHappyPath(test *testing.T) {
	info, err := Detect("darwin", "arm64", appleSiliconDocker(true), "2026-06-18T00:00:00Z")
	if err != nil {
		test.Fatal(err)
	}
	if info.Detected != "docker" || !info.Rootless {
		test.Fatalf("container: %+v", info)
	}
	if !info.Microsandbox.Available || info.Microsandbox.Virtualization != "hvf" {
		test.Fatalf("microsandbox: %+v", info)
	}
	if err := Verify(info); err != nil {
		test.Fatalf("verify should pass: %v", err)
	}
}

func TestDetectNoContainerRuntime(test *testing.T) {
	_, err := Detect("darwin", "arm64", fakeProber{bins: map[string]bool{"msb": true}}, "test")
	if !errors.Is(err, ErrNoContainerRuntime) {
		test.Fatalf("want ErrNoContainerRuntime, got %v", err)
	}
}

func TestDetectMsbMissing(test *testing.T) {
	prober := fakeProber{bins: map[string]bool{"docker": true}, dockerOut: "[name=rootless]"}
	_, err := Detect("darwin", "arm64", prober, "test")
	if !errors.Is(err, ErrMsbMissing) {
		test.Fatalf("want ErrMsbMissing, got %v", err)
	}
}

func TestVerifyRootlessRequired(test *testing.T) {
	info, err := Detect("darwin", "arm64", appleSiliconDocker(false), "test")
	if err != nil {
		test.Fatal(err)
	}
	if info.Rootless {
		test.Fatal("expected non-rootless")
	}
	if !errors.Is(Verify(info), ErrRootlessUnavailable) {
		test.Fatalf("verify should fail rootless, got %v", Verify(info))
	}
}

func TestVerifyVirtualizationRequired(test *testing.T) {
	// Rootless docker, but Linux without /dev/kvm → virtualization unavailable.
	prober := fakeProber{bins: map[string]bool{"docker": true, "msb": true}, dockerOut: "[name=rootless]"}
	info, err := Detect("linux", "amd64", prober, "test")
	if err != nil {
		test.Fatal(err)
	}
	if !errors.Is(Verify(info), ErrVirtualizationUnavailable) {
		test.Fatalf("verify should fail virtualization, got %v", Verify(info))
	}
}

func TestPodmanDetectedRootless(test *testing.T) {
	prober := fakeProber{bins: map[string]bool{"podman": true, "msb": true}, files: map[string]bool{"/dev/kvm": true}}
	info, err := Detect("linux", "arm64", prober, "test")
	if err != nil {
		test.Fatal(err)
	}
	if info.Detected != "podman" || !info.Rootless {
		test.Fatalf("podman: %+v", info)
	}
}

func TestPersistLoadRoundTrip(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	saved, err := Detect("darwin", "arm64", appleSiliconDocker(true), "2026-06-18T00:00:00Z")
	if err != nil {
		test.Fatal(err)
	}
	if err := Persist(saved); err != nil {
		test.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		test.Fatal(err)
	}
	if loaded == nil || loaded.Detected != "docker" || loaded.Microsandbox.Virtualization != "hvf" {
		test.Fatalf("round-trip: %+v", loaded)
	}
}

func TestLoadMissingReturnsNil(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	loaded, err := Load()
	if err != nil || loaded != nil {
		test.Fatalf("missing runtime.json should be (nil,nil), got (%+v,%v)", loaded, err)
	}
}
