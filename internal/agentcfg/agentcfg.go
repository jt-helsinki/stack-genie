// Package agentcfg renders the provider configuration the in-workspace agent
// CLIs (opencode, pi) need to talk to the host Headroom proxy through a scoped
// LiteLLM virtual key (arch §15, §17). At workspace start the platform mints a
// per-workspace virtual key and writes one provider file per agent CLI into the
// microVM so the agent reaches the host gateway with that key.
//
// These are pure generators: they marshal stable, indented JSON and never touch
// disk or any external tool, so they are exhaustively unit-tested. The virtual
// key flows host→VM only and is never written to platform disk.
package agentcfg

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ProviderID is the provider handle both agent CLIs use for the host gateway.
const ProviderID = "aip-gateway"

// OpenCodeConfig renders opencode's provider config (`~/.config/opencode/opencode.json`).
//
// opencode talks to the gateway as an OpenAI-compatible provider
// (`@ai-sdk/openai-compatible`). It CAN inject per-request body fields via each
// model's options (AI SDK providerOptions → request body), so the per-project
// Headroom knobs (keepTurns, outputBufferTokens) ride on every model as
// headroom_keep_turns / headroom_output_buffer_tokens. The baseURL carries the
// required /v1 suffix and apiKey carries the scoped virtual key.
//
// In the catalog-driven model system there is no built-in default model; the
// served list (from the live gateway) may even be empty until the user adds a
// provider key or pulls an Ollama model. defaultModel is therefore optional: when
// empty no top-level `model` is written and opencode falls back to its own
// default-model selection.
func OpenCodeConfig(gatewayURL, apiKey, defaultModel string, models []string, keepTurns, outputBufferTokens int) ([]byte, error) {
	modelEntries := make(map[string]any, len(models))
	for _, model := range models {
		modelEntries[model] = map[string]any{
			"name": model,
			"options": map[string]any{
				"headroom_keep_turns":           keepTurns,
				"headroom_output_buffer_tokens": outputBufferTokens,
			},
		}
	}
	document := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			ProviderID: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "AI Platform Gateway",
				"options": map[string]any{
					"baseURL": gatewayURL,
					"apiKey":  apiKey,
				},
				"models": modelEntries,
			},
		},
	}
	if defaultModel != "" {
		document["model"] = ProviderID + "/" + defaultModel
	}
	return marshalStable(document)
}

// PiConfig renders pi's provider config (`~/.pi/agent/models.json`).
//
// pi talks to the gateway as an OpenAI-completions provider. Unlike opencode, pi
// CANNOT inject per-request body fields, so the Headroom knobs are deliberately
// NOT written here — pi's requests fall back to Headroom's server-side defaults.
// Per-project compression tuning therefore applies to opencode only; pi uses the
// host Headroom defaults. The baseUrl carries the required /v1 suffix and apiKey
// carries the scoped virtual key.
func PiConfig(gatewayURL, apiKey, defaultModel string, models []string) ([]byte, error) {
	modelEntries := make([]map[string]any, 0, len(models))
	for _, model := range models {
		modelEntries = append(modelEntries, map[string]any{
			"id":   model,
			"name": model,
		})
	}
	document := map[string]any{
		"providers": map[string]any{
			ProviderID: map[string]any{
				"baseUrl": gatewayURL,
				"api":     "openai-completions",
				"apiKey":  apiKey,
				"models":  modelEntries,
			},
		},
	}
	return marshalStable(document)
}

// TmuxConfig renders the managed tmux configuration written to
// `/home/workspace/.tmux.conf` at workspace start. The platform's workspace
// session model is "tmux-transparent": persistent, reattachable per-CLI tmux
// sessions back `ai agent`/`ai attach`/`ai sessions`, but the user never types a
// tmux command. To keep tmux invisible to a casual user it hides the status bar;
// it enables the mouse so the wheel scrolls naturally through scrollback, sets
// vi-style copy-mode keys, and keeps a generous scrollback history.
//
// Terminal capabilities for modern TUI agent CLIs (OpenCode, …): the microVM is
// HEADLESS — there is no terminal emulator inside it. Each agent runs inside tmux
// over a PTY (`msb exec -t`) and its byte stream is rendered by the USER'S HOST
// terminal emulator (WezTerm/Ghostty/iTerm/…). The fix for correct rendering is
// therefore not a VM-side emulator but terminfo + tmux directives so the VM emits
// the right sequences THROUGH tmux:
//   - default-terminal "tmux-256color": the modern terminfo entry tmux advertises
//     to programs inside it (shipped by the image's ncurses-term package).
//   - terminal-features ",*:RGB": pass 24-bit truecolor through for ALL outer
//     terminals (the host emulator decides what it actually renders).
//   - terminal-features ",*:extkeys" + extended-keys on: forward CSI-u / kitty
//     extended key encodings so OpenCode's shift+enter and ctrl-combos survive the
//     tmux layer instead of being collapsed to legacy codes.
//   - escape-time 10: a short escape timeout so single Esc / Alt chords feel snappy
//     in a TUI rather than lagging behind tmux's key-sequence wait.
//
// These directives require tmux >= 3.2 (terminal-features, extended-keys); the
// shipped bases all satisfy this (debian trixie ~3.4, debian bookworm 3.3a,
// ubuntu 24.04 3.4, almalinux 10 ~3.4). Note (a known limitation): tmux does NOT
// proxy GPU/graphics protocols (kitty graphics, sixel), so a TUI's image-rendering
// features will not work inside the tmux session.
func TmuxConfig() []byte {
	return []byte(`# Managed by the AI Development Platform — tmux-transparent workspace sessions.
# Do not edit by hand; this file is rewritten on every workspace start.
set -g mouse on
set -g status off
setw -g mode-keys vi
set -g history-limit 50000

# Terminal capabilities so modern TUI agent CLIs render correctly THROUGH tmux.
# The VM is headless: the user's HOST terminal emulator draws the PTY stream, so
# these only need to make tmux emit/forward the right sequences (terminfo ships
# via the image's ncurses-term). Requires tmux >= 3.2.
set -g default-terminal "tmux-256color"
set -as terminal-features ",*:RGB"
set -as terminal-features ",*:extkeys"
set -s extended-keys on
set -sg escape-time 10
`)
}

// marshalStable produces deterministic, indented JSON. encoding/json sorts map
// keys, so the output is stable across runs (no spurious workspace-start diffs).
func marshalStable(document any) ([]byte, error) {
	return json.MarshalIndent(document, "", "  ")
}

// Guest paths the agent provider configs live at inside the workspace microVM.
// RefreshScript rewrites these two files in place. They mirror the constants the
// workspace package writes at start (openCodeGuestPath/piGuestPath).
const (
	openCodeGuestPath = "/home/workspace/.config/opencode/opencode.json"
	piGuestPath       = "/home/workspace/.pi/agent/models.json"
)

// modelSentinel is the per-model token the shell substitutes with each real model
// id. sentinelA/sentinelB are two distinct probe models the host-side splitter
// renders to derive the exact prefix / per-model item / separator / suffix from
// MarshalIndent's output, so the script reproduces Go's layout byte-for-byte. They
// are chosen so they never collide with a real model id or any other JSON token,
// and they sort so A precedes B (the splitter relies on render order).
const (
	modelSentinel = "@@AIP_MODEL@@"
	sentinelA     = "Aaipmodelprobe"
	sentinelB     = "Zaipmodelprobe"
)

// RefreshScript generates a self-contained POSIX shell script that, run INSIDE
// the workspace microVM, re-pulls the in-VM agent model picker WITHOUT restarting
// the microVM. It is installed at /usr/local/bin/refresh-models at workspace start
// (see internal/workspace). Live in-VM execution is a `hardware bring-up`
// verification item; the host-side generation and the script's own logic are
// fully unit-tested (the generated script is executed against a fake curl).
//
// In the catalog-driven model system the in-VM picker is exactly the set of models
// the LiteLLM gateway currently SERVES (its DB-backed models — a keyed provider's
// catalog models + the registered Ollama models). The script:
//   - requires curl (clear error + exit if absent);
//   - GETs <gatewayBaseURL>/models (the OpenAI-compatible list endpoint) with the
//     scoped virtual key, and extracts the served model ids PORTABLY (grep/sed over
//     the "id":"…" fields — no jq/python);
//   - dedups + sorts them (LC_ALL=C sort -u) exactly as workspace.pickerModels does,
//     so the result matches a fresh workspace start;
//   - rewrites opencode.json + pi models.json BYTE-IDENTICAL to what OpenCodeConfig
//     / PiConfig would produce for that served list, default, gateway, and key —
//     by splicing the model fragments into Go-rendered JSON skeletons;
//   - DEGRADES: if the fetch fails it leaves the existing configs untouched and
//     warns (it never wipes them to an empty list);
//   - prints a short human summary.
//
// gatewayBaseURL is the SAME url written into the agent configs (carrying the /v1
// suffix); the script GETs the /models endpoint at that base. apiKey is the scoped
// virtual key (host→VM only). defaultModel is optional (empty → no default written).
func RefreshScript(gatewayBaseURL, apiKey, defaultModel string, keepTurns, outputBufferTokens int) ([]byte, error) {
	// Render the two JSON skeletons with a single sentinel model so we can split
	// each into a prefix / per-model template / suffix the shell splices into. The
	// rendered fragments inherit MarshalIndent's exact indentation, guaranteeing
	// byte parity with OpenCodeConfig / PiConfig for the same merged list.
	openCode, err := splitSkeleton(func(models []string) ([]byte, error) {
		return OpenCodeConfig(gatewayBaseURL, apiKey, defaultModel, models, keepTurns, outputBufferTokens)
	})
	if err != nil {
		return nil, fmt.Errorf("render opencode skeleton: %w", err)
	}
	pi, err := splitSkeleton(func(models []string) ([]byte, error) {
		return PiConfig(gatewayBaseURL, apiKey, defaultModel, models)
	})
	if err != nil {
		return nil, fmt.Errorf("render pi skeleton: %w", err)
	}

	var script bytes.Buffer
	script.WriteString("#!/usr/bin/env bash\n")
	script.WriteString(`# Managed by the AI Development Platform — refresh the in-workspace agent model
# picker. Run this INSIDE the workspace after changing the served models on the host
# (add a provider key with 'ai keys', or pull/remove an Ollama model):
#   refresh-models
# It re-fetches the models the gateway currently SERVES (its DB-backed models) from
# the gateway's /v1/models endpoint and rewrites the agent CLI configs in place.
# Restart your agent CLI afterwards to pick up the new list.
# Do not edit by hand; this file is rewritten on every workspace start.
set -u

`)
	// The baked-in values. The gateway URL keeps its /v1 suffix (it is written into
	// the configs verbatim); the served-models list endpoint is <base>/models.
	script.WriteString("GATEWAY_URL=" + shellQuote(gatewayBaseURL) + "\n")
	script.WriteString("MODELS_URL=" + shellQuote(modelsURL(gatewayBaseURL)) + "\n")
	script.WriteString("API_KEY=" + shellQuote(apiKey) + "\n")
	script.WriteString("OPENCODE_PATH=" + shellQuote(openCodeGuestPath) + "\n")
	script.WriteString("PI_PATH=" + shellQuote(piGuestPath) + "\n\n")

	// The four JSON fragments per config, base64-encoded so arbitrary bytes
	// (newlines, quotes, indentation, the inter-entry separator) survive embedding
	// in the script unchanged.
	script.WriteString("OPENCODE_PREFIX=" + b64Literal(openCode.prefix) + "\n")
	script.WriteString("OPENCODE_ITEM=" + b64Literal(openCode.item) + "\n")
	script.WriteString("OPENCODE_SEP=" + b64Literal(openCode.separator) + "\n")
	script.WriteString("OPENCODE_SUFFIX=" + b64Literal(openCode.suffix) + "\n")
	script.WriteString("PI_PREFIX=" + b64Literal(pi.prefix) + "\n")
	script.WriteString("PI_ITEM=" + b64Literal(pi.item) + "\n")
	script.WriteString("PI_SEP=" + b64Literal(pi.separator) + "\n")
	script.WriteString("PI_SUFFIX=" + b64Literal(pi.suffix) + "\n\n")

	script.WriteString(refreshScriptBody)
	return script.Bytes(), nil
}

// refreshScriptBody is the fixed logic of the refresh script. It consumes the
// baked variables RefreshScript prepends. The MODEL_SENTINEL token in the item
// templates is replaced with each (already JSON-escaped) model id; the model ids
// here are simple (alias / "ollama/<name>" / "<provider>/<model>") so a literal
// substitution of the bare value is correct — and the unit test pins parity.
var refreshScriptBody = strings.NewReplacer("@@SENTINEL@@", modelSentinel).Replace(`SENTINEL='@@SENTINEL@@'

if ! command -v curl >/dev/null 2>&1; then
  echo "refresh-models: curl is not installed in this workspace — add 'curl' to the image and recreate the workspace" >&2
  exit 1
fi

# decode <base64> -> stdout (portable: busybox/coreutils/macOS all accept -d).
decode() { printf '%s' "$1" | base64 -d 2>/dev/null || printf '%s' "$1" | base64 --decode; }

# JSON-escape a single line for embedding as a bare value (backslash, quote).
json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# Fetch the models the gateway currently SERVES from its OpenAI-compatible
# /v1/models endpoint (authenticated with the scoped virtual key). Extract each
# "id":"…" PORTABLY (no jq/python): one served model id per line. If the fetch
# fails we leave the existing configs UNTOUCHED rather than wiping them.
if ! served_body="$(curl -fsS --max-time 10 -H "Authorization: Bearer $API_KEY" "$MODELS_URL" 2>/dev/null)"; then
  echo "refresh-models: could not reach the gateway model list ($MODELS_URL) — leaving the current configs unchanged" >&2
  exit 1
fi
served="$(printf '%s' "$served_body" \
  | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"' \
  | sed -e 's/.*:[[:space:]]*"//' -e 's/"$//')"

# Dedup + sort EXACTLY as pickerModels does (LC_ALL=C lexical sort, unique).
# Result: one served model id per line.
merged="$(printf '%s\n' "$served" | sed '/^$/d' | LC_ALL=C sort -u)"
merged_count="$(printf '%s\n' "$merged" | sed '/^$/d' | wc -l | tr -d ' ')"

# render <prefix-b64> <item-b64> <sep-b64> <suffix-b64> -> full JSON on stdout,
# splicing one decoded item per model (sentinel -> escaped model id) joined by the
# decoded inter-entry separator, exactly reproducing MarshalIndent's layout.
render() {
  prefix="$(decode "$1")"; item_tmpl="$(decode "$2")"; sep="$(decode "$3")"; suffix="$(decode "$4")"
  printf '%s' "$prefix"
  first=1
  while IFS= read -r model; do
    [ -z "$model" ] && continue
    esc="$(json_escape "$model")"
    item="${item_tmpl//$SENTINEL/$esc}"
    if [ "$first" -eq 1 ]; then first=0; else printf '%s' "$sep"; fi
    printf '%s' "$item"
  done <<EOF
$merged
EOF
  printf '%s' "$suffix"
}

write_config() {
  path="$1"; content="$2"
  mkdir -p "$(dirname "$path")" || return 1
  tmp="$path.refresh.$$"
  printf '%s' "$content" >"$tmp" || return 1
  mv "$tmp" "$path"
}

opencode_json="$(render "$OPENCODE_PREFIX" "$OPENCODE_ITEM" "$OPENCODE_SEP" "$OPENCODE_SUFFIX")"
pi_json="$(render "$PI_PREFIX" "$PI_ITEM" "$PI_SEP" "$PI_SUFFIX")"

write_config "$OPENCODE_PATH" "$opencode_json" || { echo "refresh-models: failed to write $OPENCODE_PATH" >&2; exit 1; }
write_config "$PI_PATH" "$pi_json" || { echo "refresh-models: failed to write $PI_PATH" >&2; exit 1; }

echo "refreshed: $merged_count served models — restart your agent CLI to pick them up"
`)

// skeleton is the decomposition of a config's JSON around its model collection:
// the constant prefix, the per-model item template (containing modelSentinel once
// per model-id occurrence), the constant inter-entry separator, and the constant
// suffix. The script emits prefix + item·sep·item·… + suffix, reproducing
// MarshalIndent's exact byte layout.
type skeleton struct {
	prefix    []byte
	item      []byte
	separator []byte
	suffix    []byte
}

// splitSkeleton derives the decomposition by rendering the config with two distinct
// probe models (sentinelA, sentinelB) and diffing the result against a single-model
// render. The common prefix/suffix are constant; the middle of the two-model render
// is item(A) + separator + item(B), and the single-model item locates where each
// item ends — so the separator is whatever MarshalIndent places BETWEEN entries
// (for a map: "},\n        " style; for an array: "},\n    " style — captured
// verbatim rather than assumed).
func splitSkeleton(render func(models []string) ([]byte, error)) (skeleton, error) {
	one, err := render([]string{modelSentinel})
	if err != nil {
		return skeleton{}, err
	}
	// sentinelA sorts before sentinelB, matching the script's sorted model order, so
	// the two-model render places A's entry first.
	two, err := render([]string{sentinelA, sentinelB})
	if err != nil {
		return skeleton{}, err
	}

	// The prefix is the longest common prefix of the one- and two-model renders; the
	// suffix is the longest common suffix. Both are constant across model count.
	prefix := commonPrefix(one, two)
	suffix := commonSuffix(one[len(prefix):], two[len(prefix):])

	// The single-model item is exactly the middle of the one-model render.
	item := one[len(prefix) : len(one)-len(suffix)]
	if bytes.Count(item, []byte(modelSentinel)) < 1 {
		return skeleton{}, fmt.Errorf("single-model item does not contain the model sentinel")
	}

	// The two-model middle is itemA + separator + itemB. Derive itemA/itemB by
	// substituting the sentinels into the known item template, then the separator is
	// what remains between them.
	middle := two[len(prefix) : len(two)-len(suffix)]
	itemA := bytes.ReplaceAll(item, []byte(modelSentinel), []byte(sentinelA))
	itemB := bytes.ReplaceAll(item, []byte(modelSentinel), []byte(sentinelB))
	if !bytes.HasPrefix(middle, itemA) || !bytes.HasSuffix(middle, itemB) {
		return skeleton{}, fmt.Errorf("two-model middle does not bracket the derived items")
	}
	separator := middle[len(itemA) : len(middle)-len(itemB)]

	return skeleton{prefix: clone(prefix), item: clone(item), separator: clone(separator), suffix: clone(suffix)}, nil
}

func clone(in []byte) []byte { return append([]byte{}, in...) }

// commonPrefix returns the longest common leading byte slice of a and b.
func commonPrefix(a, b []byte) []byte {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	index := 0
	for index < limit && a[index] == b[index] {
		index++
	}
	return a[:index]
}

// commonSuffix returns the longest common trailing byte slice of a and b.
func commonSuffix(a, b []byte) []byte {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	index := 0
	for index < limit && a[len(a)-1-index] == b[len(b)-1-index] {
		index++
	}
	return a[len(a)-index:]
}

// modelsURL derives the OpenAI-compatible served-models list endpoint from the
// gateway base URL the agent configs use. The configs carry the /v1 suffix, so the
// list endpoint is <base>/models (e.g. ".../v1" → ".../v1/models"). A trailing
// slash on the base is tolerated.
func modelsURL(gatewayBaseURL string) string {
	return strings.TrimRight(gatewayBaseURL, "/") + "/models"
}

// shellQuote single-quotes a value for safe embedding in the script, escaping any
// embedded single quotes the POSIX way ('\”).
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// b64Literal renders bytes as a single-quoted base64 string literal for the script
// (so arbitrary JSON bytes — newlines, quotes, indentation — embed unchanged). The
// in-VM script decodes it with `base64 -d`.
func b64Literal(raw []byte) string {
	return "'" + base64.StdEncoding.EncodeToString(raw) + "'"
}
