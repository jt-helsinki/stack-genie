package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCreatesOverlayUnderHome(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	overlayPath, err := Ensure("aip-app")
	if err != nil {
		test.Fatal(err)
	}
	want := filepath.Join(home, ".ai-platform", "overlays", "aip-app")
	if overlayPath != want {
		test.Fatalf("overlay path = %q, want %q", overlayPath, want)
	}
	present, err := Exists("aip-app")
	if err != nil || !present {
		test.Fatalf("overlay should exist after Ensure: present=%v err=%v", present, err)
	}
}

func TestEnsureIsIdempotent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	first, err := Ensure("aip-app")
	if err != nil {
		test.Fatal(err)
	}
	// A file written into the overlay must survive a re-Ensure (this is what
	// makes installs persist across restart/recreation, arch §26).
	marker := filepath.Join(first, "installed")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		test.Fatal(err)
	}
	if _, err := Ensure("aip-app"); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		test.Fatalf("re-Ensure must not wipe the overlay contents: %v", err)
	}
}

func TestRemoveIsPermanentAndIdempotent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	if _, err := Ensure("aip-app"); err != nil {
		test.Fatal(err)
	}
	if err := Remove("aip-app"); err != nil {
		test.Fatal(err)
	}
	present, err := Exists("aip-app")
	if err != nil || present {
		test.Fatalf("overlay should be gone after Remove: present=%v err=%v", present, err)
	}
	// Removing a non-existent overlay is not an error.
	if err := Remove("aip-app"); err != nil {
		test.Fatalf("Remove must be idempotent: %v", err)
	}
}
