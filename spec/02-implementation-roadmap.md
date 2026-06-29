# 02-implementation-roadmap.md

# AI Development Platform

The Microsandbox Github repo can be found at:

[https://github.com/microsandbox/microsandbox](https://github.com/microsandbox/microsandbox)

The Microsandbox documentation is found at:

[https://docs.microsandbox.dev/](https://docs.microsandbox.dev/)

The Go SDK used to drive workspace microVMs is found at:

[https://github.com/microsandbox/microsandbox/tree/main/sdk/go](https://github.com/microsandbox/microsandbox/tree/main/sdk/go)

## Implementation Roadmap

Version: 1.0

---

# 0. Overview

This document defines the phased implementation plan for the AI Development Platform.

It translates the full architecture into incremental delivery slices.

Each slice must be independently testable and fully working before proceeding to the next.

> **Status:** the full surface is implemented — slices S1, S2, S4–S6 (S3 and the
> retired S7 tag are removed) — host-side and, on a provisioned Apple Silicon
> host, verified end-to-end against the live external tools. The slices below
> describe the delivery order; the few seams still pending live hardware bring-up
> are grep-able as `hardware bring-up` and tracked in `docs/HARDWARE-BRINGUP.md`.

---

# 1. Implementation Philosophy

## Incremental Delivery

The system must be built in small, functional slices.

Each slice must:

* produce a working system
* pass acceptance tests
* not depend on future features

---

## OS Always Selected (no silent default)

```text id="sl0a1"
The user always selects the OS — no OS is ever applied silently.
```

The OS is selected in the `ai create` wizard (CLI §3.1): on a TTY the wizard
*presents* `debian-trixie` as the default to confirm or change, but the user
always makes the selection. The OS can also be supplied via the `--os` flag,
which on a TTY pre-seeds the wizard (the wizard still shows). Under `--json` or
no TTY there is no prompt and the spec is built straight from the flags, so
`--os` is **required** — a missing (or invalid) `--os` fails with exit 2. Slice 1
ships only the `debian-trixie` template; Slice 5 adds the rest.

---

## Cross-Slice Stability

Each slice must not break previous functionality.

---

## No Partial Features

A feature is either:

* fully working
* or not included

---

# 2. Slice 1 — Core Foundation (MVP)

## Goal

Deliver a working minimal platform.

---

## Scope

* macOS host support (Apple Silicon — required by the Microsandbox microVM runtime)
* Docker runtime for the service tier (Podman deferred to Slice 6); Microsandbox microVM runtime for workspaces
* rootless by default for the service tier (see 01-architecture-spec.md §6)
* `debian-trixie` OS Dockerfile template (only OS in Slice 1; selected in the `ai create` wizard or via `--os`, no implicit default); `ai create` writes `.ai-platform/Dockerfile` from it (plus the selected agent CLIs) and registers the workspace — the workspace OCI image is built later by `ai start`
* Single-agent system
* LiteLLM integration
* keys-in-LiteLLM credentials (real provider keys live in the gateway; the agent holds a scoped virtual key)
* Microsandbox workspace (microVM) creation

---

## Components

### Host

* macOS bootstrap launcher (thin shell script that fetches/execs the Go binary)
* Docker installation detection + Microsandbox runtime / virtualization (Apple Hypervisor) detection
* LiteLLM container startup
* keys-in-LiteLLM credentials: real provider keys live in the LiteLLM gateway
  (env passthrough at launch / its Postgres-backed store), never on platform disk
  or in the workspace; the agent authenticates to the gateway with a scoped
  virtual key. The secret-injection acceptance test depends on this credential
  path being fully functional.

---

### Runtime

* Docker (service tier) + Microsandbox (workspace microVMs)
* rootless by default for the service tier (fail `doctor` if rootless is unavailable; no rooted fallback)
* fail `doctor` if the host lacks the virtualization Microsandbox requires (Apple Silicon on macOS); no degraded non-microVM fallback

---

### Workspace System

* ai create (writes `.ai-platform/Dockerfile` from the debian-trixie template plus the selected agent CLIs and registers the workspace; the image is built later by `ai start`)
* single workspace per directory
* persistent host-mounted workspaces

---

### Model Layer

* LiteLLM configured
* models are **DB-backed** (`general_settings.store_model_in_db: true`): the
  rendered `config.yaml` carries **no `model_list`, no named aliases, no
  per-provider wildcards, and no default model**. Served models are managed via
  `ai keys add <provider>` (registers that provider's models.dev catalog models)
  and `ai models pull` (registers `ollama/<name>` local models):

```yaml id="m1l0"
general_settings:
  store_model_in_db: true   # served models live in the gateway DB, not config.yaml
```

---

### Keys Layer

* per-provider API keys added via `ai keys add <provider>` (encrypted in the
  LiteLLM Postgres DB via `LITELLM_SALT_KEY`; never in `config.yaml`, never on
  platform disk)
* real provider keys never leave the gateway; the workspace agent holds only a scoped virtual key

---

## Commands Implemented

```bash id="c1"
ai setup
ai create
ai delete            # `ai destroy` is an alias of this
ai start|stop|restart|exec
ai services status
ai keys add|list|remove
ai models status|test
ai state show|repair
ai doctor
ai logs
```

These are exactly the commands exercised by the `[S1]` acceptance tests. There
is no snapshot/upgrade/rollback or backup machinery — a workspace's environment is
its `.ai-platform/Dockerfile` (architecture §25), and overlay persistence lands
in Slice 4.

---

## Acceptance Criteria

* platform installs on macOS (Apple Silicon)
* project is created successfully
* workspace runs the debian-trixie microVM
* LiteLLM responds
* secrets resolved via the LiteLLM credential store (agent uses a scoped virtual key)
* no manual configuration required

---

# 3. Slice 2 — Context Optimization Layer

## Goal

Introduce Headroom + Caveman.

---

## Scope

* Headroom context management (input compression) — host service-tier proxy
  (`aip-headroom`, pulled image `ghcr.io/chopratejas/headroom:slim`, internal
  port :8787) in front of LiteLLM; no longer baked into the workspace image, and
  now **internal-only on `aip-net`** behind the nginx gateway (no host publish).
  Per-project strategy maps to Headroom per-request knobs.
* Caveman output compression — in-agent skill

(No platform memory system — agent memory is the agent's concern, architecture
§11.)

---

## Components

### Headroom

* token budgeting
* input-context compression
* summarization

---

### Caveman

* output-token compression
* compact commit / review messages

---

## Commands

```bash id="c2"
ai context status
ai context strategy <conservative|balanced|aggressive>
ai context caveman <lite|full|ultra|wenyan>
```

---

## Acceptance Criteria

* Headroom reduces input context size
* Caveman reduces agent output size
* large repos do not overload context

---

# 4. Slice 3 — Removed (multi-agent is not a platform concern)

The platform provides **one workspace per directory**. Running multiple AI agents
in a workspace — and any git they need (branches, worktrees, merges, rebases) — is
the **in-workspace agent CLI's** job, not the platform's (architecture §20–22).
There are no platform-managed agent identities or worktrees, and the `[S3]`
acceptance tag is retired. (The flat `ai agent` / `ai attach` / `ai sessions`
commands that DO exist are thin tmux session launchers/reattachers for an
in-workspace agent CLI — a session surface, not orchestration; see CLI §4.5b.)

---

# 5. Slice 4 — Overlay Persistence

## Goal

Make installed programs and agent state persist across workspace restart and
recreation, via the per-workspace overlay. (Environments are defined by the
project's `.ai-platform/Dockerfile` from Slice 1 — there is no snapshot
versioning/upgrade/rollback.)

---

## Scope

* per-workspace persistent writable overlay (architecture §26)
* overlay mounted over the image built from `.ai-platform/Dockerfile`
* overlay survives stop/start (`ai stop` → `ai start`/`ai restart` re-mount)
* overlay removed only when its workspace is permanently removed

---

## Commands

No new commands — persistence is automatic. (Environment changes are made by
editing `.ai-platform/Dockerfile` and recreating the workspace.)

---

## Acceptance Criteria

* a program installed in the workspace is still present after recreation
* agent state written outside the project mount persists across recreation
* the read-only image is never mutated; all changes land in the overlay

---

# 6. Slice 5 — Extended OS Support

## Goal

Add the remaining OS Dockerfile templates (Slice 1 ships the debian-trixie template).

---

## Scope

* `alma` template (Alma 10)
* `debian-bookworm` template (`debian:bookworm-slim`)
* `ubuntu` template (Ubuntu minimal)

---

## Acceptance Criteria

* all four OS templates selectable in the `ai create` wizard (or via `--os`)
* the user always selects the OS — no OS is applied silently
* all OS images expose an identical **base** tooling surface (agent CLIs are whatever the user selected)

---

# 7. Slice 6 — Linux Host Support

## Goal

Add Linux host compatibility.

---

## Scope

* Linux bootstrap (requires KVM virtualization for Microsandbox workspaces)
* Podman support (service tier)
* runtime abstraction improvements

---

## Acceptance Criteria

* ai setup works on Linux (KVM available)
* Docker or Podman auto-detected for the service tier
* Microsandbox microVMs run via KVM
* no workflow differences vs macOS

---

# 8. Cross-Cutting Systems

These are implemented progressively across slices.

---

## 8.1 LiteLLM

* host container (`aip-litellm`, internal :4000), backed by a Postgres container
  (`aip-litellm-db`) for the DB-backed admin UI / virtual keys + the encrypted
  provider-credential store. LiteLLM is **internal-only on `aip-net`** — reached
  through the nginx gateway (`litellm.<domain>:18787` admin UI, `…:18787/llm`
  host-CLI path), never host-published directly.
* unified routing; **no default model** — served models are DB-backed
  (`store_model_in_db: true`), catalog-driven, and registered on demand
* provider abstraction via **DB-backed served models** synced from the models.dev
  catalog when a provider key is added (`ai keys add`) and from Ollama when a
  local model is pulled (`ai models pull` → `ollama/<name>`); the rendered config
  has no `model_list`, no wildcards, and no named aliases
* **always-on guardrails** rendered into the generated LiteLLM config (all
  `default_on: true`, so no request — cloud included — can bypass them), scoped
  to **secrets/credentials, not general PII** (masking general PII was removed —
  it corrupted ordinary coding prompts):
  * `presidio-secrets-input` (pre_call) + `presidio-secrets-output` (post_call)
    mask only financial/identity secrets (CREDIT_CARD, US_SSN, US_BANK_NUMBER,
    IBAN_CODE, CRYPTO), backed by the `aip-presidio-analyzer` +
    `aip-presidio-anonymizer` service-tier containers
  * `hide-secrets` (LiteLLM's in-process detect-secrets) for API keys/tokens
  * (the in-process `detect_prompt_injection` callback was REMOVED — it
    false-positived on ordinary coding/Ollama traffic)
  * a `tool_permission` **tool firewall** (post_call) that DENIES destructive
    command tool-calls (`git push --force`, `rm -rf`, `terraform destroy`,
    `kubectl delete`, …) — for coding agents the bigger risk is destructive tool
    execution, not prompt content
* (The unmaintained LLM Guard was removed; Guardrails AI remains deliberately
  deferred.)

---

## 8.2 Credentials & Egress

* **keys-in-LiteLLM**: real provider keys live in the LiteLLM gateway (env
  passthrough at launch / its Postgres-backed store); the workspace agent holds
  only a scoped LiteLLM virtual key, never a real provider secret
* `ai keys` (add|list|remove) manages the per-provider keys, stored **encrypted
  in the LiteLLM Postgres DB** (`LITELLM_SALT_KEY`); no .env files, no keys in
  `config.yaml`, none on platform disk or in the workspace
* **egress**: a per-project **Microsandbox NetworkPolicy**, configured via
  `ai network` (modes `deny`/`public`/`unrestricted` + allowed host services +
  published ports); **default mode `public`** (allow-outbound to the open
  internet, private ranges still blocked; lets in-VM apps pull images and AI
  processes reach the internet, DNS-audited and re-lockable), with `deny` the
  locked-down posture; no egress proxy
* **secret masking / audit**: LiteLLM's always-on guardrails on every request
  (Presidio scoped to financial/identity secrets + `hide-secrets` + the
  `tool_permission` tool firewall; the `detect_prompt_injection` callback was
  removed as a false-positive source), which cloud routes cannot bypass (§8.1);
  plus per-domain DNS egress audit via the `aip-dns`
  CoreDNS resolver, surfaced by `ai network log`

---

## 8.3 LiteLLM routing

* thin gateway only (unified endpoint, aliasing, failover, provider-key injection)
* no per-task routing policy — the agent selects its model
* the in-workspace agent's model picker is **exactly the models the LiteLLM
  gateway currently serves** (its live DB-backed model set), read at workspace
  start via the injected `workspace.ServedModels` source
  (`litellm.KeyManager.ListModels`) and built by `workspace.Manager.pickerModels`.
  It is **not** a union of aliases + Ollama + a cloud seed — there are no aliases
  and no `cloud_models.yaml`. When the gateway is unreachable it degrades to an
  **empty** picker and writes **no** default model.

(MCP is not a platform concern — the agent manages it.)

---

## 8.7 Host service control commands

* `ai domain [name]` — show/set the platform base domain (default `aip.local`)
* `ai gateway show|set|clear` — machine-wide gateway address selection
* `ai litellm password` — set/rotate the LiteLLM admin UI password (env
  passthrough; offers to persist to `~/.ai-platform/.ai-platform.env`)
* `ai services start|stop|restart|update [service|all]` — manage the platform
  container set; `update` re-pulls moved tags (e.g. `latest`) and recreates
  affected containers
* `ai network ... log` — per-domain DNS egress audit via the `aip-dns` resolver

---

## 8.4 Sandbox Strategy

Default:

```text id="d1"
one Microsandbox microVM per workspace (hardware isolation, libkrun)
```

The container runtime (Docker/Podman) is used only for the service tier
(the `aip-dns` CoreDNS egress-audit resolver, the containerized Ollama
`aip-ollama`, the Presidio secret-masking pair, LiteLLM + its Postgres, the
Headroom input-compression proxy, and the `aip-proxy` nginx gateway), never to run
a workspace. The host tier has no optional services (Open WebUI is now a
per-workspace **in-VM** app and Odysseus was removed). All service-tier containers
share the private `aip-net` network, and **only the `aip-proxy` nginx gateway is
host-published** (the host port `18787`); every other service is internal-only on
`aip-net` and reached through it (Postgres + DNS stay loopback for admin access).

---

## 8.5 Networking

* the **`aip-proxy` nginx gateway is the SOLE host entry** to the service tier,
  on host port `18787`. It binds **127.0.0.1** in standalone/client roles and
  **0.0.0.0** in server role (`internal/setup` `currentBindHost()`). It forwards
  to the internal-only Headroom (`aip-headroom:8787`) → LiteLLM
  (`aip-litellm:4000`), and serves the Host-based UI subdomain
  (`litellm.<domain>` — the only host UI vhost) plus host-CLI gateway paths
  (`/v1` model path, `/ollama`, `/llm`). The platform base domain is set by
  `ai domain` (default `aip.local`; host-CLI URLs render under `localhost:18787`).
  TLS/HTTPS termination at nginx is still deferred.
* **role-based UI auth**: the server role secures the exposed UI (LiteLLM admin
  via `ai litellm password`); standalone/client are open. Passwords/master key are
  passed by **env passthrough** and can be persisted out of argv/disk in
  `~/.ai-platform/.ai-platform.env` (`internal/envfile`).
* microVM has a virtual NIC (virtio-net + gvproxy, userspace); no `host.docker.internal`, no host Docker socket
* machine-wide gateway selection via `ai gateway show|set|clear` →
  `ai_platform_host` in `runtime.yaml`; `runtime.ResolveGateway` derives the
  `http://<host>:<port>/v1` agent base URL (bare host or `host:port`, default
  port `18787`; empty → `host.microsandbox.internal:18787` for standalone/local)
* cross-platform resolution
* Ollama reached only via LiteLLM, never directly by the workspace
* egress is a per-project **Microsandbox NetworkPolicy** built on msb's deny
  fallthrough plus explicit allow rules. The **default mode is `public`**
  (allow-outbound to the open internet; private ranges still blocked by the deny
  fallthrough), which lets a fresh workspace pull in-VM app images and reach the
  internet; `deny` mode permits only the trusted host service ports + published
  ports, and the model gateway is always reachable in every mode. Per-project
  egress is configured by the `ai network` command (modes
  `deny`/`public`/`unrestricted` — default `public` — plus allowed host services
  and published ports, stored in `config.yaml`). The policy is fully userspace;
  there is **no egress proxy**. Enforcement renders the `ai network` declarations
  into Microsandbox net-rules at workspace create (`egress.MsbNetworkArgs` →
  `msb create`).

---

## 8.6 Audit Logging

* lifecycle tracking
* no secret logging

---

# 9. Global Acceptance Criteria

System is complete when:

```bash id="g1"
ai setup
ai create --name my-project   # interactive wizard: pick OS + agent CLIs (defaults: debian-trixie, OpenCode)
```

produces (for the OS the user selected):

* selected-OS workspace (e.g. Debian trixie)
* working LiteLLM
* keys-in-LiteLLM secrets (agent uses a scoped virtual key)
* Headroom input compression
* Caveman output compression
* Dockerfile-defined environment + overlay persistence
* reproducible environments
* zero manual configuration

---

# 10. Implementation Rule

No slice is considered complete until:

* all commands work
* all acceptance tests pass
* no manual configuration is required
