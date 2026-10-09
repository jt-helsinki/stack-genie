package nono

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeAtomicJSON writes value as indented JSON to path, creating parent
// directories, via a temp file + rename so a crash mid-write never leaves a
// partial file. Mirrors internal/conffile.WriteAtomic's guarantee for the one
// format conffile doesn't handle (nono profiles must be JSON, not YAML).
func writeAtomicJSON(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed

	encoder := json.NewEncoder(tempFile)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(value); encodeErr != nil {
		_ = tempFile.Close()
		return encodeErr
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
