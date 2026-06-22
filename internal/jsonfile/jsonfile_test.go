package jsonfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/jsonfile"
)

type sampleRecord struct {
	Name    string   `json:"name"`
	Count   int      `json:"count"`
	Enabled bool     `json:"enabled"`
	Tags    []string `json:"tags"`
}

func TestWriteAtomicReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	want := sampleRecord{
		Name:    "platform",
		Count:   42,
		Enabled: true,
		Tags:    []string{"alpha", "beta"},
	}
	if err := jsonfile.WriteAtomic(path, want); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	var got sampleRecord
	if err := jsonfile.Read(path, &got); err != nil {
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

func TestWriteAtomicProducesIndentedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	if err := jsonfile.WriteAtomic(path, sampleRecord{Name: "indent", Count: 1}); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	contents := string(raw)
	if !strings.Contains(contents, "  \"name\": \"indent\"") {
		t.Errorf("expected two-space indented JSON, got:\n%s", contents)
	}
	if !strings.HasSuffix(contents, "}\n") {
		t.Errorf("expected trailing newline from encoder, got:\n%q", contents)
	}
}

func TestWriteAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	if err := jsonfile.WriteAtomic(path, sampleRecord{Name: "atomic"}); err != nil {
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
	if entries[0].Name() != "record.json" {
		t.Errorf("expected destination file record.json, got %q", entries[0].Name())
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("leftover temp file present: %q", entry.Name())
		}
	}
}

func TestWriteAtomicCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "record.json")

	if err := jsonfile.WriteAtomic(path, sampleRecord{Name: "nested"}); err != nil {
		t.Fatalf("WriteAtomic returned error: %v", err)
	}

	var got sampleRecord
	if err := jsonfile.Read(path, &got); err != nil {
		t.Fatalf("Read after nested write returned error: %v", err)
	}
	if got.Name != "nested" {
		t.Errorf("got Name %q, want %q", got.Name, "nested")
	}
}

func TestWriteAtomicOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	if err := jsonfile.WriteAtomic(path, sampleRecord{Name: "first", Count: 1}); err != nil {
		t.Fatalf("first WriteAtomic returned error: %v", err)
	}
	if err := jsonfile.WriteAtomic(path, sampleRecord{Name: "second", Count: 2}); err != nil {
		t.Fatalf("second WriteAtomic returned error: %v", err)
	}

	var got sampleRecord
	if err := jsonfile.Read(path, &got); err != nil {
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
	path := filepath.Join(dir, "record.json")

	if err := os.WriteFile(path, []byte(`{"name":"x","count":1,"surprise":true}`), 0o644); err != nil {
		t.Fatalf("seed WriteFile returned error: %v", err)
	}

	var got sampleRecord
	err := jsonfile.Read(path, &got)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "surprise") {
		t.Errorf("expected error to mention the unknown field, got: %v", err)
	}
}

func TestReadMissingFileWrapsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	var got sampleRecord
	err := jsonfile.Read(path, &got)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error to wrap os.ErrNotExist, got: %v", err)
	}
}

func TestReadMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	if err := os.WriteFile(path, []byte(`{"name": "broken"`), 0o644); err != nil {
		t.Fatalf("seed WriteFile returned error: %v", err)
	}

	var got sampleRecord
	err := jsonfile.Read(path, &got)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	// The error is wrapped with the path for context.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("expected error to include the path %q, got: %v", path, err)
	}
}

func TestWriteAtomicEncodeFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	// A channel value cannot be marshalled to JSON; the encoder must fail.
	err := jsonfile.WriteAtomic(path, make(chan int))
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
