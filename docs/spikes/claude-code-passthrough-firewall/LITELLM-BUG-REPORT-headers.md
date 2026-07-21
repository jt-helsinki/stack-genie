# `pass_through_endpoints` + `forward_headers: true` contradicts the header-forwarding docs — forwards the proxy `Authorization` upstream (key leak) and does NOT strip the `x-pass-` prefix

### What happened

The header-forwarding docs (https://docs.litellm.ai/docs/proxy/forward_client_headers) make two guarantees:

1. > "The proxy's `Authorization` header (used for proxy authentication) is **never** forwarded to LLM providers, even with this setting enabled."
2. > "Headers prefixed with `x-pass-` are always forwarded with the prefix stripped, regardless of settings."

Neither holds for a generic `pass_through_endpoints` entry with `forward_headers: true`. On that route LiteLLM performs a **raw, verbatim header relay**:

- The proxy `Authorization` header — which carries the **LiteLLM master/virtual key** — is forwarded **verbatim to the upstream provider** (contradicts guarantee #1, and leaks the proxy credential to the third-party LLM API).
- An `x-pass-authorization` header arrives at the upstream **unchanged** (`x-pass-authorization`), i.e. the prefix is **not** stripped and it is not rewritten to `authorization` (contradicts guarantee #2).

The `x-pass-` prefix-stripping and the "Authorization is never forwarded" rule appear to apply only to the `forward_client_headers_to_llm_api` mechanism and/or the provider-native passthroughs — **not** to a generic `pass_through_endpoints` route with `forward_headers: true`, which is a raw relay. The docs don't distinguish the two, which leads to a dangerous assumption that the proxy key is safe on a pass-through route.

**Expected** (per the docs): the proxy `Authorization` header is stripped before the upstream call, and `x-pass-authorization` is forwarded as `authorization` (prefix stripped).
**Actual:** the proxy `Authorization` (LiteLLM key) is forwarded verbatim to the provider, and `x-pass-authorization` is forwarded verbatim (prefix intact).

### Steps to reproduce

1. **Mock upstream that echoes the headers it received** — `mock.py`:

   ```python
   from http.server import BaseHTTPRequestHandler, HTTPServer
   import json
   class H(BaseHTTPRequestHandler):
       def do_POST(self):
           self.rfile.read(int(self.headers.get('content-length', 0)))
           body = json.dumps({
               "received_headers": {k.lower(): v for k, v in self.headers.items()}
           }).encode()
           self.send_response(200); self.send_header("content-type", "application/json")
           self.send_header("content-length", str(len(body))); self.end_headers()
           self.wfile.write(body)
   HTTPServer(("0.0.0.0", 8899), H).serve_forever()
   ```
   `python3 mock.py &`

2. **`config.yaml`** — a generic pass-through with `forward_headers: true`:

   ```yaml
   model_list: []
   general_settings:
     master_key: "sk-litellm-master"
     store_model_in_db: false
     forward_client_headers_to_llm_api: true
     forward_llm_provider_auth_headers: true
     pass_through_endpoints:
       - path: "/anthropic/v1/messages"
         target: "http://host.docker.internal:8899/v1/messages"
         forward_headers: true
   ```

3. **Run LiteLLM**:
   ```bash
   docker run --rm -p 4000:4000 \
     -v "$PWD/config.yaml:/app/config.yaml" \
     --add-host host.docker.internal:host-gateway \
     ghcr.io/berriai/litellm:1.90.2 \
     --config /app/config.yaml --detailed_debug
   ```

4. **Send a request** with the proxy key on `Authorization` and a provider credential on `x-pass-authorization`:
   ```bash
   curl -sS http://localhost:4000/anthropic/v1/messages \
     -H "Authorization: Bearer sk-litellm-master" \
     -H "x-pass-authorization: Bearer PROVIDER_OAUTH_123" \
     -H "content-type: application/json" \
     -d '{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"go"}]}'
   ```

### Relevant log output

The mock upstream echoes back the headers it actually received. Observed (abridged):

```json
{
  "received_headers": {
    "authorization": "Bearer sk-litellm-master",
    "x-pass-authorization": "Bearer PROVIDER_OAUTH_123",
    "content-type": "application/json",
    ...
  }
}
```

- `authorization: Bearer sk-litellm-master` reached the upstream → the **proxy/master key was forwarded verbatim** (docs guarantee #1 says it never is — this is a credential leak to the third-party provider).
- `x-pass-authorization` reached the upstream **unchanged** → the `x-pass-` prefix was **not** stripped and it was **not** rewritten to `authorization` (docs guarantee #2 says it always is, prefix stripped).

### LiteLLM Version

`v1.90.2` (Docker image `ghcr.io/berriai/litellm:1.90.2`, i.e. `latest` at time of testing).

---

### Impact / suggested fix

- **Security:** anyone relying on the documented behavior may unknowingly forward their LiteLLM master/virtual key to the upstream LLM provider on every pass-through request when `forward_headers: true` is set.
- Either (a) make `pass_through_endpoints` + `forward_headers` honor the same rules the docs state (strip the proxy `Authorization`; strip the `x-pass-` prefix and remap to `authorization`), or (b) clearly document that a generic `pass_through_endpoints` with `forward_headers: true` is a **raw verbatim relay** to which the `forward_client_headers` guarantees (Authorization stripping, `x-pass-` prefix handling) do **not** apply, and warn that the proxy key will be forwarded.
