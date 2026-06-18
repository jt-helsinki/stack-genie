package sandbox

import "testing"

type fakeProber struct {
	bins  map[string]bool
	files map[string]bool
}

func (prober fakeProber) LookPath(file string) (string, error) {
	if prober.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", errNotFound
}
func (prober fakeProber) Exists(path string) bool { return prober.files[path] }

var errNotFound = &lookupError{}

type lookupError struct{}

func (*lookupError) Error() string { return "not found" }

func TestDetectAppleSilicon(test *testing.T) {
	prober := fakeProber{bins: map[string]bool{"msb": true}}
	info := Detect("darwin", "arm64", prober)
	if !info.MsbInstalled || info.Virtualization != "hvf" || !info.Available {
		test.Fatalf("apple silicon: %+v", info)
	}
}

func TestDetectIntelMacUnsupported(test *testing.T) {
	prober := fakeProber{bins: map[string]bool{"msb": true}}
	info := Detect("darwin", "amd64", prober)
	if info.Available || info.Virtualization != "" {
		test.Fatalf("intel mac must be unsupported: %+v", info)
	}
}

func TestDetectLinuxKVM(test *testing.T) {
	prober := fakeProber{bins: map[string]bool{"msb": true}, files: map[string]bool{"/dev/kvm": true}}
	info := Detect("linux", "amd64", prober)
	if info.Virtualization != "kvm" || !info.Available {
		test.Fatalf("linux kvm: %+v", info)
	}
	// No /dev/kvm → unavailable.
	withoutKVM := Detect("linux", "amd64", fakeProber{bins: map[string]bool{"msb": true}})
	if withoutKVM.Available {
		test.Fatalf("linux without /dev/kvm must be unavailable: %+v", withoutKVM)
	}
}

func TestDetectMsbMissing(test *testing.T) {
	info := Detect("darwin", "arm64", fakeProber{})
	if info.MsbInstalled {
		test.Fatalf("msb should be reported missing: %+v", info)
	}
}
