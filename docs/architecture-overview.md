# AI Development Platform — architecture overview

![Architecture diagram](architecture.png)

## End-to-end flow

A coding agent runs inside a hardware-isolated **Microsandbox microVM** (one per project workspace) and speaks the OpenAI API to a single host gateway at `http://host.microsandbox.internal:18787/v1`. Workspace egress defaults to **"public"** — the open internet is reachable (so the in-VM container runtime can pull images and AI processes reach the net) while private ranges stay blocked, every DNS name the VM looks up is logged for audit, and a project is re-lockable to default-deny with `ai network egress deny`. The **nginx gateway** (`aip-proxy`) is the sole host entry: every other service container is internal-only on the `aip-net` Docker network. The request enters nginx, is passed to **Headroom** for input compression, then to **LiteLLM**, which applies always-on guardrails and routes the call to a **local Ollama** model or a **cloud provider**. Crucially, the agent holds only a scoped LiteLLM **virtual key**; the real provider API keys live inside LiteLLM, never in the workspace, and the streamed response returns along the same path. The host CLI reaches the gateway on loopback `127.0.0.1:18787`.

## The pieces

- **Workspace microVM / agent CLI** — hardware-isolated per-project sandbox (opencode · pi · claude · codex · gemini); holds only a scoped LiteLLM virtual key, with egress defaulting to "public" (DNS-audited, re-lockable to deny).
- **In-VM container runtime + apps** — every workspace microVM ships a rootful OCI runtime (containerd + nerdctl + runc + CNI), on which the platform runs opt-in AI apps as `nerdctl` containers *inside* the VM — **Open WebUI** and **AnythingLLM** (`ai create --apps`, `ai apps`). Each app mounts `/workspace`, is published on a unique per-`(workspace, app)` host port, and routes its model calls through the *same* gateway path as the agent (never LiteLLM directly).
- **nginx gateway (`aip-proxy`, host :18787)** — the **sole** host entry point and TLS-termination point; every other service container is internal-only on `aip-net` (the only loopback exceptions are Postgres :5442 and `aip-dns` :15353). It routes `/` and `/v1` to Headroom (the SSE-friendly model path), `/llm` to the LiteLLM admin surface, `/ollama` to the Ollama HTTP API, and serves the LiteLLM admin UI as the lone Host-vhost subdomain (`litellm.<domain>`).
- **Headroom (`aip-headroom`, internal-only)** — input-compression proxy sitting in front of LiteLLM, applying per-project context knobs.
- **LiteLLM (`aip-litellm`, internal-only)** — the model router and the enforcement point for always-on guardrails (secret masking, prompt-injection detection, destructive tool-call firewall); holds the real provider keys. Its admin UI is reached only through nginx (`/llm` or the `litellm.<domain>` vhost), never published directly.
- **Presidio (`aip-presidio-analyzer` + `aip-presidio-anonymizer`)** — backs LiteLLM's always-on pre/post-call secret-masking guardrail (financial/identity secrets).
- **Postgres (`aip-litellm-db`, loopback :5442)** — backs LiteLLM's admin UI, virtual keys, spend, and the encrypted provider-credential store (the one stateful service).
- **Ollama (`aip-ollama`, internal-only)** — required local-model backend LiteLLM routes to (DB-backed, catalog-driven — no built-in default model); models persist on the host, reached by the host CLI via nginx's `/ollama` route.
- **DNS audit (`aip-dns`, CoreDNS, loopback :15353)** — the egress-audit resolver microVMs forward DNS to, so attempted names are logged (`ai network log`) — audit, not enforcement.
- **Keys-in-LiteLLM** — real OpenAI/Anthropic/Gemini/Groq keys live only in LiteLLM's credential store; the agent authenticates to the gateway with a revocable, scoped virtual key.

There are no optional **host** services: Open WebUI is now a per-workspace in-VM app (see above) and Odysseus was removed from the platform entirely.
