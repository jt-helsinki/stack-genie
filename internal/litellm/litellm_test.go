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

// TestRenderDefaultRouting verifies the catalog-driven (DB-backed) config: NO
// model_list and NO default_model (models are added via /model/new), but
// general_settings.store_model_in_db: true so the added models persist, NO
// prompt-injection callback (removed — it false-positived on coding traffic), and
// the guardrails. Rendered here with EVERY guardrail enabled (GuardrailKeys) so the
// per-guardrail assertions below exercise all of them; the ENABLED-SET selection
// behaviour (default = Headroom only) is covered by TestBuildGuardrailsSelection.
func TestRenderDefaultRouting(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Render(DefaultRouting(), "", GuardrailKeys()); err != nil {
		test.Fatal(err)
	}
	p, _ := ConfigPath()
	b, err := os.ReadFile(p)
	if err != nil {
		test.Fatal(err)
	}
	var cfg struct {
		ModelList       []map[string]any `yaml:"model_list"`
		GeneralSettings struct {
			StoreModelInDB bool `yaml:"store_model_in_db"`
		} `yaml:"general_settings"`
		LitellmSettings struct {
			DefaultModel string   `yaml:"default_model"`
			Callbacks    []string `yaml:"callbacks"`
		} `yaml:"litellm_settings"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		test.Fatalf("rendered config is not valid yaml: %v\n%s", err, b)
	}
	// No baked-in model_list — models are DB-backed (added via /model/new).
	if len(cfg.ModelList) != 0 {
		test.Errorf("model_list should be empty (DB-backed models), got %v", cfg.ModelList)
	}
	// store_model_in_db must be true so /model/new persists.
	if !cfg.GeneralSettings.StoreModelInDB {
		test.Errorf("general_settings.store_model_in_db = false, want true")
	}
	// No default model in the catalog-driven system.
	if cfg.LitellmSettings.DefaultModel != "" {
		test.Errorf("default_model = %q, want empty (no default model)", cfg.LitellmSettings.DefaultModel)
	}
	// The in-process prompt-injection callback was REMOVED (it false-positived on
	// ordinary coding/local-model traffic — "Rejected message. This is a prompt injection
	// attack."), so no callback is configured.
	if len(cfg.LitellmSettings.Callbacks) != 0 {
		test.Fatalf("litellm_settings.callbacks = %v, want none (detect_prompt_injection removed)", cfg.LitellmSettings.Callbacks)
	}

	// Always-on guardrails: Presidio pre/post + hide-secrets (secret masking,
	// unchanged), the tool-firewall (tool_permission), and headroom-compression
	// (input compression, guardrail: headroom) — all default_on, so no request,
	// cloud included, can bypass them (arch §17).
	var guards struct {
		Guardrails []struct {
			GuardrailName string `yaml:"guardrail_name"`
			LitellmParams struct {
				Guardrail          string `yaml:"guardrail"`
				Mode               string `yaml:"mode"`
				DefaultOn          bool   `yaml:"default_on"`
				APIBase            string `yaml:"api_base"`
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
		"headroom-compression":    "headroom",
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
		// headroom-compression is a pre_call guardrail pointed at the standalone
		// Headroom service; LiteLLM POSTs to {api_base}/v1/compress.
		if guard.GuardrailName == "headroom-compression" {
			if guard.LitellmParams.Mode != "pre_call" {
				test.Errorf("headroom-compression mode = %q, want pre_call", guard.LitellmParams.Mode)
			}
			if guard.LitellmParams.APIBase != "http://aip-headroom:8787" {
				test.Errorf("headroom-compression api_base = %q, want http://aip-headroom:8787", guard.LitellmParams.APIBase)
			}
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

// renderedGuardrailNames renders a config with the given enabled guardrail set and
// returns the guardrail_name values in the config, in order.
func renderedGuardrailNames(test *testing.T, enabled []string) []string {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	if err := Render(DefaultRouting(), "", enabled); err != nil {
		test.Fatal(err)
	}
	path, _ := ConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	var cfg struct {
		Guardrails []struct {
			GuardrailName string `yaml:"guardrail_name"`
		} `yaml:"guardrails"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		test.Fatalf("rendered config not valid yaml: %v\n%s", err, raw)
	}
	names := make([]string, 0, len(cfg.Guardrails))
	for _, guard := range cfg.Guardrails {
		names = append(names, guard.GuardrailName)
	}
	return names
}

// TestBuildGuardrailsSelection: only the ENABLED guardrails are rendered — the default
// (Headroom only), a chosen subset, secret-masking's input+output expansion, and the
// empty (all-off) set. An unselected guardrail must not appear at all (so it never
// references a backend that isn't running).
func TestBuildGuardrailsSelection(test *testing.T) {
	// DefaultGuardrails = Headroom only.
	if got := DefaultGuardrails(); len(got) != 1 || got[0] != GuardrailHeadroom {
		test.Fatalf("DefaultGuardrails() = %v, want [%q]", got, GuardrailHeadroom)
	}
	if got := renderedGuardrailNames(test, DefaultGuardrails()); len(got) != 1 || got[0] != "headroom-compression" {
		test.Errorf("default render = %v, want [headroom-compression]", got)
	}
	// A subset: tool-firewall + headroom. No presidio/hide-secrets entries.
	got := renderedGuardrailNames(test, []string{GuardrailToolFirewall, GuardrailHeadroom})
	want := map[string]bool{"tool-firewall": true, "headroom-compression": true}
	if len(got) != len(want) {
		test.Fatalf("subset render = %v, want the 2 selected guardrails", got)
	}
	for _, name := range got {
		if !want[name] {
			test.Errorf("subset render included unselected guardrail %q: %v", name, got)
		}
	}
	// secret-masking is ONE option that expands to the presidio input+output pair.
	masking := renderedGuardrailNames(test, []string{GuardrailSecretMasking})
	if len(masking) != 2 {
		test.Fatalf("secret-masking render = %v, want the 2 presidio entries", masking)
	}
	for _, name := range masking {
		if name != "presidio-secrets-input" && name != "presidio-secrets-output" {
			test.Errorf("secret-masking rendered unexpected guardrail %q", name)
		}
	}
	// The empty (all-off) set renders no guardrails.
	if got := renderedGuardrailNames(test, []string{}); len(got) != 0 {
		test.Errorf("empty selection render = %v, want no guardrails", got)
	}
}

// TestRenderEnablesStandaloneRedisCache pins the response cache config: enabled with a
// standalone redis backend. Per the LiteLLM quick-start the connection comes from the
// REDIS_HOST/REDIS_PORT env on the container (asserted in the setup package), so
// cache_params carries ONLY the type — NO host/port and NO redis_startup_nodes (aip-valkey
// is a standalone single instance, not a cluster).
func TestRenderEnablesStandaloneRedisCache(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := Render(DefaultRouting(), "", DefaultGuardrails()); err != nil {
		test.Fatal(err)
	}
	path, _ := ConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	var cfg struct {
		LitellmSettings struct {
			Cache       bool `yaml:"cache"`
			CacheParams struct {
				Type              string           `yaml:"type"`
				Host              string           `yaml:"host"`
				Port              string           `yaml:"port"`
				RedisStartupNodes []map[string]any `yaml:"redis_startup_nodes"`
			} `yaml:"cache_params"`
		} `yaml:"litellm_settings"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		test.Fatalf("rendered config is not valid yaml: %v\n%s", err, raw)
	}
	if !cfg.LitellmSettings.Cache {
		test.Errorf("litellm_settings.cache = false, want true")
	}
	if cfg.LitellmSettings.CacheParams.Type != "redis" {
		test.Errorf("cache_params.type = %q, want redis", cfg.LitellmSettings.CacheParams.Type)
	}
	// Standalone: no cluster startup nodes, and no literal host/port (the connection is
	// supplied via REDIS_HOST/REDIS_PORT env).
	if len(cfg.LitellmSettings.CacheParams.RedisStartupNodes) != 0 {
		test.Errorf("cache_params.redis_startup_nodes = %+v, want none (standalone, not cluster)", cfg.LitellmSettings.CacheParams.RedisStartupNodes)
	}
	if cfg.LitellmSettings.CacheParams.Host != "" || cfg.LitellmSettings.CacheParams.Port != "" {
		test.Errorf("cache_params should carry only type (connection via REDIS_* env), got host=%q port=%q",
			cfg.LitellmSettings.CacheParams.Host, cfg.LitellmSettings.CacheParams.Port)
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
	// providerConfigPath set → the file is used verbatim; the guardrail selection
	// (here nil) is ignored, so the passed-through model_list survives untouched.
	if err := Render(DefaultRouting(), src, nil); err != nil {
		test.Fatal(err)
	}
	p, _ := ConfigPath()
	got, _ := os.ReadFile(p)
	if string(got) != want {
		test.Fatalf("provider config not passed through verbatim:\n got %q\nwant %q", got, want)
	}
}

func TestStatusInfoHumanUnreachable(test *testing.T) {
	info := StatusInfo{
		Healthy:   false,
		Providers: []string{"anthropic", "vllm", "openai"},
		Default:   "gemma4",
		Local:     true,
		BaseURL:   "http://127.0.0.1:14000",
	}
	rendered := info.Human()
	for _, fragment := range []string{
		"not reachable",            // clear down state
		"http://127.0.0.1:14000",   // where
		"ai services start",        // how to fix
		"Default model     gemma4", // labeled, not a raw dump
		"vLLM",                     // local models split out
		"anthropic, openai",        // cloud providers, vllm removed from the list
		"ai keys add",              // cloud needs a key
		"ai models test gemma4",    // next step
	} {
		if !strings.Contains(rendered, fragment) {
			test.Errorf("status Human() missing %q:\n%s", fragment, rendered)
		}
	}
	// vllm must NOT appear in the cloud-providers line.
	if strings.Contains(rendered, "anthropic, vllm") {
		test.Errorf("vllm should be shown as a local model, not in the cloud list:\n%s", rendered)
	}
}

func TestStatusInfoHumanReachable(test *testing.T) {
	info := StatusInfo{Healthy: true, Providers: []string{"openai"}, Default: "gemma4", Local: true, BaseURL: "http://127.0.0.1:14000"}
	rendered := info.Human()
	if !strings.Contains(rendered, "✓ reachable") {
		test.Errorf("expected a reachable marker:\n%s", rendered)
	}
	if strings.Contains(rendered, "ai services start") {
		test.Errorf("a healthy gateway should not show the start-it hint:\n%s", rendered)
	}
}

// TestStatusInfoHumanServedModels verifies the Human renderer lists the LIVE
// served models (with provider/mode descriptors), and falls back to the note when
// the list could not be fetched.
func TestStatusInfoHumanServedModels(test *testing.T) {
	withModels := StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"anthropic", "vllm"},
		Local:     true,
		BaseURL:   "http://127.0.0.1:14000",
		Models: []Model{
			{Name: "gemma4", Provider: "vllm", Mode: "chat"},
			{Name: "openai/*", Provider: "openai"},
		},
	}
	rendered := withModels.Human()
	for _, fragment := range []string{"Served models", "gemma4", "vllm, chat", "openai/*", "(openai)"} {
		if !strings.Contains(rendered, fragment) {
			test.Errorf("served-models render missing %q:\n%s", fragment, rendered)
		}
	}

	withNote := StatusInfo{Healthy: true, Default: "gemma4", BaseURL: "http://127.0.0.1:14000",
		ModelsNote: "could not list served models: unauthorized"}
	noteRendered := withNote.Human()
	if !strings.Contains(noteRendered, "could not list served models") {
		test.Errorf("expected the model-list note in the render:\n%s", noteRendered)
	}
}

// TestDisplayModelsCollapsesWildcards verifies the display-set rule: concrete
// models whose provider has a `<provider>/*` wildcard are dropped, the wildcards
// themselves are kept, and concrete models with no wildcard are kept.
func TestDisplayModelsCollapsesWildcards(test *testing.T) {
	models := []Model{
		{Name: "anthropic/*", Provider: "anthropic"},
		{Name: "claude-opus", Provider: "anthropic"}, // covered by anthropic/* → dropped
		{Name: "openai/*", Provider: "openai"},
		{Name: "gpt-5.5", Provider: "openai"}, // covered by openai/* → dropped
		{Name: "gemma4", Provider: "vllm"},    // no vllm/* wildcard → kept
	}
	display := DisplayModels(models)
	got := make([]string, 0, len(display))
	for _, model := range display {
		got = append(got, model.Name)
	}
	want := []string{"anthropic/*", "openai/*", "gemma4"}
	if len(got) != len(want) {
		test.Fatalf("DisplayModels = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			test.Fatalf("DisplayModels = %v, want %v", got, want)
		}
	}
}

// When every served model collapses under a wildcard, Human omits the served-models
// block entirely but still shows the providers line.
func TestStatusInfoHumanOmitsServedWhenAllCollapse(test *testing.T) {
	info := StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"anthropic", "openai"},
		BaseURL:   "http://127.0.0.1:14000",
		Models: []Model{
			{Name: "anthropic/*", Provider: "anthropic"},
			{Name: "claude-opus", Provider: "anthropic"},
		},
	}
	rendered := info.Human()
	// anthropic/* is a wildcard, so the block is NOT empty; the alias is collapsed.
	if strings.Contains(rendered, "claude-opus") {
		test.Errorf("alias under anthropic/* should be collapsed:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Cloud providers") {
		test.Errorf("providers line must remain:\n%s", rendered)
	}

	// A set that is ENTIRELY concrete-under-wildcard with no surviving entries (an
	// empty display) drops the Served-models block.
	allCollapsed := StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"openai"},
		BaseURL:   "http://127.0.0.1:14000",
		Models:    []Model{}, // nothing served
	}
	if strings.Contains(allCollapsed.Human(), "Served models") {
		test.Errorf("Served-models block must be omitted when the display set is empty:\n%s", allCollapsed.Human())
	}
	if !strings.Contains(allCollapsed.Human(), "Cloud providers") {
		test.Errorf("providers line must remain even with no served models:\n%s", allCollapsed.Human())
	}
}

func TestParseProviderError(test *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"model 'vllm/nope' not found","type":"not_found"}}`: "model 'vllm/nope' not found",
		`{"error":{"message":"  Invalid API key  "}}`:                            "Invalid API key",
		`Bad Gateway`: "Bad Gateway",
		``:            "",
	}
	for body, want := range cases {
		if got := parseProviderError([]byte(body)); got != want {
			test.Errorf("parseProviderError(%q) = %q, want %q", body, got, want)
		}
	}
}

// TestModelsParsesModelInfo verifies Models() parses LiteLLM's /model/info shape
// (model_name + litellm_params.model for the provider prefix + model_info.mode),
// handles wildcard ids gracefully, and sends the Bearer master key.
func TestModelsParsesModelInfo(test *testing.T) {
	// Inject a master key so the Bearer header must be sent (production reads it from
	// the live container; tests must not).
	original := resolveMasterKey
	resolveMasterKey = func() string { return "sk-test-key" }
	defer func() { resolveMasterKey = original }()

	var gotAuth string
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotAuth = request.Header.Get("Authorization")
		gotPath = request.URL.Path
		_, _ = writer.Write([]byte(`{"data":[
			{"model_name":"vllm/gemma4","litellm_params":{"model":"openai/gemma4"},"model_info":{"mode":"chat"}},
			{"model_name":"gpt-5.5","litellm_params":{"model":"anthropic/claude"},"model_info":{"mode":"chat"}},
			{"model_name":"openai/*","litellm_params":{"model":"openai/*"},"model_info":{}},
			{"model_name":"text-embed","litellm_params":{"model":"openai/text-embedding-3"},"model_info":{"mode":"embedding"}}
		]}`))
	}))
	defer server.Close()

	client := realClient{adminURL: server.URL, gatewayURL: server.URL, httpClient: server.Client()}
	models, err := client.Models()
	if err != nil {
		test.Fatalf("Models() error: %v", err)
	}
	if gotPath != "/model/info" {
		test.Errorf("queried %q, want /model/info", gotPath)
	}
	if gotAuth != "Bearer sk-test-key" {
		test.Errorf("Authorization = %q, want the Bearer master key", gotAuth)
	}
	// Sorted by name: gpt-5.5, openai/*, text-embed, vllm/gemma4.
	byName := map[string]Model{}
	for _, model := range models {
		byName[model.Name] = model
	}
	// A vLLM model's routed target is openai/<alias>, so its derived Provider is openai.
	if got := byName["vllm/gemma4"]; got.Provider != "openai" || got.Mode != "chat" {
		test.Errorf("vllm/gemma4 = %+v, want provider openai mode chat", got)
	}
	if got := byName["gpt-5.5"]; got.Provider != "anthropic" {
		test.Errorf("gpt-5.5 provider = %q, want anthropic", got.Provider)
	}
	// Wildcard id handled gracefully: provider derived from the prefix.
	if got := byName["openai/*"]; got.Provider != "openai" {
		test.Errorf("openai/* provider = %q, want openai", got.Provider)
	}
	if got := byName["text-embed"]; got.Mode != "embedding" {
		test.Errorf("text-embed mode = %q, want embedding", got.Mode)
	}

	// Providers derived from the LIVE list (not hardcoded routing).
	providers := providersFromModels(models)
	if strings.Join(providers, ",") != "anthropic,openai" {
		test.Errorf("providers = %v, want [anthropic openai]", providers)
	}
	if !hasLocalModel(models) {
		test.Error("expected a local vLLM served model")
	}
}

// TestModelsUnauthorized verifies a 401/403 from the gateway returns a clean error
// (so the CLI can map an exit code / show a note rather than crashing).
func TestModelsUnauthorized(test *testing.T) {
	original := resolveMasterKey
	resolveMasterKey = func() string { return "" }
	defer func() { resolveMasterKey = original }()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"Authentication Error"}}`))
	}))
	defer server.Close()

	client := realClient{adminURL: server.URL, gatewayURL: server.URL, httpClient: server.Client()}
	if _, err := client.Models(); err == nil {
		test.Fatal("expected an error for a 401")
	} else if !strings.Contains(err.Error(), "unauthorized") {
		test.Errorf("error = %v, want it to mention unauthorized", err)
	}
}

// TestStatusFetchesLiveModels verifies Status() reports the LIVE served list +
// derived providers when the gateway is healthy, while the default stays the
// platform handle.
func TestStatusFetchesLiveModels(test *testing.T) {
	original := resolveMasterKey
	resolveMasterKey = func() string { return "" }
	defer func() { resolveMasterKey = original }()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health/liveliness":
			writer.WriteHeader(http.StatusOK)
		case "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"vllm/gemma4","litellm_params":{"model":"openai/gemma4"},"model_info":{"mode":"chat"}},
				{"model_name":"claude-opus","litellm_params":{"model":"anthropic/claude-opus-4-8"},"model_info":{"mode":"chat"}}
			]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := realClient{adminURL: server.URL, gatewayURL: server.URL, httpClient: server.Client()}
	info, err := client.Status()
	if err != nil {
		test.Fatalf("Status() error: %v", err)
	}
	if !info.Healthy {
		test.Fatal("gateway should be healthy")
	}
	if info.Default != DefaultRouting().Default {
		test.Errorf("default = %q, want the platform handle %q", info.Default, DefaultRouting().Default)
	}
	if len(info.Models) != 2 {
		test.Fatalf("served models = %d, want 2", len(info.Models))
	}
	if strings.Join(info.Providers, ",") != "anthropic,openai" {
		test.Errorf("providers = %v, want [anthropic openai] derived from the live list", info.Providers)
	}
	if !info.Local {
		test.Error("Local should be true (a vLLM-backed model is served)")
	}
}

// TestStatusHealthyButModelListFails verifies that when the gateway is reachable
// but the model-list call fails, Status() still reports health and records a note
// rather than erroring out.
func TestStatusHealthyButModelListFails(test *testing.T) {
	original := resolveMasterKey
	resolveMasterKey = func() string { return "" }
	defer func() { resolveMasterKey = original }()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health/liveliness":
			writer.WriteHeader(http.StatusOK)
		default: // /model/info → 500
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := realClient{adminURL: server.URL, gatewayURL: server.URL, httpClient: server.Client()}
	info, err := client.Status()
	if err != nil {
		test.Fatalf("Status() must not error when only the model list fails: %v", err)
	}
	if !info.Healthy {
		test.Error("gateway should still be healthy")
	}
	if len(info.Models) != 0 {
		test.Errorf("models should be empty on a failed list, got %v", info.Models)
	}
	if info.ModelsNote == "" {
		test.Error("expected a ModelsNote explaining the failed model list")
	}
}

func TestTestSurfacesProviderError(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"error":{"message":"model not found: vllm/nope"}}`))
	}))
	defer server.Close()

	client := realClient{adminURL: server.URL, gatewayURL: server.URL, httpClient: server.Client()}
	result, err := client.Test("vllm/nope")
	if err != nil {
		test.Fatalf("transport error not expected: %v", err)
	}
	if result.OK {
		test.Fatal("expected OK=false for a 404")
	}
	if result.Status != http.StatusNotFound {
		test.Errorf("status = %d, want 404", result.Status)
	}
	if result.Error != "model not found: vllm/nope" {
		test.Errorf("error = %q, want the provider message", result.Error)
	}
}

// TestBaseURLsRouteThroughNginxGateway verifies the host CLI reaches LiteLLM ONLY
// through the nginx gateway (never the container at :4000): the admin surface on
// /llm and the chat path on /v1, both on host :18787 at 127.0.0.1 (NOT localhost →
// ::1, which would refuse the dial). LITELLM_BASE_URL overrides the admin base and
// the chat base derives from it (swapping /llm → /v1).
func TestBaseURLsRouteThroughNginxGateway(test *testing.T) {
	test.Setenv("LITELLM_BASE_URL", "")
	if got := AdminBaseURL(); got != "http://127.0.0.1:18787/llm" {
		test.Errorf("admin base = %q, want the nginx /llm route on 127.0.0.1", got)
	}
	if got := GatewayBaseURL(); got != "http://127.0.0.1:18787/v1" {
		test.Errorf("gateway base = %q, want the nginx /v1 route on 127.0.0.1", got)
	}
	// An override of the admin base keeps the chat base on the matching gateway.
	test.Setenv("LITELLM_BASE_URL", "http://gw.lan:9999/llm")
	if got := AdminBaseURL(); got != "http://gw.lan:9999/llm" {
		test.Errorf("admin base override = %q", got)
	}
	if got := GatewayBaseURL(); got != "http://gw.lan:9999/v1" {
		test.Errorf("gateway base derived from override = %q, want .../v1", got)
	}
}

// TestTestUsesGatewayChatPath verifies the chat probe (`ai models test`) hits the
// gateway /v1 chat path (Headroom → LiteLLM, the real model path), not the admin
// /llm base — so it exercises compression + guardrails.
func TestTestUsesGatewayChatPath(test *testing.T) {
	original := resolveMasterKey
	resolveMasterKey = func() string { return "" }
	defer func() { resolveMasterKey = original }()

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		_, _ = writer.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	// Admin base distinct from the gateway base, so the chat path is unambiguous.
	client := realClient{adminURL: server.URL + "/llm", gatewayURL: server.URL + "/v1", httpClient: server.Client()}
	if _, err := client.Test("gemma4"); err != nil {
		test.Fatalf("Test: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		test.Errorf("chat probe queried %q, want /v1/chat/completions (the gateway path)", gotPath)
	}
}
