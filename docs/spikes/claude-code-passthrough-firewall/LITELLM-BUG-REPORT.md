# `post_call` guardrails attached to a pass-through endpoint are consulted but never enforce (e.g. `tool_permission` does not block)

### What happened

A `tool_permission` guardrail with `mode: post_call`, `default_action: deny`, `on_disallowed_action: block` attached to a `pass_through_endpoints` route is **loaded, attached, and considered** for the request (the debug logs confirm all three), but it **never inspects the upstream response body and never blocks**. A response containing a `tool_use`/tool-call that the rule is configured to deny is relayed back to the client unchanged, with HTTP 200.

The docs at https://docs.litellm.ai/docs/proxy/pass_through_guardrails state guardrails run on pass-through endpoints and support `pre_call`/`post_call`, so I expected a `post_call` `deny` rule to block. It does not — for a tool-call guardrail the tool call only exists in the *response*, and `post_call` is the guardrail's only supported hook, so there is no working mode to enforce it on a pass-through.

Root cause appears to be that `tool_permission`'s response inspection operates on LiteLLM's **normalized** `ModelResponse` (`choices[].message.tool_calls`), which is never produced for a raw pass-through relay — the upstream body is returned opaquely, so the hook has nothing to inspect. The same is observed on the **provider-native** Anthropic pass-through (`/anthropic/*`): there the guardrail is invoked only as `logging_only` (which cannot block) and via `pre_call` (unsupported by `tool_permission`), and the post-response step is an async logging handler, not an inline enforcing gate.

**Expected:** a `post_call` guardrail with `default_action: deny` / `on_disallowed_action: block` attached to a pass-through endpoint should inspect the upstream response and block/reject a disallowed tool call (same as it does on a normalized `/chat/completions` model route).
**Actual:** the disallowed tool call is returned to the client unblocked (HTTP 200).

### Steps to reproduce

1. **Minimal mock upstream** that returns an Anthropic-style response containing a `tool_use` block — `mock.py`:

   ```python
   from http.server import BaseHTTPRequestHandler, HTTPServer
   import json
   class H(BaseHTTPRequestHandler):
       def do_POST(self):
           self.rfile.read(int(self.headers.get('content-length', 0)))
           body = json.dumps({
               "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-x",
               "stop_reason": "tool_use",
               "content": [{"type": "tool_use", "id": "t1", "name": "Bash",
                            "input": {"command": "rm -rf /"}}],
           }).encode()
           self.send_response(200); self.send_header("content-type", "application/json")
           self.send_header("content-length", str(len(body))); self.end_headers()
           self.wfile.write(body)
   HTTPServer(("0.0.0.0", 8899), H).serve_forever()
   ```
   `python3 mock.py &`

2. **`config.yaml`** — a pass-through endpoint with a `tool_permission` guardrail attached:

   ```yaml
   model_list: []
   general_settings:
     master_key: "sk-litellm-master"
     store_model_in_db: false
     pass_through_endpoints:
       - path: "/anthropic/v1/messages"
         target: "http://host.docker.internal:8899/v1/messages"
         forward_headers: true
         guardrails:
           tool-firewall:
   guardrails:
     - guardrail_name: "tool-firewall"
       litellm_params:
         guardrail: tool_permission
         mode: "post_call"
         rules:
           - id: "allow_safe_bash"
             tool_name: "Bash"
             decision: "allow"
             allowed_param_patterns:
               "command": '^(?!.*(rm\s+-rf|terraform\s+destroy|kubectl\s+delete)).*$'
         default_action: "deny"
         on_disallowed_action: "block"
   ```

3. **Run LiteLLM**:
   ```bash
   docker run --rm -p 4000:4000 \
     -v "$PWD/config.yaml:/app/config.yaml" \
     --add-host host.docker.internal:host-gateway \
     ghcr.io/berriai/litellm:1.90.2 \
     --config /app/config.yaml --detailed_debug
   ```

4. **Send a request** whose upstream response contains the disallowed `Bash rm -rf /` tool call:
   ```bash
   curl -sS http://localhost:4000/anthropic/v1/messages \
     -H "Authorization: Bearer sk-litellm-master" \
     -H "content-type: application/json" \
     -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"go"}]}'
   ```

**Observed:** HTTP 200, and the response body contains the `tool_use` with `"command": "rm -rf /"` intact — the guardrail did not block. Expected a block/rejection from the `deny` rule.

### Relevant log output

The guardrail is loaded and attached, and `should_run_guardrail` returns true for `post_call` — but `tool_permission.py` logs only its init line and never performs any inspection/deny:

```
tool_permission.py:77 - Tool Permission Guardrail initialized with 1 rules, default_action: deny
passthrough_guardrails.py:265 - Added passthrough-specific guardrail: tool-firewall
passthrough_guardrails.py:287 - Collected guardrails for passthrough endpoint: ['tool-firewall']
pass_through_endpoints.py:843 - Added guardrails to passthrough request metadata: {'tool-firewall': True}
custom_guardrail.py:610 - should_run_guardrail ... event_type=post_call guardrail_supported_event_hooks=post_call requested_guardrails={'tool-firewall': True} default_on=True
```

After this, there is **no** further `tool_permission` log line (no inspection, no deny). On the provider-native `/anthropic/*` passthrough the guardrail is instead invoked only as `logging_only` and `pre_call`, and the post-response path is `anthropic_passthrough_logging_handler` (async logging), not an enforcing gate — so nothing blocks there either.

### LiteLLM Version

`v1.90.2` (Docker image `ghcr.io/berriai/litellm:1.90.2`, i.e. `latest` at time of testing).

---

### Additional context / suggested fix

If pass-through guardrails are intended only for logging/observability and cannot enforce on the response, the [pass-through guardrails docs](https://docs.litellm.ai/docs/proxy/pass_through_guardrails) should say so explicitly (today they document `pre_call`/`post_call` without noting that `post_call` cannot block on a pass-through). Otherwise, the response-inspecting guardrails (`tool_permission`, moderation, etc.) should be run against the relayed pass-through response body — including the provider-native shape (e.g. Anthropic `content[].tool_use`), not only the normalized `choices[].message.tool_calls` — so a `block` action actually rejects the response.
