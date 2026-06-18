// Package jsonfile provides the platform's shared JSON persistence: atomic
// writes (temp file + rename, so a crash mid-write never leaves a partial file —
// arch §23) and strict reads that reject unknown fields (repo-layout §12).
package jsonfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes value as indented JSON to path, creating parent
// directories. It writes a temp file in the same directory and renames it over
// the target.
func WriteAtomic(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	defer os.Remove(tempName) // no-op once renamed

	encoder := json.NewEncoder(tempFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// Read decodes path into value, rejecting unknown fields. The returned error
// wraps os.ErrNotExist when the file is absent, so callers can use errors.Is.
func Read(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
