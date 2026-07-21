// Package conffile provides the platform's shared on-disk persistence: atomic
// writes (temp file + rename, so a crash mid-write never leaves a partial file —
// arch §23) and strict reads that reject unknown fields (repo-layout §12). State
// and config files are stored as YAML for consistency with config.yaml /
// profile.yaml; the --json output envelope (internal/output) is a separate
// machine contract and is not handled here.
package conffile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteAtomic writes value as YAML to path, creating parent directories. It
// writes a temp file in the same directory and renames it over the target.
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
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed

	if err := encodeYAML(tempFile, value); err != nil {
		_ = tempFile.Close()
		return err
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

// encodeYAML writes value as indented YAML to writer. yaml.v3 reports an
// unsupported type (e.g. a channel) by panicking inside Encode, so we recover it
// into a normal error and let the atomic-write cleanup remove the temp file.
func encodeYAML(writer io.Writer, value any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("yaml encode: %v", recovered)
		}
	}()
	encoder := yaml.NewEncoder(writer)
	encoder.SetIndent(2)
	if encodeErr := encoder.Encode(value); encodeErr != nil {
		_ = encoder.Close()
		return encodeErr
	}
	return encoder.Close()
}

// Read decodes path into value, rejecting unknown fields (the YAML equivalent of
// json.DisallowUnknownFields). The returned error wraps os.ErrNotExist when the
// file is absent, so callers can use errors.Is.
func Read(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
