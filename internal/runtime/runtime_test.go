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
	podmanOut string // stdout for `podman info ...`
	podmanErr bool   // when true, `podman info` fails
}

func (prober fakeProber) LookPath(file string) (string, error) {
	if prober.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", exec.ErrNotFound
}
func (prober fakeProber) Run(name string, _ ...string) ([]byte, error) {
	switch name {
	case "docker":
		return []byte(prober.dockerOut), nil
	case "podman":
		if prober.podmanErr {
			return nil, exec.ErrNotFound
		}
		return []byte(prober.podmanOut), nil
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

func TestPodmanRootfulDetected(test *testing.T) {
	// An explicit "false" from `podman info` means a rootful install.
	prober := fakeProber{
		bins:      map[string]bool{"podman": true, "msb": true},
		files:     map[string]bool{"/dev/kvm": true},
		podmanOut: "false\n",
	}
	info, err := Detect("linux", "amd64", prober, "test")
	if err != nil {
		test.Fatal(err)
	}
	if info.Detected != "podman" || info.Rootless {
		test.Fatalf("rootful podman: %+v", info)
	}
	if !errors.Is(Verify(info), ErrRootlessUnavailable) {
		test.Fatalf("verify should fail rootless, got %v", Verify(info))
	}
}

func TestPodmanRootlessProbeErrorDefaultsTrue(test *testing.T) {
	// Podman is rootless by default; a probe hiccup must not block setup.
	prober := fakeProber{
		bins:      map[string]bool{"podman": true, "msb": true},
		files:     map[string]bool{"/dev/kvm": true},
		podmanErr: true,
	}
	info, err := Detect("linux", "amd64", prober, "test")
	if err != nil {
		test.Fatal(err)
	}
	if !info.Rootless {
		test.Fatalf("podman should default to rootless on probe error: %+v", info)
	}
}

// TestDetectLinuxSetupHappyPath is the [S6] Linux-bootstrap criterion: with a
// rootless container runtime and KVM available, detection + Verify pass exactly
// as on macOS — no workflow difference.
func TestDetectLinuxSetupHappyPath(test *testing.T) {
	prober := fakeProber{
		bins:      map[string]bool{"podman": true, "msb": true},
		files:     map[string]bool{"/dev/kvm": true},
		podmanOut: "true",
	}
	info, err := Detect("linux", "amd64", prober, "test")
	if err != nil {
		test.Fatal(err)
	}
	if info.Microsandbox.Virtualization != "kvm" || !info.Microsandbox.Available {
		test.Fatalf("linux microVM via kvm expected: %+v", info)
	}
	if err := Verify(info); err != nil {
		test.Fatalf("linux setup should verify: %v", err)
	}
}

func TestContainerRuntimeArgs(test *testing.T) {
	for _, name := range []string{"docker", "podman"} {
		containerRuntime := ContainerRuntime{Name: name}
		build := containerRuntime.BuildArgs("aip-app:latest", "/p/.ai-platform/Dockerfile", "/p")
		want := []string{"build", "-t", "aip-app:latest", "-f", "/p/.ai-platform/Dockerfile", "/p"}
		if !equalArgs(build, want) {
			test.Errorf("%s build args = %v, want %v", name, build, want)
		}
		run := containerRuntime.RunArgs("litellm", "litellm:latest", "-p", "4000:4000")
		wantRun := []string{"run", "-d", "--name", "litellm", "-p", "4000:4000", "litellm:latest"}
		if !equalArgs(run, wantRun) {
			test.Errorf("%s run args = %v, want %v", name, run, wantRun)
		}
	}
}

func equalArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
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
