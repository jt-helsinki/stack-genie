// Package litellm renders the LiteLLM gateway config (arch §14–15) and exposes a
// client for `ai models status|test`. LiteLLM is a thin shared gateway in the
// catalog-driven model system: the rendered config carries NO model_list — models
// are DB-backed (store_model_in_db), added/removed over the admin API as the user
// adds provider keys (`ai keys`) or pulls/removes Ollama models. Provider API keys
// are stored encrypted in the LiteLLM DB (LITELLM_SALT_KEY), never written to
// platform disk or this config. There is no built-in default model.
package litellm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"gopkg.in/yaml.v3"
)

// OllamaAPIBase is where LiteLLM reaches the platform's Ollama container on the
// shared docker network (aip-net), used as the api_base for ollama/* models.
const OllamaAPIBase = "http://aip-ollama:11434"

// HeadroomAPIBase is where LiteLLM reaches the Headroom input-compression service
// on the shared docker network (aip-net). Headroom is no longer an nginx proxy in
// FRONT of LiteLLM — it is a LiteLLM pre_call GUARDRAIL: LiteLLM POSTs the request
// messages to {HeadroomAPIBase}/v1/compress in-process before dispatch (see the
// headroom-compression guardrail in buildGuardrails). Requires LiteLLM v1.92.x+.
const HeadroomAPIBase = "http://aip-headroom:8787"

// Routing is the platform's model routing. The named handles + per-provider
// wildcards were intentionally REMOVED in the catalog-driven model system
// (Phase B): models are now DB-backed (added via /model/new, see models_admin.go
// + reconcile.go) and there is NO built-in default model — the user adds provider
// keys / pulls Ollama models and the sync engine registers them. The struct is
// retained (zero-valued) so the few callers that still take a Routing keep a
// stable signature; both fields are empty.
type Routing struct {
	Default string            `yaml:"default" json:"default"`
	Aliases map[string]string `yaml:"aliases" json:"aliases"`
}

// DefaultRouting returns the zero routing: no default model and no aliases. The
// catalog-driven system manages the model list in the DB, so nothing is baked in
// here. Kept for the callers that still pass a Routing through (workspace agent
// config, refresh script) — they degrade to "no static handles", sourcing the
// in-VM picker from exactly the models the gateway currently serves (its live
// DB-backed model list), with no curated cloud seed.
func DefaultRouting() Routing {
	return Routing{}
}

// ConfigPath returns config/litellm/config.yaml.
func ConfigPath() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "litellm", "config.yaml"), nil
}

// Render writes the LiteLLM config. When providerConfigPath is set (e.g. the
// acceptance harness's mock-provider config, or a real provider config), its
// contents are used verbatim (guardrails come from that file); otherwise a config is
// generated from routing with placeholder credentials, rendering ONLY the enabled
// guardrails (the user's `ai setup` selection — DefaultGuardrails when none).
func Render(routing Routing, providerConfigPath string, guardrails []string) error {
	destination, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}

	_ = routing // routing is now empty (DB-backed models); kept for signature stability

	var rendered []byte
	if providerConfigPath != "" {
		rendered, err = os.ReadFile(providerConfigPath)
		if err != nil {
			return fmt.Errorf("provider config: %w", err)
		}
	} else {
		rendered, err = yaml.Marshal(build(guardrails))
		if err != nil {
			return err
		}
	}
	// Docker creates a DIRECTORY at the config path when it is bind-mounted
	// (`-v <path>:/app/config.yaml`) before this file is ever rendered — which then
	// makes LiteLLM crash with IsADirectoryError. Replace such a directory so the
	// render (and the subsequent mount) yields a real file.
	if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
		if err := os.RemoveAll(destination); err != nil {
			return err
		}
	}
	return os.WriteFile(destination, rendered, 0o644)
}

// build renders the LiteLLM config in the catalog-driven (DB-backed) model
// system: there is NO model_list — models are added via /model/new and persisted
// in the DB (general_settings.store_model_in_db: true). The guardrails + the
// in-process prompt-injection callback are unchanged (always-on). With no static
// model_list there is also no default_model (the user adds keys / pulls Ollama and
// the sync engine registers models).
func build(guardrails []string) map[string]any {
	return map[string]any{
		// store_model_in_db persists models added over the admin API (/model/new)
		// and their stored credentials in the Postgres DB (encrypted at rest by
		// LITELLM_SALT_KEY) so they survive restarts — the catalog-driven model
		// system manages the model list here, not in this file.
		"general_settings": map[string]any{
			"store_model_in_db": true,
		},
		// Response caching backed by the Valkey (Redis-compatible) container — a STANDARD,
		// standalone SINGLE instance on the shared network (aip-valkey:6379, no auth). Per
		// the LiteLLM caching quick-start (docs.litellm.ai/docs/proxy/caching), the proxy
		// reads the connection from the REDIS_HOST/REDIS_PORT ENV VARS (set on the LiteLLM
		// container in litellmRunArgs) automatically; config.yaml only enables the cache
		// and selects the redis backend. This is the documented, reliable path — literal
		// host/port inside cache_params did not initialize the cache.
		"litellm_settings": map[string]any{
			"cache": true,
			"cache_params": map[string]any{
				"type": "redis",
			},
		},
		// NOTE: the in-process prompt-injection detector (detect_prompt_injection) was
		// REMOVED. It is a crude local heuristic (similarity to known attack strings)
		// that false-positives on ordinary coding traffic — including normal Ollama
		// requests, which it rejected with "400: Rejected message. This is a prompt
		// injection attack." Like the removed PII masking and LLM Guard, it corrupted
		// legitimate use, so it is gone. For a CODING agent the real risk is destructive
		// TOOL EXECUTION, which the tool_permission firewall (buildGuardrails) handles
		// when enabled; prompt-content scanning is low-value and high-false-positive here.
		// The rendered guardrail set is the user's `ai setup` selection (Headroom by
		// default) — see buildGuardrails.
		"guardrails": buildGuardrails(guardrails),
	}
}

// secretEntities is the Presidio entity set the platform masks — deliberately
// scoped to unambiguous financial/identity SECRETS only. General PII (PERSON,
// LOCATION, DATE_TIME, EMAIL_ADDRESS, …) is intentionally NOT masked: a coding
// agent's prompts legitimately contain names, places, paths and emails, and
// masking them corrupts the prompt before the model sees it (e.g. "capital of
// France" → "capital of <LOCATION>"). Credentials/API keys/tokens are handled
// separately by the hide-secrets guardrail (detect-secrets).
var secretEntities = []string{
	"CREDIT_CARD",
	"US_SSN",
	"US_BANK_NUMBER",
	"IBAN_CODE",
	"CRYPTO",
}

// destructiveCommandPatterns is the set of Python-regex-compatible (RE2-safe, no
// lookahead) fragments matched against a shell tool-call's command argument by the
// tool-firewall guardrail (see buildGuardrails). Each fragment targets one
// destructive command family. Edit this list to tune the firewall — it is composed
// into one alternation regex per param path. These are deliberately conservative
// (high-signal, low-false-positive) destructive operations a coding agent should
// not run unattended.
var destructiveCommandPatterns = []string{
	`rm\s+-[a-z]*(rf|fr)`,                 // rm -rf / rm -fr (any flag order)
	`git\s+push\s+.*(--force|-f)\b`,       // git push --force / -f
	`git\s+reset\s+--hard`,                // git reset --hard
	`git\s+clean\s+-[a-z]*f`,              // git clean -…f
	`terraform\s+destroy`,                 // terraform destroy
	`terraform\s+apply\s+.*-auto-approve`, // unattended terraform apply
	`kubectl\s+delete`,                    // kubectl delete
	`\bdd\s+if=`,                          // dd if=… (disk overwrite)
	`\bmkfs`,                              // mkfs (filesystem create/wipe)
}

// destructiveCommandRegex is the alternation regex composed from
// destructiveCommandPatterns, used as the tool-firewall's allowed_param_patterns
// value for the command arg. LiteLLM's tool_permission guardrail matches params
// with re.FULLMATCH (verified against its source), so the alternation is wrapped
// `.*( … ).*` to fullmatch a command that CONTAINS a destructive fragment, and the
// `s` flag makes `.` span newlines (multi-line command strings). A shell tool-call
// is denied when its command argument fullmatches this (default_action: allow lets
// everything else through).
func destructiveCommandRegex() string {
	return "(?is).*(" + strings.Join(destructiveCommandPatterns, "|") + ").*"
}

// shellToolNameRegex matches the shell-tool NAMES the supported agent CLIs expose,
// verified against their tool schemas: opencode/pi `bash`, claude-code `Bash`
// (case-insensitive), codex `shell`/`shell_command`/`exec_command`, gemini
// `run_shell_command`. hardware bring-up: confirm these against the LIVE tool
// schemas on a provisioned host — a destructive call under an unmatched tool name
// slips the firewall (see docs/HARDWARE-BRINGUP.md).
const shellToolNameRegex = `(?i)^(bash|shell|sh|run|run_command|run_shell_command|shell_command|exec_command|execute|exec|command|terminal)$`

// commandParamPaths are the tool-call argument paths that carry the shell command
// across the supported agents, in LiteLLM tool_permission's flattened dot/`[]`
// notation (verified against its source): `command` (opencode/pi/claude-code/gemini/
// codex shell_command — string), `command[]` (codex `shell` — array, matched
// per-element), and `cmd` (codex exec_command — string). NOT `arguments.command`,
// which matches nothing (the guardrail evaluates the PARSED arguments object).
var commandParamPaths = []string{"command", "command[]", "cmd"}

// Guardrail keys — the stable identifiers the user selects at `ai setup`, persisted
// in runtime.yaml (Info.Guardrails) and consumed by buildGuardrails. GuardrailSecretMasking
// is ONE selectable option that expands to the presidio input+output config pair.
const (
	GuardrailHeadroom      = "headroom"       // Headroom input-compression (DEFAULT ON)
	GuardrailSecretMasking = "secret-masking" // Presidio secret masking (input + output)
	GuardrailHideSecrets   = "hide-secrets"   // detect-secrets API-key/token stripping
	GuardrailToolFirewall  = "tool-firewall"  // tool_permission destructive-command firewall
)

// Guardrail is one selectable LiteLLM guardrail the `ai setup` picker offers. Key is
// the stable identifier (persisted); Title/Description drive the picker; DefaultOn
// marks the guardrails enabled out of the box — only Headroom (the user opts the rest
// in). A guardrail is "installed" only when its key is in the enabled set passed to
// buildGuardrails; an unselected one is not rendered into the gateway config at all
// (so a disabled guardrail never references a backend that isn't running).
type Guardrail struct {
	Key         string
	Title       string
	Description string
	DefaultOn   bool
}

// Guardrails lists the selectable guardrails in `ai setup` picker order. The platform
// ships only fully self-hostable, zero-config-token guardrails (no Hub tokens, no cloud
// APIs). Presidio's input+output pair is ONE option (buildGuardrails expands it).
// Guardrails AI remains deferred (needs a Hub token + manual per-guard install).
var Guardrails = []Guardrail{
	{
		Key:         GuardrailHeadroom,
		Title:       "Input compression (Headroom)",
		Description: "Compresses request messages via the Headroom service before dispatch to cut token use.",
		DefaultOn:   true,
	},
	{
		Key:         GuardrailSecretMasking,
		Title:       "Secret masking (Presidio)",
		Description: "Masks financial/identity secrets (credit card, SSN, IBAN, …) out of prompts and responses. Requires the Presidio containers.",
		DefaultOn:   false,
	},
	{
		Key:         GuardrailHideSecrets,
		Title:       "API-key / token detection (detect-secrets)",
		Description: "LiteLLM's in-process secret detector — strips API keys/tokens/credentials from prompts. No external server.",
		DefaultOn:   false,
	},
	{
		Key:         GuardrailToolFirewall,
		Title:       "Destructive-command tool firewall",
		Description: "Denies destructive command tool-calls (rm -rf, git push --force, terraform destroy, kubectl delete, …) at the gateway.",
		DefaultOn:   false,
	},
}

// DefaultGuardrails returns the guardrail keys enabled by default (Headroom only). A
// runtime.yaml with no persisted selection (a legacy install, or a client) falls back
// to this.
func DefaultGuardrails() []string {
	keys := make([]string, 0, 1)
	for _, guardrail := range Guardrails {
		if guardrail.DefaultOn {
			keys = append(keys, guardrail.Key)
		}
	}
	return keys
}

// GuardrailKeys returns every selectable guardrail key (for input validation).
func GuardrailKeys() []string {
	keys := make([]string, 0, len(Guardrails))
	for _, guardrail := range Guardrails {
		keys = append(keys, guardrail.Key)
	}
	return keys
}

// buildGuardrails renders ONLY the enabled guardrails (arch §17) — the set the user
// chose at `ai setup` (persisted in runtime.yaml), defaulting to Headroom. Each
// rendered guardrail is default_on:true (active on every request; because every route,
// cloud included, traverses the proxy, cloud calls are covered too); an UNSELECTED
// guardrail is omitted entirely, so a disabled one never references a backend that
// isn't running. The order matches the `Guardrails` catalog. A nil/empty enabled set
// renders no guardrails.
func buildGuardrails(enabled []string) []map[string]any {
	on := make(map[string]bool, len(enabled))
	for _, key := range enabled {
		on[key] = true
	}
	// Mask only the scoped secret entities, at a high confidence threshold to
	// avoid false positives on ordinary code/text.
	entityActions := make(map[string]any, len(secretEntities))
	for _, entity := range secretEntities {
		entityActions[entity] = "MASK"
	}
	scoredThresholds := map[string]any{"DEFAULT": 0.6}

	guardrails := make([]map[string]any, 0, len(Guardrails)+1)
	// Secret masking (Presidio): pre_call masks secrets out of the prompt before the
	// model sees them; post_call masks them out of the response. Restricted to
	// secretEntities. Backed by the analyzer/anonymizer containers reached via
	// PRESIDIO_*_API_BASE on the LiteLLM container (see setup_real.go) — started only
	// when this guardrail is enabled.
	if on[GuardrailSecretMasking] {
		guardrails = append(guardrails,
			map[string]any{
				"guardrail_name": "presidio-secrets-input",
				"litellm_params": map[string]any{
					"guardrail":                 "presidio",
					"mode":                      "pre_call",
					"default_on":                true,
					"presidio_filter_scope":     "input",
					"pii_entities_config":       entityActions,
					"presidio_score_thresholds": scoredThresholds,
				},
			},
			map[string]any{
				"guardrail_name": "presidio-secrets-output",
				"litellm_params": map[string]any{
					"guardrail":                 "presidio",
					"mode":                      "post_call",
					"default_on":                true,
					"presidio_filter_scope":     "output",
					"pii_entities_config":       entityActions,
					"presidio_score_thresholds": scoredThresholds,
				},
			},
		)
	}
	// hide-secrets: LiteLLM's in-process secret detector (bundled detect-secrets) —
	// strips API keys/tokens/credentials from the prompt. No external server.
	if on[GuardrailHideSecrets] {
		guardrails = append(guardrails, map[string]any{
			"guardrail_name": "hide-secrets",
			"litellm_params": map[string]any{
				"guardrail":  "hide-secrets",
				"mode":       "pre_call",
				"default_on": true,
			},
		})
	}
	// tool-firewall: LiteLLM's tool_permission guardrail. default_action allow
	// (everything permitted) EXCEPT the per-path deny rules; on_disallowed_action block
	// rejects the response when a destructive shell tool-call is matched.
	if on[GuardrailToolFirewall] {
		guardrails = append(guardrails, map[string]any{
			"guardrail_name": "tool-firewall",
			"litellm_params": map[string]any{
				"guardrail":            "tool_permission",
				"mode":                 "post_call",
				"default_on":           true,
				"default_action":       "allow",
				"on_disallowed_action": "block",
				"rules":                toolFirewallRules(),
			},
		})
	}
	// headroom-compression: input compression. LiteLLM POSTs the request messages to
	// {api_base}/v1/compress and swaps in the compressed result before dispatch. Headroom
	// is a standalone service on aip-net (HeadroomAPIBase), NOT an nginx proxy in front
	// of LiteLLM. Requires LiteLLM v1.92.x+.
	if on[GuardrailHeadroom] {
		guardrails = append(guardrails, map[string]any{
			"guardrail_name": "headroom-compression",
			"litellm_params": map[string]any{
				"guardrail":  "headroom",
				"mode":       "pre_call",
				"api_base":   HeadroomAPIBase,
				"default_on": true,
			},
		})
	}
	return guardrails
}

// toolFirewallRules builds one tool_permission deny rule per command param path
// (commandParamPaths). Each matches the shell-tool name regex and denies when the
// command argument at that path fullmatches a destructive command.
func toolFirewallRules() []map[string]any {
	rules := make([]map[string]any, 0, len(commandParamPaths))
	for _, path := range commandParamPaths {
		ruleID := "deny-destructive-" + strings.NewReplacer("[]", "-array", ".", "-").Replace(path)
		rules = append(rules, map[string]any{
			"id":        ruleID,
			"tool_name": shellToolNameRegex,
			"decision":  "deny",
			"allowed_param_patterns": map[string]any{
				path: destructiveCommandRegex(),
			},
		})
	}
	return rules
}

// Model is one model the LiteLLM gateway currently serves, as reported by the
// gateway itself (/model/info or /v1/models) — NOT the hardcoded DefaultRouting.
// Name is the served model_name/id (which may be a provider wildcard like
// `openai/*`); Provider is the prefix before the first `/` (e.g. "ollama",
// "openai"), empty for a bare alias with no prefix; Mode is the served model's
// mode (e.g. "chat", "embedding") when /model/info exposes it, else empty.
type Model struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// DisplayModels collapses the live served-model list to the set worth SHOWING a
// human: it drops any concrete model whose provider already has a
// `<provider>/*` wildcard entry, because a named alias (gemma4, claude-opus, …)
// is just a concrete model under its provider's wildcard — listing both is
// redundant. The wildcards themselves are kept, as are concrete models whose
// provider has no wildcard. The input order is preserved.
//
// The full live list stays available on StatusInfo.Models (and in the --json
// envelope); only the human/TUI rendering is filtered through this helper.
func DisplayModels(models []Model) []Model {
	hasWildcard := make(map[string]bool, len(models))
	for _, model := range models {
		if strings.HasSuffix(model.Name, "/*") {
			hasWildcard[model.Provider] = true
		}
	}
	display := make([]Model, 0, len(models))
	for _, model := range models {
		// Keep the wildcard entries themselves; drop concrete models already
		// covered by their provider's wildcard.
		if !strings.HasSuffix(model.Name, "/*") && hasWildcard[model.Provider] {
			continue
		}
		display = append(display, model)
	}
	return display
}

// StatusInfo is the result of `ai models status` (CLI §8.1).
type StatusInfo struct {
	Healthy bool `json:"healthy"`
	// Providers is the DISTINCT set of provider prefixes derived from the LIVE
	// served-model list (Models), not from the hardcoded routing.
	Providers []string `json:"providers"`
	// Default is the platform's configured default-model handle. The gateway's
	// model-list endpoints do NOT mark a default, so this legitimately stays from
	// platform config (DefaultRouting().Default), not from the live list.
	Default string `json:"default"`
	Ollama  bool   `json:"ollama"`
	// Models is the LIVE list of models the gateway serves, sourced from LiteLLM's
	// /model/info//v1/models endpoints. Empty when the gateway is unreachable or the
	// model-list call failed (see ModelsNote).
	Models []Model `json:"models,omitempty"`
	// ModelsNote explains why Models is empty when the gateway is otherwise reachable
	// (e.g. the model-list call was unauthorized or failed) — so status still renders
	// health rather than erroring out. Empty when the list was fetched fine.
	ModelsNote string `json:"models_note,omitempty"`
	// BaseURL is the gateway endpoint the status was probed against (for the
	// human-readable rendering); omitted from JSON when empty.
	BaseURL string `json:"base_url,omitempty"`
}

// statusIndent is the left padding used to align a continuation/detail line under
// the value column of `ai models status` (the labels are rendered left of it). It is
// the single source of that alignment, reused by every detail/continuation line.
const statusIndent = "                  "

// Human renders `ai models status` as a labeled, actionable summary rather than a
// raw field dump: gateway reachability (with a fix hint when it is down), the
// default model, the local-model (Ollama, no key) vs cloud-provider (needs a key)
// split, and how to probe a model. The per-section rendering is delegated to small
// helpers so this stays a simple sequence of appends.
func (info StatusInfo) Human() string {
	var builder strings.Builder
	builder.WriteString(info.humanGatewayLine())
	builder.WriteString("\n")
	if info.Default != "" {
		builder.WriteString(ui.Label.Render("Default model") + "     " + ui.Value.Render(info.Default) + ui.Muted.Render("  (used unless an agent names another)") + "\n")
	}
	if info.Ollama {
		builder.WriteString(ui.Label.Render("Local models") + "      " + ui.Value.Render("Ollama") + ui.Muted.Render(" — no API key needed (install models with `") + ui.Primary.Render("ollama pull <name>") + ui.Muted.Render("`)") + "\n")
	}
	builder.WriteString(info.humanCloudProviders())
	builder.WriteString(info.humanServedModels())
	builder.WriteString("\n")
	builder.WriteString(info.humanProbeHint())
	return builder.String()
}

// humanGatewayLine renders the gateway-reachability line (plus the fix hint when it
// is down).
func (info StatusInfo) humanGatewayLine() string {
	endpoint := ""
	if info.BaseURL != "" {
		endpoint = " (" + ui.Value.Render(info.BaseURL) + ")"
	}
	if info.Healthy {
		return ui.Label.Render("LiteLLM gateway") + "   " + ui.Success.Render(ui.IconOK+" reachable") + endpoint + "\n"
	}
	return ui.Label.Render("LiteLLM gateway") + "   " + ui.Failure.Render(ui.IconFail+" not reachable") + endpoint + "\n" +
		statusIndent + ui.Muted.Render(ui.IconArrow+" start it with `") + ui.Primary.Render("ai services start") + ui.Muted.Render("`, then `") + ui.Primary.Render("ai doctor") + ui.Muted.Render("` (or `") + ui.Primary.Render("ai setup") + ui.Muted.Render("` on first run)") + "\n"
}

// humanCloudProviders renders the cloud-provider summary (the providers that need a
// key), or "" when there are none.
func (info StatusInfo) humanCloudProviders() string {
	cloud := make([]string, 0, len(info.Providers))
	for _, provider := range info.Providers {
		if provider != "ollama" && provider != "" {
			cloud = append(cloud, provider)
		}
	}
	if len(cloud) == 0 {
		return ""
	}
	return ui.Label.Render("Cloud providers") + "   " + ui.Value.Render(strings.Join(cloud, ", ")) + "\n" +
		statusIndent + ui.Muted.Render("each needs a key once: `") + ui.Primary.Render("ai keys add <provider>") + ui.Muted.Render("`") + "\n"
}

// humanServedModels renders the LIVE served-model list, straight from the gateway
// (not the hardcoded routing) — collapsed via DisplayModels so concrete models
// already covered by their provider's `*/` wildcard are dropped. When the filtered
// set is empty, the block is omitted. When the gateway is up but the list could not
// be fetched, the note is shown instead. Returns "" when there is nothing to show.
func (info StatusInfo) humanServedModels() string {
	display := DisplayModels(info.Models)
	switch {
	case len(display) > 0:
		var builder strings.Builder
		builder.WriteString("\n")
		builder.WriteString(ui.Label.Render("Served models") + "     " + ui.Muted.Render("(live from the gateway)") + "\n")
		for _, model := range display {
			builder.WriteString(servedModelLine(model))
		}
		return builder.String()
	case len(info.Models) == 0 && info.ModelsNote != "":
		return "\n" + ui.Label.Render("Served models") + "     " + ui.Muted.Render(info.ModelsNote) + "\n"
	default:
		return ""
	}
}

// servedModelLine renders one served-model row (name + an optional "(provider,
// mode)" descriptor).
func servedModelLine(model Model) string {
	line := statusIndent + ui.Value.Render(model.Name)
	descriptor := model.Provider
	if model.Mode != "" {
		if descriptor != "" {
			descriptor += ", "
		}
		descriptor += model.Mode
	}
	if descriptor != "" {
		line += "  " + ui.Muted.Render("("+descriptor+")")
	}
	return line + "\n"
}

// humanProbeHint renders the closing "probe a model with …" hint.
func (info StatusInfo) humanProbeHint() string {
	probeModel := info.Default
	if probeModel == "" {
		probeModel = "<model>"
	}
	if info.Healthy {
		return ui.Muted.Render("Probe a model with `") + ui.Primary.Render("ai models test "+probeModel) + ui.Muted.Render("`.")
	}
	return ui.Muted.Render("Once the gateway is up, probe a model with `") + ui.Primary.Render("ai models test "+probeModel) + ui.Muted.Render("`.")
}

// TestResult is the result of `ai models test` (CLI §8.2). When OK is false,
// Status carries the gateway/provider HTTP status and Error the provider's error
// message (so the user learns *why* — bad model, missing key, provider down).
type TestResult struct {
	Model     string `json:"model"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Human renders the success line for `ai models test` (the failure path is
// rendered by the CLI as an error with an actionable hint).
func (result TestResult) Human() string {
	return ui.Success.Render(ui.IconOK) + " " + ui.Value.Render(result.Model) + " reachable via LiteLLM " + ui.Muted.Render(fmt.Sprintf("(%dms)", result.LatencyMS))
}

// Client talks to the running LiteLLM gateway. The real impl makes HTTP calls;
// tests use a fake.
type Client interface {
	Status() (StatusInfo, error)
	Test(model string) (TestResult, error)
	// Models returns the LIVE list of models the gateway currently serves, sourced
	// from LiteLLM's own endpoints (not the hardcoded DefaultRouting). It returns a
	// non-nil error when the gateway is unreachable or the call is unauthorized, so
	// callers can map an exit code or degrade gracefully.
	Models() ([]Model, error)
}
