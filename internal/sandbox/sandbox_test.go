package sandbox

import "testing"

type fakeProber struct {
	bins  map[string]bool
	files map[string]bool
}

func (f fakeProber) LookPath(file string) (string, error) {
	if f.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", errNotFound
}
func (f fakeProber) Exists(path string) bool { return f.files[path] }

var errNotFound = &lookErr{}

type lookErr struct{}

func (*lookErr) Error() string { return "not found" }

func TestDetectAppleSilicon(t *testing.T) {
	p := fakeProber{bins: map[string]bool{"msb": true}}
	got := Detect("darwin", "arm64", p)
	if !got.MsbInstalled || got.Virtualization != "hvf" || !got.Available {
		t.Fatalf("apple silicon: %+v", got)
	}
}

func TestDetectIntelMacUnsupported(t *testing.T) {
	p := fakeProber{bins: map[string]bool{"msb": true}}
	got := Detect("darwin", "amd64", p)
	if got.Available || got.Virtualization != "" {
		t.Fatalf("intel mac must be unsupported: %+v", got)
	}
}

func TestDetectLinuxKVM(t *testing.T) {
	p := fakeProber{bins: map[string]bool{"msb": true}, files: map[string]bool{"/dev/kvm": true}}
	got := Detect("linux", "amd64", p)
	if got.Virtualization != "kvm" || !got.Available {
		t.Fatalf("linux kvm: %+v", got)
	}
	// No /dev/kvm → unavailable.
	got = Detect("linux", "amd64", fakeProber{bins: map[string]bool{"msb": true}})
	if got.Available {
		t.Fatalf("linux without /dev/kvm must be unavailable: %+v", got)
	}
}

func TestDetectMsbMissing(t *testing.T) {
	got := Detect("darwin", "arm64", fakeProber{})
	if got.MsbInstalled {
		t.Fatalf("msb should be reported missing: %+v", got)
	}
}
