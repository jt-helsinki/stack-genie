package versions_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/versions"
)

// expectedServices documents the pins the default registry must carry, together
// with the runtime mode each service uses (repo-layout §12.6). The native
// microsandbox runtime is intentionally absent — it is a detected prerequisite,
// not a pulled/pinned image, so it is excluded from the version pin set.
var expectedServices = map[string]string{
	"litellm":             "container",
	"litellm-db":          "container",
	"headroom":            "container",
	"presidio-analyzer":   "container",
	"presidio-anonymizer": "container",
	"proxy":               "container",
	"ollama":              "container",
	"dns":                 "container",
}

func TestDefaultSchemaVersion(t *testing.T) {
	file := versions.Default()
	if file == nil {
		t.Fatal("Default() returned nil")
	}
	if file.SchemaVersion != versions.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", file.SchemaVersion, versions.SchemaVersion)
	}
}

func TestDefaultServicesPresent(t *testing.T) {
	file := versions.Default()
	if len(file.Services) != len(expectedServices) {
		t.Errorf("got %d services, want %d: %v", len(file.Services), len(expectedServices), file.Services)
	}
	for name, wantMode := range expectedServices {
		entry, ok := file.Services[name]
		if !ok {
			t.Errorf("missing service %q", name)
			continue
		}
		if entry.Mode != wantMode {
			t.Errorf("service %q: Mode = %q, want %q", name, entry.Mode, wantMode)
		}
		// Every container service is pinned by image+tag (mostly `latest`, but
		// some are pinned to a specific tag — e.g. litellm-db to a small Alpine
		// Postgres; the invariant is a non-empty image+tag, not a fixed value).
		if wantMode == "container" {
			if entry.Image == "" {
				t.Errorf("container service %q has empty Image", name)
			}
			if entry.Tag == "" {
				t.Errorf("container service %q has empty Tag", name)
			}
		}
	}
}

func TestDefaultServiceFieldsAreSane(t *testing.T) {
	file := versions.Default()
	for name, entry := range file.Services {
		switch entry.Mode {
		case "container":
			if entry.Image == "" {
				t.Errorf("container service %q has empty Image", name)
			}
			// Container services are pinned by tag (no digest — platform-specific).
			if entry.Tag == "" {
				t.Errorf("container service %q has empty Tag", name)
			}
			if entry.Version != "" || entry.SHA256 != "" {
				t.Errorf("container service %q should not pin native fields (Version=%q SHA256=%q)", name, entry.Version, entry.SHA256)
			}
		case "native":
			if entry.Version == "" {
				t.Errorf("native service %q has empty Version", name)
			}
			if entry.SHA256 == "" {
				t.Errorf("native service %q has empty SHA256", name)
			}
			if entry.Image != "" || entry.Tag != "" {
				t.Errorf("native service %q should not pin container fields (Image=%q Tag=%q)", name, entry.Image, entry.Tag)
			}
		default:
			t.Errorf("service %q has unexpected Mode %q", name, entry.Mode)
		}
	}
}

func TestDefaultReturnsIndependentInstances(t *testing.T) {
	first := versions.Default()
	first.Services["ollama"] = versions.Service{Mode: "mutated"}
	second := versions.Default()
	if second.Services["ollama"].Mode != "container" {
		t.Errorf("Default() shares mutable state across calls: ollama mode = %q", second.Services["ollama"].Mode)
	}
}

func TestPathUnderRedirectedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := versions.Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	want := filepath.Join(home, ".ai-platform", "config", "versions.yaml")
	if path != want {
		t.Errorf("Path() = %q, want %q", path, want)
	}
}

func TestEnsureDefaultCreatesThenIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := versions.Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}

	created, err := versions.EnsureDefault()
	if err != nil {
		t.Fatalf("first EnsureDefault() error: %v", err)
	}
	if !created {
		t.Error("first EnsureDefault() created = false, want true")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("versions.yaml not created: %v", statErr)
	}

	// Mutate the on-disk file so we can prove a second call does NOT overwrite it.
	sentinel := &versions.File{
		SchemaVersion: 99,
		Services:      map[string]versions.Service{"sentinel": {Mode: "native", Version: "keep", SHA256: "keep"}},
	}
	if writeErr := conffile.WriteAtomic(path, sentinel); writeErr != nil {
		t.Fatalf("seeding sentinel: %v", writeErr)
	}

	created, err = versions.EnsureDefault()
	if err != nil {
		t.Fatalf("second EnsureDefault() error: %v", err)
	}
	if created {
		t.Error("second EnsureDefault() created = true, want false (idempotent)")
	}

	reloaded, err := versions.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if reloaded == nil {
		t.Fatal("Load() returned nil after EnsureDefault")
	}
	if reloaded.SchemaVersion != 99 || reloaded.Services["sentinel"].Version != "keep" {
		t.Errorf("EnsureDefault overwrote existing file: got %+v", reloaded)
	}
}

func TestWriteDefaultRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := versions.WriteDefault(); err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	loaded, err := versions.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load() returned nil after WriteDefault")
	}

	want := versions.Default()
	if loaded.SchemaVersion != want.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", loaded.SchemaVersion, want.SchemaVersion)
	}
	if len(loaded.Services) != len(want.Services) {
		t.Fatalf("got %d services, want %d", len(loaded.Services), len(want.Services))
	}
	for name, wantEntry := range want.Services {
		gotEntry, ok := loaded.Services[name]
		if !ok {
			t.Errorf("round-trip missing service %q", name)
			continue
		}
		if gotEntry != wantEntry {
			t.Errorf("service %q round-trip mismatch: got %+v, want %+v", name, gotEntry, wantEntry)
		}
	}
}

func TestWriteDefaultOverwritesExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := versions.Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	stale := &versions.File{
		SchemaVersion: 0,
		Services:      map[string]versions.Service{"stale": {Mode: "container", Image: "old", Tag: "old"}},
	}
	if writeErr := conffile.WriteAtomic(path, stale); writeErr != nil {
		t.Fatalf("seeding stale file: %v", writeErr)
	}

	if err := versions.WriteDefault(); err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	loaded, err := versions.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if _, ok := loaded.Services["stale"]; ok {
		t.Error("WriteDefault did not overwrite stale entry")
	}
	if _, ok := loaded.Services["ollama"]; !ok {
		t.Error("WriteDefault did not write the default registry")
	}
}

func TestLoadAbsentReturnsNilNil(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	loaded, err := versions.Load()
	if err != nil {
		t.Fatalf("Load() on absent file error: %v", err)
	}
	if loaded != nil {
		t.Errorf("Load() on absent file = %+v, want nil", loaded)
	}
}
