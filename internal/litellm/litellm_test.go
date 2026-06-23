package litellm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderDefaultRouting(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Render(DefaultRouting(), ""); err != nil {
		test.Fatal(err)
	}
	p, _ := ConfigPath()
	b, err := os.ReadFile(p)
	if err != nil {
		test.Fatal(err)
	}
	var cfg struct {
		ModelList []struct {
			ModelName     string `yaml:"model_name"`
			LitellmParams struct {
				Model  string `yaml:"model"`
				APIKey string `yaml:"api_key"`
			} `yaml:"litellm_params"`
		} `yaml:"model_list"`
		LitellmSettings struct {
			DefaultModel string   `yaml:"default_model"`
			Callbacks    []string `yaml:"callbacks"`
		} `yaml:"litellm_settings"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		test.Fatalf("rendered config is not valid yaml: %v\n%s", err, b)
	}
	// The default model is the local Ollama backend (provider = ollama).
	if cfg.LitellmSettings.DefaultModel != "gemma4" {
		test.Fatalf("default_model = %q, want gemma4", cfg.LitellmSettings.DefaultModel)
	}
	// Prompt-injection is the IN-PROCESS detector (replaces the removed LLM Guard).
	if len(cfg.LitellmSettings.Callbacks) != 1 || cfg.LitellmSettings.Callbacks[0] != "detect_prompt_injection" {
		test.Fatalf("litellm_settings.callbacks = %v, want [detect_prompt_injection]", cfg.LitellmSettings.Callbacks)
	}
	byName := map[string]string{}
	keyByName := map[string]string{}
	for _, entry := range cfg.ModelList {
		byName[entry.ModelName] = entry.LitellmParams.Model
		keyByName[entry.ModelName] = entry.LitellmParams.APIKey
	}
	if byName["gpt-5.5"] != "openai/gpt-5.5" {
		test.Fatalf("gpt-5.5 -> %q", byName["gpt-5.5"])
	}
	// Credentials are placeholders only (never real values), arch §17.
	if keyByName["gpt-5.5"] != "os.environ/OPENAI_API_KEY" {
		test.Fatalf("gpt-5.5 api_key = %q, want placeholder", keyByName["gpt-5.5"])
	}
	if byName["claude-opus"] != "anthropic/claude-opus-4-8" {
		test.Fatalf("claude-opus -> %q, want anthropic/claude-opus-4-8", byName["claude-opus"])
	}
	if byName["gemini-pro"] != "gemini/gemini-3.5-flash" {
		test.Fatalf("gemini-pro -> %q, want gemini/gemini-3.5-flash", byName["gemini-pro"])
	}

	// Full catalogue: each provider exposes a wildcard so any model is routable
	// without enumerating it. Cloud wildcards carry the placeholder key; Ollama
	// needs none.
	for alias, wantKey := range map[string]string{
		"ollama/*":    "",
		"openai/*":    "os.environ/OPENAI_API_KEY",
		"anthropic/*": "os.environ/ANTHROPIC_API_KEY",
		"gemini/*":    "os.environ/GEMINI_API_KEY",
		"groq/*":      "os.environ/GROQ_API_KEY",
	} {
		if byName[alias] != alias {
			test.Errorf("wildcard %q -> %q, want %q", alias, byName[alias], alias)
		}
		if keyByName[alias] != wantKey {
			test.Errorf("wildcard %q api_key = %q, want %q", alias, keyByName[alias], wantKey)
		}
	}
	// Ollama needs no credential.
	if keyByName["gemma4"] != "" {
		test.Fatalf("ollama alias should have no api_key, got %q", keyByName["gemma4"])
	}
	if byName["gemma4"] != "ollama/gemma4:31b" {
		test.Fatalf("gemma4 -> %q, want ollama/gemma4:31b", byName["gemma4"])
	}

	// Always-on guardrails: Presidio pre/post + hide-secrets (secret masking,
	// unchanged) and the tool-firewall (tool_permission) — all default_on, so no
	// request, cloud included, can bypass them (arch §17).
	var guards struct {
		Guardrails []struct {
			GuardrailName string `yaml:"guardrail_name"`
			LitellmParams struct {
				Guardrail          string `yaml:"guardrail"`
				Mode               string `yaml:"mode"`
				DefaultOn          bool   `yaml:"default_on"`
				FilterScope        string `yaml:"presidio_filter_scope"`
				DefaultAction      string `yaml:"default_action"`
				OnDisallowedAction string `yaml:"on_disallowed_action"`
				Rules              []struct {
					ID                   string            `yaml:"id"`
					ToolName             string            `yaml:"tool_name"`
					Decision             string            `yaml:"decision"`
					AllowedParamPatterns map[string]string `yaml:"allowed_param_patterns"`
				} `yaml:"rules"`
			} `yaml:"litellm_params"`
		} `yaml:"guardrails"`
	}
	if err := yaml.Unmarshal(b, &guards); err != nil {
		test.Fatalf("guardrails not valid yaml: %v", err)
	}
	// Secret masking (unchanged) + the tool-firewall — all always-on, all
	// self-hostable (no Hub tokens / cloud APIs).
	wantBackends := map[string]string{
		"presidio-secrets-input":  "presidio",
		"presidio-secrets-output": "presidio",
		"hide-secrets":            "hide-secrets",
		"tool-firewall":           "tool_permission",
	}
	if len(guards.Guardrails) != len(wantBackends) {
		test.Fatalf("guardrails = %d, want %d", len(guards.Guardrails), len(wantBackends))
	}
	var toolFirewallSeen bool
	for _, guard := range guards.Guardrails {
		wantBackend, known := wantBackends[guard.GuardrailName]
		if !known {
			test.Errorf("unexpected guardrail %q", guard.GuardrailName)
			continue
		}
		if guard.LitellmParams.Guardrail != wantBackend {
			test.Errorf("guardrail %q backend = %q, want %q", guard.GuardrailName, guard.LitellmParams.Guardrail, wantBackend)
		}
		if !guard.LitellmParams.DefaultOn {
			test.Errorf("guardrail %q not default_on (would be bypassable)", guard.GuardrailName)
		}
		if guard.GuardrailName != "tool-firewall" {
			continue
		}
		toolFirewallSeen = true
		// The tool firewall allows by default and blocks the matched destructive
		// tool-calls.
		if guard.LitellmParams.DefaultAction != "allow" {
			test.Errorf("tool-firewall default_action = %q, want allow", guard.LitellmParams.DefaultAction)
		}
		if guard.LitellmParams.OnDisallowedAction != "block" {
			test.Errorf("tool-firewall on_disallowed_action = %q, want block", guard.LitellmParams.OnDisallowedAction)
		}
		if len(guard.LitellmParams.Rules) == 0 {
			test.Fatalf("tool-firewall has no deny rules")
		}
		// At least one deny rule's command pattern must carry a destructive token.
		var sawDestructive bool
		for _, rule := range guard.LitellmParams.Rules {
			if rule.Decision != "deny" {
				test.Errorf("tool-firewall rule %q decision = %q, want deny", rule.ID, rule.Decision)
			}
			for _, pattern := range rule.AllowedParamPatterns {
				if strings.Contains(pattern, "rm") && strings.Contains(pattern, "rf") {
					sawDestructive = true
				}
			}
		}
		if !sawDestructive {
			test.Errorf("tool-firewall has no rule whose command pattern matches a destructive token (e.g. rm -rf)")
		}
	}
	if !toolFirewallSeen {
		test.Errorf("tool-firewall guardrail missing")
	}
}

// TestDestructiveCommandRegexBlocksOnlyDestructive guards the tool-firewall's core
// behavior: LiteLLM's tool_permission matches allowed_param_patterns with
// re.fullmatch, so the regex MUST fullmatch a command that contains a destructive
// fragment (else the deny rule never fires — the bug that made the firewall
// fail-open) and MUST NOT fullmatch benign commands (else false blocks). We
// emulate re.fullmatch with \A…\z anchors.
func TestDestructiveCommandRegexBlocksOnlyDestructive(test *testing.T) {
	fullmatch := regexp.MustCompile(`\A(?:` + destructiveCommandRegex() + `)\z`)

	blocked := []string{
		"rm -rf /",
		"sudo rm -rf --no-preserve-root /tmp/x",
		"git push --force origin main",
		"git push -f",
		"git reset --hard HEAD~3",
		"terraform destroy -auto-approve",
		"kubectl delete namespace prod",
		"echo building && rm -rf build",
		"dd if=/dev/zero of=/dev/sda",
	}
	for _, command := range blocked {
		if !fullmatch.MatchString(command) {
			test.Errorf("destructive command NOT matched (would slip the firewall): %q", command)
		}
	}

	allowed := []string{
		"ls -la",
		"git status",
		"git push origin main",
		"echo hello world",
		"go test ./...",
		"kubectl get pods",
		"terraform plan",
	}
	for _, command := range allowed {
		if fullmatch.MatchString(command) {
			test.Errorf("benign command WRONGLY matched (false block): %q", command)
		}
	}
}

func TestRenderProviderConfigPassthrough(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	src := filepath.Join(home, "mock-litellm.yaml")
	want := "model_list: [{model_name: gpt-5, litellm_params: {model: openai/gpt-5}}]\n"
	if err := os.WriteFile(src, []byte(want), 0o644); err != nil {
		test.Fatal(err)
	}
	if err := Render(DefaultRouting(), src); err != nil {
		test.Fatal(err)
	}
	p, _ := ConfigPath()
	got, _ := os.ReadFile(p)
	if string(got) != want {
		test.Fatalf("provider config not passed through verbatim:\n got %q\nwant %q", got, want)
	}
}

func TestProvidersAndOllama(test *testing.T) {
	routing := DefaultRouting()
	providers := providersOf(routing)
	if len(providers) == 0 {
		test.Fatal("expected providers")
	}
	if !hasOllama(routing) {
		test.Fatal("default routing includes an ollama alias")
	}
}

func TestStatusInfoHumanUnreachable(test *testing.T) {
	info := StatusInfo{
		Healthy:   false,
		Providers: []string{"anthropic", "ollama", "openai"},
		Default:   "gemma4",
		Ollama:    true,
		BaseURL:   "http://127.0.0.1:14000",
	}
	rendered := info.Human()
	for _, fragment := range []string{
		"not reachable",            // clear down state
		"http://127.0.0.1:14000",   // where
		"ai services start",        // how to fix
		"Default model     gemma4", // labeled, not a raw dump
		"Ollama",                   // local models split out
		"anthropic, openai",        // cloud providers, ollama removed from the list
		"ai secrets set",           // cloud needs a key
		"ai models test gemma4",    // next step
	} {
		if !strings.Contains(rendered, fragment) {
			test.Errorf("status Human() missing %q:\n%s", fragment, rendered)
		}
	}
	// ollama must NOT appear in the cloud-providers line.
	if strings.Contains(rendered, "anthropic, ollama") {
		test.Errorf("ollama should be shown as a local model, not in the cloud list:\n%s", rendered)
	}
}

func TestStatusInfoHumanReachable(test *testing.T) {
	info := StatusInfo{Healthy: true, Providers: []string{"openai"}, Default: "gemma4", Ollama: true, BaseURL: "http://127.0.0.1:14000"}
	rendered := info.Human()
	if !strings.Contains(rendered, "✓ reachable") {
		test.Errorf("expected a reachable marker:\n%s", rendered)
	}
	if strings.Contains(rendered, "ai services start") {
		test.Errorf("a healthy gateway should not show the start-it hint:\n%s", rendered)
	}
}

func TestParseProviderError(test *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"model 'ollama/nope' not found","type":"not_found"}}`: "model 'ollama/nope' not found",
		`{"error":{"message":"  Invalid API key  "}}`:                              "Invalid API key",
		`Bad Gateway`: "Bad Gateway",
		``:            "",
	}
	for body, want := range cases {
		if got := parseProviderError([]byte(body)); got != want {
			test.Errorf("parseProviderError(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestTestSurfacesProviderError(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"error":{"message":"model not found: ollama/nope"}}`))
	}))
	defer server.Close()

	client := realClient{baseURL: server.URL, httpClient: server.Client()}
	result, err := client.Test("ollama/nope")
	if err != nil {
		test.Fatalf("transport error not expected: %v", err)
	}
	if result.OK {
		test.Fatal("expected OK=false for a 404")
	}
	if result.Status != http.StatusNotFound {
		test.Errorf("status = %d, want 404", result.Status)
	}
	if result.Error != "model not found: ollama/nope" {
		test.Errorf("error = %q, want the provider message", result.Error)
	}
}
