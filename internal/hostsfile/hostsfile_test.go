package hostsfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleEntries() []Entry {
	return []Entry{
		{IP: "127.0.0.1", Names: []string{"litellm.aip.local", "chat.aip.local"}},
		{IP: "127.0.0.1", Names: []string{"odysseus.aip.local"}},
	}
}

// tempPath returns a path to a not-yet-existing file inside a fresh temp dir, so
// tests never touch the real /etc/hosts.
func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "hosts")
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func readFileText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

func TestRenderDeterministic(t *testing.T) {
	block := Render(sampleEntries())
	expected := markerBegin + "\n" +
		managedNotice + "\n" +
		"127.0.0.1\tlitellm.aip.local chat.aip.local\n" +
		"127.0.0.1\todysseus.aip.local\n" +
		markerEnd + "\n"
	if block != expected {
		t.Fatalf("Render mismatch:\n got:\n%q\nwant:\n%q", block, expected)
	}
	if Render(sampleEntries()) != block {
		t.Fatal("Render not deterministic")
	}
}

func TestRenderSkipsBlankEntries(t *testing.T) {
	entries := []Entry{
		{IP: "", Names: []string{"x"}},
		{IP: "127.0.0.1", Names: []string{"", "  "}},
		{IP: "127.0.0.1", Names: []string{"good.aip.local"}},
	}
	block := Render(entries)
	if strings.Contains(block, "\tx") {
		t.Fatal("entry with empty IP should be skipped")
	}
	if !strings.Contains(block, "127.0.0.1\tgood.aip.local") {
		t.Fatal("valid entry missing")
	}
	if strings.Count(block, "127.0.0.1") != 1 {
		t.Fatalf("expected one rendered entry, got block:\n%s", block)
	}
}

func TestApplyNewFile(t *testing.T) {
	path := tempPath(t)
	changed, err := Apply(path, sampleEntries())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on new file")
	}
	if got := readFileText(t, path); got != Render(sampleEntries()) {
		t.Fatalf("new file should equal block:\n%s", got)
	}
}

func TestApplyPreservesOtherEntries(t *testing.T) {
	path := tempPath(t)
	existing := "127.0.0.1\tlocalhost\n255.255.255.255\tbroadcasthost\n::1\tlocalhost\n"
	writeFile(t, path, existing)

	changed, err := Apply(path, sampleEntries())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	got := readFileText(t, path)
	if !strings.HasPrefix(got, existing) {
		t.Fatalf("original lines not preserved at top:\n%s", got)
	}
	if !strings.Contains(got, Render(sampleEntries())) {
		t.Fatalf("managed block missing:\n%s", got)
	}
}

func TestApplyLeavesUnmanagedSameNameEntries(t *testing.T) {
	path := tempPath(t)
	existing := "10.0.0.5\tlitellm.aip.local\n"
	writeFile(t, path, existing)

	if _, err := Apply(path, sampleEntries()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readFileText(t, path)
	if !strings.Contains(got, "10.0.0.5\tlitellm.aip.local") {
		t.Fatalf("unmanaged same-name entry should be left alone:\n%s", got)
	}
}

func TestApplyReplacesStaleBlock(t *testing.T) {
	path := tempPath(t)
	stale := "127.0.0.1\tlocalhost\n" +
		markerBegin + "\n" +
		managedNotice + "\n" +
		"127.0.0.1\told.aip.local\n" +
		markerEnd + "\n" +
		"# tail comment\n"
	writeFile(t, path, stale)

	changed, err := Apply(path, sampleEntries())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true replacing stale block")
	}
	got := readFileText(t, path)
	if strings.Contains(got, "old.aip.local") {
		t.Fatalf("stale entry should be gone:\n%s", got)
	}
	if !strings.Contains(got, "litellm.aip.local") {
		t.Fatalf("new entries missing:\n%s", got)
	}
	if !strings.HasPrefix(got, "127.0.0.1\tlocalhost\n") {
		t.Fatalf("leading line not preserved:\n%s", got)
	}
	if !strings.HasSuffix(got, "# tail comment\n") {
		t.Fatalf("trailing line not preserved:\n%s", got)
	}
	if strings.Count(got, markerBegin) != 1 {
		t.Fatalf("expected exactly one block:\n%s", got)
	}
}

func TestApplyIdempotent(t *testing.T) {
	path := tempPath(t)
	if _, err := Apply(path, sampleEntries()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	changed, err := Apply(path, sampleEntries())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false on idempotent Apply")
	}
}

func TestApplyNoTrailingNewline(t *testing.T) {
	path := tempPath(t)
	writeFile(t, path, "127.0.0.1\tlocalhost") // no trailing newline

	changed, err := Apply(path, sampleEntries())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	got := readFileText(t, path)
	if !strings.HasPrefix(got, "127.0.0.1\tlocalhost\n") {
		t.Fatalf("original line should gain newline and be preserved:\n%q", got)
	}
	if !strings.Contains(got, Render(sampleEntries())) {
		t.Fatalf("block missing:\n%s", got)
	}
	// A subsequent Apply must be idempotent even though input had no newline.
	changed, err = Apply(path, sampleEntries())
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if changed {
		t.Fatal("expected idempotent second Apply")
	}
}

func TestApplyEmptyEntriesRemovesBlock(t *testing.T) {
	path := tempPath(t)
	if _, err := Apply(path, sampleEntries()); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	changed, err := Apply(path, nil)
	if err != nil {
		t.Fatalf("Apply empty: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true removing block via empty entries")
	}
	if strings.Contains(readFileText(t, path), markerBegin) {
		t.Fatal("block should be removed")
	}
}

func TestRemoveStripsBlockKeepsOthers(t *testing.T) {
	path := tempPath(t)
	content := "127.0.0.1\tlocalhost\n\n" +
		Render(sampleEntries()) +
		"\n# trailing\n"
	writeFile(t, path, content)

	changed, err := Remove(path)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	got := readFileText(t, path)
	if strings.Contains(got, markerBegin) {
		t.Fatalf("block not removed:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1\tlocalhost") {
		t.Fatalf("other entries lost:\n%s", got)
	}
	if !strings.Contains(got, "# trailing") {
		t.Fatalf("trailing comment lost:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("removal left a triple blank line:\n%q", got)
	}
}

func TestRemoveAbsent(t *testing.T) {
	path := tempPath(t)
	writeFile(t, path, "127.0.0.1\tlocalhost\n")
	changed, err := Remove(path)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when no block present")
	}
}

func TestRemoveMissingFile(t *testing.T) {
	path := tempPath(t)
	changed, err := Remove(path)
	if err != nil {
		t.Fatalf("Remove missing: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false for missing file")
	}
}

func TestPlanComputesBytesWithoutWriting(t *testing.T) {
	path := tempPath(t)
	existing := "127.0.0.1\tlocalhost\n"
	writeFile(t, path, existing)

	newContent, changed, err := Plan(path, sampleEntries())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for a file without the block")
	}
	// The file must be untouched: Plan is read-only.
	if got := readFileText(t, path); got != existing {
		t.Fatalf("Plan must not write the file, got:\n%s", got)
	}
	if !strings.Contains(string(newContent), Render(sampleEntries())) {
		t.Fatalf("planned bytes missing the block:\n%s", newContent)
	}
	if !strings.HasPrefix(string(newContent), existing) {
		t.Fatalf("planned bytes must preserve existing lines:\n%s", newContent)
	}
}

func TestPlanIdempotentReportsUnchanged(t *testing.T) {
	path := tempPath(t)
	if _, err := Apply(path, sampleEntries()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := readFileText(t, path)
	newContent, changed, err := Plan(path, sampleEntries())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when block already up to date")
	}
	if string(newContent) != before {
		t.Fatalf("unchanged plan should return the current bytes:\n%s", newContent)
	}
}

func TestPlanEmptyEntriesRemovesBlock(t *testing.T) {
	path := tempPath(t)
	content := "127.0.0.1\tlocalhost\n\n" + Render(sampleEntries()) + "\n# trailing\n"
	writeFile(t, path, content)

	newContent, changed, err := Plan(path, nil)
	if err != nil {
		t.Fatalf("Plan empty: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true removing the block")
	}
	if strings.Contains(string(newContent), markerBegin) {
		t.Fatalf("planned bytes should not contain the block:\n%s", newContent)
	}
	// The file is untouched.
	if got := readFileText(t, path); got != content {
		t.Fatalf("Plan must not write the file:\n%s", got)
	}
}

func TestPlanMissingFile(t *testing.T) {
	path := tempPath(t)
	newContent, changed, err := Plan(path, sampleEntries())
	if err != nil {
		t.Fatalf("Plan missing: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for a missing file")
	}
	if string(newContent) != Render(sampleEntries()) {
		t.Fatalf("missing-file plan should equal the rendered block:\n%s", newContent)
	}
	// Empty entries on a missing file: nothing to remove, unchanged.
	_, changed, err = Plan(path, nil)
	if err != nil {
		t.Fatalf("Plan missing empty: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false removing from a missing file")
	}
}

func TestStatus(t *testing.T) {
	path := tempPath(t)

	// Absent file.
	present, upToDate, err := Status(path, sampleEntries())
	if err != nil {
		t.Fatalf("Status absent: %v", err)
	}
	if present || upToDate {
		t.Fatalf("absent file should be present=false upToDate=false, got %v/%v", present, upToDate)
	}

	// File without our block.
	writeFile(t, path, "127.0.0.1\tlocalhost\n")
	present, upToDate, _ = Status(path, sampleEntries())
	if present || upToDate {
		t.Fatalf("no block: present=false upToDate=false, got %v/%v", present, upToDate)
	}

	// Up to date.
	if _, err := Apply(path, sampleEntries()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	present, upToDate, _ = Status(path, sampleEntries())
	if !present || !upToDate {
		t.Fatalf("expected present=true upToDate=true, got %v/%v", present, upToDate)
	}

	// Stale: present but does not match.
	other := []Entry{{IP: "127.0.0.1", Names: []string{"different.aip.local"}}}
	present, upToDate, _ = Status(path, other)
	if !present || upToDate {
		t.Fatalf("stale block: present=true upToDate=false, got %v/%v", present, upToDate)
	}
}
