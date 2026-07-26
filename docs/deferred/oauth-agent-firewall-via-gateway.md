# Deferred: OAuth/subscription agent through the gateway with the tool firewall enforced

**Status: BLOCKED on upstream LiteLLM fixes.** Proven not achievable on LiteLLM
v1.90.2 by the spike below. Not implemented; picked up when the upstream gate fix lands.

## Goal

Route a coding agent that authenticates with its OWN subscription/OAuth login (e.g.
Claude Code on a Claude Pro/Max plan) THROUGH the platform gateway (nginx → LiteLLM)
so that the platform's always-on guardrails STILL enforce — the destructive-command
tool firewall (`tool_permission`) and the Presidio secret-masking guardrails — while
the caller's own OAuth bearer reaches the provider unchanged. In short: per-user
subscription OAuth **and** gateway-enforced firewall, together, on one route.

## Why it's blocked

The spike ([`FINDINGS.md`](../spikes/claude-code-passthrough-firewall/FINDINGS.md))
stood up an isolated, reproducible LiteLLM + mock-upstream harness and proved no
LiteLLM route delivers both. Two upstream bugs are the root cause:

- **Bug #1 — pass-through `post_call` guardrails never enforce**
  ([`LITELLM-BUG-REPORT.md`](../spikes/claude-code-passthrough-firewall/LITELLM-BUG-REPORT.md)).
  A `tool_permission` (`post_call`, `default_action: deny`, `on_disallowed_action:
  block`) guardrail attached to a pass-through endpoint is loaded, attached, and
  *considered* (debug logs confirm all three) but its response-inspection hook never
  runs against the relayed body — a destructive `rm -rf /` tool_use is returned to the
  client unblocked (HTTP 200). Enforcement happens ONLY on LiteLLM's normalized
  model-call pipeline (`choices[].message.tool_calls`), which by construction uses a
  LiteLLM-managed provider key, not the caller's OAuth. Same result on the
  provider-native `/anthropic/*` passthrough (guardrail seen only as `logging_only` /
  unsupported `pre_call`).

- **Bug #2 — `forward_headers: true` is a raw relay that contradicts the docs**
  ([`LITELLM-BUG-REPORT-headers.md`](../spikes/claude-code-passthrough-firewall/LITELLM-BUG-REPORT-headers.md)).
  On a generic `pass_through_endpoints` route the proxy `Authorization` (LiteLLM
  master/virtual key) is forwarded VERBATIM upstream (a credential leak the docs say
  never happens) and the `x-pass-` prefix is NOT stripped. This dictates the exact
  wiring: never put a LiteLLM key on `Authorization` for the OAuth route.

Both bug reports are written up ready to file against the upstream issue tracker
(<https://github.com/BerriAI/litellm/issues>; cf. existing issue #13380 "pass-through
OAuth for Anthropic") and may already be filed. Reproduced on LiteLLM `v1.90.2`
(`ghcr.io/berriai/litellm:1.90.2`, i.e. `latest` at test time).

## Trigger to revisit

**The make-or-break fix is Bug #1:** when LiteLLM adds an inline, enforcing
`post_call` guardrail gate to the pass-through response path — one that inspects the
relayed body (including the provider-native Anthropic `content[].tool_use` shape, not
only the normalized OpenAI shape) and honours `on_disallowed_action: block`. Bug #2
(header handling) is secondary but affects the exact wiring; watch it too.

## What to build once unblocked

Point the OAuth agent at `<gateway>/anthropic` — an **unauthenticated** pass-through
endpoint (`auth: false`) so LiteLLM does not consume `Authorization` and relays the
caller's `Authorization: Bearer <oauth>` to the provider verbatim (spike E2c, no nginx
header rewriting needed for the credential). Attach the `tool_permission` + Presidio
guardrails to that route; once Bug #1 is fixed they enforce on the response while the
OAuth reaches the provider. Wiring note per Bug #2: keep the route unauthenticated and
put NO LiteLLM key on `Authorization` (it would leak upstream under `forward_headers`).

## Interim path already chosen

Until this is unblocked the platform goes the OTHER way: OAuth/plan agents talk to the
provider DIRECTLY (bypassing the gateway, and therefore the firewall + masking
guardrails). Input compression is preserved where `headroom wrap` supports the CLI —
the oauth-wrappable set is **claude-code** and **codex** (each gets an
`alias <cli>='headroom wrap <cli>'`); **gemini** is NOT wrappable, so an oauth gemini
goes direct with no wrap. This is an explicit per-agent opt-in with the guardrail loss
documented (`ai create` WARNS for each oauth agent). It is implemented separately;
this doc only references it as the interim route.

## Fallback if LiteLLM never fixes Bug #1

Enforce the tool firewall in a small response-inspecting proxy OUTSIDE LiteLLM that
parses the Anthropic `content[].tool_use` blocks and blocks destructive commands
(nginx+lua or a tiny Go/Python service), while LiteLLM/nginx only relays the caller's
OAuth. This fully decouples credential relay from tool firewalling and is the spike's
recommended next step (FINDINGS.md "Recommended paths forward", #1).
