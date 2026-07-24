# AI Development Platform — architecture

Top-level containers and the model data path in words. The **canonical diagram** is
[`docs/architecture.mmd`](architecture.mmd) (Mermaid source, complete and
authoritative — includes every service: proxy, headroom, litellm + DB, presidio,
ollama, valkey, redisinsight, dns); its rendered export `docs/architecture.png` is
embedded in [`architecture-overview.md`](architecture-overview.md).

> **Note:** after any architecture change, edit `docs/architecture.mmd` and
> regenerate the PNG from it (e.g. `mmdc -i docs/architecture.mmd -o
> docs/architecture.png`). Do not hand-maintain a separate diagram here — the single
> source avoids drift.

## The path, in words

1. The **agent CLI** runs inside a hardware-isolated **Microsandbox microVM**. Its
   provider config points `base_url` at the host gateway (`http://host.microsandbox.internal:18787/v1`)
   and authenticates with a **scoped LiteLLM virtual key** — the real provider
   keys are never on the workspace.
2. Workspace **egress defaults to "public"** (Microsandbox net-rules applied at
   create — the open internet is reachable so in-VM `nerdctl` can pull images and AI
   processes reach the net, while private ranges stay blocked). Every DNS name is
   logged by **aip-dns** (`ai network log`), and a project is re-lockable to
   default-deny with `ai network egress deny` (only the model gateway +
   allow-listed host services then reachable).
3. The request hits **aip-proxy** (nginx, host `:18787` — the TLS-termination point
   and the **sole** host entry; every other service container is internal-only on
   `aip-net`, the only loopback exception being `aip-dns` `:15353` — Postgres
   `aip-litellm-db` is also internal-only, no host port, reached at
   `aip-litellm-db:5432`). The default `/` and `/v1` routes forward **directly to aip-litellm**;
   `/llm` and `/ollama` (and the `litellm.<domain>` + `valkey.<domain>` vhosts) front
   the LiteLLM admin + Ollama + RedisInsight surfaces. nginx no longer routes to Headroom at all. The host CLI
   reaches the gateway on loopback `127.0.0.1:18787`.
4. **aip-litellm** is the router. **Guardrails are user-selectable** (chosen at
   `ai setup` via a picker / `--guardrails`, persisted in `runtime.yaml`); only the
   enabled ones are rendered into the LiteLLM config, and each rendered guardrail is
   `default_on: true` — so since every route (cloud included) traverses the proxy, a
   client cannot opt out of an **enabled** guardrail. **Headroom input compression**
   (`headroom-compression`, `pre_call`) is the only guardrail on by default: LiteLLM
   calls it **in-process**, POSTing the request messages to
   `http://aip-headroom:8787/v1/compress` and swapping in the compressed result
   before dispatch. The security guardrails are **opt-in**: **Presidio** secret
   masking (financial/identity secrets only, not general PII — CREDIT_CARD, US_SSN,
   US_BANK_NUMBER, IBAN_CODE, CRYPTO), `hide-secrets` API-key/token detection, and a
   **tool-firewall** (`tool_permission`) that denies destructive command tool-calls.
   An unselected guardrail is omitted entirely (never references a backend that isn't
   running — e.g. Presidio is only launched when secret-masking is selected). (The
   in-process prompt-injection detector and the unmaintained LLM Guard were both
   removed — they false-positived on ordinary coding/Ollama traffic.) Its admin UI /
   virtual keys / spend live in **aip-litellm-db**.
5. LiteLLM routes to **aip-ollama** (a registered local Ollama model) or to a
   **cloud provider** using the real key it holds. The model set is DB-backed and
   catalog-driven with **no built-in default model**. The response streams back along
   the same path (SSE-friendly through nginx) to the agent.

Each workspace microVM ships a **rootful in-VM container runtime** (containerd +
nerdctl + runc + CNI), on which the platform runs **opt-in AI apps** (`internal/apps`)
as `nerdctl` containers *inside* the VM — **Open WebUI** (`openwebui`, guest port
8080) and **AnythingLLM** (`anythingllm`, guest port 3001), selected with
`ai create --apps` or managed with `ai apps`. Each app gets the workspace project dir
(`~/project`) mounted at `/workspace` in its container, published on a unique
per-`(workspace, app)` host port (chosen at create via `--app-port <app>=<port>`,
persisted in the project `config.yaml` `apps:` block), and points at the *same*
gateway path (`http://host.microsandbox.internal:18787/v1` with the workspace's scoped
virtual key) — never LiteLLM directly. Agent-CLI web dashboards get the same treatment:
currently only **hermes** (`hermes dashboard`, default port 9119, `--app-port
hermes=<port>`, persisted in `config.yaml` `agent_dashboards:`). There are no optional
**host** services: the former host Open WebUI is now this in-VM app, and Odysseus was
removed entirely.

Every OS base image also bakes in a common dev-tooling layer: Git, the GitHub CLI,
**Node.js** (pinned Node 24 LTS), the latest **Python 3** (system-wide, backing the
per-project `~/project/.venv-msb` virtualenv created at start), **uv** (Astral's
Python package/tool manager, installed for the workspace user onto `~/.local/bin`),
**rtk** (`rtk-ai/rtk`, a dev-command output compressor, via its `install.sh`), and the
**Headroom** CLI (`headroom-ai[proxy]`, for in-VM `headroom wrap <cli>`). Because
Node.js and Python 3 are baked in, neither is a `--stacks` option; the selectable
software stacks are `go`, `rust`, `java`, `maven`, `deno`.

The per-project **AI tools** are a separate, selectable set (one `--tools`
multi-select at `ai create`, or the wizard's AI-tools step): **caveman**, **graphify**,
**code-review-graph**, and **codebase-memory-mcp** (defaults: the first three ON,
codebase-memory-mcp OFF). Each maps to a `context.*_enabled` bool in `config.yaml`.
**Graphify** (the knowledge-graph skill for AI coding assistants — PyPI `graphifyy`,
CLI `graphify`) is NO LONGER baked into every OS base: when selected it is installed
for the workspace user by a CONDITIONAL Dockerfile snippet
(`internal/templates/files/tools/graphify/`) via `uv tool install "graphifyy[…extras]"`
(all optional extras except the region/DB/niche-specific
`chinese,azure,bedrock,falkordb,neo4j,leiden,dm,pascal`), and each selected agent CLI
registers Graphify with itself **at workspace start**, once per project
(`graphify install` for claude-code, `graphify install --platform <cli>` for
codex/gemini/opencode/copilot) — not in the Dockerfile, since `--project` writes into
the bind-mounted project dir. Graphify's headless LLM backend is an Ollama model chosen
at `ai create` (`--graphify-model`), routed through the gateway as `ollama/<model>`.
**code-review-graph** (`code-review-graph.com`; per-CLI `install --platform`, then
`build` + a D3 graph visualization) and **codebase-memory-mcp**
(`DeusData/codebase-memory-mcp`; auto-detecting `install`, optional on-demand 3D graph
UI on `:9749`) are likewise appended to the project Dockerfile as CONDITIONAL snippets
ONLY when chosen (not baked into every base) and, when selected, registered as an MCP
server with each installed agent CLI at workspace start
(`workspace.registerCodeReviewGraph` / `registerCodebaseMemory`, once-guarded +
best-effort). Both are local and keyless. **caveman** is installed at workspace start
by its own upstream installer (`registerCaveman`). See `spec/01-architecture-spec.md`
for the full design and `AGENTS.md` for the container/wiring summary.
