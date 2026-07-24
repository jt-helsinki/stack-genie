package agentcfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
// PATH. The fake echoes the canned /model/info JSON (the gateway's served-model set
// with per-model tool support) when reachable and fails (exit 7, like real curl's
// "couldn't connect") otherwise — so a test can simulate both the reachable and
// unreachable gateway. The script invokes curl with `-fsS --max-time 10 -H
// 'Authorization: Bearer …' <INFO_URL>`, so the fake ignores its args and emits the body.
func writeFakeCurl(test *testing.T, dir, modelsJSON string, reachable bool) {
	test.Helper()
	var body string
	if reachable {
		body = "cat <<'JSON'\n" + modelsJSON + "\nJSON\nexit 0\n"
	} else {
		body = "exit 7\n"
	}
	script := "#!/usr/bin/env bash\n" + body
	path := filepath.Join(dir, "curl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		test.Fatal(err)
	}
}

// runRefresh writes the generated script to a temp dir, redirects the config path
// at a temp file, and runs it with binDir prepended to PATH so the fake curl is
// found. It returns the rewritten opencode config bytes plus whether the file
// was written (the degrade-to-untouched path writes nothing).
func runRefresh(test *testing.T, binDir string) (openCode []byte, ranOK bool) {
	test.Helper()
	bash := requireBash(test)

	work := test.TempDir()
	scriptBytes, err := RefreshScript(
		"http://host.microsandbox.internal:18787/v1",
		"sk-workspace-scoped-1234",
		"",
		5, 8000,
	)
	if err != nil {
		test.Fatal(err)
	}

	// Redirect the guest path to a temp file. The script hard-codes the guest
	// path; rather than write to /home/workspace we rewrite the path literal.
	openCodeFile := filepath.Join(work, "opencode.json")
	rewritten := strings.NewReplacer(
		openCodeGuestPath, openCodeFile,
	).Replace(string(scriptBytes))

	scriptPath := filepath.Join(work, "refresh-models")
	if err := os.WriteFile(scriptPath, []byte(rewritten), 0o755); err != nil {
		test.Fatal(err)
	}

	cmd := exec.Command(bash, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, runErr := cmd.CombinedOutput()
	test.Logf("refresh-models output:\n%s", out)
	ranOK = runErr == nil

	openCode, _ = os.ReadFile(openCodeFile)
	return openCode, ranOK
}

// TestRefreshScriptParityWithGenerators executes the generated script with a fake
// curl returning a served-model list, and asserts the rewritten config is
// BYTE-IDENTICAL to OpenCodeConfig for the deduped+sorted served list —
// so an in-VM refresh matches a fresh workspace start.
func TestRefreshScriptParityWithGenerators(test *testing.T) {
	binDir := test.TempDir()
	// The /model/info shape: {"data":[{"model_name":"…","model_info":{...}}, …]}.
	// Deliberately unsorted + with a duplicate so the script's dedup+sort is exercised,
	// and with MIXED tool support: an Ollama model marked non-tool
	// (supports_function_calling:false), a tool-capable Ollama model (true), and a cloud
	// model that OMITS the field (unknown → treated as tool-capable) — so both the
	// tool_call:true and tool_call:false item variants are exercised.
	models := `{"data":[` +
		`{"model_name":"ollama/qwen2.5:7b","model_info":{"id":"a","supports_function_calling":false}},` +
		`{"model_name":"anthropic/claude-opus-4-8","model_info":{"id":"b"}},` +
		`{"model_name":"ollama/llama3.2:latest","model_info":{"id":"c","supports_function_calling":true}},` +
		`{"model_name":"anthropic/claude-opus-4-8","model_info":{"id":"b"}}` +
		`]}`
	writeFakeCurl(test, binDir, models, true)

	gotOpenCode, ranOK := runRefresh(test, binDir)
	if !ranOK {
		test.Fatal("refresh-models must succeed when the gateway is reachable")
	}

	// The expected set: deduped + sorted by name exactly as pickerModels, with tool
	// support from supports_function_calling (absent = tool-capable).
	merged := []Model{
		{Name: "anthropic/claude-opus-4-8", Tools: true},
		{Name: "ollama/llama3.2:latest", Tools: true},
		{Name: "ollama/qwen2.5:7b", Tools: false},
	}

	// refresh-models rewrites a KEYLESS config (the {env:}/$VAR refs), never the literal
	// scoped key — so parity is against the generator called with the key ref.
	wantOpenCode, err := OpenCodeConfig(
		"http://host.microsandbox.internal:18787/v1", OpenCodeAPIKeyRef, "", merged, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}

	if string(gotOpenCode) != string(wantOpenCode) {
		test.Fatalf("opencode.json not byte-identical to OpenCodeConfig:\n--- got ---\n%s\n--- want ---\n%s", gotOpenCode, wantOpenCode)
	}
	// Security invariant: the rewritten (host-disk, project) config is KEYLESS — the
	// scoped key used for the fetch must NEVER be baked into it.
	if strings.Contains(string(gotOpenCode), "sk-workspace-scoped-1234") {
		test.Fatal("refresh-models must not write the scoped key into the on-disk config")
	}
}

// TestRefreshScriptDegradesWhenGatewayUnreachable: when the /v1/models fetch fails
// the script exits non-zero and leaves the existing configs UNTOUCHED (it does not
// wipe them to an empty list).
func TestRefreshScriptDegradesWhenGatewayUnreachable(test *testing.T) {
	binDir := test.TempDir()
	writeFakeCurl(test, binDir, "", false) // curl exits non-zero for every URL

	gotOpenCode, ranOK := runRefresh(test, binDir)
	if ranOK {
		test.Fatal("refresh-models must exit non-zero when the gateway is unreachable")
	}
	if len(gotOpenCode) != 0 {
		test.Fatalf("an unreachable gateway must leave the config untouched (wrote opencode=%dB)", len(gotOpenCode))
	}
}

// TestRefreshScriptErrorsWhenCurlMissing: with no curl on PATH the script must exit
// non-zero with a clear message and NOT rewrite the configs.
func TestRefreshScriptErrorsWhenCurlMissing(test *testing.T) {
	bash := requireBash(test)
	work := test.TempDir()

	scriptBytes, err := RefreshScript(
		"http://host.microsandbox.internal:18787/v1", "sk-workspace-scoped-1234", "", 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	openCodeFile := filepath.Join(work, "opencode.json")
	rewritten := strings.NewReplacer(
		openCodeGuestPath, openCodeFile,
	).Replace(string(scriptBytes))
	scriptPath := filepath.Join(work, "refresh-models")
	if err := os.WriteFile(scriptPath, []byte(rewritten), 0o755); err != nil {
		test.Fatal(err)
	}

	// Empty PATH (plus the work dir) so `command -v curl` fails.
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

// TestRefreshScriptModelInfoURL: the model-info endpoint is the gateway ROOT (with the
// /v1 suffix and any trailing slash trimmed) plus /llm/model/info.
func TestRefreshScriptModelInfoURL(test *testing.T) {
	cases := map[string]string{
		"http://host.microsandbox.internal:18787/v1": "http://host.microsandbox.internal:18787/llm/model/info",
		"http://srv:9999/v1/":                        "http://srv:9999/llm/model/info",
		"http://srv:9999":                            "http://srv:9999/llm/model/info",
	}
	for input, want := range cases {
		if got := modelInfoURL(input); got != want {
			test.Errorf("modelInfoURL(%q) = %q, want %q", input, got, want)
		}
	}
}
