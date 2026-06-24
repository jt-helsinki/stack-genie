# AI Development Platform — architecture overview

![Architecture diagram](architecture.png)

## End-to-end flow

A coding agent runs inside a hardware-isolated **Microsandbox microVM** (one per project workspace) and speaks the OpenAI API to a single host gateway at `http://host:18787/v1`. Workspace egress is **default-deny** — only that gateway is reachable, and every DNS name the VM looks up is logged for audit. The request enters the **nginx gateway** (`aip-proxy`), is passed to **Headroom** for input compression, then to **LiteLLM**, which applies always-on guardrails and routes the call to a **local Ollama** model or a **cloud provider**. Crucially, the agent holds only a scoped LiteLLM **virtual key**; the real provider API keys live inside LiteLLM, never in the workspace, and the streamed response returns along the same path.

## The pieces

- **Workspace microVM / agent CLI** — hardware-isolated per-project sandbox (opencode · pi · claude · codex · gemini); holds only a scoped LiteLLM virtual key, with default-deny egress to the gateway alone.
- **nginx gateway (`aip-proxy`, host :18787)** — the single entry point and TLS-termination point; SSE-friendly reverse proxy to Headroom.
- **Headroom (`aip-headroom`, internal-only)** — input-compression proxy sitting in front of LiteLLM, applying per-project context knobs.
- **LiteLLM (`aip-litellm`)** — the model router and the enforcement point for always-on guardrails (secret masking, prompt-injection detection, destructive tool-call firewall); holds the real provider keys.
- **Presidio (`aip-presidio`)** — backs LiteLLM's always-on pre/post-call secret-masking guardrail (financial/identity secrets).
- **Postgres (`aip-litellm-db`)** — backs LiteLLM's admin UI, virtual keys, spend, and the encrypted provider-credential store (the one stateful service).
- **Ollama (`aip-ollama`)** — required local-model backend LiteLLM routes to (default `gemma`); models persist on the host.
- **DNS audit (`aip-dns`, CoreDNS)** — the egress-audit resolver microVMs forward DNS to, so attempted names are logged (`ai network log`) — audit, not enforcement.
- **Optional UIs (`aip-open-webui`, `aip-odysseus`)** — chat UI / AI workspace, off by default, both routed through the *same* nginx → Headroom → LiteLLM gateway path, never direct to LiteLLM.
- **Keys-in-LiteLLM** — real OpenAI/Anthropic/Gemini/Groq keys live only in LiteLLM's credential store; the agent authenticates to the gateway with a revocable, scoped virtual key.
