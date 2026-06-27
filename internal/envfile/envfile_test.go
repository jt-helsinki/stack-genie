package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

// seedFile writes content to path, creating the parent dir (the env file now lives
// inside ~/.ai-platform/, which a temp HOME does not have yet).
func seedFile(test *testing.T, path, content string) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		test.Fatalf("seed file: %v", err)
	}
}

// TestLoadFillsOnlyMissingKeys: Load sets a key absent from the env but leaves
// an already-present one untouched (existing env / shell exports WIN).
func TestLoadFillsOnlyMissingKeys(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	path, err := Path()
	if err != nil {
		test.Fatalf("Path: %v", err)
	}
	content := "# a comment\n\n" +
		"export UI_PASSWORD='from-file'\n" +
		"LITELLM_MASTER_KEY=\"sk-fromfile\"\n" +
		"PLAIN=bare\n"
	seedFile(test, path, content)
	// UI_PASSWORD already exported — must NOT be overwritten by the file.
	test.Setenv("UI_PASSWORD", "from-env")

	if err := Load(); err != nil {
		test.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("UI_PASSWORD"); got != "from-env" {
		test.Errorf("UI_PASSWORD = %q, want existing env to win (from-env)", got)
	}
	if got := os.Getenv("LITELLM_MASTER_KEY"); got != "sk-fromfile" {
		test.Errorf("LITELLM_MASTER_KEY = %q, want unquoted sk-fromfile", got)
	}
	if got := os.Getenv("PLAIN"); got != "bare" {
		test.Errorf("PLAIN = %q, want bare", got)
	}
}

// TestLoadMissingFileIsNoop: a missing file is not an error.
func TestLoadMissingFileIsNoop(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Load(); err != nil {
		test.Fatalf("Load with no file: %v", err)
	}
}

// TestWriteRoundTripPermsAndMerge: Write creates the file 0600, round-trips
// through Load, preserves unknown lines, and updates an existing managed key.
func TestWriteRoundTripPermsAndMerge(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	path, err := Path()
	if err != nil {
		test.Fatalf("Path: %v", err)
	}
	// Pre-existing file with an unmanaged line and a stale managed value.
	seed := "# keep me\nexport OTHER='untouched'\nexport UI_PASSWORD='old'\n"
	seedFile(test, path, seed)

	if err := Write(map[string]string{
		"UI_PASSWORD":        "new-pw-with-'-quote",
		"LITELLM_MASTER_KEY": "sk-123",
	}); err != nil {
		test.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		test.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		test.Errorf("perm = %o, want 600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		test.Fatalf("read: %v", err)
	}
	body := string(raw)
	if !contains(body, "# keep me") || !contains(body, "export OTHER='untouched'") {
		test.Errorf("unknown lines not preserved:\n%s", body)
	}
	if contains(body, "UI_PASSWORD='old'") {
		test.Errorf("stale managed value not updated:\n%s", body)
	}

	// Round-trip: Load into a clean env (clear the keys first) and confirm values.
	test.Setenv("UI_PASSWORD", "")
	_ = os.Unsetenv("UI_PASSWORD")
	_ = os.Unsetenv("LITELLM_MASTER_KEY")
	if err := Load(); err != nil {
		test.Fatalf("Load after Write: %v", err)
	}
	if got := os.Getenv("UI_PASSWORD"); got != "new-pw-with-'-quote" {
		test.Errorf("round-trip UI_PASSWORD = %q", got)
	}
	if got := os.Getenv("LITELLM_MASTER_KEY"); got != "sk-123" {
		test.Errorf("round-trip LITELLM_MASTER_KEY = %q", got)
	}
}

// TestWriteCreatesFileWhenAbsent: Write with no existing file creates it 0600.
func TestWriteCreatesFileWhenAbsent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Write(map[string]string{"UI_PASSWORD": "p"}); err != nil {
		test.Fatalf("Write: %v", err)
	}
	path, _ := Path()
	info, err := os.Stat(path)
	if err != nil {
		test.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		test.Errorf("perm = %o, want 600", perm)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for offset := 0; offset+len(needle) <= len(haystack); offset++ {
		if haystack[offset:offset+len(needle)] == needle {
			return offset
		}
	}
	return -1
}
