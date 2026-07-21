# Spike: Claude-Code subscription-OAuth through LiteLLM pass-through + tool firewall

**Date:** 2026-07-05
**LiteLLM image:** `ghcr.io/berriai/litellm:latest` (present locally at run time)
**Status:** SPIKE — isolated, reproducible, NOT wired into the platform. No platform code or `aip-*` service touched.

Run it: `./run.sh` (E1–E3, generic pass-through) and `./run-e4.sh` (E4, provider-native
`/anthropic/*` passthrough). Each stands up a mock upstream + LiteLLM on an isolated
docker network, prints results, and tears everything down; re-runnable.

---

## The question

Can a Claude-Code-style client that authenticates with its OWN subscription
OAuth bearer route through LiteLLM's Anthropic pass-through such that:

- **(a)** LiteLLM forwards the caller's `Authorization` bearer UNCHANGED to the
  upstream Anthropic-compatible endpoint, AND
- **(b)** LiteLLM's `tool_permission` guardrail (the destructive-command tool
  firewall) still runs and BLOCKS a dangerous tool call in the middle?

**One-line verdict: NO — not on any LiteLLM passthrough route (generic OR
provider-native `/anthropic/*`).** The enforcing firewall runs only on LiteLLM's
normalized model-call pipeline, which can't carry the caller's own OAuth. You get (a)
*or* (b), never both. E4 (below) closed the make-or-break question: the native
Anthropic passthrough does NOT block either, and does not forward the caller's OAuth.

---

## What was stood up

1. **`mock-upstream.py`** — a threaded python `http.server` "Anthropic" upstream.
   Accepts `POST /v1/messages`, RECORDS every request header to `requests.jsonl`
   (and prints each), and returns a canned Anthropic Messages JSON response that
   varies by a body marker:
   - `SCENARIO_DESTRUCTIVE` -> `tool_use` block invoking `Bash` with `rm -rf /`
   - `SCENARIO_BENIGN_TOOL` -> `tool_use` block invoking `Bash` with `ls -la`
   - otherwise -> plain text (no tool_use)
   - `GET /_last` returns the last request's headers (host reads it as proof).
2. **`spike-litellm`** container with `litellm-config.yaml` mounted (`store_model_in_db:
   false`, no DB). Two Anthropic pass-through endpoints — one authenticated
   (`/anthropic/v1/messages`), one unauthenticated (`/open/v1/messages`) — each with
   `forward_headers: true` and the `tool_permission` firewall attached.

Isolated docker network `spike-passthrough-net`, containers prefixed `spike-`,
everything torn down on exit (even on failure).

---

## Config keys — verified against LiteLLM docs (NOT guessed)

- Pass-through endpoints & `forward_headers`:
  https://docs.litellm.ai/docs/proxy/pass_through
- Guardrails on pass-through (`general_settings.pass_through_endpoints[].guardrails`):
  https://docs.litellm.ai/docs/proxy/pass_through_guardrails
- Forward client / provider auth headers, `x-pass-` prefix, Authorization stripping:
  https://docs.litellm.ai/docs/proxy/forward_client_headers
- `tool_permission` guardrail schema (`rules`/`tool_name`/`decision`/
  `allowed_param_patterns`/`default_action`/`on_disallowed_action`):
  https://docs.litellm.ai/docs/proxy/guardrails/tool_permission

The docs described three credential-forwarding mechanisms. **The empirical
behaviour of `latest` diverged from the docs in two important ways** (see E1) —
which is exactly why this was worth spiking rather than trusting the docs.

---

## E1 — Header forwarding: does the agent's OAuth reach the upstream?

Caller OAuth token used throughout: `SPIKE_OAUTH_TOKEN_123`.
LiteLLM master key: `sk-spike-litellm-master`.

| Case | Headers sent to LiteLLM | HTTP | What the mock upstream ACTUALLY received |
|------|-------------------------|------|------------------------------------------|
| **E1a** | `Authorization: Bearer <OAUTH>` only (authed route) | **400** `No connected db` | **nothing** — request never reached upstream |
| **E1b** | `Authorization: Bearer <master>` + `x-pass-authorization: Bearer <OAUTH>` | 200 | `authorization: Bearer sk-spike-litellm-master` **and** `x-pass-authorization: Bearer SPIKE_OAUTH_TOKEN_123` |
| **E1c** | `Authorization: Bearer <master>` + `x-api-key: <OAUTH>` | 200 | `authorization: Bearer sk-spike-litellm-master` **and** `x-api-key: SPIKE_OAUTH_TOKEN_123` |

**Result: WORKED (question answered), with two surprises vs the docs:**

1. **A generic `pass_through_endpoints` with `forward_headers: true` does NOT strip
   the proxy `Authorization` header.** It forwarded ALL headers verbatim — the
   LiteLLM master key leaked upstream on `Authorization` in E1b/E1c. The docs'
   "Authorization is never forwarded" rule applies to the
   `forward_client_headers_to_llm_api` mechanism, **not** to a generic
   `pass_through_endpoints` entry with `forward_headers: true`, which is a raw relay.
2. **The `x-pass-` prefix was NOT stripped** on this generic endpoint — the mock
   received the header as `x-pass-authorization`, not rewritten to `authorization`.
   So `x-pass-` prefix-stripping is (in `latest`) specific to LiteLLM's
   provider-native passthroughs, not generic `pass_through_endpoints`.

E1a proves the naive path fails: sending only `Authorization: Bearer <OAUTH>` to an
**authenticated** route makes LiteLLM treat the OAuth as a virtual key and try a DB
lookup (400 `No connected db`) — the OAuth is consumed by proxy auth and never
reaches the provider.

---

## E2 — The Authorization-header collision (the crux)

| Case | Route / headers | HTTP | Finding |
|------|-----------------|------|---------|
| **E2a** | authed route, no `Authorization` | **401** `No api key passed in` | authed pass-through REQUIRES a LiteLLM key on `Authorization` |
| **E2b** | authed route, wrong key | **400** `No connected db` | LiteLLM validates it as a virtual key (needs DB); only the in-memory master key works DB-less |
| **E2c** | **unauthenticated** route `/open`, only `Authorization: Bearer <OAUTH>` | **200** | mock received `authorization: Bearer SPIKE_OAUTH_TOKEN_123` — **the caller's raw OAuth, forwarded verbatim** |

**Result: WORKED — crux resolved.** There is a clean way to deliver the agent's
own subscription OAuth to Anthropic **with no header rewriting at all**:

> Run the pass-through **unauthenticated** (`auth: false`). LiteLLM does not consume
> `Authorization` for its own auth on that route, so Claude Code's own
> `Authorization: Bearer <oauth>` is relayed to the upstream verbatim (E2c).

This is *simpler* than the "put the litellm key on Authorization and the OAuth on
`x-pass-authorization`/`x-api-key`" pattern — and it avoids the **master-key-leak**
that E1b/E1c exposed (with `forward_headers: true`, whatever sits on `Authorization`
is forwarded upstream, so putting a LiteLLM key there leaks it to Anthropic).

The catch is entirely on the firewall side — see E3.

---

## E3 — Firewall: does `tool_permission` block the destructive tool call?

The firewall rule mirrors the platform's intent: allow `Bash` **only** when its
`command` matches a negative-lookahead excluding `rm -rf` / `git push --force` /
`terraform destroy` / `kubectl delete`, `default_action: deny`,
`on_disallowed_action: block`, `mode: post_call` (the tool call appears in the
RESPONSE, so post_call is the only relevant mode — and the guardrail advertises
`guardrail_supported_event_hooks=post_call`).

| Case | Route | Response body | HTTP | Firewall verdict |
|------|-------|---------------|------|------------------|
| **E3a** | authed passthrough | benign `Bash ls -la` | 200 | passed through (correct — allowed) |
| **E3b** | authed passthrough | destructive `Bash rm -rf /` | **200** | **NOT blocked — passed through unchanged** (FAIL) |
| **E3c** | unauthenticated passthrough | destructive `Bash rm -rf /` | **200** | **NOT blocked — passed through unchanged** (FAIL) |

**Result: FAILED.** The destructive `rm -rf /` tool_use was relayed back to the
client verbatim on both routes. The firewall did not fire.

### Why (from `--detailed_debug` logs, the `diag.sh` diagnostic)

The guardrail is correctly loaded and attached — the logs show, in order:

```
tool_permission.py:77 - Tool Permission Guardrail initialized with 1 rules, default_action: deny
passthrough_guardrails.py:265 - Added passthrough-specific guardrail: tool-firewall
passthrough_guardrails.py:287 - Collected guardrails for passthrough endpoint: ['tool-firewall']
pass_through_endpoints.py:843 - Added guardrails to passthrough request metadata: {'tool-firewall': True}
custom_guardrail.py:610 - should_run_guardrail ... event_type=post_call guardrail_supported_event_hooks=post_call requested_guardrails={'tool-firewall': True} default_on=True
```

So LiteLLM *considers* the guardrail for `post_call` on the passthrough — but
`tool_permission.py` logs **only its init line** and never any inspection/deny
activity. The guardrail's response-inspection hook does not actually run against the
relayed passthrough body.

**Root cause:** a generic `pass_through_endpoints` relays the upstream response
**opaquely**. The `tool_permission` post_call hook inspects LiteLLM's *normalized*
`ModelResponse` (OpenAI `choices[].message.tool_calls` shape), which is **never
produced** for a raw passthrough. Corroborating evidence: the diagnostic's DIAG-2
(same mock returned via a normal registered-model route) failed with `provider
returned a response with no 'choices'` — i.e. the normalized pipeline, the only place
post_call tool inspection happens, expects OpenAI-shaped output, and the raw Anthropic
`content[].tool_use` body never enters it on the passthrough.

`pre_call` is not an escape hatch: `tool_permission` supports `post_call` only, and
the tool call exists only in the response anyway.

---

## E4 — Provider-NATIVE Anthropic passthrough (the make-or-break follow-up)

The generic `pass_through_endpoints` relays opaquely (E3). LiteLLM ALSO has a
built-in, provider-**native** Anthropic passthrough at `/anthropic/*`
(`litellm.proxy.anthropic_endpoints.endpoints`, lazy-loaded when `ANTHROPIC_API_KEY`
is set). It is format-aware, so the hypothesis was: it might run the response through
a pipeline where the `tool_permission` post_call hook fires.

**Setup (see `run-e4.sh` + `litellm-config-native.yaml`):** enabled the native route
(`ANTHROPIC_API_KEY=sk-ant-dummy-spike`), redirected its upstream to the mock via
`ANTHROPIC_API_BASE=http://host.docker.internal:8897` +
`LITELLM_ANTHROPIC_DISABLE_URL_SUFFIX=true` (both verified against the docs), and
attached the SAME firewall as `default_on: true` (the native route has no
per-endpoint `guardrails:` block). The override successfully routed
`POST /anthropic/v1/messages` to the mock's `/v1/messages`.

| Case | Route / body | HTTP | Result |
|------|--------------|------|--------|
| **E4a** | native route, benign `Bash ls -la` | 200 | passed through (allowed) |
| **E4b** | native route, destructive `Bash rm -rf /` | **200** | **NOT blocked — passed through unchanged** (FAIL) |
| **E4c** | native route, ONLY `Authorization: Bearer <OAUTH>` (no litellm key) | **400** `No connected db` | caller's OAuth is CONSUMED by proxy auth (treated as a virtual key), not forwarded |

**Result: FAILED — the native route does NOT block either, and does NOT forward the
caller's OAuth.**

### (b) Firewall — from `--detailed_debug` logs

`tool_permission.py` again logged **only its init line** and never inspected the
body. On the native route the guardrail was consulted only as `pre_call` (unsupported
→ skip) and **`logging_only`** — never as an *enforcing* `post_call` gate. And the
native route's post-response handling is an **async logging** step that even errored:

```
anthropic_passthrough_logging_handler.py:304 - Error creating Anthropic response
  logging payload: litellm.BadRequestError: LLM Provider NOT provided ... model=claude-x
custom_guardrail.py:610 - should_run_guardrail ... event_type=logging_only
  guardrail_supported_event_hooks=post_call requested_guardrails=[] default_on=True
```

**Structural finding:** a passthrough route (generic OR provider-native) returns the
upstream response to the client **without an inline enforcing post_call guardrail
gate**. On the native route the response is only seen by the *async logging* pipeline
(`logging_only` mode, which cannot block, and here failed to even parse). So
`on_disallowed_action: block` never gets a chance to fire. (The logging-payload error
is partly an artifact of the mock/base-override — `model=claude-x` has no provider
prefix — but it is irrelevant to the verdict: `logging_only` cannot block regardless,
and the enforcing `post_call` gate is simply not wired into the passthrough response
path.)

### (a) OAuth forwarding — from the mock's recorded headers

On the native route LiteLLM uses its OWN stored credential for the upstream and does
its own caller auth:

- E4a/E4b (caller sent the litellm master key): mock received `x-api-key:
  sk-ant-dummy-spike` — LiteLLM **injected the stored `ANTHROPIC_API_KEY`** as the
  upstream `x-api-key`, NOT the caller's credential. (It also leaked the master key on
  `Authorization`, same `forward_headers` behaviour as E1.)
- E4c (caller sent only `Authorization: Bearer <OAUTH>`): **400** — the native route
  requires a LiteLLM key on `Authorization` and treats the OAuth as a virtual key
  (DB lookup). The caller's OAuth is consumed, never forwarded.

So the native route behaves like a **routed/normalized** call for credentials
(LiteLLM-managed key), which is exactly the mode that CAN'T carry the user's own
subscription OAuth.

---

## The crux tension (verdict)

**Revised after E4.** Every mode tested falls into one of three buckets — and none
delivers BOTH the caller's own OAuth upstream AND the enforcing tool firewall:

| Mode | Caller's subscription OAuth reaches Anthropic | `tool_permission` firewall ENFORCED (blocks) |
|---|:---:|:---:|
| **Generic pass-through** (`pass_through_endpoints`, raw relay) | YES (E2c, unauth route) | **NO** (E3 — response never inspected inline) |
| **Provider-native Anthropic passthrough** (`/anthropic/*`) | **NO** (E4c — LiteLLM injects its stored `ANTHROPIC_API_KEY`; caller auth required) | **NO** (E4b — response only hits async `logging_only`, cannot block) |
| **Normalized/routed model call** (registered `anthropic/*` model) | **NO** (uses LiteLLM-stored provider key) | YES (the only pipeline where post_call guardrails enforce) |

**Verdict: NOT viable.** The enforcing `tool_permission` post_call gate runs ONLY on
LiteLLM's normalized model-call pipeline (`/chat/completions` / the `/v1/messages`
unified endpoint routed to a registered model) — which by construction uses a
LiteLLM-managed provider key, not the caller's OAuth. BOTH passthrough routes that
*can* forward the caller's credential (generic; and, for the native route, only via
its own logging path) do **not** run an enforcing firewall on the response. There is
no single LiteLLM route that gives OAuth-forwarding AND firewall enforcement together.

### What config *does* achieve (a) alone

Unauthenticated pass-through, caller's `Authorization` relayed verbatim:

```yaml
general_settings:
  store_model_in_db: false
  pass_through_endpoints:
    - path: "/anthropic/v1/messages"
      target: "https://api.anthropic.com/v1/messages"
      forward_headers: true
      auth: false           # do NOT let LiteLLM consume Authorization
```

Claude Code sends its own `Authorization: Bearer <oauth>`; it arrives at Anthropic
unchanged. **No nginx header rewriting is required for the credential** (contrary to
the initial hypothesis). Note: `forward_headers: true` forwards *everything* on this
route, so do NOT also put a LiteLLM key on `Authorization` — it would leak upstream.

### What remains blocked / fragile

- **The firewall does not run on ANY passthrough path** — neither generic (E3) nor
  provider-native (E4). Enforcement requires the normalized model-call pipeline,
  which can't carry the caller's OAuth.

### Recommended paths forward (given E4 closed the highest-value unknown)

1. **Enforce the firewall OUTSIDE LiteLLM for the OAuth path.** Since no LiteLLM
   passthrough runs an inline blocking gate on the response, put a small
   response-inspecting proxy (nginx+lua, or a tiny Go/Python service) in front of /
   behind LiteLLM that parses Anthropic `content[].tool_use` and blocks destructive
   commands. LiteLLM (or even plain nginx) then only relays the caller's OAuth. This
   fully decouples "credential relay" from "tool firewall" and is the only path that
   preserves subscription-OAuth-per-user.
2. **Give up per-user OAuth; use a routed `anthropic/*` model + a LiteLLM-managed
   provider key.** Here the firewall enforces (normalized pipeline). If you want the
   caller's credential to be the upstream auth, try `forward_llm_provider_auth_headers:
   true` with the credential on `x-api-key` — but Anthropic *subscription* OAuth is an
   `Authorization: Bearer` OAuth token, not an `x-api-key`, and it is unclear the
   subscription OAuth is even accepted on the standard `/v1/messages` API. Fragile.
3. **File/track upstream:** enforcing (blocking) guardrails on the native Anthropic
   passthrough is a genuine gap (cf. LiteLLM issue #13380 "pass-through OAuth for
   Anthropic"). If LiteLLM adds an inline post_call guardrail gate to the passthrough
   response path, path #1 could collapse back into LiteLLM.

---

## Reproducing

```bash
cd docs/spikes/claude-code-passthrough-firewall
./run.sh            # E1-E3 (generic pass_through_endpoints): header forwarding, collision, firewall
./run-e4.sh         # E4    (provider-native /anthropic/* passthrough): firewall + OAuth forwarding
```

Both stand up an isolated `spike-*` network + containers, print results + the
recorded upstream headers, and tear down on exit (even on failure). Artifacts after a
run: `requests.jsonl` (every header the mock received, JSON per line) and `mock.log`.
Both are regenerated each run.

Files: `litellm-config.yaml` (E1–E3), `litellm-config-native.yaml` (E4),
`mock-upstream.py`, `run.sh`, `run-e4.sh`.
