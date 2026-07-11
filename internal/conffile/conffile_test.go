package conffile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
)

type sampleRecord struct {
	Name    string   `yaml:"name"`
	Count   int      `yaml:"count"`
	Enabled bool     `yaml:"enabled"`
	Tags    []string `yaml:"tags"`
}

func TestWriteAtomicReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	want := sampleRecord{
		Name:    "platform",
		Count:   42,
		Enabled: true,
		Tags:    []string{"alpha", "beta"},
	}
	if err := conffile.WriteAtomic(path, want); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	var got sampleRecord
	if err := conffile.Read(path, &got); err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if got.Name != want.Name || got.Count != want.Count || got.Enabled != want.Enabled {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
	if len(got.Tags) != len(want.Tags) {
		t.Fatalf("Tags length mismatch: got %d, want %d", len(got.Tags), len(want.Tags))
	}
	for index := range want.Tags {
		if got.Tags[index] != want.Tags[index] {
			t.Errorf("Tags[%d] mismatch: got %q, want %q", index, got.Tags[index], want.Tags[index])
		}
	}
}

func TestWriteAtomicProducesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	if err := conffile.WriteAtomic(path, sampleRecord{Name: "yaml", Count: 1}); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	contents := string(raw)
	if !strings.Contains(contents, "name: yaml") {
		t.Errorf("expected YAML key/value, got:\n%s", contents)
	}
	if strings.Contains(contents, "{") {
		t.Errorf("expected block YAML, not flow/JSON, got:\n%s", contents)
	}
}

func TestWriteAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	if err := conffile.WriteAtomic(path, sampleRecord{Name: "atomic"}); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir returned error: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("expected exactly one file after write, got %d: %v", len(entries), names)
	}
	if entries[0].Name() != "record.yaml" {
		t.Errorf("expected destination file record.yaml, got %q", entries[0].Name())
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("leftover temp file present: %q", entry.Name())
		}
	}
}

func TestWriteAtomicCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "record.yaml")

	if err := conffile.WriteAtomic(path, sampleRecord{Name: "nested"}); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	var got sampleRecord
	if err := conffile.Read(path, &got); err != nil {
		t.Fatalf("Read after nested write returned error: %v", err)
	}
	if got.Name != "nested" {
		t.Errorf("got Name %q, want %q", got.Name, "nested")
	}
}

func TestWriteAtomicOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	if err := conffile.WriteAtomic(path, sampleRecord{Name: "first", Count: 1}); err != nil {
		t.Fatalf("first WriteAtomic returned error: %v", err)
	}
	if err := conffile.WriteAtomic(path, sampleRecord{Name: "second", Count: 2}); err != nil {
		t.Fatalf("second WriteAtomic returned error: %v", err)
	}

	var got sampleRecord
	if err := conffile.Read(path, &got); err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if got.Name != "second" || got.Count != 2 {
		t.Errorf("overwrite did not replace contents: got %+v", got)
	}

	// Overwriting must not leave temp files behind either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected one file after overwrite, got %d", len(entries))
	}
}

func TestReadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	if err := os.WriteFile(path, []byte("name: x\ncount: 1\nsurprise: true\n"), 0o644); err != nil {
		t.Fatalf("seed WriteFile returned error: %v", err)
	}

	var got sampleRecord
	err := conffile.Read(path, &got)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "surprise") {
		t.Errorf("expected error to mention the unknown field, got: %v", err)
	}
}

func TestReadMissingFileWrapsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.yaml")

	var got sampleRecord
	err := conffile.Read(path, &got)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error to wrap os.ErrNotExist, got: %v", err)
	}
}

func TestReadMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	// Unbalanced flow mapping is not valid YAML.
	if err := os.WriteFile(path, []byte("name: [broken\n"), 0o644); err != nil {
		t.Fatalf("seed WriteFile returned error: %v", err)
	}

	var got sampleRecord
	err := conffile.Read(path, &got)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	// The error is wrapped with the path for context.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("expected error to include the path %q, got: %v", path, err)
	}
}

func TestWriteAtomicEncodeFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yaml")

	// A channel value cannot be marshalled to YAML; the encoder must fail.
	err := conffile.WriteAtomic(path, make(chan int))
	if err == nil {
		t.Fatal("expected error encoding an unsupported type, got nil")
	}

	// The failed write must not leave the destination or any temp files behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir returned error: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("expected no files after failed encode, found %q", entry.Name())
	}
}
