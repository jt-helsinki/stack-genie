// Package hostsfile manages an AI-platform-owned block of entries inside a hosts
// file (normally /etc/hosts). It is the building block for `ai setup` to map the
// platform's UI subdomains (e.g. litellm.aip.local) to 127.0.0.1 in standalone
// mode.
//
// Only the platform's own block — delimited by stable marker lines — is touched.
// Everything else in the file (other entries, blank lines, comments, and their
// order) is preserved verbatim. The block is replaced wholesale on each Apply, so
// stale managed entries are cleaned up while unmanaged entries for the same names
// are left alone (the platform does not deduplicate across the rest of the file).
//
// This package assumes a Unix hosts file (LF line endings); the platform targets
// macOS and Linux only.
//
// Privilege note: writing /etc/hosts requires root. This package performs a plain
// read-modify-write on the path it is given and returns the OS error (a
// permission error if it cannot write); it never shells out to sudo. The caller
// (internal/setup) decides how to elevate.
package hostsfile

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath is the conventional hosts file location on macOS and Linux.
const DefaultPath = "/etc/hosts"

// Marker lines delimiting the managed block. They must stay stable across
// versions so an older block can always be found and replaced.
const (
	markerBegin   = "# >>> ai-platform (managed by ai) >>>"
	markerEnd     = "# <<< ai-platform (managed by ai) <<<"
	managedNotice = "# This block is managed by the ai CLI. Manual edits will be overwritten."
)

// Entry is a single hosts mapping: one IP and the names that resolve to it.
type Entry struct {
	IP    string
	Names []string
}

// Render returns the managed block text (begin marker, managed notice, one line
// per entry, end marker). The output is deterministic: entries are rendered in
// the order given, each as "IP\tname1 name2 …". The block ends with a trailing
// newline. Entries with no IP or no names are skipped.
func Render(entries []Entry) string {
	var builder strings.Builder
	builder.WriteString(markerBegin)
	builder.WriteByte('\n')
	builder.WriteString(managedNotice)
	builder.WriteByte('\n')
	for _, entry := range entries {
		ipText := strings.TrimSpace(entry.IP)
		if ipText == "" {
			continue
		}
		names := make([]string, 0, len(entry.Names))
		for _, name := range entry.Names {
			trimmed := strings.TrimSpace(name)
			if trimmed != "" {
				names = append(names, trimmed)
			}
		}
		if len(names) == 0 {
			continue
		}
		builder.WriteString(ipText)
		builder.WriteByte('\t')
		builder.WriteString(strings.Join(names, " "))
		builder.WriteByte('\n')
	}
	builder.WriteString(markerEnd)
	builder.WriteByte('\n')
	return builder.String()
}

// Apply replaces the managed block in the file at path with the block rendered
// from entries, preserving every other line and its order; if no managed block
// is present it appends one. The write is atomic (temp file + rename). It returns
// changed=false (and writes nothing) when the file already contains exactly the
// rendered block, so Apply is idempotent. When entries is empty the block is
// removed (equivalent to Remove).
//
// Privilege note: writing path (e.g. /etc/hosts) may require root. Apply returns
// the underlying OS error — typically a permission error — without attempting to
// elevate; the caller decides how to gain privilege.
func Apply(path string, entries []Entry) (changed bool, err error) {
	if len(entries) == 0 {
		return Remove(path)
	}

	original, existed, err := readFile(path)
	if err != nil {
		return false, err
	}

	block := Render(entries)
	updated := replaceBlock(original, block)

	if existed && updated == original {
		return false, nil
	}
	if err := writeAtomic(path, updated); err != nil {
		return false, err
	}
	return true, nil
}

// Plan computes the new file bytes Apply WOULD write, WITHOUT touching the file.
// It is the read-only counterpart used by privileged callers (internal/setup,
// internal/uninstall) that must perform the actual write themselves via sudo —
// they Plan the bytes here, write them to a temp file, then `sudo cp` over the
// target. changed mirrors Apply: false (with the unchanged current bytes) when
// the file already contains exactly the rendered block, true otherwise. An empty
// entries set plans the file with the block REMOVED (the Remove behavior). A
// missing file is not an error (it plans as if starting from empty content).
func Plan(path string, entries []Entry) (newContent []byte, changed bool, err error) {
	original, existed, err := readFile(path)
	if err != nil {
		return nil, false, err
	}
	if len(entries) == 0 {
		updated, found := stripBlock(original)
		if !existed || !found || updated == original {
			return []byte(original), false, nil
		}
		return []byte(updated), true, nil
	}
	block := Render(entries)
	updated := replaceBlock(original, block)
	if existed && updated == original {
		return []byte(original), false, nil
	}
	return []byte(updated), true, nil
}

// Remove strips the managed block from the file at path (and a single blank line
// that the block leaves stranded between two adjacent lines), preserving
// everything else, and writes atomically. It returns changed=false (writing
// nothing) when no managed block is present.
//
// Privilege note: as with Apply, writing path may require root; Remove returns
// the OS error and does not elevate.
func Remove(path string) (changed bool, err error) {
	original, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	updated, found := stripBlock(original)
	if !found || updated == original {
		return false, nil
	}
	if err := writeAtomic(path, updated); err != nil {
		return false, err
	}
	return true, nil
}

// Status reports whether a managed block is present in the file at path and, if
// so, whether it matches Render(entries). It is read-only. A missing file is
// reported as present=false, upToDate=false, with no error.
func Status(path string, entries []Entry) (present bool, upToDate bool, err error) {
	original, existed, err := readFile(path)
	if err != nil {
		return false, false, err
	}
	if !existed {
		return false, false, nil
	}
	block, found := extractBlock(original)
	if !found {
		return false, false, nil
	}
	return true, block == Render(entries), nil
}

// readFile reads path. A missing file is not an error: it returns existed=false
// with empty content.
func readFile(path string) (content string, existed bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", false, nil
		}
		return "", false, readErr
	}
	return string(data), true, nil
}

// writeAtomic writes content to path via a temp file in the same directory then
// renames it over the target (the conffile pattern, but for plain text). It
// preserves the existing file mode when the target already exists.
func writeAtomic(path string, content string) error {
	dir := filepath.Dir(path)

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}

	tempFile, err := os.CreateTemp(dir, ".hosts-*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed

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
	if err := os.Chmod(tempName, mode); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// blockBounds locates the managed block in content, returning the line indices
// [beginIndex, endIndex] (inclusive) of the begin and end markers. found is false
// when no complete block is present.
func blockBounds(lines []string) (beginIndex int, endIndex int, found bool) {
	beginIndex, endIndex = -1, -1
	for index, line := range lines {
		switch strings.TrimSpace(line) {
		case markerBegin:
			if beginIndex == -1 {
				beginIndex = index
			}
		case markerEnd:
			if beginIndex != -1 {
				endIndex = index
				return beginIndex, endIndex, true
			}
		}
	}
	return -1, -1, false
}

// extractBlock returns the managed block text (markers included, with a trailing
// newline so it matches Render) if present.
func extractBlock(content string) (block string, found bool) {
	lines := splitLines(content)
	beginIndex, endIndex, ok := blockBounds(lines)
	if !ok {
		return "", false
	}
	return strings.Join(lines[beginIndex:endIndex+1], "\n") + "\n", true
}

// replaceBlock returns content with its managed block replaced by block. If no
// block is present, block is appended (separated from existing content by a
// blank line when the file is non-empty). block already ends with a newline.
func replaceBlock(content string, block string) string {
	lines := splitLines(content)
	beginIndex, endIndex, found := blockBounds(lines)
	if !found {
		return appendBlock(content, block)
	}
	blockLines := splitLines(block) // block has a trailing newline -> last element ""
	if len(blockLines) > 0 && blockLines[len(blockLines)-1] == "" {
		blockLines = blockLines[:len(blockLines)-1]
	}
	rebuilt := make([]string, 0, len(lines))
	rebuilt = append(rebuilt, lines[:beginIndex]...)
	rebuilt = append(rebuilt, blockLines...)
	rebuilt = append(rebuilt, lines[endIndex+1:]...)
	return joinLines(rebuilt)
}

// appendBlock appends block to content, ensuring a single blank line separates
// any existing content from the block.
func appendBlock(content string, block string) string {
	if strings.TrimSpace(content) == "" {
		return block
	}
	trimmed := strings.TrimRight(content, "\n")
	return trimmed + "\n\n" + block
}

// stripBlock removes the managed block from content, collapsing a blank line that
// the removal would otherwise strand between the lines that bracketed the block.
func stripBlock(content string) (stripped string, found bool) {
	lines := splitLines(content)
	beginIndex, endIndex, ok := blockBounds(lines)
	if !ok {
		return content, false
	}

	before := lines[:beginIndex]
	after := lines[endIndex+1:]

	// If the block sat between content with a blank line on each side, drop one
	// of them so we don't leave a double blank line.
	if len(before) > 0 && len(after) > 0 &&
		strings.TrimSpace(before[len(before)-1]) == "" &&
		strings.TrimSpace(after[0]) == "" {
		after = after[1:]
	}

	rebuilt := make([]string, 0, len(before)+len(after))
	rebuilt = append(rebuilt, before...)
	rebuilt = append(rebuilt, after...)
	return joinLines(rebuilt), true
}

// splitLines splits content into lines without dropping information: a trailing
// newline yields a final empty element, which joinLines restores.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// joinLines is the inverse of splitLines.
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
