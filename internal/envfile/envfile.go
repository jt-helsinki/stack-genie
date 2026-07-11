// Package envfile manages ~/.ai-platform/.ai-platform.env — an OPT-IN, 0600 file of `export
// KEY='VALUE'` lines that the `ai` CLI loads at startup so platform secrets
// (UI_PASSWORD, LITELLM_MASTER_KEY) persist across restarts WITHOUT the user
// having to edit their shell rc. The values are loaded into THIS process's
// environment, where the service-tier container launches pick them up via env
// passthrough (litellmRunArgs etc.).
//
// Precedence is "existing env wins": Load only fills gaps, so a value already
// exported in the user's shell (or set for this run) is never clobbered by the
// file. The file is written atomically (temp file + rename, the conffile
// pattern) at mode 0600 — it holds secrets, so it is never world/group
// readable. It is PLAIN TEXT (shell-source-able `export` lines), not YAML, so it
// is handled here rather than through internal/conffile.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// fileName is the env file's base name inside the platform state directory.
const fileName = ".ai-platform.env"

// Path returns ~/.ai-platform/.ai-platform.env (inside the platform state dir, so
// `ai uninstall --purge` removes it with the rest of ~/.ai-platform; derived from
// the home dir, so tests redirect it with t.Setenv("HOME", t.TempDir())).
func Path() (string, error) {
	dir, err := paths.PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// Load reads ~/.ai-platform/.ai-platform.env and, for each KEY=VALUE it carries, sets the
// process environment variable IF it is not already present. Existing
// environment (the user's shell exports, or values set for this run) therefore
// WINS — the file only fills gaps. A missing file is a no-op (no error); only a
// real read failure (e.g. a permission error on an existing file) is returned.
func Load() error {
	path, err := Path()
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // not configured yet — nothing to load
		}
		return err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseLine(scanner.Text())
		if !ok {
			continue
		}
		// Existing env wins: only fill a gap. os.LookupEnv distinguishes an unset
		// var from one set to "" (an explicitly-empty export still wins).
		if _, present := os.LookupEnv(key); present {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return scanner.Err()
}

// Write merges vars into ~/.ai-platform/.ai-platform.env and rewrites it atomically at mode
// 0600. A managed key present in vars is updated in place (keeping its position);
// a new key is appended as an `export KEY='VALUE'` line. Lines the file already
// carries that are NOT in vars (other exports, comments, blanks) are preserved
// verbatim. The file is created (0600) when absent.
func Write(vars map[string]string) error {
	path, err := Path()
	if err != nil {
		return err
	}

	// Read any existing lines (a missing file is an empty starting point).
	var lines []string
	if existing, readErr := os.ReadFile(path); readErr == nil {
		lines = strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil // an empty file split to [""] — treat as no lines
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}

	// Rewrite the value of any managed key already present, tracking which keys
	// were handled so the rest are appended.
	handled := make(map[string]bool, len(vars))
	for index, line := range lines {
		key, _, ok := parseLine(line)
		if !ok {
			continue
		}
		if newValue, found := vars[key]; found {
			lines[index] = exportLine(key, newValue)
			handled[key] = true
		}
	}
	// Append any not-yet-present keys, in a stable (sorted) order so repeated
	// writes are deterministic.
	var appendKeys []string
	for key := range vars {
		if !handled[key] {
			appendKeys = append(appendKeys, key)
		}
	}
	sort.Strings(appendKeys)
	for _, key := range appendKeys {
		lines = append(lines, exportLine(key, vars[key]))
	}

	content := strings.Join(lines, "\n") + "\n"
	return writeAtomic0600(path, content)
}

// parseLine parses one env-file line into (key, value, ok). It accepts `KEY=VAL`
// and `export KEY=VAL`, ignores blank lines and `#` comments, and unquotes a
// single- or double-quoted value. ok is false for a line that carries no
// assignment (so callers skip it but keep it verbatim on rewrite).
func parseLine(raw string) (key, value string, ok bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	line = strings.TrimSpace(line)
	name, rawValue, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}
	return name, unquote(strings.TrimSpace(rawValue)), true
}

// unquote strips a single matching pair of surrounding single or double quotes.
// For a single-quoted value it also reverses the POSIX `'\”` single-quote escape
// (the inverse of exportLine), so a value containing a literal single quote
// round-trips faithfully.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first == '\'' && last == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], `'\''`, "'")
	}
	if first == '"' && last == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

// exportLine renders a single managed `export KEY='VALUE'` line, single-quoting
// the value so shell metacharacters stay literal. A literal single quote in the
// value is escaped with the POSIX `'\”` idiom.
func exportLine(key, value string) string {
	escaped := strings.ReplaceAll(value, "'", `'\''`)
	return fmt.Sprintf("export %s='%s'", key, escaped)
}

// writeAtomic0600 writes content to path via a temp file + rename, at mode 0600
// (the file holds secrets). It mirrors conffile.WriteAtomic but writes plain
// text at a tighter permission and does not YAML-encode.
func writeAtomic0600(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(dir, ".ai-platform.env.tmp-*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed

	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return err
	}
	if _, err := tempFile.WriteString(content); err != nil {
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
