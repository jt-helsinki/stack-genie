package agentcfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// refreshStatic mirrors the non-dynamic part of the picker the workspace bakes in:
// the named aliases plus the curated cloud seed (workspace passes
// namedModels(routing) ∪ CloudModels()). The installed local models are NOT here —
// the script fetches those live and merges them.
var refreshStatic = func() []string {
	static := append([]string{"gemma4", "gpt-5.5", "claude-opus", "gemini-pro"}, CloudModels()...)
	sort.Strings(static)
	return dedupSorted(static)
}()

func dedupSorted(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// requireBash skips the test when bash is unavailable, so CI on any host is safe
// (the host-side generation is still covered by the parity-of-skeleton tests).
func requireBash(test *testing.T) string {
	test.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		test.Skip("bash not available; skipping in-VM script execution test")
	}
	return bash
}

// writeFakeCurl writes a fake `curl` onto a dir that the script will see first on
// PATH. The fake echoes the canned tags JSON for the /ollama/api/tags URL and
// fails (exit 7, like real curl's "couldn't connect") for anything else — so a
// test can simulate both the reachable and unreachable gateway.
func writeFakeCurl(test *testing.T, dir, tagsJSON string, reachable bool) {
	test.Helper()
	var body string
	if reachable {
		body = "cat <<'JSON'\n" + tagsJSON + "\nJSON\nexit 0\n"
	} else {
		body = "exit 7\n"
	}
	script := "#!/usr/bin/env bash\n" + body
	path := filepath.Join(dir, "curl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		test.Fatal(err)
	}
}

// runRefresh writes the generated script to a temp dir, points the config paths at
// temp files via a thin wrapper, and runs it with binDir prepended to PATH so the
// fake curl is found. It returns the rewritten opencode + pi config bytes.
func runRefresh(test *testing.T, binDir string) (openCode, pi []byte) {
	test.Helper()
	bash := requireBash(test)

	work := test.TempDir()
	scriptBytes, err := RefreshScript(
		"http://host.microsandbox.internal:18787/v1",
		"sk-workspace-scoped-1234",
		"gemma4",
		refreshStatic,
		5, 8000,
	)
	if err != nil {
		test.Fatal(err)
	}

	// Redirect the two guest paths to temp files. The script hard-codes the guest
	// paths; rather than write to /home/workspace we run the script under a wrapper
	// that overrides them via sed before exec. Simpler: rewrite the path literals.
	openCodeFile := filepath.Join(work, "opencode.json")
	piFile := filepath.Join(work, "models.json")
	rewritten := strings.NewReplacer(
		openCodeGuestPath, openCodeFile,
		piGuestPath, piFile,
	).Replace(string(scriptBytes))

	scriptPath := filepath.Join(work, "refresh-models")
	if err := os.WriteFile(scriptPath, []byte(rewritten), 0o755); err != nil {
		test.Fatal(err)
	}

	cmd := exec.Command(bash, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		test.Fatalf("refresh-models failed: %v\n%s", err, out)
	}
	test.Logf("refresh-models output:\n%s", out)

	openCode, err = os.ReadFile(openCodeFile)
	if err != nil {
		test.Fatalf("opencode config not written: %v", err)
	}
	pi, err = os.ReadFile(piFile)
	if err != nil {
		test.Fatalf("pi config not written: %v", err)
	}
	return openCode, pi
}

// TestRefreshScriptParityWithGenerators executes the generated script with a fake
// curl returning two installed local models, and asserts the rewritten configs are
// BYTE-IDENTICAL to OpenCodeConfig / PiConfig for the merged+sorted model list — so
// an in-VM refresh matches a fresh workspace start.
func TestRefreshScriptParityWithGenerators(test *testing.T) {
	binDir := test.TempDir()
	tags := `{"models":[{"name":"llama3.2:latest","size":1},{"name":"qwen2.5:7b","size":2}]}`
	writeFakeCurl(test, binDir, tags, true)

	gotOpenCode, gotPi := runRefresh(test, binDir)

	// The expected merged list: static ∪ the two fetched local models (ollama/<name>),
	// deduped + sorted exactly as pickerModels does.
	merged := dedupSorted(append(append([]string{}, refreshStatic...),
		"ollama/llama3.2:latest", "ollama/qwen2.5:7b"))

	wantOpenCode, err := OpenCodeConfig(
		"http://host.microsandbox.internal:18787/v1", "sk-workspace-scoped-1234", "gemma4", merged, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	wantPi, err := PiConfig(
		"http://host.microsandbox.internal:18787/v1", "sk-workspace-scoped-1234", "gemma4", merged)
	if err != nil {
		test.Fatal(err)
	}

	if string(gotOpenCode) != string(wantOpenCode) {
		test.Fatalf("opencode.json not byte-identical to OpenCodeConfig:\n--- got ---\n%s\n--- want ---\n%s", gotOpenCode, wantOpenCode)
	}
	if string(gotPi) != string(wantPi) {
		test.Fatalf("models.json not byte-identical to PiConfig:\n--- got ---\n%s\n--- want ---\n%s", gotPi, wantPi)
	}
}

// TestRefreshScriptDegradesWhenGatewayUnreachable: when the tags fetch fails the
// script keeps the static (baked) models — it does NOT wipe the configs — and warns.
func TestRefreshScriptDegradesWhenGatewayUnreachable(test *testing.T) {
	binDir := test.TempDir()
	writeFakeCurl(test, binDir, "", false) // curl exits non-zero for every URL

	gotOpenCode, gotPi := runRefresh(test, binDir)

	wantOpenCode, err := OpenCodeConfig(
		"http://host.microsandbox.internal:18787/v1", "sk-workspace-scoped-1234", "gemma4", refreshStatic, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	wantPi, err := PiConfig(
		"http://host.microsandbox.internal:18787/v1", "sk-workspace-scoped-1234", "gemma4", refreshStatic)
	if err != nil {
		test.Fatal(err)
	}
	if string(gotOpenCode) != string(wantOpenCode) {
		test.Fatalf("degraded opencode.json must keep the static models:\n--- got ---\n%s\n--- want ---\n%s", gotOpenCode, wantOpenCode)
	}
	if string(gotPi) != string(wantPi) {
		test.Fatalf("degraded models.json must keep the static models:\n--- got ---\n%s\n--- want ---\n%s", gotPi, wantPi)
	}
}

// TestRefreshScriptErrorsWhenCurlMissing: with no curl on PATH the script must exit
// non-zero with a clear message and NOT rewrite the configs.
func TestRefreshScriptErrorsWhenCurlMissing(test *testing.T) {
	bash := requireBash(test)
	work := test.TempDir()

	scriptBytes, err := RefreshScript(
		"http://host.microsandbox.internal:18787/v1", "sk-workspace-scoped-1234", "gemma4", refreshStatic, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	openCodeFile := filepath.Join(work, "opencode.json")
	piFile := filepath.Join(work, "models.json")
	rewritten := strings.NewReplacer(
		openCodeGuestPath, openCodeFile,
		piGuestPath, piFile,
	).Replace(string(scriptBytes))
	scriptPath := filepath.Join(work, "refresh-models")
	if err := os.WriteFile(scriptPath, []byte(rewritten), 0o755); err != nil {
		test.Fatal(err)
	}

	// Empty PATH (plus the bash dir so the interpreter line still resolves builtins
	// via the absolute bash we exec) so `command -v curl` fails.
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = []string{"PATH=" + work} // no curl anywhere
	out, err := cmd.CombinedOutput()
	if err == nil {
		test.Fatalf("refresh-models must fail when curl is missing; output:\n%s", out)
	}
	if !strings.Contains(string(out), "curl is not installed") {
		test.Errorf("missing the curl-not-installed message; got:\n%s", out)
	}
	if _, statErr := os.Stat(openCodeFile); statErr == nil {
		test.Error("refresh-models must not rewrite configs when curl is missing")
	}
}

// TestRefreshScriptTagsURLTrimsV1: the auth-free tags URL is the gateway origin
// without /v1, with the /ollama/api/tags path.
func TestRefreshScriptTagsURL(test *testing.T) {
	cases := map[string]string{
		"http://host.microsandbox.internal:18787/v1": "http://host.microsandbox.internal:18787/ollama/api/tags",
		"http://srv:9999/v1/":                        "http://srv:9999/ollama/api/tags",
		"http://srv:9999":                            "http://srv:9999/ollama/api/tags",
	}
	for input, want := range cases {
		if got := tagsURL(input); got != want {
			test.Errorf("tagsURL(%q) = %q, want %q", input, got, want)
		}
	}
}
