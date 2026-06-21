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

The OS is chosen in the `ai project create` wizard (CLI §3.1): the wizard
*presents* `debian-trixie` as the default to confirm or change, but the user
always makes the selection. There is no `--os` flag. Slice 1 ships only the
`debian-trixie` template; Slice 5 adds the rest. A non-interactive `create` (no
TTY) cannot prompt and fails if no OS is
selected.

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
* `debian-trixie` OS Dockerfile template (only OS in Slice 1; selected in the `ai project create` wizard, no implicit default); `ai project create` writes `.ai-platform/Dockerfile` from it (plus the selected agent CLIs) and builds the workspace OCI image from it
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

### Project System

* ai project create (writes `.ai-platform/Dockerfile` from the debian-trixie template, builds the image)
* single workspace per project
* persistent host-mounted projects

---

### Model Layer

* LiteLLM configured
* basic routing:

```yaml id="m1l0"
default: gemma4   # -> ollama/gemma4:31b (local; Ollama is the default provider)
```

---

### Secrets Layer

* keys-in-LiteLLM credential store required (`ai secrets` manages LiteLLM-side credentials)
* real provider keys never leave the gateway; the workspace agent holds only a scoped virtual key

---

## Commands Implemented

```bash id="c1"
ai setup
ai project create
ai project delete
ai workspace start|stop|destroy|exec
ai services status
ai secrets set|map|list
ai models status|test
ai state show|repair
ai doctor
ai logs
```

These are exactly the commands exercised by the `[S1]` acceptance tests. There
is no snapshot/upgrade/rollback or backup machinery — a project's environment is
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
  (`aip-headroom`, pulled image `ghcr.io/chopratejas/headroom:slim`, :8787) in
  front of LiteLLM; no longer baked into the workspace image. Per-project
  strategy maps to Headroom per-request knobs.
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

The platform provides **one workspace per project**. Running multiple AI agents
on a project — and any git they need (branches, worktrees, merges, rebases) — is
the **in-workspace agent CLI's** job, not the platform's (architecture §20–22).
There are no `ai agent` commands and no platform-managed worktrees. The `[S3]`
acceptance tag is retired.

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
* overlay survives stop/start and `ai workspace destroy` recreation
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

* all four OS templates selectable in the `ai project create` wizard
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

* host container (`aip-litellm`, :4000), backed by a Postgres container
  (`aip-litellm-db`) for the DB-backed admin UI / virtual keys
* unified routing
* provider abstraction
* an **always-on Presidio PII guardrail** rendered into the generated LiteLLM
  config (pre_call input + post_call output, both `default_on: true` — no
  request, cloud included, can bypass it), backed by the
  `aip-presidio-analyzer` + `aip-presidio-anonymizer` service-tier containers

---

## 8.2 Credentials & Egress

* **keys-in-LiteLLM**: real provider keys live in the LiteLLM gateway (env
  passthrough at launch / its Postgres-backed store); the workspace agent holds
  only a scoped LiteLLM virtual key, never a real provider secret
* `ai secrets` manages the LiteLLM-side credentials; no .env files, no secrets on
  platform disk or in the workspace
* **egress**: a default-deny **Microsandbox NetworkPolicy** per project, configured
  via `ai network` (modes `deny`/`public`/`unrestricted` + allowed host services +
  published ports); no egress proxy
* **PII/audit**: LiteLLM's always-on **Presidio** guardrails on every request,
  which cloud routes cannot bypass (§8.1)

---

## 8.3 LiteLLM routing

* thin gateway only (unified endpoint, aliasing, failover, provider-key injection)
* no per-task routing policy — the agent selects its model

(MCP is not a platform concern — the agent manages it.)

---

## 8.4 Sandbox Strategy

Default:

```text id="d1"
one Microsandbox microVM per workspace (hardware isolation, libkrun)
```

The container runtime (Docker/Podman) is used only for the service tier
(LiteLLM + its Postgres, the containerized Ollama `aip-ollama`, the Presidio
PII-guardrail pair, and the host Headroom proxy), never to run a workspace. All
service-tier containers share the private `aip-net` network.

---

## 8.5 Networking

* microVM has a virtual NIC (virtio-net + gvproxy, userspace); no `host.docker.internal`, no host Docker socket
* AI_PLATFORM_HOST abstraction for reaching trusted host services (LiteLLM)
* cross-platform resolution
* Ollama reached only via LiteLLM, never directly by the workspace
* egress is a **default-deny Microsandbox NetworkPolicy** per project: deny by
  default, permitting only trusted host service ports + published ports. Per-project
  egress is configured by the `ai network` command (modes `deny`/`public`/`unrestricted`
  — default `deny` — plus allowed host services and published ports, stored in
  `config.yaml`). The policy is fully userspace; there is **no egress proxy**. Live
  NetworkPolicy enforcement at workspace start (applying the `ai network`
  declarations via the Microsandbox Go SDK) is a deferred hardware bring-up seam.

---

## 8.6 Audit Logging

* lifecycle tracking
* no secret logging

---

# 9. Global Acceptance Criteria

System is complete when:

```bash id="g1"
ai setup
ai project create my-project   # interactive wizard: pick OS + agent CLIs (defaults: debian-trixie, OpenCode)
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
