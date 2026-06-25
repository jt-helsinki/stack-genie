# AI Development Platform — architecture

Top-level containers and the model data path. The diagram below is
[Mermaid](https://mermaid.live) — it renders to an image on GitHub and in most
Markdown viewers; an ASCII version follows for terminals.

> **Note:** `docs/architecture.png` (used by `docs/architecture-overview.md`) is a
> rendered export of `docs/architecture.mmd` and must be **regenerated from that
> Mermaid source** after any architecture change (e.g.
> `mmdc -i docs/architecture.mmd -o docs/architecture.png`).

## Containers + model data flow (Mermaid)

```mermaid
flowchart LR
  subgraph host["Host machine (macOS Apple Silicon / Linux)"]
    direction LR

    subgraph vm["Microsandbox microVM — one per project workspace (msb)"]
      agent["Agent CLI<br/>opencode · pi · claude · codex · gemini<br/>(in a tmux session)"]
    end

    subgraph tier["Docker service tier — aip-net (all internal-only except nginx + loopback DB/DNS)"]
      proxy["aip-proxy<br/>nginx · host :18787 — SOLE ENTRY, TLS-ready<br/>/ &amp; /v1 → Headroom · /llm → LiteLLM · /ollama → Ollama<br/>vhosts: litellm. · chat. · odysseus.&lt;domain&gt;"]
      headroom["aip-headroom :8787<br/>input compression<br/>(internal only)"]
      litellm["aip-litellm :4000<br/>router + always-on guardrails<br/>admin UI via /llm + litellm.&lt;domain&gt; (internal only)"]
      db[("aip-litellm-db<br/>Postgres (loopback :5442)<br/>keys · spend · creds")]
      presidio["aip-presidio-{analyzer,anonymizer}<br/>secret masking"]
      ollama["aip-ollama :11434<br/>local models (default gemma)<br/>(internal only)"]
      dns["aip-dns (loopback :15353)<br/>CoreDNS egress audit"]
      owui["aip-open-webui :8080<br/>(optional chat UI · internal only)"]
      ody["aip-odysseus :7000<br/>(optional · internal only · + chromadb/searxng/ntfy)"]
    end
  end

  cli["Host CLI / UI<br/>(loopback 127.0.0.1:18787)"]
  cloud["Cloud providers<br/>OpenAI · Anthropic · Gemini · Groq<br/>(real keys held in LiteLLM)"]

  agent -->|"OpenAI API · base_url http://host:18787/v1<br/>Authorization: scoped LiteLLM virtual key"| proxy
  cli -->|"/v1 model path · /llm + /ollama admin"| proxy
  agent -. "every DNS name (audited);<br/>egress default-deny, only the gateway allowed" .-> dns
  proxy --> headroom --> litellm
  litellm <-->|"pre/post-call guardrails"| presidio
  litellm --- db
  litellm -->|"local route (default: ollama/gemma)"| ollama
  litellm -->|"cloud route (real provider key)"| cloud
  owui -->|"same gateway path (model calls)"| proxy
  ody -->|"same gateway path (model calls)"| proxy
```

## Same thing in ASCII

```text
                     Host machine
 ┌───────────────────────────────────────────────────────────────────────────┐
 │  Microsandbox microVM (per workspace)                                       │
 │  ┌───────────────────────────────┐                                         │
 │  │ agent CLI (opencode/pi/…)      │   egress: DEFAULT-DENY (msb net-rules)  │
 │  │ tmux session                   │ ······ DNS ······▶ aip-dns (audit)      │
 │  └───────────────┬───────────────┘   only the gateway host:port is allowed │
 │                  │ OpenAI API, base_url = http://host:18787/v1             │
 │                  │ Authorization: scoped LiteLLM virtual key (no real keys)│
 │  Docker service tier (aip-net) — every container internal-only but nginx     │
 │                  ▼                  (loopback exceptions: DB :5442, DNS :15353)│
 │            aip-proxy (nginx, host :18787 — SOLE host entry)                  │
 │              / & /v1 → Headroom · /llm → LiteLLM · /ollama → Ollama          │
 │              vhosts: litellm./chat./odysseus.<domain>  (admin + optional UIs)│
 │                  ▼                    ▲                                      │
 │            aip-headroom :8787         └── host CLI / UI (loopback :18787)    │
 │              (input compression, internal-only)   aip-open-webui  (optional) │
 │                  ▼                                 aip-odysseus    (optional) │
 │            aip-litellm  :4000   ── guardrails ──▶ aip-presidio (secrets)    │
 │              router (admin UI via /llm + litellm.<domain>, internal-only)    │
 │                  │  └── aip-litellm-db (Postgres) (+ tool-firewall,          │
 │          ┌───────┴────────┐                         prompt-injection)        │
 │          ▼                ▼                                                 │
 │     aip-ollama       Cloud providers (OpenAI/Anthropic/Gemini/Groq)         │
 │     :11434           real provider keys live IN LiteLLM, never in the VM    │
 │     local models                                                            │
 └───────────────────────────────────────────────────────────────────────────┘
```

## The path, in words

1. The **agent CLI** runs inside a hardware-isolated **Microsandbox microVM**. Its
   provider config points `base_url` at the host gateway (`http://host:18787/v1`)
   and authenticates with a **scoped LiteLLM virtual key** — the real provider
   keys are never on the workspace.
2. Workspace **egress is default-deny** (Microsandbox net-rules applied at create);
   only the model gateway is reachable, and every DNS name is logged by **aip-dns**
   (`ai network log`).
3. The request hits **aip-proxy** (nginx, host `:18787` — the TLS-termination point
   and the **sole** host entry; every other service container is internal-only on
   `aip-net`, the only loopback exceptions being Postgres `:5442` and `aip-dns`
   `:15353`). The default `/` and `/v1` routes forward to **aip-headroom** (input
   compression), then to **aip-litellm**; `/llm` and `/ollama` (and the
   `litellm.<domain>` vhost) front the LiteLLM admin + Ollama HTTP surfaces
   directly. The host CLI and the optional UIs reach the same gateway on loopback
   `127.0.0.1:18787`.
4. **aip-litellm** is the router. Always-on **guardrails** run on every request and
   every route (cloud included, since all traffic traverses the proxy): **Presidio**
   secret masking + `hide-secrets`, a **tool-firewall** (`tool_permission`) that
   denies destructive command tool-calls, and an in-process **prompt-injection**
   detector. Its admin UI / virtual keys / spend live in **aip-litellm-db**.
5. LiteLLM routes to **aip-ollama** (local models — the default `gemma`) or to a
   **cloud provider** using the real key it holds. The response streams back along
   the same path (SSE-friendly through nginx) to the agent.

Optional UIs (**aip-open-webui**, **aip-odysseus**) use the *same* gateway path —
never LiteLLM directly. See `spec/01-architecture-spec.md` for the full design and
`AGENTS.md` for the container/wiring summary.
