# Spike: Claude-Code OAuth through LiteLLM pass-through + tool firewall

Isolated, reproducible proof-of-concept. De-risks: can a Claude-Code-style client
that authenticates with its OWN subscription OAuth bearer route through LiteLLM's
Anthropic pass-through such that (a) the caller's `Authorization` is forwarded
UNCHANGED upstream AND (b) the `tool_permission` firewall still BLOCKS a destructive
tool call?

**Verdict: NO — on no LiteLLM passthrough route (generic `pass_through_endpoints` OR
the provider-native `/anthropic/*` route).** The enforcing `tool_permission` firewall
runs only on LiteLLM's normalized model-call pipeline, which by construction uses a
LiteLLM-managed provider key, not the caller's OAuth. You get (a) or (b), never both.
See `FINDINGS.md` for the full write-up (E1–E4), captured evidence, and recommended
paths forward (chiefly: enforce the firewall in a small response-inspecting proxy
outside LiteLLM).

## Files

- `FINDINGS.md`             — the question, per-experiment results (E1/E2/E3 generic, E4 native), the crux, revised verdict, next steps.
- `litellm-config.yaml`     — LiteLLM config for E1-E3 (generic `pass_through_endpoints`, config-only, no DB).
- `litellm-config-native.yaml` — LiteLLM config for E4 (provider-native Anthropic passthrough + default_on firewall).
- `mock-upstream.py`        — threaded mock "Anthropic" upstream; records request headers, returns canned tool_use responses.
- `run.sh`                  — E1-E3: header forwarding, the Authorization collision, firewall on the generic route. Re-runnable.
- `run-e4.sh`               — E4: firewall + OAuth forwarding on the native `/anthropic/*` route. Re-runnable.

## Run

```bash
./run.sh        # E1-E3 (generic pass-through)
./run-e4.sh     # E4    (provider-native /anthropic/* passthrough)
```

Needs Docker (daemon up) + python3. Uses `ghcr.io/berriai/litellm:latest`, isolated
`spike-*` networks, and `spike-`-prefixed containers — never touches `aip-*`
containers/networks. Cleans up on exit even on failure.

Runtime artifacts (regenerated each run): `requests.jsonl`, `mock.log`.
