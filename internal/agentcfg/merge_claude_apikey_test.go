package agentcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMergeClaudeSettingsAPIKey is the api-key counterpart to TestMergeClaudeSettingsOAuth:
// merging the generated keyless gateway env block into an existing project settings.json
// must set ANTHROPIC_BASE_URL to the gateway ROOT (no /v1) while the user's other keys
// survive.
func TestMergeClaudeSettingsAPIKey(test *testing.T) {
	existing := []byte(`{"env":{"OTHER":"x"},"theme":"dark"}`)
	merged, err := MergeClaudeSettings(existing, "http://gw:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(merged, &document); err != nil {
		test.Fatalf("merged settings not valid JSON: %s\n%s", err, merged)
	}
	envBlock, ok := document["env"].(map[string]any)
	if !ok {
		test.Fatalf("env not an object: %v", document["env"])
	}
	if got := envBlock[claudeBaseURLVar]; got != "http://gw:18787" {
		test.Errorf("%s = %v, want gateway root %q (no /v1)", claudeBaseURLVar, got, "http://gw:18787")
	}
	if got := envBlock["OTHER"]; got != "x" {
		test.Errorf("user's env.OTHER key must survive the merge; got %v", got)
	}
	if got := document["theme"]; got != "dark" {
		test.Errorf("user's top-level theme key must survive the merge; got %v", got)
	}
}

// TestMergeClaudeSettingsAPIKeyNilExisting checks a nil/empty existing file degrades to
// valid generated settings carrying the gateway base URL.
func TestMergeClaudeSettingsAPIKeyNilExisting(test *testing.T) {
	for _, existing := range [][]byte{nil, []byte(""), []byte("   ")} {
		merged, err := MergeClaudeSettings(existing, "http://gw:18787")
		if err != nil {
			test.Fatalf("existing=%q: %s", existing, err)
		}
		var document map[string]any
		if err := json.Unmarshal(merged, &document); err != nil {
			test.Fatalf("existing=%q: merged not valid JSON: %s\n%s", existing, err, merged)
		}
		envBlock, ok := document["env"].(map[string]any)
		if !ok {
			test.Fatalf("existing=%q: env not an object: %v", existing, document["env"])
		}
		if got := envBlock[claudeBaseURLVar]; got != "http://gw:18787" {
			test.Errorf("existing=%q: %s = %v, want %q", existing, claudeBaseURLVar, got, "http://gw:18787")
		}
	}
}

// TestMergeClaudeSettingsAPIKeyKeyless asserts the merged api-key settings never embed a
// bearer token — the token flows via the exported env var, not this on-disk file.
func TestMergeClaudeSettingsAPIKeyKeyless(test *testing.T) {
	merged, err := MergeClaudeSettings([]byte(`{"theme":"dark"}`), "http://gw:18787/v1")
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(merged), "ANTHROPIC_AUTH_TOKEN") {
		test.Errorf("api-key claude settings must be keyless (no token on disk):\n%s", merged)
	}
}
