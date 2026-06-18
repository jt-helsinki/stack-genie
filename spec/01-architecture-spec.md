# 01-architecture-spec.md

# AI Development Platform

## Architecture Specification

Version: 1.0

Status: Target End-State Architecture

---

# 1. Purpose

This document defines the end-state architecture for the AI Development Platform.

The platform provides reproducible AI-powered software development environments using Microsandbox microVMs, centralized model access, context optimization, secure secret brokering, and host-backed project persistence.

This document describes the final architecture regardless of implementation phase.

Implementation sequencing is defined separately in:

```text
02-implementation-roadmap.md
```

---

# 2. Goals

The platform must provide:

* One-command installation
* One-command project creation
* Reproducible development environments
* Host-backed persistent source code
* AI-native development workflows
* Hardware-isolated microVM workspaces (Microsandbox)
* Rootless container runtime for the service tier by default
* Centralized model access
* Context optimization
* Secure secret management
* Dockerfile-defined, reproducible environments
* Cross-platform host support

---

# 3. Design Principles

## Reproducibility

All projects must be reproducible from:

* source repository (including `.ai-platform/Dockerfile`)
* project configuration

No project behavior should depend on manual workstation configuration.

---

## Isolation

Projects must be isolated from:

* other projects
* host operating system
* unrelated agents

Isolation is enforced through Microsandbox microVMs (hardware-level isolation
via libkrun), one per workspace.

---

## Persistence

Project source code is always stored on the host.

Workspace recreation must never destroy project data.

---

## Security

Secrets must never be:

* committed to repositories
* stored in plaintext
* distributed through environment files

Runtime injection is mandatory.

---

## Host Agnostic

The platform must function consistently on:

* macOS (Apple Silicon — required by the Microsandbox microVM runtime)
* Linux (KVM virtualization must be available)
* Windows (via WSL2 with nested virtualization — **at-risk**; see §6 and §30)

Behavior must remain consistent regardless of runtime. Intel Macs are **not
supported** because the Microsandbox microVM runtime requires Apple Silicon.

---

# 4. High-Level Architecture

The platform's layers relate along two distinct axes. Drawing them as one
linear stack is misleading, so they are shown separately.

## 4.1 Infrastructure Containment (what runs inside what)

The platform uses **two distinct runtimes** on the host, for two distinct
purposes:

```text
Host Layer
 ├─ Microsandbox microVM runtime (libkrun)         ← workspaces
 │   └─ Sandbox Layer (workspace microVM)
 │       └─ AI Tooling Layer (OpenCode / Claude Code / Codex / Gemini CLI — selected per env)
 │
 └─ Container Runtime (Docker / Podman)            ← service tier
     └─ container-tier services (LiteLLM · Headroom · Ollama)
```

Workspaces are **microVMs** (hardware isolation), not containers. The
container runtime is used only for the stateless service tier, never for
workspaces.

Host services (run on the host, not inside a workspace):

```text
LiteLLM (Model Layer) · Headroom · Ollama   — container tier (Docker/Podman)
ClawPatrol (Secrets Layer)                  — native (OS service)
Microsandbox                                — microVM runtime, driven by the `ai` CLI via the Go SDK / `msb` (no daemon)
```

(The model-request path below involves the LiteLLM/ClawPatrol/Headroom subset;
Ollama is a container-tier service too — see §5. Microsandbox is not a
long-running service: it is invoked directly to create and drive workspace
microVMs.)

The Project Layer is the top-level, user-facing artifact: host-stored source
under `~/projects/<project>` mounted into the workspace.

## 4.2 Model-Request Path (how a call flows)

```text
Agent (AI Tooling)
 ↓  Headroom compresses input · Caveman steers output   (Context Optimization)
LiteLLM (Model Layer)
 ↓  ClawPatrol injects credentials on the wire           (Secrets Layer)
Provider
```

Context Optimization sits *between* the agent and the model (not above the AI
Tooling), and the Secrets Layer sits on the wire *between* the model and the
provider. The agent reaches Headroom across the microVM boundary via
`AI_PLATFORM_HOST`; the Headroom → LiteLLM → ClawPatrol → provider hops are
host-side. See Sections 8–9 (context optimization), 17 (secrets), and
**29 (the full per-component networking model)** for detail.

---

# 5. Host Layer

The Host Layer provides all persistent infrastructure.

Supported hosts:

* macOS
* Linux
* Windows

Responsibilities:

* Project storage
* Shared AI resources
* LiteLLM deployment
* ClawPatrol deployment
* Optional Ollama deployment
* OS Dockerfile templates
* Platform state

---

## Host Directory Structure

```text
~/.ai-platform/
```

Contains:

```text
~/.ai-platform/
├── agents/
├── audit/
├── cache/
├── config/        # global settings + projects index (no per-project state)
├── logs/
├── overlays/
├── prompts/
├── skills/
├── templates/     # OS Dockerfile templates + shared templates
└── tools/
```

`~/.ai-platform/` holds **only global settings and host artifacts** — there is
no per-project state here. Per-project state lives in the project itself (see
below).

---

## State Management

State is split between **global** and **project-local**:

* **global** (`~/.ai-platform/config/`): detected runtime (`runtime.json`),
  pinned service versions (`versions.json`), global config (`config.yaml`),
  rendered host-service configs, and a small **projects index**
  (`projects.json`: maps project name → path; written only on create/delete)
* **project-local** (`<project>/.ai-platform/`): everything specific to one
  project, stored *in the project* so it travels with the repo and can be
  git-tracked or ignored:

```text
<project>/.ai-platform/
├── Dockerfile        # tracked — defines the environment (§25)
├── config.yaml       # tracked — project config
├── profile.yaml      # tracked — project profile
├── project.json      # tracked — { name, os, created }
├── skills/caveman/   # tracked — platform-seeded Caveman skill (§9)
├── .gitignore        # ignores run/
└── run/              # gitignored — host-local runtime state
    └── workspaces/<workspace-id>.json
```

Rules:

* **tracked** files (`Dockerfile`, `config.yaml`, `profile.yaml`,
  `project.json`, `skills/caveman/`) are the environment + config definition —
  committable so the project is reproducible from git
* **`run/`** holds host-local runtime handles (Microsandbox sandbox ids, status)
  and is **gitignored** — machine-specific, never committed
* per-entity sharding (one file per workspace under `run/`)
  avoids write contention
* **all state writes are atomic**: write to a temp file in the same directory,
  then `rename()` over the target — a crash mid-write never corrupts state
* `ai state repair` reconstructs `run/` by reading the project + Microsandbox/git;
  project discovery uses the global `projects.json` index

Large host-local artifacts (the per-workspace **overlays**, §26) stay under
`~/.ai-platform/overlays/` — they are not git material and do not belong in the
project.

---

## Host Services Control Plane

The platform's host services — LiteLLM, ClawPatrol, Headroom, and (optional)
Ollama — plus the Microsandbox microVM runtime are installed, configured, and
supervised by the `ai` CLI. The CLI is the **single control plane**: the user
never invokes `docker compose`, `msb`, `launchctl`, or `systemctl` directly.

### One Tool, Uniform Lifecycle

All services are managed through the same commands regardless of how they run:

```text
ai setup            install + configure + start everything (idempotent)
ai setup --upgrade  bump pinned versions and re-reconcile
ai services status           health across all services
ai services restart <svc>    uniform lifecycle (hides container vs native)
ai doctor                    checks + repair suggestions
ai logs --service <svc>      one log surface
```

### Run Modes (an implementation detail, hidden from the user)

| Service | Run mode | Why |
|---|---|---|
| LiteLLM | container (via Runtime) | HTTP only; no host privileges |
| Headroom | container (via Runtime) | HTTP proxy; no host privileges |
| Ollama (optional) | container (via Runtime) on all platforms | uniform deployment; CPU-only on macOS (Docker has no GPU passthrough) |
| ClawPatrol | native (OS service) | terminates the workspace WireGuard tunnel (in user space) + injects credentials on the wire (§17, §29) |
| Microsandbox | microVM runtime, invoked on demand | drives workspace microVMs via the Go SDK / `msb`; no daemon to supervise (§7) |

**docker compose is not used.** Container-tier services are managed directly
through the runtime abstraction (§6), so docker and podman remain
interchangeable and there is no second orchestration mechanism. The native
service (ClawPatrol) is registered with the OS service manager (launchd /
systemd / Windows service) so it restarts on boot without user action.
Microsandbox is **not** a long-running service — the `ai` CLI invokes it
directly (Go SDK / `msb`) to create, start, stop, and destroy workspace
microVMs; `ai setup` only verifies the runtime is installed and the host
supports virtualization (§6).

### Configuration by Reconciliation

The platform `config.yaml` (§27) is the single source of truth. The CLI
**renders** each service's native config from it and reconciles desired vs.
actual state; `ai setup` means "make reality match config" and is safe
to re-run. Pinned versions/digests live in `config/versions.json`. Rendered
service configs live under `config/<service>/`.

### Install + Configure Summary

* **install**: container images are pulled by digest; native binaries are
  downloaded as pinned, checksum-verified release artifacts into
  `tools/<name>/<version>/`
* **configure**: rendered from platform config — LiteLLM routing/aliases (§14–15)
  referencing **placeholder** credentials only; ClawPatrol gateway (HCL) holds
  the real credentials in its own SQLite; Microsandbox driven non-interactively
  per workspace (image, mounts/volumes, resource limits) via the Go SDK / `msb`;
  Ollama registered as a LiteLLM provider when enabled
* **startup ordering**: container runtime + Microsandbox runtime verified →
  ClawPatrol (credentials loaded) → LiteLLM (references placeholders) →
  [Ollama] → verify (workspace microVMs are created on demand, not at setup)

### Secrets Boundary

Real secrets live **only** in ClawPatrol's store. Every other service holds
placeholders, so no plaintext secret reaches LiteLLM config, the Microsandbox
workspace, or a backup.

---

# 6. Runtime Layer

The platform uses two runtimes for two purposes.

## 6.1 Container Runtime (service tier)

Used for the stateless container-tier services (LiteLLM, Headroom, Ollama).

Supported runtimes (end-state):

* Docker
* Podman

Requirements:

* automatic detection
* runtime abstraction
* consistent behavior

Users should not need to modify workflows based on runtime choice.

### Rootless

**Rootless is the default and required mode** wherever the runtime supports it.
The platform must not require a root daemon or the host Docker socket (see
§30). On a runtime or host that cannot provide rootless operation, the platform
must fail the relevant `doctor` check rather than silently fall back to a
rooted daemon.

## 6.2 microVM Runtime (workspaces)

Workspaces run as **Microsandbox microVMs** (libkrun), not containers. This
runtime is driven directly by the `ai` CLI through the Microsandbox **Go SDK /
`msb` CLI** — there is no daemon and no host Docker socket involved (§7, §30).

Host requirements:

* **macOS:** Apple Silicon (the microVM runtime requires the Apple Hypervisor;
  Intel Macs are unsupported)
* **Linux:** KVM virtualization available to the user
* **Windows:** WSL2 with nested virtualization — **at-risk**; the `doctor` check
  must report clearly when nested virtualization is unavailable

The platform consumes OCI images: a workspace image is built from the project's
`.ai-platform/Dockerfile` (§25) and run as a microVM. On any host that cannot
provide the required virtualization, the platform must fail the relevant
`doctor` check rather than silently degrade.

## Delivery Phasing

Runtime support is delivered incrementally (see `02-implementation-roadmap.md`):

* Slice 1: Docker (service tier) + Microsandbox (workspaces), rootless by
  default, macOS (Apple Silicon).
* Slice 6: Podman support + full container-runtime abstraction on Linux.

Acceptance tests that exercise Podman or Docker/Podman equivalence apply from
Slice 6 onward; the rootless test applies from Slice 1. Each test in
`05-acceptance-tests.md` is tagged with the slice from which it is expected to
pass.

---

# 7. Sandbox Layer (Microsandbox)

Microsandbox provides:

* workspace isolation (hardware-level, via libkrun microVMs)
* workspace lifecycle (one workspace per project)

Each workspace is a microVM with its own Linux kernel, root filesystem, and
network boundary — a stronger isolation boundary than a container namespace.
The runtime has **no daemon**: the `ai` CLI drives it directly through the
Microsandbox Go SDK / `msb` CLI.

---

## Workspace Types

### Project Workspace

**One workspace per project** — the development environment for that project.
There are no per-agent workspaces; running multiple agents inside the workspace
is the in-workspace agent CLI's concern (§20).

---

## Workspace Lifecycle

States:

```text
Created
 ↓
Started
 ↓
Stopped
 ↓
Archived
 ↓
Destroyed
```

Workspaces are disposable.

Project data is not.

---

## Microsandbox Integration Mapping

The platform drives Microsandbox through the Microsandbox Go SDK / `msb` CLI.
The mapping below is normative.

### Workspace Naming

```text
workspace:  aip-<project>
```

The name is derived deterministically from the project identifier, so the
platform can resolve a workspace from state without a separate lookup table.

### Image

The workspace image is an **OCI image built from the project's
`.ai-platform/Dockerfile`** (§25) and run as the Microsandbox microVM for that
workspace. There is no shared, versioned snapshot image — each project builds
from its own Dockerfile. (Microsandbox consumes OCI images directly, so the
Dockerfile flow is unchanged; the image is built once and booted as a microVM.)

### Mount Rules

```text
host  ~/projects/<project>            →  workspace  ~/workspace        (read-write)
host  ~/.ai-platform/agents,skills,   →  workspace  (shared resources) (read-only)
      prompts,templates
```

* the host project directory is the single source of truth, mounted read-write
  at `~/workspace`
* no persistent data is written outside the mounted paths

### Lifecycle Mapping

| Platform state (workspace §7) | Microsandbox operation (`msb` / Go SDK) |
|---|---|
| Created   | `create` (no start) |
| Started   | `start` |
| Stopped   | `stop` |
| Archived  | `stop` + retain metadata |
| Destroyed | `rm` |

### Ports

Workspace microVMs reach host services (LiteLLM, Headroom, ClawPatrol) via
`AI_PLATFORM_HOST` (§29) — never `host.docker.internal`. The platform injects
`AI_PLATFORM_HOST` and the service ports into the workspace environment at
start. The workspace has a virtual NIC whose default route is a WireGuard tunnel
to the ClawPatrol gateway, so all external egress is confined to the broker
(§29.3–29.4). The full per-component path is specified in §29.

### Logs

Microsandbox workspace logs are streamed to `~/.ai-platform/logs/microsandbox.log`;
per-workspace detail is retrievable via `ai logs --workspace <project>`.

### Cleanup

Cleanup depends on whether the removal is recoverable:

* **`ai workspace destroy`** deletes only the Microsandbox microVM/runtime handle.
  It **keeps the persistent overlay** (§26) and the host source. The workspace
  is fully recoverable with `ai workspace start`, which rebuilds from
  `.ai-platform/Dockerfile` and re-mounts the same overlay. (Non-destructive.)
* **`ai project delete`** is permanent for the project: delete its workspace and
  **its overlay**, remove the project's `projects.json` index entry, and clear
  `<project>/.ai-platform/run/`. Host source is preserved unless `--purge` is
  given.

---

# 8. Context Optimization Layer

The platform includes a mandatory context optimization layer.

Purpose:

* reduce token usage
* improve context quality
* reduce costs
* improve scalability

Components:

* Headroom — compresses input context
* Caveman — compresses agent output

These bound token usage on both sides of every model request: Headroom
compresses the *input* sent to the model, and Caveman compresses the *output*
the agent emits.

---

# 9. Caveman

Caveman is the output-token compression component.

The GitHub repository is found at:

[https://github.com/JuliusBrussee/caveman](https://github.com/JuliusBrussee/caveman)

Caveman reduces the number of tokens an agent *emits* by rewriting agent
communication into a terse, fragment-based style while preserving technical
accuracy. It compresses expression, not reasoning, and never alters code, URLs,
or factual content.

Responsibilities:

* output-token reduction
* compact commit messages
* single-line review comments

---

## Compression Levels

Caveman exposes selectable compression levels:

* lite — remove filler only
* full — default compression
* ultra — telegraphic style
* wenyan — maximum compression

---

## Caveman Integration

Caveman integrates as an agent skill **seeded per project** into
`<project>/.ai-platform/skills/caveman/` at project creation (not baked into the
workspace image). It is **git-tracked** — committed with the project like the
Dockerfile, so it travels with the repo and the project stays reproducible from
git alone; it is not re-installed or auto-upgraded on workspace start. It is the
output-side complement to Headroom.

---

## Context Optimization Workflow

Headroom is an interception proxy on the request path; Caveman is a
generation-steering skill loaded in the agent. Headroom shrinks what goes
*in*; Caveman shrinks what comes *out*.

```text
        Caveman skill (loaded in agent → steers terse generation)
                              │
                              ▼
  Agent ── full context ──► Headroom ──► LiteLLM ──► Model
    ▲          (compresses input context)             │
    └──────────── compact output (Caveman-steered) ───┘
```

---

# 10. Headroom

Headroom manages context budgets.

Headroom runs **on the host** as a proxy on the model-request path, in front of
LiteLLM. It is not installed inside workspaces — every request from a workspace
agent traverses the host Headroom proxy before reaching the model.

The Github repository is found at:

[https://github.com/chopratejas/headroom](https://github.com/chopratejas/headroom)

Responsibilities:

* token accounting
* summarization
* compression
* budget enforcement

---

## Headroom Rules

Every model request must pass through Headroom.

No provider should receive unbounded context.

---

## Example Configuration

```yaml
context:
  max_tokens: 64000
  compression_threshold: 0.75
  strategy: balanced
```

Supported strategies:

* conservative
* balanced
* aggressive

---

# 11. Agent Memory — Removed

The platform does **not** manage agent or project memory. This is delegated to
the in-workspace agent (OpenCode/Claude Code/Codex/Gemini), which maintains its own
project understanding (e.g. `AGENTS.md`) and conversation history in the
host-backed project source, so it persists across workspace recreation without
platform involvement. Reimplementing memory would duplicate the agent.

---

# 12. AI Tooling Layer

## Base tooling (always installed)

Every workspace image installs this base tooling via the OS Dockerfile
templates (§25); it is identical across all OSes:

* Git
* GitHub CLI

## Agent CLIs (selected per environment)

The AI coding-agent CLIs are **not** all baked in. One or more are chosen at
environment setup (`ai project create`, CLI §3.1) from the supported list:

* **OpenCode** — the default; pre-selected and the default agent
* **Claude Code**
* **Codex**
* **Gemini CLI**

Selection is **multi-select**: install any subset (at least one), with OpenCode
pre-selected. The chosen CLIs are written into the project's
`.ai-platform/Dockerfile` at creation, so the installed set is reproducible from
the project rather than a fixed, baked-in surface. The **default agent** — which
CLI new agents use unless told otherwise — is recorded as `agent.default_tool`
(repo-layout §12.4) and must be one of the installed CLIs (default OpenCode).

Not baked into the image:

* **Caveman** — seeded per project into `<project>/.ai-platform/skills/caveman/`
  at project creation and **git-tracked** (an agent skill, not an image concern;
  see §9), so it travels with the project and is independent of the image
* **Headroom** — runs on the host as a proxy on the model-request path (§10)

MCP servers are configured and run by the agent itself (the platform does not
manage MCP).

---

# 13. Tool Provider Layer

Supported providers (selected per environment, §12):

* OpenCode — default
* Claude Code
* Codex
* Gemini CLI

Future providers:

* Cursor CLI
* Aider
* OpenHands

Platform services must remain tool-independent.

---

## Example Configuration

The installed set and the default agent CLI (which must be one of the installed
CLIs):

```yaml
agent:
  tools: [opencode, claude-code]   # installed in this environment
  default_tool: opencode           # default agent CLI; must be in agent.tools
```

---

# 14. Model Layer

All model access flows through LiteLLM. On the full path, Headroom compresses
the request before LiteLLM, and ClawPatrol injects credentials on the wire
between LiteLLM and the provider.

```text
Agent
 ↓  (Headroom compresses input)
LiteLLM
 ↓  (ClawPatrol injects credentials on the wire)
Provider
```

---

## Supported Providers

* OpenAI
* Anthropic
* Google Gemini
* OpenRouter
* Ollama

---

## Routing

The platform does **not** define per-task routing policies — model selection is
the agent's job (OpenCode/Claude Code/Codex/Gemini each choose their model). LiteLLM
provides only a unified endpoint, provider aliasing, a single default, and
failover.

```yaml
routing:
  default: gpt-5
```

---

# 15. LiteLLM

Runs on the host as a **thin shared gateway**.

Purpose:

* unified provider endpoint for all tools (and Ollama)
* single egress point for ClawPatrol credential injection
* model aliasing
* failover
* monitoring

It does not make model-selection decisions on the agent's behalf.

---

## Example Aliases

```yaml
gpt-5: openai/gpt-5

claude-sonnet: anthropic/claude-sonnet

gemini-pro: gemini/gemini-pro

llama: ollama/llama
```

---

# 16. Ollama

Rules:

* never installed in workspaces
* runs as a **container-tier service** (Docker/Podman, via the runtime
  abstraction, §6.1) on all platforms — never a native host install
* on macOS the container is **CPU-only** (Docker has no GPU passthrough); use
  remote deployment for GPU-accelerated inference
* remote deployment supported

All access occurs through LiteLLM.

---

# 17. Secrets Layer

The only supported secret system is ClawPatrol.

The GitHub repository for ClawPatrol is found at:

[https://github.com/denoland/clawpatrol](https://github.com/denoland/clawpatrol)

and the documentation is found at:

[https://clawpatrol.dev/docs/introduction/](https://clawpatrol.dev/docs/introduction/)

ClawPatrol is both a **security firewall** for agent traffic and a
**credential broker**. The same gateway that enforces access policy also holds
the real credentials, so the platform uses it as its single secrets system.

---

## How Secrets Are Injected

ClawPatrol injects credentials **at the wire level**, not into the workspace:

* the agent process holds only a placeholder value
  (e.g. `GITHUB_TOKEN=ghp_clawpatrol_placeholder_do_not_use`)
* the ClawPatrol gateway substitutes the real credential into the request
  in transit
* the agent never sees, stores, or can exfiltrate the real secret

```text
Agent in workspace microVM (placeholder)
 ↓  WireGuard tunnel (the microVM is a WireGuard peer of the gateway)
ClawPatrol Gateway (swaps in real credential, enforces policy)
 ↓
Provider / git / MCP / web
```

The gateway is a single Go binary that holds policy, credentials, and audit
logs in local SQLite. The workspace microVM connects to it **over WireGuard**:
because the microVM has its own network boundary (§29), the gateway intercepts
at the tunnel — it does not rely on sharing the agent's network namespace. The
gateway is reachable from the workspace at `AI_PLATFORM_HOST` (§29.2).

---

## TLS Interception and the Trust Anchor

To inject a credential into an **HTTPS** request, the gateway must read and
rewrite the request, so it **terminates TLS** at the gateway: it presents a
certificate signed by a **ClawPatrol local CA**, inspects and injects, then
re-originates TLS to the real destination with that destination's genuine
certificate. For the in-workspace agent to trust the gateway rather than see a
certificate error, the **ClawPatrol CA root must be installed in the workspace's
trust store**.

This is a mandatory bootstrap step the platform performs — it is not optional and
HTTPS credential injection does not work without it:

* the CA root is generated/held by ClawPatrol; the platform **never** ships a
  shared or pre-baked CA
* **every client whose traffic the gateway brokers must trust the CA.** That is
  two places: (1) the **workspace** — installed at workspace start into the
  system trust store and the common per-tool stores agents use (e.g.
  `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`),
  via injected environment and/or the trust-store path, **not** baked into the
  OCI image so rotating the CA needs no rebuild; and (2) **host-side LiteLLM**,
  whose egress to providers is brokered by the gateway, configured at `ai setup`
* the CA root is a **public** trust anchor, not a secret: it contains no
  credential material, so installing it does not violate the secrets boundary
  (§5). The corresponding CA **private key** stays only in ClawPatrol's store,
  never in the workspace

### Threat-model note

A workspace-scoped interception CA means the gateway can read the plaintext of
every TLS request the agent makes. This is **by design** — it is exactly how
credential injection and request policy work — but it widens trust in the
gateway, so:

* the CA is **per-install and per-workspace-scoped**, generated locally; a
  compromise of one host's CA does not affect any other install
* the CA is trusted **only inside the workspace**, never added to the host's
  trust store
* the gateway still logs only that an injection occurred, never plaintext bodies
  or secret values (§31)
* `ai doctor` verifies the workspace trusts the current ClawPatrol CA and flags
  drift after a CA rotation

---

## Credential Bootstrap

The user imports credentials into ClawPatrol; the platform never writes secrets
to disk itself. The entry points are the `ai secrets` commands
(`04-cli-specification.md` §16.1):

* `ai secrets set <name> --stdin` stores a credential in ClawPatrol's SQLite
* `ai secrets map <name> --env <ENV_VAR>` binds it to the workspace placeholder
  the gateway swaps on the wire

`ai setup` determines which provider credentials the configured routing
needs (§14). A missing credential is **not** a setup hard-fail: interactively
`setup` may prompt for it; non-interactively it proceeds and warns. A provider
whose credential is absent is reported by `ai doctor`, and the actual model call
fails (exit `5`) only when that credential is genuinely needed at request time.
The platform never fabricates or defaults a secret.

## OS Permissions Required by ClawPatrol

The gateway terminates the workspace **WireGuard** tunnel (§29.3) in **user
space** (§29.1) and brokers egress. By design this avoids a privileged host
network interface — the default termination is a userspace WireGuard endpoint on
a loopback UDP port (no `utun`, no NetworkExtension, no admin networking prompt):

* **macOS**: none beyond the hypervisor entitlement that Microsandbox already
  needs — the userspace WG endpoint is an ordinary UDP listener
* **Linux**: none beyond the userspace WG UDP listener (no `CAP_NET_ADMIN`)
* **Windows**: the WSL2 userspace WG endpoint

(The legacy alternative — a kernel `utun`/NetworkExtension WireGuard interface —
is **not** used, precisely so the tunnel does not contend with the hypervisor for
privileged host facilities; see plan §8.2. A userspace-WG sidecar in front of
ClawPatrol preserves this if the gateway cannot terminate WG in user space
itself.) Because the workspace is a microVM that routes all external egress
through the tunnel (§29.4), the gateway intercepts at the tunnel rather than by
sharing the agent's network namespace. If a required facility is unavailable,
`setup` / `doctor` fail the ClawPatrol check with exit `5` and actionable
guidance (no silent degraded mode).

In the **initial slice** the gateway runs as a forward proxy instead of a WG
terminator (§29.5), which needs even less — a single listening port — so these
permissions are an end-state ceiling, not an S1 prerequisite.

## Platform-Specific Service Setup

The gateway is registered as a managed native service so it survives reboot:

* **macOS**: launchd user agent
* **Linux**: systemd unit (user, or system where required for netns)
* **Windows**: Windows service

These are written and loaded by `ai setup`; the user does not author
them (see §5, Host Services Control Plane).

---

## Requirements

No:

* .env files
* plaintext secrets
* repository secrets

All credentials are brokered by ClawPatrol and substituted at runtime. Real
secret values never reach the workspace filesystem or environment.

---

## Security Firewall Role

Because ClawPatrol sits on the wire, it also enforces:

* allow / deny rules on outbound requests (HCL + CEL)
* human-in-the-loop approval for risky actions
* full audit logging (secret values never logged)

---

## Supported Secret Types

* API keys
* OAuth credentials
* model credentials
* MCP server credentials

ClawPatrol brokers **any** credential used on an outbound request from the
workspace — including credentials needed by MCP servers the agent spawns. The
platform does not manage MCP connections (the agent does), but it still secures
the secrets those connections use, via the same wire-level placeholder
injection.

---

# 18. Shared Resources

Host-managed shared resources:

```text
~/.ai-platform/
```

Includes:

```text
agents/
skills/
prompts/
templates/
```

Mounted read-only.

---

# 19. Project Architecture

Host location:

```text
~/projects/<project>
```

Workspace location:

```text
~/workspace
```

---

## Generated Structure

```text
.ai-platform/
  Dockerfile         # tracked — environment (§25)
  config.yaml        # tracked
  profile.yaml       # tracked
  project.json       # tracked — { name, os, created }
  skills/caveman/    # tracked — platform-seeded Caveman skill (§9)
  .gitignore         # ignores run/
  run/               # gitignored — workspace/agent runtime state

.worktrees/

docs/

scripts/

src/

tests/

README.md
```

`<project>/.ai-platform/` holds the project's environment (Dockerfile), config,
and gitignored runtime state — no platform-managed memory (delegated to the
agent, §11). MCP is configured by the agent, not the platform. This matches the
canonical project layout in `03-repository-layout.md` §2.1.

---

## Identifier Rules

Project names become directory names and the Microsandbox workspace name (§7),
so they are validated:

```text
project name:  ^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$
```

* lowercase alphanumeric and hyphens; 2–40 chars; no leading/trailing hyphen
* a name that fails validation exits `2`
* project names are unique across the platform
* the derived workspace name is `aip-<project>` (§7), which therefore also
  satisfies OCI image / Microsandbox naming constraints

---

# 20. Multi-Agent System — Removed (not a platform concern)

The platform provides **one workspace per project**. Running multiple AI agents
on a project — and any source isolation that needs (git branches, worktrees,
separate checkouts) — is the **in-workspace agent CLI's** job, not the platform's
(consistent with §11: the platform doesn't manage the agent's concerns). The
platform never creates per-agent workspaces, branches, or worktrees.

---

# 21. Git Workflow

The platform's only git involvement is at project creation: it runs `git init`
(or `git clone` with `--clone`, CLI §3.1) so the project is a repo. **Everything
else is the in-workspace agent's job** — branches, commits, rebases, merges,
conflict resolution, worktrees, and pull requests. The platform makes no model
calls and runs no merges.

The project's `config.yaml` may carry a `git.merge_strategy` hint
(`squash | merge | rebase`) that the in-workspace agent **may** read, but the
platform does not act on it.

---

# 22. Conflict Resolution — Removed

The platform does **not** perform AI merge or conflict resolution, nor any git
beyond init/clone (§21). Branching, merging, and conflict resolution are
LLM-on-code work done by the in-workspace agent (OpenCode/Claude Code/Codex/Gemini).

---

# 23. Agent Lifecycle — Removed

There is no platform-managed agent lifecycle (the platform has no "agent"
entity, §20). The lifecycle of any agents a user runs inside a workspace is the
agent CLI's concern.

---

# 24. Sandbox Strategy

Default — one hardware-isolated microVM per workspace:

```text
Workspace
 └─ Microsandbox microVM (libkrun)
```

Each project workspace is its own microVM, so installs
and state are isolated and persist independently (overlay, §26). The container
runtime (Docker/Podman) is used only for the service tier (§6.1), never to run
a workspace.

---

# 25. Project Environment (Dockerfile)

A project's environment is defined by a **Dockerfile that lives in the project**
at `.ai-platform/Dockerfile`. The platform writes this Dockerfile at project creation,
seeded from the OS template the user selects in the setup wizard (CLI §3.1) and
extended with the selected agent CLIs (§12); from then on it is an ordinary file
in the project's git repo — the user owns and edits it.

The workspace image is **built from `.ai-platform/Dockerfile`**. There is no snapshot
versioning, pinning, upgrade, or rollback: the Dockerfile (and its git history)
is the single source of truth for the environment. To change the environment,
edit `.ai-platform/Dockerfile` and recreate the workspace.

## OS Templates

The platform ships one Dockerfile **template** per supported OS, used only to
seed a new project's `.ai-platform/Dockerfile`:

| OS key (wizard choice) | base image | template |
|---|---|---|
| `alma` | Alma 10 (`almalinux:10`) | `templates/dockerfiles/alma/Dockerfile` |
| `debian-trixie` | `debian:trixie-slim` | `templates/dockerfiles/debian-trixie/Dockerfile` |
| `debian-bookworm` | `debian:bookworm-slim` | `templates/dockerfiles/debian-bookworm/Dockerfile` |
| `ubuntu` | Ubuntu minimal | `templates/dockerfiles/ubuntu/Dockerfile` |

* **The OS is always chosen by the user** at creation, from the supported list,
  via the interactive setup wizard (CLI §3.1) — there is no `--os` flag and no
  silently-applied default. The wizard *presents* a recommended default
  (`debian-trixie`) the user confirms or changes; the chosen key selects which
  template seeds `.ai-platform/Dockerfile`.
* each template installs the **base** tooling layer (§12: Git, GitHub CLI) on its
  base image — identical across all OSes; the **agent CLIs** and **software stacks**
  selected at creation (§12; see Software Stacks below) are then added to the
  project's generated `.ai-platform/Dockerfile` (acceptance tests §6)
* each template provisions a **non-root workspace user with passwordless sudo**,
  so a user can install packages at runtime (those installs persist via the
  overlay, §26)
* after creation the template is irrelevant — the project's own
  `.ai-platform/Dockerfile` is what builds the workspace, so a user can freely
  customize it

## Software Stacks

The user selects the language/tool stacks to install when setting up the
environment (CLI §3.1, step 5) — a **multi-select** of e.g. `java`, `maven`,
`node`, `deno`, `go`, `python`, `rust`. The set is **extensible**: each stack is
a small install snippet the platform ships under
`~/.ai-platform/templates/stacks/<stack>` (repo-layout §1.5), and the
`ai project create` Dockerfile generator composes the selected snippets into the
project's `.ai-platform/Dockerfile` (after the base tooling, alongside the agent
CLIs). The selected set is recorded in the project's tracked `profile.yaml`
(`stacks: [...]`, repo-layout §12) so it is reproducible from git, and — like the
rest of the Dockerfile — the user owns and may edit it afterward. Adding a stack
later is just editing `.ai-platform/Dockerfile` (or `profile.yaml` + re-seed) and
recreating the workspace.

## Rules

* the workspace image is built from the project's `.ai-platform/Dockerfile`
* rebuilding is the only "version" mechanism — there are no platform-managed
  snapshot versions, pins, or rollbacks
* ad-hoc installs made inside a running workspace persist via the overlay (§26)
  without editing the Dockerfile

---

# 26. Overlay System (Persistence)

A workspace is:

```text
Image    (built from <project>/.ai-platform/Dockerfile, §25; read-only)
 + Overlay (persistent writable layer, host-backed)
```

The **overlay is the persistence mechanism**, implemented with a Microsandbox
**named volume** per workspace. The microVM root (from the OCI image) is
disposable; everything written on top of it lands in the overlay volume, which
is stored on the host and re-mounted every time the workspace starts or is
recreated.

## What Persists

Anything written in the workspace persists across stop / start / recreation:

* **installed programs** — `apt`/`dnf`/`npm i -g`/etc. survive restart
* **agent state** — whatever the in-workspace agent writes (its session,
  config, `AGENTS.md`, history) persists; the platform doesn't manage it, but
  the overlay keeps it
* any other files written outside the mounted project source

The whole writable layer is persisted — there is no manifest of "declared
paths" to maintain. (Project source under `~/projects/<project>` is separately
host-mounted and is the user's git repo.)

## Scope

One overlay **per workspace** (one per project) — installs and state written in
the workspace persist independently of the read-only image:

```text
~/.ai-platform/overlays/<workspace-id>/
```

## Lifecycle

* mounted over the microVM root (the image built from `.ai-platform/Dockerfile`)
  at workspace start, as a Microsandbox named volume (§6.2, §7)
* survives workspace stop/start and `ai workspace destroy` recreation
  (destroy keeps the overlay; `ai workspace start` re-mounts it)
* removed only on **permanent** removal: `ai agent remove` (that agent's
  overlay) or `ai project delete` (all the project's overlays)
* it is **local persistence, not a backup** — if the host disk is lost the
  overlay is lost; reinstall (source is in git, §32)
* when `.ai-platform/Dockerfile` changes and the workspace is rebuilt, the same
  overlay is re-mounted over the new image

---

# 27. Configuration Hierarchy

Precedence:

```text
Project
 > Global
```

Locations:

```text
<project>/.ai-platform/config.yaml      # project (tracked)

~/.ai-platform/config/config.yaml       # global
```

A value set in the project's `config.yaml` overrides the global default. (There
is no separate workspace layer — the workspace mounts the project, so the
project config *is* the workspace config — and no snapshot-defaults layer, since
the environment is defined by the project's Dockerfile, §25.)

---

# 28. Resource Limits

Example:

```yaml
workspace:
  cpu_limit: 4
  memory_limit: 8G
```

Purpose:

* prevent runaway agents
* improve system stability

---

# 29. Networking (how the workspace reaches every component)

A workspace is a **microVM**, not a container, so it has its own network
boundary. There is no shared Docker network and no `host.docker.internal`.

## 29.1 Transport — virtual NIC + WireGuard (both planes in user space)

The workspace microVM is given a **virtual network interface** (Microsandbox
virtio-net), and a **WireGuard** tunnel is brought up inside it to the ClawPatrol
gateway. The microVM's **default route is the WireGuard tunnel**, so every
external connection — regardless of the tool that makes it — is carried to the
gateway at layer 3. There is no shared Docker network and the microVM never
touches the host Docker socket (§30). (This is the single egress transport;
libkrun's no-NIC "transparent socket" mode is **not** used, because it cannot
give ClawPatrol a transparent L3 capture point.)

To avoid stacking privileged host facilities, **every plane stays in user
space** — the hypervisor and the tunnel never contend for the same host
resource:

| Plane | Where it runs | Host privilege |
|---|---|---|
| libkrun microVM (HVF on Apple Silicon / KVM on Linux) | host, user space | hypervisor entitlement only (Apple Silicon), baked into code signing |
| Workspace egress NIC | **gvproxy** (userspace; macOS has no host TAP) | none |
| WireGuard **guest** end (`wg0`, default route) | inside the Linux microVM | none — it is the guest kernel |
| WireGuard **host** end (ClawPatrol) | **userspace WireGuard** (e.g. boringtun) on a loopback UDP port reached via gvproxy | none — a UDP listener, no `utun`/NetworkExtension |

The host side of the tunnel is therefore **not** a kernel `utun` /
NetworkExtension interface: terminating WireGuard in user space at the gateway is
what keeps the macOS hypervisor entitlement the *only* special privilege in the
stack (no kext, no admin networking prompt). If ClawPatrol cannot terminate
WireGuard in user space directly, a small **userspace-WireGuard sidecar** sits in
front of it (WG → local plaintext socket → gateway), preserving this property.

## 29.2 Host address — `AI_PLATFORM_HOST`

Never hardcode `host.docker.internal`. The platform resolves the host address
reachable from the microVM and injects it as `AI_PLATFORM_HOST`, together with
each service port, into the workspace environment at start (§7, Ports). Trusted
host services (Headroom, LiteLLM) are reached directly at `AI_PLATFORM_HOST`;
all other egress goes through the WireGuard default route. The same config works
on macOS, Linux, and WSL2.

## 29.3 Reaching each component

```text
                          workspace microVM (virtio-net + wg0)
                                 │
        ┌────────────────────────┼───────────────────────────────┐
        │ trusted host services  │  default route = WireGuard     │
        ▼ (direct, AI_PLATFORM_HOST)                ▼ (all other egress, L3)
   Headroom ──► LiteLLM ──► ClawPatrol gateway ──►  ClawPatrol gateway
                  │           (injects provider          (injects creds,
                  ▼            credential on egress)       enforces policy)
              [Ollama]                                        ▼
                                                         git / MCP / web
```

* **Model requests** — the in-workspace agent sends the request to **Headroom**
  at `AI_PLATFORM_HOST:<headroom_port>` (input compression), which forwards to
  **LiteLLM** (routing). LiteLLM's egress to the provider passes through the
  **ClawPatrol** gateway, which swaps the placeholder provider key for the real
  one on the wire (§17). Headroom → LiteLLM → ClawPatrol → provider are all
  host-side hops (loopback / container-runtime network), not across the microVM
  boundary.
* **Ollama** — never reached directly by the workspace. It is a container-tier
  service (§16) that **LiteLLM** calls host-side; the agent only ever talks to
  LiteLLM. (Ollama is not a credentialed egress, so it does not traverse
  ClawPatrol.)
* **All other workspace egress** (git push, MCP servers, arbitrary web) leaves
  the microVM via the **WireGuard default route to the ClawPatrol gateway**
  (§17). Because capture is at layer 3, *every* tool is covered — proxy-aware or
  not. The gateway injects any required credential and applies allow/deny
  policy; the agent holds only placeholders.

## 29.4 Egress confinement (Microsandbox network policy)

WireGuard provides the default route, but defense-in-depth requires that nothing
can route *around* it. Each workspace microVM therefore also runs under a
restricted Microsandbox **network policy** so the only reachable endpoints are
(a) the WireGuard endpoint of the ClawPatrol gateway and (b) the trusted host
service ports (`AI_PLATFORM_HOST`). Any other direct workspace-to-internet
connection is denied at the runtime, so a misconfigured tunnel fails closed
rather than leaking uncredentialed, unaudited traffic (§30).

This must work identically across all supported hosts.

## 29.5 Delivery phasing

The WireGuard L3 model above is the **end-state** (this document describes the
final architecture regardless of phase, §1). It is delivered in two steps
(roadmap §9.5, plan §8.2):

* **Initial slice — ClawPatrol forward proxy.** The same gvproxy NIC and the same
  default-deny Microsandbox network policy, but egress is mediated by setting
  `HTTPS_PROXY`/`HTTP_PROXY` in the workspace to the ClawPatrol gateway instead of
  a WireGuard default route. Identical privilege profile (all userspace; only the
  hypervisor entitlement is special). Trade-off: only proxy-aware tools are
  credential-injected; proxy-unaware tools are denied by the network policy (fail
  closed) rather than transparently captured.
* **Target — WireGuard default route.** Replaces the proxy with the L3 tunnel so
  *every* tool is covered transparently. Gated by the userspace-WG-with-ClawPatrol
  feasibility spike (plan §8.2).

Both steps keep the §29.4 confinement guarantee; they differ only in whether
non-proxy-aware traffic is *injected* or *denied*.

---

# 30. Security Model

Requirements:

* ClawPatrol only
* no plaintext secrets
* no host Docker socket by default (the service tier runs rootless; workspaces
  use the Microsandbox microVM runtime, which never touches the Docker socket)
* workspace isolation (hardware-level — each workspace is a microVM, so a
  workspace escape is contained at the VM boundary, not the host kernel)
* project isolation
* agent isolation
* **egress confinement** — each workspace runs under a restricted Microsandbox
  network policy whose only permitted external path is the ClawPatrol gateway
  (plus the trusted host service ports), so the credential broker cannot be
  bypassed (§29.4)
* **least host privilege** — the hypervisor and the egress tunnel both run in
  user space (§29.1). On Apple Silicon the *only* elevated facility is the macOS
  hypervisor entitlement (code-signed, no kext); workspace networking
  (gvproxy + userspace WireGuard) needs no `utun`, NetworkExtension, admin
  approval, or host Docker socket. ClawPatrol's host network privilege is for the
  tunnel terminator alone
* **scoped TLS-interception CA** — ClawPatrol injects into HTTPS by terminating
  TLS, so a ClawPatrol CA root is installed in the **workspace** trust store
  (§17, "TLS Interception and the Trust Anchor"). The CA is per-install,
  per-workspace-scoped, generated locally, and trusted **only inside the
  workspace** — never added to the host trust store. The CA private key stays in
  ClawPatrol's store; the workspace holds only the public root (no secret
  material). This is a deliberate widening of trust in the gateway and is the
  enabler for wire-level injection

On Windows, workspace isolation depends on WSL2 nested virtualization being
available; where it is not, the platform must fail the `doctor` check rather
than run workspaces without microVM isolation (§6.2).

---

# 31. Audit Logging

Location:

```text
~/.ai-platform/audit/
```

Events:

* workspace creation
* workspace deletion
* secret-access events (that an injection occurred — never the value)
* model configuration changes

Secret values must never be logged. ClawPatrol owns the authoritative
credential-access log (in its own SQLite); the platform audit log records only
that a secret-access event occurred, with no secret material.

---

# 32. Backup and Recovery — Removed

The platform has no backup/restore feature. It isn't needed:

* **project source** lives in `~/projects/<project>` and is the user's own git
  repo (backed up by pushing to a remote)
* **platform state** is reconstructable from the filesystem via `ai state repair`
* **secrets** live in ClawPatrol's own store
* **installed programs** persist in the per-workspace **overlay** (§26), which
  survives workspace restart and recreation

The environment is reproducible from the project's `.ai-platform/Dockerfile`
(§25), which is in git — so there is nothing bespoke to back up.

---

# 33. Monitoring and Diagnostics

The platform must support:

* health checks
* diagnostics
* dependency verification
* provider verification

Monitoring is required for:

* Microsandbox
* LiteLLM
* ClawPatrol
* Ollama
* Docker
* Podman
* Caveman
* Headroom

---

# 34. Success Criteria

The architecture is considered successful when a user can:

```bash
ai setup

ai project create my-project   # interactive wizard: select OS + agent CLIs (defaults: debian-trixie, OpenCode)
```

and immediately receive (for the OS the user selected):

* selected-OS workspace microVM (e.g. Debian trixie)
* host-backed source code
* hardware-isolated Microsandbox microVM (rootless container runtime for the
  service tier)
* LiteLLM integration
* ClawPatrol secrets
* Headroom input compression
* Caveman output compression
* reproducible, Dockerfile-defined environments
* zero manual Microsandbox configuration
* project persistence (overlay)
    
