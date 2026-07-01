# 01-architecture-spec.md

# AI Development Platform

## Architecture Specification

Version: 1.0

Status: Target End-State Architecture

---

# 1. Purpose

This document defines the end-state architecture for the AI Development Platform.

The platform provides reproducible AI-powered software development environments using Microsandbox microVMs, centralized model access, context optimization, off-disk provider credentials (held in the LiteLLM gateway), and host-backed project persistence.

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
* stored in plaintext on platform disk
* distributed through environment files

Provider keys live only in the LiteLLM gateway (keys-in-LiteLLM, §17); the
workspace agent holds only a scoped LiteLLM virtual key.

---

## Host Agnostic

The platform must function consistently on:

* macOS (Apple Silicon — required by the Microsandbox microVM runtime)
* Linux (KVM virtualization must be available)

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
 │       ├─ AI Tooling Layer (OpenCode + Pi by default; Claude Code / Codex / Gemini CLI optional — selected per env)
 │       ├─ In-VM OCI runtime (rootful containerd + nerdctl) → opt-in apps: Open WebUI · AnythingLLM (§7)
 │       └─ Context Optimization (Caveman skill — per project, §8–10; Headroom now host-side)
 │
 └─ Container Runtime (Docker / Podman)            ← service tier (network aip-net)
     └─ nginx reverse proxy (aip-proxy — SOLE host entry, publishes :18787)
         └─ INTERNAL-ONLY containers on aip-net (no host publish):
            Headroom · LiteLLM (+ Postgres) · Presidio (analyzer + anonymizer) · Ollama · DNS audit (CoreDNS)
```

Workspaces are **microVMs** (hardware isolation), not containers. The
container runtime is used only for the stateless service tier, never for
workspaces.

Host services (run on the host, not inside a workspace):

```text
nginx proxy (aip-proxy)                                 — the SOLE host entry, publishes :18787 (default server + UI vhosts)
Headroom · LiteLLM (Model Layer) · Presidio · Ollama    — container tier (Docker/Podman, network aip-net), INTERNAL-ONLY behind nginx
Microsandbox                                            — microVM runtime, driven by the `ai` CLI via the Go SDK / `msb` (no daemon)
```

The entire host service tier is containers on `aip-net`: nginx (`aip-proxy`),
Ollama, Presidio (analyzer + anonymizer), LiteLLM (+ Postgres), Headroom, and the
CoreDNS egress-audit resolver. There are no optional host services. There is no
native host service. **`aip-proxy` (nginx) is the SOLE host entry point** — every
other service container is INTERNAL-ONLY on `aip-net` (reached by name, no host
publish), except the two loopback-published support containers `aip-litellm-db`
(`127.0.0.1:5442`) and `aip-dns` (`127.0.0.1:15353/udp`, for the microVM
`--dns-nameserver`). See §5/§10 for the full nginx routing model.

Headroom is now a **shared host container** (`aip-headroom`, §10), the input
compression proxy that agents send to and that forwards to LiteLLM — it is no
longer installed inside the workspace image.

(The model-request path below involves the Headroom/LiteLLM/Presidio subset;
Ollama is a container-tier service too — see §5. Microsandbox is not a
long-running service: it is invoked directly to create and drive workspace
microVMs.)

The Project Layer is the top-level, user-facing artifact: host-stored source in
the project directory (the cwd at `ai create`), bind-mounted into the workspace
at `~/project` (i.e. `/home/workspace/project`, §12).

## 4.2 Model-Request Path (how a call flows)

```text
Agent (AI Tooling, in workspace)
 ╎  microVM boundary → AI_PLATFORM_HOST
nginx (aip-proxy, host :18787, location /v1 → Headroom)  (SOLE host entry)
 ↓
Headroom (aip-headroom:8787, internal-only)              (input compression)
 ↓  forwards to LiteLLM
LiteLLM (Model Layer, aip-litellm:4000, internal-only)
 ↓  always-on secret-masking guardrails, pre_call (Presidio secrets + hide-secrets)
 ↓  route to provider (Ollama or cloud); real provider key from LiteLLM's store
 ↓  always-on Presidio post_call guardrail (mask secrets out of the response)
Provider · Caveman steers output                         (Context Optimization)
```

Context Optimization sits *between* the agent and the model (Caveman steers the
agent's output; Headroom compresses the input). Headroom is now a **shared host
container** (`aip-headroom`, §10): the agent sends to the **nginx gateway** (host
:18787) across the microVM boundary via `AI_PLATFORM_HOST`, nginx (`location /v1`)
forwards to Headroom (internal-only on :8787), and Headroom forwards to LiteLLM.
LiteLLM runs an
always-on secret-masking guardrails on every request (pre_call and post_call, §15),
so even cloud calls are guarded — they do not bypass them. The real
provider API keys live **in the LiteLLM gateway** (§17), never in the workspace;
the workspace agent holds only a scoped LiteLLM virtual key. The
LiteLLM → provider hops are host-side. See Sections 8–9 (context optimization),
15 (LiteLLM + guardrails), 17 (keys-in-LiteLLM), and **29 (the full
per-component networking model + Microsandbox egress policy)** for detail.

---

# 5. Host Layer

The Host Layer provides all persistent infrastructure.

Supported hosts:

* macOS (Apple Silicon)
* Linux

Responsibilities:

* Project storage
* Shared AI resources
* Headroom deployment (shared input-compression proxy)
* LiteLLM deployment
* Presidio deployment (analyzer + anonymizer, backing LiteLLM's secret-masking guardrail)
* Ollama deployment (required local model backend)
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
├── cache/          # re-fetchable caches: catalog.yaml (models.dev), ollama-models.yaml (ollama.com)
├── config/         # global settings + projects index (no per-project state)
├── logs/
├── overlays/
├── prompts/
├── skills/
├── templates/      # OS Dockerfile templates + shared templates
├── volumes/        # host data volumes only (litellm-db, models store)
└── tools/
```

`~/.ai-platform/` holds **only global settings and host artifacts** — there is
no per-project state here. Per-project state lives in the project itself (see
below).

---

## State Management

State is split between **global** and **project-local**:

* **global** (`~/.ai-platform/config/`): detected runtime (`runtime.yaml`),
  pinned service versions (`versions.yaml`), global config (`config.yaml`),
  rendered host-service configs, and a small **projects index**
  (`projects.yaml`: maps project name → path; written only on create/delete)
* **project-local** (`<project>/.ai-platform/`): everything specific to one
  project, stored *in the project* so it travels with the repo and can be
  git-tracked or ignored:

```text
<project>/.ai-platform/
├── Dockerfile        # tracked — defines the environment (§25)
├── config.yaml       # tracked — project config
├── profile.yaml      # tracked — project profile
├── project.yaml      # tracked — { name, os, created }
├── skills/caveman/   # tracked — platform-seeded Caveman skill (§9)
├── .gitignore        # ignores run/
└── run/              # gitignored — host-local runtime state
    └── workspaces/<workspace-id>.json
```

Rules:

* **tracked** files (`Dockerfile`, `config.yaml`, `profile.yaml`,
  `project.yaml`, `skills/caveman/`) are the environment + config definition —
  committable so the project is reproducible from git
* **`run/`** holds host-local runtime handles (Microsandbox sandbox ids, status)
  and is **gitignored** — machine-specific, never committed
* per-entity sharding (one file per workspace under `run/`)
  avoids write contention
* **all state writes are atomic**: write to a temp file in the same directory,
  then `rename()` over the target — a crash mid-write never corrupts state
* `ai state repair` reconstructs `run/` by reading the project + Microsandbox/git;
  project discovery uses the global `projects.yaml` index

Large host-local artifacts (the per-workspace **overlays**, §26) stay under
`~/.ai-platform/overlays/` — they are not git material and do not belong in the
project.

---

## Host Services Control Plane

The platform's host services — Headroom, LiteLLM (+ its Postgres), Presidio
(analyzer + anonymizer), Ollama (required), and the CoreDNS egress-audit
resolver (there are no optional host services) — plus the
Microsandbox microVM runtime are installed, configured, and supervised by the
`ai` CLI. The CLI is the **single control plane**: the user never invokes
`docker compose`, `msb`, `launchctl`, or `systemctl` directly. The whole service
tier is containers: they share a private docker network (`aip-net`) and are
reconciled in order: network → DNS → Ollama → Presidio → LiteLLM (+ DB) → Headroom → nginx proxy (last).
(The destructive-tool-call firewall and the `hide-secrets` detector are in-process
in LiteLLM — they need no companion container; only Presidio, which backs the
secret-masking guardrail, runs as its own analyzer + anonymizer containers. §15.
The in-process prompt-injection detector was removed — §15.)
(Headroom is now a shared host container, no longer installed in the workspace
image, §10. It is INTERNAL-ONLY behind the `aip-proxy` nginx reverse proxy, which
is the gateway entry on host :18787 — see §10/§15.)

**`aip-proxy` (nginx) is the SOLE host entry point to the service tier.** Every
other service container is INTERNAL-ONLY on `aip-net` (reached by name) — none
publishes a port to the host. Only nginx publishes, and it publishes a **single
port**: host **:18787**. Everything is served on that one port, split by route and
by `server_name` (Host-based vhosts):

- The **DEFAULT server** (`server_name <domain> localhost _;`, `default_server`) —
  the model path + management surfaces, what the host CLI (`127.0.0.1:18787` — IPv4,
  not `localhost`, which can resolve to IPv6 `::1`) and
  the microVM gateway (`host.microsandbox.internal:18787`) hit:
  - `location /` → Headroom → LiteLLM (the DEFAULT route; agents + the UIs' model
    calls ride this).
  - `location /v1/` → Headroom too (the agent chat path, SSE-friendly, unchanged).
  - `location /llm/` → `aip-litellm:4000` (prefix stripped) — the LiteLLM admin
    surface, **bypassing Headroom**.
  - `location /ollama/` → `aip-ollama:11434` (prefix stripped) — the Ollama HTTP
    API, **bypassing Headroom**.
- The **single web UI is a Host-based VHOST (subdomain)** on the SAME :18787, NOT
  a separate host port: `litellm.<domain>` → `aip-litellm:4000` (the admin UI at
  `/ui`; litellm is the ONLY host UI vhost). The vhost carries WebSocket upgrade
  headers and **bypasses Headroom** (it serves the admin UI directly). The
  `<domain>` is the resolved platform base domain (`runtime.yaml` `domain`, default
  `aip.local`; `ai domain`). (Open WebUI is now a per-workspace in-VM app — see
  `internal/apps`, §7 — not a host service; Odysseus has been removed from the
  platform entirely.)

In **standalone** mode `ai setup` writes an `/etc/hosts` block (with consent + sudo;
on no-TTY/--json/declined it prints the block to add manually) pointing
`litellm.<domain>` at `127.0.0.1` (litellm is the only host UI vhost). In
**server** mode the platform
does NOT edit `/etc/hosts` — `ai setup`/`ai doctor` print the operator contract:
create real DNS (`*.<domain>` wildcard or per-host) → this server's IP and provide
a TLS cert terminated at nginx. The blocks are structured so a per-vhost
`listen 443 ssl;` + ssl directives can be added later (TLS termination, out of
scope now) — TLS terminates **per-vhost at nginx**, and once HTTPS is configured the
`:80`/http listener on the single :18787 entry **MUST** `return 301
https://$host$request_uri;` (http → https redirect); no redirect is emitted today
because there is no https listener yet (a 301 with no :443 would break every
plain-http caller). nginx is reconciled LAST so its upstreams are
up first. (`aip-litellm-db` and `aip-dns` stay loopback-published — DNS must stay
`127.0.0.1:15353` for the microVM `--dns-nameserver`.) The live end-to-end routing
through these nginx routes — and especially the litellm UI vhost and the sudo
`/etc/hosts` write — is a **hardware bring-up** verification item
(`docs/HARDWARE-BRINGUP.md`).

### One Tool, Uniform Lifecycle

All services are managed through the same commands regardless of how they run:

```text
ai setup            install + configure + start everything (idempotent)
ai setup --upgrade  bump pinned versions and re-reconcile
ai services status           health across all services
ai services restart <svc>    uniform lifecycle (hides container vs native)
ai services update [svc]     re-pull latest images + recreate (TUI: `p` key)
ai doctor                    checks + repair suggestions
ai logs --service <svc>      one log surface
```

### Run Modes (an implementation detail, hidden from the user)

| Service | Run mode | Why |
|---|---|---|
| nginx proxy | container (via Runtime) `aip-proxy` (`nginx:stable-alpine3.23-slim`) | the SOLE host ENTRY to the service tier: publishes ONLY :18787 — the default server (`/`+`/v1`→Headroom, `/llm`→LiteLLM, `/ollama`→Ollama) plus the single Host-based UI vhost on the same port (`litellm.<domain>`); HTTPS termination point later (§10) |
| Headroom | container (via Runtime) `aip-headroom` | shared input-compression proxy in front of LiteLLM; INTERNAL-ONLY on :8787 behind nginx (no host publish); HTTP only (§10) |
| LiteLLM | container (via Runtime) `aip-litellm` (+ `aip-litellm-db` Postgres) | INTERNAL-ONLY: no host publish, reached by name (`aip-litellm:4000`) by Headroom + nginx's `/llm` route; HTTP only; no host privileges |
| Presidio | two containers (via Runtime) `aip-presidio-analyzer` + `aip-presidio-anonymizer` | back LiteLLM's always-on secret-masking guardrail; internal-only, not published (§15) |
| Ollama (required) | container (via Runtime) `aip-ollama` on all platforms | local model backend LiteLLM routes to; INTERNAL-ONLY (no host publish — reached by name, and from the host via nginx's `/ollama` route); CPU-only on macOS (Docker has no GPU passthrough) |
| DNS audit resolver | container (via Runtime) `aip-dns` (CoreDNS) | egress-audit resolver: microVMs forward DNS here so attempted names are logged for `ai network log`; published to host loopback only; audit, not enforcement (§29.7) |
| Microsandbox | microVM runtime, invoked on demand | drives workspace microVMs via the Go SDK / `msb`; no daemon to supervise (§7) |

**docker compose is not used.** Container-tier services are managed directly
through the runtime abstraction (§6), so docker and podman remain
interchangeable and there is no second orchestration mechanism. The whole
service tier runs as containers — there is no native OS service to register.
Microsandbox is **not** a long-running service — the `ai` CLI invokes it
directly (Go SDK / `msb`) to create, start, stop, and destroy workspace
microVMs; `ai setup` only verifies the runtime is installed and the host
supports virtualization (§6).

### Configuration by Reconciliation

The platform `config.yaml` (§27) is the single source of truth. The CLI
**renders** each service's native config from it and reconciles desired vs.
actual state; `ai setup` means "make reality match config" and is safe
to re-run. Pinned versions/digests live in `config/versions.yaml`. Rendered
service configs live under `config/<service>/`.

### Install + Configure Summary

* **install**: container images are pulled by pinned **image + tag** (default
  `latest`; no digests — they are platform/arch specific — `config/versions.yaml`,
  §27); native binaries are downloaded as pinned, checksum-verified release
  artifacts into `tools/<name>/<version>/`
* **configure**: rendered from platform config — the LiteLLM config carries no
  model list (models are DB-backed, §14–15), so the **real provider credentials
  live in the LiteLLM gateway**, stored **encrypted in its Postgres DB** under
  `LITELLM_SALT_KEY` and managed via `ai keys` (§17) — never on platform disk;
  Microsandbox driven non-interactively per workspace (image, mounts/volumes,
  resource limits) via the Go SDK / `msb`; Ollama registered as a LiteLLM
  provider (required local backend)
* **startup ordering**: container runtime + Microsandbox runtime verified →
  container tier reconciled in order
  `aip-net` network → DNS (CoreDNS) → Ollama → Presidio (analyzer + anonymizer) →
  LiteLLM (+ Postgres) → Headroom →
  **nginx proxy (last** — its upstreams must be up first since nginx resolves
  literal `proxy_pass` hosts at config-load) → verify
  (workspace microVMs are created on demand, not at setup)

### Secrets Boundary

Real provider keys live **only in the LiteLLM gateway** (env passthrough at
launch / its Postgres-backed store, §17), never on platform disk and never in the
workspace. The workspace agent holds only a scoped LiteLLM **virtual key** (the
gateway key), not provider secrets — so no plaintext provider credential reaches
the Microsandbox workspace, the rendered config on disk, or a backup.

---

# 6. Runtime Layer

The platform uses two runtimes for two purposes.

## 6.1 Container Runtime (service tier)

Used for the container-tier services — the nginx proxy (`aip-proxy`, the sole
host entry), Headroom, LiteLLM (+ its Postgres), Presidio (analyzer +
anonymizer), Ollama (required), and the CoreDNS egress-audit resolver — which
share a private docker network (`aip-net`). There are no optional host services.

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

The check is what the requirement protects against — a **rooted daemon on the
host** — not a literal `rootless` flag. On macOS, Docker Desktop (and Podman's
machine) run the engine inside a managed VM, so there is no rooted host daemon
and no host socket exposure; this satisfies the requirement by construction and
`runtime.Verify` treats it as rootless. On Linux, where
the engine runs on the host kernel directly, a **genuine rootless engine** is
required and is detected from `docker info` (Podman is rootless by default).

## 6.2 microVM Runtime (workspaces)

Workspaces run as **Microsandbox microVMs** (libkrun), not containers. This
runtime is driven directly by the `ai` CLI through the **in-process Microsandbox
Go SDK** (the default backend — a cgo binding that holds one reused relay
connection per workspace; the legacy `msb`-CLI shell-out is a fallback selected
with `AIP_WORKSPACE_BACKEND=cli`). There is no daemon and no host Docker socket
involved (§7, §30).

Host requirements:

* **macOS:** Apple Silicon (the microVM runtime requires the Apple Hypervisor;
  Intel Macs are unsupported)
* **Linux:** KVM virtualization available to the user

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

Every workspace image also ships a **rootful in-VM OCI container runtime** —
containerd + nerdctl + runc + CNI plugins + buildkit, installed from the pinned
`nerdctl-full` release tarball (one static, distro-agnostic artifact, arch-aware
amd64/arm64) into `/usr/local` in every OS base Dockerfile, alongside the CNI
runtime deps (`ca-certificates`, `iptables`, `iproute`). The runtime is **started
at workspace start** (not baked running into the image): `ensureContainerd`
(`internal/workspace`) probes `nerdctl info` as root and, if needed, boots
`containerd` detached (`setsid`, as root) so it runs for the VM's life, then **polls
`nerdctl info`** (true readiness — the daemon serving requests, NOT merely the
`/run/containerd/containerd.sock` file existing) so the runtime is actually up
before the exec returns (msb tears down the exec's process group on return, which
would otherwise kill the just-forked daemon). The poll is bounded (~30s, 150 ×
0.2s) so a broken runtime never hangs forever, and `ensureContainerd` **returns
whether containerd is READY**. Bringing it up is **best-effort** — a failure does
not fail the workspace start; `Start` only launches the in-VM apps when the runtime
IS ready, otherwise it skips them with one warning (no per-app "cannot access
containerd socket" fatal).

#### In-VM apps (Phase 1)

On this runtime the platform runs **opt-in AI applications** as `nerdctl`
containers **inside** the workspace microVM — currently **Open WebUI**
(`ghcr.io/open-webui/open-webui:latest`, web port 8080, data `/app/backend/data`)
and **AnythingLLM** (`mintplexlabs/anythingllm:latest`, web port 3001, storage
`/app/server/storage`). Each app is described by a declarative manifest
(`internal/apps`): image (pinned image+tag, no digest — same convention as the
service tier), container port, persisted data dir, an optional `/workspace` mount,
a memory limit, and a gateway-pointing env builder. Each app is routed through the
**same model gateway** the agent CLIs use — `http://host.microsandbox.internal:18787/v1`
(the resolved gateway) with the workspace's own scoped LiteLLM virtual key (the
catalog-driven system has **no default model**, so the model handle passed is
empty and the app's user picks a served model) — via
`OPENAI_API_BASE_URL`/`OPENAI_API_KEY` (Open WebUI, plus
`ENABLE_OLLAMA_API=false`/`WEBUI_AUTH=false` for a single-user in-VM instance) and
`LLM_PROVIDER=generic-openai` + `GENERIC_OPEN_AI_*` (AnythingLLM).

Apps are **opt-in** (chosen at `ai create`, default OFF) and have a full
lifecycle via **`ai apps <list|add|remove|update|start|stop|restart> [app] [name]`**
and a per-workspace **Apps** view in `ai ui`. Each installed app is allocated a
**unique host port** (recorded in the project `config.yaml`'s `apps:` block) so two
running microVMs never collide. The port chain is:

```text
host:<port>  --(msb published port: -p <port>:<port>)-->  VM:<port>  --(nerdctl -p <port>:<containerPort>)-->  container:<containerPort>
```

The host and guest side use the **same number**, reusing the existing
`ai network publish` / `egress.MsbNetworkArgs` published-ports plumbing — a port is
published **only while its app is installed**. App data is bind-mounted from
`/persist/apps/<key>` (the workspace overlay, §26) so it survives restart. Adding
or removing an app changes the published-port set, which `msb` only applies at
workspace create, so it requires an **`ai restart`**; the container itself is
(re)started best-effort immediately. App start is **best-effort per app** — one app
failing must not fail the workspace or the others.

### Mount Rules

```text
host  <project dir> (the cwd at `ai create`)  →  workspace  ~/project (= /home/workspace/project)  (read-write)
host  ~/.ai-platform/overlays/<workspace-id>  →  workspace  /persist    (overlay, §26)
```

* the project is created **in the current working directory** (no fixed
  `~/projects` root — that path survives only as an unused fallback); the host
  project directory is the single source of truth, bind-mounted read-write at
  `~/project` (i.e. `/home/workspace/project`, the workspace working directory).
  This is a SUBDIRECTORY of the `workspace` user's home `/home/workspace` —
  deliberately not the home itself and not a top-level `/workspace`, so the bind
  mount does not shadow the baked-in `~/.local/bin` (uv/Graphify) or the key-bearing
  agent configs under `~/.config` (which must stay off the host).
* the per-workspace overlay (§26) is mounted at `/persist`
* no persistent data is written outside the mounted paths
* (a shared read-only mount of `~/.ai-platform/agents,skills,prompts,templates`
  into the workspace is **deferred** — not currently wired into the `msb`
  run-args)

### Lifecycle Mapping

| Platform state (workspace §7) | Microsandbox operation (`msb` / Go SDK) |
|---|---|
| Created   | `create` (no start) |
| Started   | `start` |
| Stopped   | `stop` |
| Archived  | `stop` + retain metadata |
| Destroyed | `rm` |

### Ports

Workspace microVMs reach host services via the **nginx gateway** at
`AI_PLATFORM_HOST:18787` (§29) — never `host.docker.internal`. The agent sends its
model calls to that single gateway port (`location /v1` → Headroom → LiteLLM, §10).
The platform injects `AI_PLATFORM_HOST` and the service ports into the workspace
environment at start. The workspace runs under a Microsandbox **NetworkPolicy**
(§29.4) whose default posture is **`public`** — the open internet is reachable,
private/internal ranges stay blocked, and the model gateway plus any explicitly
allow-listed host services are always reachable; a project can re-lock to `deny`
via `ai network`. The full per-component path is specified in §29.

### Logs

Microsandbox workspace logs are streamed to `~/.ai-platform/logs/microsandbox.log`;
per-workspace detail is retrievable via `ai logs --workspace <project>`.

### Cleanup

Cleanup depends on whether the removal is recoverable:

* **`ai stop`** stops the Microsandbox microVM but **keeps everything** —
  state, overlay, definition. The workspace is fully recoverable with `ai start`
  (which rebuilds from `.ai-platform/Dockerfile` and re-mounts the same overlay)
  or `ai restart`. (Non-destructive — this is how you pause a workspace.)
* **`ai delete`** (alias `ai destroy`) is permanent: it tears down the microVM
  (idempotent), removes the project's whole `.ai-platform` directory **and its
  overlay**, and de-registers it from `config/projects.yaml`. The user's OTHER
  files are kept unless `--purge` is given (which removes the whole project
  directory). There is **no** separate "tear down the microVM but keep the
  project" verb — use `ai stop` to pause.

---

# 8. Context Optimization Layer

The platform includes a mandatory context optimization layer.

Purpose:

* reduce token usage
* improve context quality
* reduce costs
* improve scalability

Components:

* Headroom — compresses input context (now a shared host container, §10)
* Caveman — compresses agent output (per-project, in-workspace skill, §9)

These bound token usage on both sides of every model request: Headroom
compresses the *input* sent to the model, and Caveman compresses the *output*
the agent emits. Per-project Headroom tuning still applies — the platform keeps a
per-project strategy and rides its knobs in the request body so they survive the
shared host Headroom (§10).

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

Headroom and Caveman are the two halves of context optimization. **Caveman** is a
per-project, in-workspace generation-steering skill loaded in the agent.
**Headroom** is now a **shared host container** (`aip-headroom`, §10) — an
input-compression proxy in front of LiteLLM — but its tuning is still
per-project: the platform maps a per-project strategy onto Headroom's request-body
knobs (§10) so each project's compression behavior survives the shared proxy.
Headroom shrinks what goes *in*; Caveman shrinks what comes *out*.

```text
   workspace microVM                          │ host
   ─────────────────                          │
        Caveman skill (loaded in agent → steers terse generation)
                              │               │
                              ▼               │
  Agent ─ full context ─┼─► nginx ─► Headroom ─compressed─► LiteLLM ─► Model
    ▲  (per-project      │  (:18787,  (aip-headroom         (keys-in-       │
    │   knobs in body)   │   /v1)      :8787)                LiteLLM)       │
    └──────────── compact output (Caveman-steered) ────────────────────────┘
```

---

# 10. Headroom

Headroom manages context budgets.

Headroom now runs as a **shared host container** (`aip-headroom`, image
`ghcr.io/chopratejas/headroom:slim` — pulled, never built, listening on `:8787`).
It is the **input-compression proxy in front of LiteLLM**, forwarding to LiteLLM
via `OPENAI_TARGET_API_URL=http://aip-litellm:4000`. Headroom is now
**INTERNAL-ONLY** on `aip-net` (no host publish): the gateway ENTRY is the
**`aip-proxy` nginx reverse proxy** (`nginx:stable-alpine3.23-slim`), the **SOLE host entry
point** to the whole service tier. The model path is **microVM → nginx (:18787,
`location /v1`) → Headroom (compress) → LiteLLM** — preserved unchanged, this is
what every workspace agent's `base_url=…/v1` hits; the nginx config disables
response buffering and uses long timeouts so streamed (SSE) LLM responses flush
promptly. On the **same :18787**, nginx additionally fronts the LiteLLM
admin/management surface on **`location /llm`** (→ `aip-litellm:4000`, prefix
stripped: `/model/info`, `/v1/models`, `/health*`, `/key*`, `/credentials`, …) and
the Ollama HTTP API on **`location /ollama`** (→ `aip-ollama:11434`, prefix
stripped) — the specific `/llm` and `/ollama` prefixes match before the catch-all
`/` (the default route, also → Headroom). `/llm` and `/ollama` (and the UI vhost
below) **bypass Headroom** — only `/` + `/v1` ride the compression proxy. The
single web UI is served as a **Host-based vhost on the SAME :18787**, NOT a separate
host port: `litellm.<domain>` → `aip-litellm:4000` (admin UI at `/ui`; litellm is
the ONLY host UI vhost, with WebSocket upgrade headers). `<domain>`
is the resolved platform base domain (`runtime.yaml` `domain`, default `aip.local`;
`ai domain`). Standalone points that name at `127.0.0.1` via an `/etc/hosts`
managed block written by `ai setup` (consent + sudo, else a manual block);
server mode does NOT edit `/etc/hosts` and instead prints the DNS (`*.<domain>` →
this server) + TLS (cert terminated at nginx) operator contract. Because nginx is
the only publisher, **every other service container is
INTERNAL-ONLY on `aip-net`** (LiteLLM, Ollama, Presidio, Headroom no
longer publish to the host); only `aip-litellm-db` and `aip-dns` stay
loopback-published. This is transparent to workspaces (the gateway URL stays
`host:18787`) and lets nginx terminate TLS later, per-vhost (in server mode it binds
`0.0.0.0:18787` — the eventual public HTTPS endpoint). The host CLI reaches LiteLLM
(admin via `…/llm`, the chat test via `…/v1`) and Ollama (via `…/ollama`) through
nginx, never a container directly. Headroom is **no longer installed inside the
workspace image** — the agent in the workspace reaches the host gateway across the
microVM boundary via `AI_PLATFORM_HOST` (§29). The live end-to-end routing through
these nginx routes — the litellm UI vhost and the sudo `/etc/hosts` write — is
verified at hardware bring-up. (Open WebUI is now a per-workspace in-VM app, §7;
Odysseus has been removed from the platform.)

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

## Per-Project Strategy

Headroom's compression is content-aware and automatic — it has **no named-strategy
header**. The only documented per-request controls are two request-**body** fields:

* `keep_turns` — how many recent conversation turns to retain verbatim
* `output_buffer_tokens` — tokens reserved for the model's output

The platform keeps a per-project strategy (`ai context`; values `conservative` /
`balanced` / `aggressive`, default `balanced`) and **maps it onto those two
knobs** (higher = less compression):

| strategy | keep_turns | output_buffer_tokens |
|---|---|---|
| conservative | 8 | 12000 |
| balanced (default) | 5 | 8000 |
| aggressive | 2 | 4000 |

`balanced` is Headroom's own documented default. These values ride in the agent's
request body (`extra_body`), baked at workspace start, so per-project tuning
survives the shared host Headroom container. (Go: `internal/contextopt.HeadroomParams`.)

```yaml
context:
  max_tokens: 64000
  compression_threshold: 0.75
  strategy: balanced       # conservative | balanced | aggressive
```

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
* **Python 3** — the base image's latest, installed system-wide (`python3` +
  `python3-venv` where the distro splits it out). This backs the per-project
  `~/project/.venv-msb` virtualenv created at workspace start (§26).
* **uv** — Astral's Python package/tool manager, installed for the **workspace
  user** (`curl -LsSf https://astral.sh/uv/install.sh | sh`, onto
  `~/.local/bin`, which is on `PATH`).
* **Graphify** — the knowledge-graph skill for AI coding assistants
  (github.com/safishamsi/graphify, PyPI package `graphifyy`, CLI `graphify`),
  installed by default for the workspace user via
  `uv tool install "graphifyy[<extras>]"`. The bundled optional extras are all of
  Graphify's extras **except** the region/DB-specific ones (`chinese`, `azure`,
  `bedrock`, `falkordb`, `neo4j`, `leiden`, `dm`) — i.e. the included set is
  `pdf,office,video,postgres,google,svg,sql,terraform,ollama,openai,gemini,anthropic`.
  Each selected agent CLI then **registers Graphify with itself at workspace
  start** (NOT at image build): `Manager.registerGraphify` runs `graphify install`
  for Claude Code (the default `graphify` platform) and `graphify install --platform
  <cli>` for Codex, the Gemini CLI, OpenCode, and Pi, all in `~/project`. It must run
  at start because `--project` writes project-scoped skill/plugin/hook files into the
  bind-mounted project dir (which does not exist at build); it runs **once per
  project**, guarded by a marker file (`~/project/.ai-platform/.graphify-installed`)
  so user edits to those files are not clobbered on every restart.
  Graphify's headless LLM backend is an **Ollama model chosen at `ai create`** (the
  wizard's optional model+tag select, or `--graphify-model`), stored as
  `agent.graphify_model` in the project `config.yaml`. The chosen model is pulled
  into the local Ollama store and registered in LiteLLM if absent (best-effort — a
  pull failure is a create warning, not a failure), and at workspace start Graphify
  is routed through the gateway as `ollama/<model>` via `OPENAI_*` env vars (see
  §17) — never directly to Ollama.

## Agent CLIs (selected per environment)

The AI coding-agent CLIs are **not** all baked in. One or more are chosen at
environment setup (`ai create`, CLI §3.1) from the supported list:

* **OpenCode** — the default; pre-selected and the default agent
* **Pi** — pre-selected by default (also wired to LiteLLM)
* **Claude Code**
* **Codex**
* **Gemini CLI**

Selection is **multi-select**: install any subset (at least one), with **OpenCode
and Pi** pre-selected by default. The chosen CLIs are written into the project's
`.ai-platform/Dockerfile` at creation, so the installed set is reproducible from
the project rather than a fixed, baked-in surface. The **default agent** — which
CLI new agents use unless told otherwise — is recorded as `agent.default_tool`
(repo-layout §12.4) and must be one of the installed CLIs (default OpenCode).

Context optimization (§8–10):

* **Headroom** — input compression, now a **shared host container**
  (`aip-headroom`, §10), **not** baked into the workspace image. The agent sends
  its model calls to host Headroom (via `AI_PLATFORM_HOST`), which forwards to
  LiteLLM. Per-project tuning rides in the request body (§10).
* **Caveman** — output compression, **not** baked into the image: seeded per
  project into `<project>/.ai-platform/skills/caveman/` at creation and
  **git-tracked** (an agent skill, see §9), so it travels with the project.

MCP servers are configured and run by the agent itself (the platform does not
manage MCP).

---

# 13. Tool Provider Layer

Supported providers (selected per environment, §12):

* OpenCode — default
* Pi — also pre-selected by default
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

All model access flows through LiteLLM. On the full path, the agent sends to the
**nginx gateway** (host :18787), which routes `/v1` to the shared host Headroom
proxy (input compression), which forwards to LiteLLM; LiteLLM applies always-on
secret-masking guardrails (§15) and attaches the real provider key — held **in the
LiteLLM gateway** (§17) — to the upstream request.

```text
Agent
 ╎  microVM boundary → AI_PLATFORM_HOST
nginx (aip-proxy, host :18787, /v1 → Headroom)
 ↓
Headroom (aip-headroom:8787, input compression)
 ↓
LiteLLM
 ↓  (always-on Presidio pre_call/post_call secret-masking guardrails, §15)
 ↓  (real provider key from LiteLLM's own store; agent holds only a virtual key)
Provider
```

---

## Supported Providers

* OpenAI
* Anthropic
* Google Gemini
* Groq
* OpenRouter
* Ollama

---

## Routing

The platform does **not** define per-task routing policies — model selection is
the agent's job (OpenCode/Claude Code/Codex/Gemini each choose their model). LiteLLM
provides only a unified endpoint and failover; it makes no model-selection decision.

**Available models live entirely in the LiteLLM DB** (`general_settings.store_model_in_db: true`),
not in the rendered config — there is **no `model_list`, no named aliases, no
per-provider wildcards, and no default model** in the generated `config.yaml`
(`litellm.DefaultRouting()` is the zero `Routing`). The served set is reconciled
from the **models.dev catalog** (`internal/catalog`, fetched as JSON and cached as
YAML at `~/.ai-platform/cache/catalog.yaml`) keyed by which providers the user has
supplied an API key for: adding a provider key (`ai keys add`, §17) registers
that provider's catalog models into the DB via `litellm.SyncModels`, and an
`ollama pull`/`rm` registers/unregisters the corresponding `ollama/<name>` served
model. The catalog id is the public `model_name` verbatim; the
catalog-id→LiteLLM-prefix map (e.g. `google` → `gemini`) supplies each model's
routing prefix. In an agentic workflow the agent names a model on each request
and LiteLLM routes it to the provider (attaching its own stored key for cloud,
none for Ollama). A model is usable when it is (a) registered in the DB and
(b) actually available: an Ollama model must be **pulled** locally; a cloud
model needs its provider key present in the gateway (§17).

There is no default model: an unqualified request is the agent's responsibility.
Ollama (the required local backend, no credential) is always available once a
model is pulled.

---

# 15. LiteLLM

Runs on the host as a **thin shared gateway**.

Purpose:

* unified provider endpoint for all tools (and Ollama)
* single point that holds the real provider keys (keys-in-LiteLLM, §17)
* the DB-backed served-model catalogue (synced from models.dev by `ai keys`, §14)
* failover
* monitoring

It does not make model-selection decisions on the agent's behalf.

LiteLLM runs as container `aip-litellm` (image `ghcr.io/berriai/litellm:latest`,
`:4000`) on the `aip-net` network — **INTERNAL-ONLY** (no host publish; reached by
name `aip-litellm:4000` by Headroom and by nginx's `/llm` route + `litellm.<domain>`
vhost). Its DB-backed admin UI / virtual keys require
PostgreSQL: container `aip-litellm-db` (image `postgres:18.4-alpine3.23`, the data
dir **HOST-BIND-MOUNTED** from `~/.ai-platform/volumes/litellm-db` at
`/var/lib/postgresql` — NOT a Docker named volume — `trust` auth on the private
network, host port bound at `127.0.0.1:5442`). This Postgres is the one stateful
piece of the service tier.

**All host-persisted SYSTEM data volumes live under `~/.ai-platform/volumes/<name>`**
— a single discoverable home, so `ai uninstall --purge` (which `RemoveAll`s
`~/.ai-platform`) removes them all (no scattered Docker named volumes). Today:
`volumes/litellm-db` (the Postgres data dir above) and `volumes/models` (the Ollama
model store) — `volumes/` is ONLY true host data. **Re-fetchable caches live under
`~/.ai-platform/cache/<name>` instead** (`paths.CacheDir`): `cache/catalog.yaml` (the
models.dev catalog, fetched as JSON but persisted as YAML; a legacy `catalog.json` or
the older `volumes/catalog.json` is one-shot converted) and `cache/ollama-models.yaml`
(the Ollama installable-library list scraped from ollama.com);
these are re-downloadable copies, not SYSTEM data, and are likewise removed by
`ai uninstall --purge`. Config files (the LiteLLM/DNS/nginx configs under `config/`)
and the per-project workspace overlays are NOT system volumes and stay where they are.
*Bring-up caveat:* Postgres on a host bind mount has data-dir ownership quirks on
macOS Docker Desktop / Linux rootless (the container's `postgres` UID vs the host
dir) — verify `initdb` succeeds; some hosts may need a uid/`:Z` tweak. *Migration
caveat:* data in the old `aip-litellm-db-data` named volume / old
`~/.ai-platform/models` does NOT auto-migrate — the next `ai setup` starts fresh
(Postgres re-`initdb`s, models re-pull); `ai uninstall` still best-effort removes
the legacy named volume on upgrade.

**Admin UI auth.** The proxy ships an admin UI at `:4000/ui` (reached on the host
through nginx — the `litellm.<domain>` vhost on :18787, which redirects `/` → `/ui`,
or the `/llm` route — never a direct host port; the host CLI hits it at
`http://127.0.0.1:18787/llm`, §10). The platform
secures it by passing `UI_USERNAME` (`admin`), `UI_PASSWORD`, and
`LITELLM_MASTER_KEY` into the container **via the environment** — never inlined
in the launch argv, the rendered config, or platform disk.

**Role-based UI auth policy** (`runtime.RequireUIAuth`, true only for the
**server** role). The web UIs are open on a host that binds to loopback and
locked down on a host that binds to `0.0.0.0`:

* **standalone / client** (loopback) → OPEN access. `ai setup` does NOT prompt
  for a LiteLLM password (a single-user local box). A standalone user can opt into
  a LiteLLM password later via **`ai litellm password`**.
* **server** (`0.0.0.0`, network-exposed) → auth REQUIRED. `ai setup` REQUIRES a
  LiteLLM admin-UI password (it loops on a TTY until one is entered, and
  generates a strong random one non-interactively rather than leave the gateway
  open).

**Persisting the secrets.** `ai setup` (server) and `ai litellm password` OFFER
(on a TTY) to save `UI_PASSWORD` + `LITELLM_MASTER_KEY` to **`~/.ai-platform/.ai-platform.env`**
— an opt-in, **0600** file of `export KEY='VALUE'` lines that the `ai` CLI
**auto-loads at startup** (into its own process env, where the container
env-passthrough launches pick them up) — so they persist across restarts WITHOUT
the user editing their shell rc. Precedence is "existing env wins": the file only
fills gaps, so a value already exported in the shell is never clobbered. On
decline / non-TTY the manual `export …` block is printed instead. The container
also
carries `DATABASE_URL` (inline; it carries no secret) and the Presidio endpoints
`PRESIDIO_ANALYZER_API_BASE=http://aip-presidio-analyzer:3000` /
`PRESIDIO_ANONYMIZER_API_BASE=http://aip-presidio-anonymizer:3000`.

---

## Guardrails (always-on: secret masking, tool firewall)

The rendered LiteLLM config carries a top-level `guardrails:` block, **all
`default_on: true`** so **no request can opt out**: three secret-masking entries
plus a **tool firewall** (below). (An in-process prompt-injection callback was
removed — see below.) The masking is
deliberately scoped to **secrets and credentials, NOT general PII**: a coding
agent's prompts legitimately contain names, places, paths and emails, and masking
those corrupts the prompt before the model sees it (e.g. "capital of France" →
"capital of `<LOCATION>`"), so general PII masking was removed. Presidio runs as
two internal-only host containers (not published to the host) —
`aip-presidio-analyzer` and `aip-presidio-anonymizer`, both listening on `:3000`
internally — which LiteLLM reaches via the `PRESIDIO_*_API_BASE` env above.
Because **every route — including cloud providers — traverses the LiteLLM
proxy**, cloud calls are guarded too; they do **not** bypass this protection.

* **`presidio-secrets-input`** — `guardrail: presidio`, `mode: pre_call`,
  `presidio_filter_scope: input` — masks financial/identity **secrets**
  (CREDIT_CARD, US_SSN, US_BANK_NUMBER, IBAN_CODE, CRYPTO) out of the prompt
  before the model sees it.
* **`presidio-secrets-output`** — `guardrail: presidio`, `mode: post_call`,
  `presidio_filter_scope: output` — masks the same secret entities out of the
  response.
* **`hide-secrets`** — `guardrail: hide-secrets`, `mode: pre_call` — LiteLLM's
  in-process secret detector (bundled detect-secrets) strips API keys/tokens/
  credentials from the prompt. No external server.

```yaml
guardrails:
  - guardrail_name: presidio-secrets-input
    litellm_params: { guardrail: presidio, mode: pre_call,  presidio_filter_scope: input,  default_on: true }
  - guardrail_name: presidio-secrets-output
    litellm_params: { guardrail: presidio, mode: post_call, presidio_filter_scope: output, default_on: true }
  - guardrail_name: hide-secrets
    litellm_params: { guardrail: hide-secrets, mode: pre_call, default_on: true }
  - guardrail_name: tool-firewall
    litellm_params:
      guardrail: tool_permission
      mode: post_call
      default_on: true
      default_action: allow          # allow everything except the deny rules
      on_disallowed_action: block     # reject the response on a destructive match
      rules:
        - { id: deny-destructive-command, tool_name: "<shell-tool regex>", decision: deny,
            allowed_param_patterns: { command: "<destructive-command regex>" } }
        - { id: deny-destructive-arguments-command, tool_name: "<shell-tool regex>", decision: deny,
            allowed_param_patterns: { arguments.command: "<destructive-command regex>" } }
```

### Tool firewall (destructive command tool-calls)

For sandboxed coding agents the bigger risk is destructive **tool execution**, not
prompt content — `git push --force`, `git reset --hard`, `terraform destroy`,
`kubectl delete`, `rm -rf` matter far more than "ignore previous instructions". The
`tool-firewall` guardrail (`guardrail: tool_permission`, `mode: post_call`,
in-process — no external service) inspects the model's **tool-calls** at the gateway
and **denies** ones whose shell command matches a destructive pattern
(`destructiveCommandPatterns` in `internal/litellm`): default-allow, with deny rules
matching a shell-tool-name regex against the `command` / `arguments.command` arg;
`on_disallowed_action: block` rejects the response. Because every agent
(opencode/pi/claude-code) routes model calls through LiteLLM, this is tool-agnostic.

It is **defence-in-depth**, not the only control: it catches the model's tool-calls
(the agent path), but the microVM isolation + the per-project egress policy (§29.6 —
which a project can re-lock to `deny` to block network-dependent destructive commands
like push/terraform/kubectl) remain the hard boundary. The exact shell-tool name/param
paths per agent CLI are a `hardware bring-up` verification item
(`docs/HARDWARE-BRINGUP.md`).

### Prompt-injection scanning — REMOVED

The platform does **not** scan prompt content for injection. The in-process
`detect_prompt_injection` callback was removed: it is a crude local heuristic
(similarity to known attack strings) that **false-positives on ordinary coding
traffic** — including normal Ollama requests, which it rejected with
`400: Rejected message. This is a prompt injection attack.`. Like the earlier
removal of general-PII masking and the unmaintained **LLM Guard**
(`laiyer/llm-guard-api`), it corrupted legitimate use, so it is gone. For a
sandboxed coding agent the real risk is destructive **tool execution** (the tool
firewall above), not prompt content. **Guardrails AI** remains deferred (it needs a
Guardrails Hub token + manual per-guard install, so it cannot ship fully automated).

---

## Catalogue

The served catalogue is **DB-backed and key-driven**: the rendered LiteLLM config
carries no `model_list` (§14). Models are reconciled into the gateway DB from the
**models.dev catalog** (`internal/catalog`, fetched as JSON and saved as YAML at
`~/.ai-platform/cache/catalog.yaml` — a legacy `catalog.json` or the older
`volumes/catalog.json` is one-shot converted — refreshed at `ai setup` and on the Cloud Models
tab's `r` key) keyed by which providers the user has supplied a key for: adding a
provider key (`ai keys add`, §17) registers that provider's catalog models via
`litellm.SyncModels`; removing it unregisters them; an `ollama pull`/`rm`
registers/unregisters the matching `ollama/<name>`. Registering a model does **not**
install it: an Ollama model must still be `ollama pull`ed, and a cloud model still
needs its provider key present in the gateway (§17). The catalog id is the public
`model_name` verbatim; the catalog-id→LiteLLM-prefix map (e.g. `google` → `gemini`)
supplies the routing prefix. There is no default model.

### In-VM agent provider config — keyless host templates, key in-VM only

All **five** agent CLIs route through the gateway **by default**. Their provider
config lives as **keyless, user-editable host templates** under
`<project>/.ai-platform/agents/` (scaffolded at `ai create`; repo-layout §12.1c) and
is (re)loaded into the microVM on **every workspace start/restart** by
`workspace.registerAgentProviders` (over `internal/agentcfg`). opencode + pi route
via a JSON config file (the keyless template is **deep-merged** with the dynamic,
key-bearing values and written into the VM — user edits survive, the dynamic provider
block wins); claude-code/codex/gemini route via **environment variables** written into
the in-VM agent env file (`ANTHROPIC_BASE_URL`+`ANTHROPIC_AUTH_TOKEN`;
`GOOGLE_GEMINI_BASE_URL`+`GEMINI_API_KEY`; codex via a keyless `~/.codex/config.toml`
`[model_providers.aip-gateway]` block with `wire_api = "responses"` and an env-supplied
`env_key`). The **scoped virtual key is NEVER on host disk** (the templates are
keyless; the base URL is not a secret) — it is injected only into the final config/env
written **into the microVM**. When the project has a configured Graphify model
(`agent.graphify_model`), the same agent env file also exports `OPENAI_BASE_URL`
(the gateway `/v1`), `OPENAI_API_KEY` (the scoped virtual key), and
`OPENAI_MODEL=ollama/<model>`, so `graphify --backend openai` routes through the
gateway (nginx → Headroom → LiteLLM → Ollama) rather than directly to Ollama.

### In-VM model picker

For discoverability the in-VM agent CLIs are seeded with a **picker** built at
workspace start (`internal/workspace.pickerModels`): it is **exactly the set of
models the LiteLLM gateway currently serves** — its live DB-backed models — sourced
via the injected `workspace.ServedModels` → `litellm.KeyManager.ListModels`,
deduped and sorted. If the gateway is unreachable the picker **degrades to an empty
list** (never fatal — it must never fail a workspace start), and **no default model
is written** to the agent configs. The platform also installs an in-VM
`refresh-models` command (`/usr/local/bin/refresh-models`, from
`agentcfg.RefreshScript`) that re-fetches the served list from the gateway's
`/v1/models` endpoint (authenticated with the scoped virtual key) and rewrites the
agent-CLI configs to match a fresh start, so models registered after start can be
picked up without recreating the workspace; on failure it leaves the configs
untouched. (The installable Ollama library backing the *host-side* `ai models`
browse + the TUI Local Models tab is **scraped LIVE from ollama.com** —
`internal/ollama/library.go`, `ollama.Library()` GETs the `/library` index for every
model then each model's `/library/<model>/tags` table for the per-variant
size/context/input, and caches it as YAML at `~/.ai-platform/cache/ollama-models.yaml`,
falling back to that cache when ollama.com is unreachable — and is a separate concern
from the in-VM picker. There is no bundled `models.yaml` and no offline fallback set.)

---

# 16. Ollama

Ollama is **required** — it is the platform's local model backend, started by
`ai setup` (arch §5). LiteLLM routes local model traffic to it.

Rules:

* **required**, always provisioned (not optional)
* never installed in workspaces
* runs as a **container-tier service** (`aip-ollama`, image `ollama/ollama:latest`,
  **INTERNAL-ONLY** — no host publish; reached by name, and from the host through
  nginx's `/ollama` route; models persist on the host under
  `~/.ai-platform/volumes/models`, bind-mounted to `/models` with `OLLAMA_MODELS`
  pointing there) on the `aip-net`
  network on all platforms — never a native host install. A native Ollama bound to
  the container's own `:11434` should be stopped first. LiteLLM reaches it by
  container name — `ollama/*` models carry `api_base=http://aip-ollama:11434`.
* on macOS the container is **CPU-only** (Docker has no GPU passthrough); use
  remote deployment for GPU-accelerated inference
* remote deployment supported

All access occurs through LiteLLM; LiteLLM routes local model calls to Ollama by
container name (no credential needed for local) and applies the always-on
Presidio guardrail (§15) on these requests as on any other. Workspace egress to
all of this is governed by the Microsandbox NetworkPolicy (§29.4).

---

# 17. Secrets Layer

Provider credentials live **in the LiteLLM gateway** — never on platform disk and
never in the workspace. This is the **keys-in-LiteLLM** model:

* the real provider API keys (OpenAI, Anthropic, Gemini, Groq, …) are added with
  `ai keys add <provider>` and held **encrypted** in LiteLLM's **Postgres-backed
  credential store** (the same DB that backs the admin UI and virtual keys, §15),
  encrypted under `LITELLM_SALT_KEY` (`litellm.KeyManager.SetCredential`);
* the rendered LiteLLM config carries **no `model_list` and no key references** —
  models are DB-backed (§14) — so no plaintext key (and no model definition) is
  written into the config or anywhere on platform disk;
* when LiteLLM routes a request to a cloud provider, it attaches the real key
  from its own encrypted store on the upstream call — the agent never sees it.

The two security concerns that used to be one component's job are now split
across mechanisms that already exist on the path:

* **off-disk credentials** — keys-in-LiteLLM (this section);
* **egress enforcement** — the Microsandbox **NetworkPolicy** (default posture
  `public`: open internet allowed, private ranges blocked; re-lockable to `deny`),
  configured by `ai network` (§29.4, §29.6); there is **no egress proxy**;
* **secret masking / audit** — LiteLLM's always-on guardrails (§15), which run on
  every request and every route.

---

## The workspace holds only a virtual key

The in-workspace agent never holds a provider secret. It is given a scoped
**LiteLLM virtual key** (the gateway key) and sends all model calls to the
gateway path (nginx :18787 → Headroom → LiteLLM, §29). LiteLLM authenticates the
virtual key, then uses the *real* provider key from its own store to reach the
upstream provider:

```text
Agent in workspace microVM  (holds only the LiteLLM virtual key)
 ↓  AI_PLATFORM_HOST → nginx (:18787, /v1) → Headroom (:8787) → LiteLLM (:4000)
LiteLLM  (authenticates the virtual key; attaches the real provider key)
 ↓
Provider (cloud) / Ollama (local, no key)
```

Because the real key lives only in the gateway, a compromise of the workspace
cannot exfiltrate a provider secret — at worst it can spend against the scoped
virtual key, which the gateway can revoke or rate-limit independently.

---

## Credential Bootstrap

The user manages provider API keys through the `ai keys` commands (which **replace**
the removed `ai secrets`); the platform never writes the key values to its own disk:

* `ai keys add <provider>` stores the provider's API key — **encrypted in the
  LiteLLM Postgres DB** under `LITELLM_SALT_KEY` (`litellm.KeyManager.SetCredential`)
  — and registers that provider's models.dev catalog models into the gateway DB
  (`litellm.SyncModels`). The key is read hidden on a TTY, or via `--value`/`--stdin`
  under `--json`/no-TTY; it never appears in argv, logs, or the JSON envelope.
* `ai keys list` shows each routable provider and **whether a key is set** (plus its
  catalog model count) — never the key value (`litellm.KeyManager.ListCredentials`).
* `ai keys remove <provider>` deletes the stored key
  (`litellm.KeyManager.DeleteCredential`) and unregisters that provider's models from
  the gateway DB.

`ai setup` fetches + persists the models.dev catalog (so the saved copy is current);
it does not itself register models — a provider's catalog models are registered the
moment its key is added with `ai keys add`. A missing provider key is **not** a setup
hard-fail: a provider with no key simply has no models registered, and that is
reported by `ai keys list` / `ai doctor`.
The actual model call fails (exit `5`) only when a key is genuinely needed at request
time and absent. The platform never fabricates or defaults a key.

---

## Requirements

No:

* .env files
* plaintext secrets on platform disk
* repository secrets

Real provider keys never reach the workspace filesystem or environment — they
live only in the LiteLLM gateway, and the workspace holds only the scoped virtual
key.

---

## Egress and audit (cross-references)

The wire-level controls that used to be bundled with the secret store are now
their own mechanisms:

* **egress enforcement** is the Microsandbox NetworkPolicy applied per workspace
  and declared via `ai network` — the default outbound posture (`public` by
  default: open internet allowed, private ranges blocked; re-lockable to `deny`),
  allow-listed host services, and published ports (§29.4, §29.6). The policy is
  rendered into `msb` net-rules (`egress.MsbNetworkArgs`) and applied at workspace
  create.
* **secret masking and audit** are LiteLLM's always-on guardrails (§15): because
  the guardrails are `default_on: true` and **every** route — cloud included —
  traverses the LiteLLM proxy, nothing bypasses secret masking. The platform
  audit log (§31) records that a model-configuration or secret-access event
  occurred, never the value.

---

## Supported Secret Types

* provider API keys (OpenAI, Anthropic, Gemini, Groq, …)
* the LiteLLM master key and admin-UI password (§15)
* LiteLLM virtual keys issued to workspaces

Credentials a tool inside the workspace needs that are **not** model-provider keys
(e.g. a git token or an MCP server's credential) are the user's / agent's own
concern — the platform manages only the model-path credentials described here,
and confines all other workspace egress with the NetworkPolicy (§29.4).

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

Host location (the project directory — the cwd at `ai create`, not a fixed root):

```text
<project dir>/.ai-platform/
```

Workspace mount location (guest):

```text
~/project  (= /home/workspace/project)
```

---

## Generated Structure

```text
.ai-platform/
  Dockerfile         # tracked — environment (§25)
  config.yaml        # tracked
  profile.yaml       # tracked
  project.yaml       # tracked — { name, os, created }
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

The platform has **no git involvement at all** — version control is out of
scope. It does not init, clone, branch, commit, or merge. `ai create`
only writes the `.ai-platform/` environment definition into the current
directory and leaves any existing files (including an existing repo) untouched.
**All git is the user's and the in-workspace agent's job** — init, clone,
branches, commits, rebases, merges, conflict resolution, worktrees, and pull
requests. The platform makes no model calls and runs no git.

---

# 22. Conflict Resolution — Removed

The platform does **not** perform AI merge or conflict resolution, nor any git
at all (§21). Branching, merging, and conflict resolution are LLM-on-code work
done by the in-workspace agent (OpenCode/Claude Code/Codex/Gemini).

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
  via the interactive setup wizard (CLI §3.1) — on a TTY no default is applied
  silently. A `--os` flag exists, but on a terminal it only *pre-seeds* the
  wizard; the wizard always shows and the user confirms or changes the
  recommended default (`debian-trixie`). The chosen key selects which template
  seeds `.ai-platform/Dockerfile`.
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
environment (CLI §3.1, step 5) — a **multi-select** of `java`, `maven`,
`node`, `deno`, `go`, `rust`. **Python is not a stack option**: the latest
**Python 3**, **uv**, and **Graphify** are baked into every OS base by default
(§12: Base tooling), so there is nothing to select. (A legacy `python`
stack snippet is retained under `templates/stacks/python` for old projects only.)
The set is **extensible**: each stack is
a small install snippet the platform ships under
`~/.ai-platform/templates/stacks/<stack>` (repo-layout §1.5), and the
`ai create` Dockerfile generator composes the selected snippets into the
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
paths" to maintain. (Project source in the project directory is separately
bind-mounted at `~/project` and is the user's git repo.)

## Scope

One overlay **per workspace** (one per project) — installs and state written in
the workspace persist independently of the read-only image:

```text
~/.ai-platform/overlays/<workspace-id>/
```

## Lifecycle

* mounted over the microVM root (the image built from `.ai-platform/Dockerfile`)
  at workspace start, as a Microsandbox named volume (§6.2, §7)
* survives workspace stop/start
  (`ai stop` keeps the overlay; `ai start`/`ai restart` re-mounts it)
* removed only on **permanent** removal: `ai delete` (alias `ai destroy`) removes
  the workspace overlay (via `overlay.Remove`)
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

## 29.1 Transport — virtual NIC + Microsandbox NetworkPolicy (user space)

The workspace microVM is given a **virtual network interface** (Microsandbox
virtio-net) behind the host-side userspace network stack (gvproxy). Egress is
governed by a Microsandbox **NetworkPolicy** (§29.4) whose default outbound
posture is **`public`** — the open internet is reachable while private/internal
ranges stay blocked, and the model gateway plus any explicitly allow-listed host
services are always reachable. The policy is implemented as a default-deny
fallthrough that `public` mode widens with a broad allow-internet rule, so a
project can re-lock to `deny` (only the gateway + allow-listed services) via
`ai network`. There is no
shared Docker network and the microVM never touches the host Docker socket (§30).
There is **no egress proxy** — confinement is enforced by the NetworkPolicy, not
a forward proxy on the wire.

To avoid stacking privileged host facilities, **every plane stays in user
space** — the hypervisor and the network stack never contend for the same host
resource:

| Plane | Where it runs | Host privilege |
|---|---|---|
| libkrun microVM (HVF on Apple Silicon / KVM on Linux) | host, user space | hypervisor entitlement only (Apple Silicon), baked into code signing |
| Workspace egress NIC | **gvproxy** (userspace; macOS has no host TAP) | none |
| Egress enforcement | Microsandbox **NetworkPolicy** (default posture `public`, applied per workspace via the SDK) | none |

Keeping every plane in user space is what leaves the macOS hypervisor
entitlement the *only* special privilege in the stack (no kext, no
NetworkExtension, no admin networking prompt).

## 29.2 Host address — `AI_PLATFORM_HOST`

Never hardcode `host.docker.internal`. The workspace microVM reaches the host
through the **gateway address of Microsandbox's host-side userspace network
stack** — the slirp/gvproxy-family gateway through which every guest packet is
already routed for DNS interception and policy (§29.1). The platform resolves
that gateway once, persists it in `config/runtime.yaml` as `host_gateway`
(`internal/runtime`), and injects it — with each service port — into the
workspace environment as `AI_PLATFORM_HOST` at start (§7, Ports).

The gateway address is the **fixed, backend-independent Microsandbox guest→host
DNS name `host.microsandbox.internal`** — Microsandbox publishes the same name
under HVF on macOS and KVM on Linux, verified end-to-end during hardware bring-up.
It is **pinned** in `runtime.HostGateway` (which returns
`(DefaultGatewayHost, true)`) and persisted as `config/runtime.yaml`'s
`host_gateway`; it is never guessed. `Info.HostAddress()` returns the
`AI_PLATFORM_HOST` environment override when set (e.g. the acceptance harness) else
the persisted `host_gateway`.

The trusted platform gateway is reached at `AI_PLATFORM_HOST:18787` — the **nginx
proxy** (the sole host entry), which routes `/v1` to Headroom (internal-only on
`:8787`), which forwards to LiteLLM (`aip-litellm:4000`); all other egress is
governed by the Microsandbox NetworkPolicy (§29.4). Reaching *other* host-local
services — a developer's database or message broker — is a separate, explicitly
allow-listed zone (§29.6).

**Gateway resolution → microVM gateway URL + egress allow.** The model-gateway
endpoint every workspace on a host routes through is derived from
`Info.HostAddress()` (`internal/runtime`): the `ai_platform_host` field if set
(machine-wide, configured by `ai setup --mode client --server` or `ai gateway
set` — §CLI 10.4), else the resolved `host_gateway`. The field holds a **bare host
or `host:port`** (NOT a URL); `runtime.ResolveGateway` applies: empty →
`host.microsandbox.internal:18787` (standalone/local); `host` → that host on the
default nginx-gateway port `18787`; `host:port` → that host and port. The result drives
two things at workspace start (`internal/workspace`): (a) the in-VM agent provider
configs' `base_url` is `http://<host>:<port>/v1`, and (b) the always-on egress
allow rule (`egress.MsbNetworkArgs`) targets `<host>:tcp:<port>`. So in standalone
mode every microVM reaches the local LiteLLM via `host.microsandbox.internal`, and
in client mode every microVM on the machine routes through — and is allowed egress
to — the configured remote server's gateway.

## 29.3 Reaching each component

```text
              workspace microVM (virtio-net)
       ┌─────────────────────────────┐
       │ agent                        │   egress = Microsandbox NetworkPolicy
       └───────────────│─────────────┘   (default `public`; private ranges blocked)
        trusted gateway │ (AI_PLATFORM_HOST:18787)
                        ▼
        nginx (host :18787) ─► Headroom ─► LiteLLM ──► provider (cloud)
        (sole host entry,    (:8787,    (Presidio    │   (real key from
         /v1 → Headroom)      input      guardrail;   │    LiteLLM's store)
                              compress)  keys-in-     ▼
                                         LiteLLM)   Ollama (local)
```

* **Model requests** — the in-workspace agent sends the request across the microVM
  boundary to the **nginx gateway** at `AI_PLATFORM_HOST:18787` (the sole host
  entry), which routes `/v1` to the **shared host Headroom** proxy (input
  compression, internal-only on `:8787`), which forwards to **LiteLLM** (routing;
  always-on secret-masking guardrails, §15). LiteLLM reaches **both** the local
  Ollama backend **and** cloud
  providers, attaching the real provider key from **its own store** on cloud calls
  (keys-in-LiteLLM, §17). These hops are host-side; the workspace holds only the
  scoped LiteLLM virtual key.
* **Ollama** — the required local model backend (§16); never reached directly by
  the workspace. **LiteLLM** routes local model calls to it (no credential needed)
  and applies the always-on Presidio guardrail (§15) as on any other request.
* **All other workspace egress** (git push, MCP servers, arbitrary web) is
  governed by the Microsandbox **NetworkPolicy** (§29.4): the default `public`
  posture allows the open internet (private ranges still blocked) plus the
  explicitly allow-listed host services, and a project can re-lock to `deny` via
  `ai network`. There is no egress proxy and no wire-level credential
  injection; any non-model-path credential a tool needs is the agent's own.

## 29.4 Egress confinement (Microsandbox NetworkPolicy)

Each workspace microVM runs under a Microsandbox **NetworkPolicy** whose default
outbound posture is **`public`** (§29.6). The always-reachable set is: (a) the
trusted platform gateway at `AI_PLATFORM_HOST:18787` (the nginx proxy → Headroom →
LiteLLM), and (b) any host-local services explicitly allow-listed in
`network.allow_host_services` (§29.6). On top of that, (c) the open internet is
reachable under the default `public` posture (private/internal ranges stay blocked)
and a project can re-lock to `deny` so only (a) and (b) are reachable (§29.6). The
policy is implemented as a default-deny fallthrough that `public` mode widens with
a broad allow-internet rule, so anything outside the permitted set is denied at the
runtime — under `deny` the workspace fails closed rather than leaking traffic (§30).

**The Microsandbox NetworkPolicy is the single egress authority.** It holds the
declared posture + allow/deny rules in exactly one mechanism (§29.6, §30). Off-disk
provider credentials
are a separate concern handled by keys-in-LiteLLM (§17); secret masking and audit
are LiteLLM's always-on guardrails (§15). The host-local-services zone
(b) is plain TCP — a service's own credential, if any, is presented at that
protocol's auth layer by the tool that connects — and remains gated by this
allow-list (§29.6).

This must work identically across all supported hosts. The NetworkPolicy is
rendered into `msb` net-rules (`egress.MsbNetworkArgs`) and applied at workspace
create (§29.5, §29.6).

## 29.5 Delivery phasing

The NetworkPolicy is **applied today**: `ai network` declares the
project's `network` block and `ai start`/create translates it into
`msb` net-rules (`egress.MsbNetworkArgs`) — the egress posture (`public` by
default, re-lockable to `deny`), the allow-listed host services, and the
published-port maps — which the Microsandbox runtime enforces. The §29.2 **host-gateway address** is now **pinned**:
`runtime.HostGateway` returns the fixed `host.microsandbox.internal`, so
`gateway`-token allow-listed host services resolve in every mode. The remaining
deferred work is the **live host↔workspace reachability verification** below.

* **Now (host-side + applied).** `ai network` manages the project's `network`
  block (egress posture + allow-listed host services + published ports) in
  `config.yaml`, and it is rendered into `msb` net-rules at workspace create. The
  egress policy fixture in the acceptance suite renders a known `deny` policy so
  tests assert egress confinement against a defined policy, not ambient host
  behavior. The
  host-gateway address (§29.2) is pinned (`host.microsandbox.internal`).
* **Hardware bring-up.** Verify host↔workspace reachability end-to-end (the spike
  below).

The §29.4 confinement guarantee holds either way — the remaining work is the live
host↔workspace reachability verification.

A **reachability spike** confirms, on a provisioned host, the SDK calls for policy
+ port maps and the pinned host-gateway path of §29.2. It must demonstrate that: (1) a workspace
reaches an **allow-listed** host service (e.g. Postgres) via the gateway; (2) a
**non-allow-listed** host/internet destination is **denied**; (3) a **published**
guest port is reachable from the host; and (4) the trusted model gateway path
(Headroom → LiteLLM) is reachable while all other egress is denied. These four
make the platform's two headline promises — *it works* and *it confines egress* —
falsifiable. (Tracked in `docs/HARDWARE-BRINGUP.md`.)

## 29.6 Host-local services and published ports

A workspace is behind the userspace network stack's NAT (§29.1), so the two
"local development" directions are **explicit, not automatic**:

**Workspace → host-local service (database, message broker, …).** Microsandbox's
default posture does not expose the host's private network to the guest, so a
workspace cannot reach a host-resident Postgres / Redis / Kafka unless it is
**allow-listed by `host:port`**. These connections are made to the gateway
address (§29.2) and are **plain, direct TCP**: the wire protocols are not
necessarily HTTPS and there is no model-provider key involved (a service's own
credential, if any, is presented at that protocol's auth layer by the tool that
connects). They remain gated by the NetworkPolicy allow-list — anything not
listed is denied (§29.4). This is a distinct trust zone from internet egress.

**Host → workspace (a dev server running in the workspace).** Because the guest
is NAT'd, the host cannot address it directly; a guest port must be **published**
to a host port (the microVM equivalent of `-p`), after which the host reaches it
at `localhost:<host_port>`. Publishing is declared per project, never implicit.

A third knob sets the **default outbound posture** — `network.egress`:

* **`public`** (**default**) — the open internet is reachable, but
  **private/internal ranges stay blocked** unless explicitly allow-listed (so the
  agent can `pip install` / `npm install`, the in-VM container runtime can pull
  images, and AI processes can hit public APIs without enumerating every domain).
  This is a deliberate security-posture choice: the workspace ships allow-outbound
  so it is usable out of the box, with egress still **DNS-audited** (`ai network
  log`) and **re-lockable** per project. Empty `network.egress` resolves to
  `public`.
* **`deny`** — only the model gateway and `allow_host_services` are reachable;
  everything else is blocked. Re-lock a project with `ai network egress deny`.
* **`unrestricted`** — all egress allowed, including private ranges (escape hatch;
  least safe).

`host` in an allow rule may be a hostname/IP/**domain**, a **`*.suffix` wildcard**
(e.g. `*.npmjs.org`, matching any subdomain), or the `gateway` token for a
service on the host machine — so the same mechanism covers a host-local Postgres,
a remote/managed database, a Kafka cluster, a specific internet API, or a whole
package-registry domain family. Microsandbox enforces these targets natively
(domain, suffix-wildcard, IP/CIDR, and `public`/`private` groups). The port is
optional in `ai network allow` and defaults to **443 (HTTPS)** for bare hosts.

All of this is configured entirely via the **`ai network`** commands (CLI §10a) —
no manual file editing is required, though the project `network` block
(repo-layout §12.4) can still be edited by hand. `ai network` manages three
declarations in the project `config.yaml`: the default outbound mode
(`network.egress`: `public` (default) / `deny` / `unrestricted`), the
allow-listed host services (`network.allow_host_services`), and the published
ports (`network.publish_ports`):

```yaml
network:
  egress: public                      # public (default) | deny | unrestricted
  allow_host_services:                # extra egress the workspace may reach
    - { host: gateway,          port: 5432 }   # host-local Postgres
    - { host: db.prod.internal, port: 5432 }   # a remote/managed database
    - { host: kafka-1,          port: 9092 }   # Kafka broker(s)
    - { host: api.stripe.com,   port: 443 }    # a specific internet API
  publish_ports:                      # host → workspace
    - { guest: 3000, host: 3000 }
```

`ai network` manages the **declaration** in `config.yaml`. The actual enforcement
is the Microsandbox **NetworkPolicy**: workspace create translates this block into
`msb` net-rules (`egress.MsbNetworkArgs`) — the allow-list entries, the
default-egress mode, and the port maps — which the runtime enforces. Two rules are
emitted **always, in every mode**, ahead of the declared ones: the model-gateway
allow (§29.2) and a DNS allow pair
(`allow:egress@host:udp:53` + `allow:egress@host:tcp:53`, msb's `host` group =
`Rule::allow_dns()`) so name resolution survives default-deny (§29.7).
Host-reaching rules MUST use msb's `host` GROUP token (`allow:egress@host:tcp:<port>`),
NOT the `host.microsandbox.internal` NAME — only the `host` group engages msb's
host-forwarding; a NAME target lets the guest connect to the msb gateway but is
never forwarded to the host service (empty reply, verified live). So when the
gateway is local (standalone) its allow rule and the `gateway`-token host-service
entries target `host`; a remote gateway (client mode) and explicit domain/IP
host-services stay verbatim (see §29.5). App-data connections
(DB/Kafka/HTTP) go **direct** under this policy — they do not pass through the
model gateway.

## 29.7 Attempted-egress-by-name audit (`aip-dns`)

The service tier (§5) includes **`aip-dns`**, a CoreDNS resolver that exists for
**observability, not enforcement**. Every workspace microVM is booted with
`--dns-nameserver` pointing at it (a fixed platform setting on the host loopback,
`127.0.0.1:15353`), so Microsandbox's netstack forwards the guest's DNS to it.
CoreDNS's `log` plugin records each query; `forward` resolves it upstream and a
short `cache` smooths repeats. `ai network log` (CLI §10a) reads that log and
prints the attempted-egress-**by-name** audit.

This cleanly splits **audit** from **enforcement**:

* **Enforcement stays on the Microsandbox NetworkPolicy** (§29.4) at L3/L4. A
  resolver *answer* cannot create reachability — a name resolving to an address
  the net-rules deny is still blocked at the network layer. The resolver has no
  authority over what a workspace may reach.
* **The resolver is the audit source.** It sees the *names* a workspace tried to
  resolve, which the L3/L4 rules (operating on IPs/CIDRs/domains) do not surface
  as a human-readable list.

**v1 scope / caveats** (made explicit in `ai network log`'s output):

* **Host-wide, not per-project.** All workspaces forward to the one resolver, so
  the audit is every workspace's DNS on the machine; it is **not attributed per
  project** in v1 (the queries arrive NAT'd from the netstack, so the originating
  workspace is not distinguishable). `ai network log [project]` accepts the
  argument for forward compatibility but does not filter on it in v1.
* **Names only — not connection verdicts, not direct-IP egress.** It is a record
  of attempted *resolutions*, not of allowed/blocked connections, and traffic to a
  literal IP never touches DNS so never appears here.
* **DNS resolves under default-deny via the `host` group (verified, msb 0.5.7).**
  Under a `deny`/`public` posture msb filters DNS like any other egress, so a
  policy whose rules are all host-name/IP based matches nothing at DNS-decision
  time (the query name has not resolved to an IP yet) and *every* lookup would be
  denied. To keep name resolution working, `egress.MsbNetworkArgs` emits an
  always-on DNS allow pair — `allow:egress@host:udp:53` + `allow:egress@host:tcp:53`
  (msb's `host` GROUP token = the convenience `Rule::allow_dns()`) — in **every**
  mode. The `host` group matches the local gateway forwarder the query is
  delivered to (the only target that re-opens DNS, scoped to port 53), so it does
  **not** open general host-service access — the default-deny boundary is fully
  preserved. Because the guest's resolver is `aip-dns`, those permitted lookups
  reach CoreDNS and appear in the audit. A *denied* destination's name still
  resolves (so it may appear in the log), but the connection to its IP is blocked
  at L3/L4 — the audit is a record of *attempted resolutions*, not of
  allowed/blocked connections, and is not a substitute for the policy shown in
  `ai network show`, which remains the source of truth for what is reachable.

---

# 30. Security Model

Requirements:

* **keys-in-LiteLLM** — real provider keys live only in the LiteLLM gateway
  (§17); the workspace holds only a scoped LiteLLM virtual key
* no plaintext secrets on platform disk
* no host Docker socket by default (the service tier runs rootless; workspaces
  use the Microsandbox microVM runtime, which never touches the Docker socket)
* workspace isolation (hardware-level — each workspace is a microVM, so a
  workspace escape is contained at the VM boundary, not the host kernel)
* project isolation
* agent isolation
* **egress confinement** — each workspace runs under a Microsandbox
  **NetworkPolicy** with a deny-by-default fallthrough; its default `public`
  posture allows the open internet (private/internal ranges stay blocked) plus the
  trusted model gateway and explicitly allow-listed host services, and a project
  can re-lock to `deny` so only the gateway + allow-listed services are reachable
  (§29.4, §29.6). The policy is applied as `msb` net-rules at workspace create
  (§29.5, §29.6)
* **always-on secret protection** — LiteLLM's guardrails are `default_on`
  on every request and every route (cloud included), so no model traffic bypasses
  secret masking (§15)
* **least host privilege** — both the hypervisor and the workspace network stack
  run in user space (§29.1). On Apple Silicon the *only* elevated facility is the
  macOS hypervisor entitlement (code-signed, no kext); workspace networking
  (gvproxy + Microsandbox NetworkPolicy) needs no `utun`, NetworkExtension, admin
  approval, or host Docker socket

Workspace isolation depends on the host hypervisor (KVM on Linux, HVF on macOS)
being available; where it is not, the platform must fail the `doctor` check
rather than run workspaces without microVM isolation (§6.2).

---

# 31. Audit Logging

Location:

```text
~/.ai-platform/audit/
```

Events:

* workspace creation
* workspace deletion
* secret-access events (that a credential was used — never the value)
* model configuration changes

Secret values must never be logged. LiteLLM owns the authoritative model-request
log (spend, virtual-key usage, guardrail actions) in its Postgres-backed store;
the platform audit log records only that a secret-access or model-configuration
event occurred, with no secret material.

---

# 32. Backup and Recovery — Removed

The platform has no backup/restore feature. It isn't needed:

* **project source** lives in the project directory (the cwd at `ai create`) and
  is the user's own git repo (backed up by pushing to a remote)
* **platform state** is reconstructable from the filesystem via `ai state repair`
* **provider keys** live in the LiteLLM gateway (§17), not on platform disk
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
* nginx proxy (`aip-proxy` — the host entry; readiness probed end-to-end through
  the gateway, §10)
* LiteLLM
* Presidio
* Ollama
* DNS audit resolver (`aip-dns`, §29.7)
* Docker
* Podman
* Caveman
* Headroom

(`ai doctor` reports every service-tier service — dns, ollama, presidio, litellm,
headroom, proxy — §5.)

---

# 34. Success Criteria

The architecture is considered successful when a user can:

```bash
ai setup

ai create my-project   # interactive wizard: select OS + agent CLIs (defaults: debian-trixie, OpenCode) — scaffolds .ai-platform/
ai start               # build the OCI image + boot the microVM
```

and immediately receive (for the OS the user selected):

* selected-OS workspace microVM (e.g. Debian trixie)
* host-backed source code
* hardware-isolated Microsandbox microVM (rootless container runtime for the
  service tier)
* LiteLLM integration (provider keys held in the gateway, §17)
* always-on secret-masking guardrails on every model request (§15)
* per-project Microsandbox egress policy — default `public` (open internet,
  private ranges blocked), re-lockable to `deny` (`ai network`, §29.4)
* Headroom input compression
* Caveman output compression
* reproducible, Dockerfile-defined environments
* zero manual Microsandbox configuration
* project persistence (overlay)
    
