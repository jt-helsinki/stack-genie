package version

import "testing"

// Version is set via -ldflags at release time and defaults to a local-build
// sentinel otherwise. This test pins the documented default for unset builds.
func TestVersionDefault(t *testing.T) {
	const want = "0.0.0-dev"
	if Version != want {
		t.Fatalf("Version = %q, want default %q", Version, want)
	}
}

func TestVersionIsNonEmptyString(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
}
