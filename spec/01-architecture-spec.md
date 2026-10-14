# 01-architecture-spec.md

# AI Development Platform

## Architecture Specification

Version: 1.0

Status: Target End-State Architecture

---

# 1. Purpose

This document defines the end-state architecture for the AI Development Platform.

The platform provides reproducible AI-powered software development environments using nono (https://nono.sh) kernel-capability sandboxing — Landlock LSM on Linux, Seatbelt on macOS — to confine each agent CLI invocation to its project directory, centralized model access, context optimization, off-disk provider credentials (held in the LiteLLM gateway), and host-backed project persistence.

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
* Kernel-capability-sandboxed workspaces (nono — Landlock/Seatbelt), each agent
  invocation confined to its project directory
* Rootless container runtime for the service tier by default
* Centralized model access
* Context optimization
* Secure secret management
* Reproducible, config-defined environments (no per-workspace OS image)
* Cross-platform host support

---

# 3. Design Principles

## Reproducibility

All projects must be reproducible from:

* source repository
* project configuration (`.ai-platform/config.yaml`)

A workspace has no OS image or build step of its own — it runs on whatever
toolchains already exist on the host — so reproducibility now rests on the
project configuration and the host environment, not a platform-built image.
No project behavior should depend on manual platform configuration beyond
that.

---

## Isolation

Projects must be isolated from:

* other projects
* unrelated agents

Isolation is enforced by **nono** (https://nono.sh), a kernel-capability
sandboxing CLI: Landlock LSM on Linux, Seatbelt (`sandbox-exec`) on macOS. Each
agent CLI invocation runs as a single sandboxed **host process** — there is no
VM, no guest kernel, and no OS image. This is **same-kernel** capability
confinement (filesystem + network access grants), not hardware/VM isolation —
a deliberate, accepted trade-off against the platform's former Microsandbox
microVM runtime: a sandbox escape in nono is a kernel-LSM/Seatbelt bypass, not
a hypervisor breakout. What nono *does* guarantee: a session only has the
filesystem access its profile declares (project directory + explicitly granted
paths) and only the network access its profile's egress policy allows — it
cannot silently touch the rest of the host filesystem or reach the rest of the
host's network.

---

## Persistence

Project source code is always stored on the host, in the project directory
itself. There is no separate VM disk or overlay: the project directory **is**
the persistent state, so there is nothing for "workspace recreation" to
destroy — a nono session is stateless and disposable; only the project
directory and the host's own installed programs persist.

---

## Security

Secrets must never be:

* committed to repositories
* stored in plaintext on platform disk
* distributed through environment files

Provider keys live only in the LiteLLM gateway (keys-in-LiteLLM, §17); the
agent CLI holds only a scoped LiteLLM virtual key, delivered via nono's own
OS-keychain-backed credential injection (§7) rather than a disk file or plain
process environment.

---

## Host Agnostic

The platform must function consistently on:

* macOS (Apple Silicon or Intel)
* Linux

nono's Landlock (Linux)/Seatbelt (macOS) sandboxing has no virtualization
requirement and no Apple-Silicon-only restriction — any host the `nono`
binary runs on can sandbox workspaces. (The host-side local-inference backend,
omlx, is still macOS/Apple-Silicon-only — see §16 — but that restriction is
about omlx specifically, not the workspace runtime.) Behavior must remain
consistent regardless of host OS.

---

# 4. High-Level Architecture

The platform's layers relate along two distinct axes. Drawing them as one
linear stack is misleading, so they are shown separately.

## 4.1 Infrastructure Containment (what runs inside what)

The platform uses **two distinct mechanisms** on the host, for two distinct
purposes:

```text
Host Layer
 ├─ nono (kernel-capability sandboxing: Landlock on Linux, Seatbelt on macOS) ← workspaces
 │   └─ Sandbox Layer (one nono session per agent-CLI invocation, host process)
 │       ├─ AI Tooling Layer (OpenCode by default; Claude Code / Codex / Gemini / omp / Hermes optional — selected per project, installed directly on host)
 │       └─ Context Optimization (Caveman skill — per project, §8–10; Headroom is a host-side LiteLLM guardrail, §10, with an opportunistic `headroom wrap <cli>` around the sandboxed process when the `headroom` CLI is on host PATH)
 │
 ├─ Container Runtime (Docker / Podman)            ← service tier (network aip-net)
 │   └─ nginx reverse proxy (aip-proxy — SOLE host entry, publishes :18787)
 │       └─ INTERNAL-ONLY containers on aip-net (no host publish):
 │          Headroom · LiteLLM (+ Postgres) · Presidio (analyzer + anonymizer) · Valkey · RedisInsight · DNS audit (CoreDNS)
 │
 └─ Local inference backend (host-side, reached by the service tier via host.docker.internal)
     └─ omlx (host-side; ONE shared `omlx serve` OpenAI endpoint on :8100 serving every local model; platform probes it)
```

Workspaces are **sandboxed host processes** (same-kernel capability
confinement via nono), not VMs and not containers. The container runtime is
used only for the stateless service tier; nono's sandboxing never touches
Docker/Podman at all.

Host services (run on the host, not inside a sandbox):

```text
nginx proxy (aip-proxy)                                 — the SOLE host entry, publishes :18787 (default server + UI vhosts)
Headroom · LiteLLM (Model Layer) · Presidio             — container tier (Docker/Podman, network aip-net), INTERNAL-ONLY behind nginx
omlx (local inference)                                  — host-side backend; the service tier reaches it via host.docker.internal
nono                                                    — host-process sandboxing CLI, driven by the `ai` CLI per agent invocation (no daemon)
```

The host service tier is containers on `aip-net`: nginx (`aip-proxy`),
Presidio (analyzer + anonymizer), LiteLLM (+ Postgres), Headroom, Valkey
(`aip-valkey`, the LiteLLM response cache) and its RedisInsight GUI
(`aip-redisinsight`), and the CoreDNS egress-audit resolver. The local-inference
backend is **not** a container — host-side omlx runs
host-side and the service tier reaches it via `host.docker.internal` (§16).
There are no optional host services. There is no
native host service. **`aip-proxy` (nginx) is the SOLE host entry point** — every
other service container is INTERNAL-ONLY on `aip-net` (reached by name, no host
publish), except the loopback-published support container `aip-dns`
(`127.0.0.1:15353/udp`, used only for the DNS egress-audit log, §29.7 — the
sandboxed agent process resolves names through the host's own resolver like any
other host process, so this is observability, not a routing requirement);
`aip-litellm-db` is also INTERNAL-ONLY (no host port, reached at
`aip-litellm-db:5432`). See §5/§10 for the full nginx routing model.

Headroom is a **shared host container** (`aip-headroom`) that LiteLLM calls
in-process as a `pre_call` compression guardrail; nginx does not route to it.
Separately, a host-installed `headroom` CLI (the user's own install — the
platform does not install it) can opportunistically wrap an agent-CLI
invocation (`headroom wrap <cli>`, §7/§10) for the three CLIs it supports
(claude-code, codex, opencode) whenever it is found on host PATH.

(The model-request path below involves the Headroom/LiteLLM/Presidio subset;
omlx is a host-side local-inference backend the service
tier reaches via `host.docker.internal` — see §5/§16. nono is not a
long-running service either: it is invoked directly, per agent-CLI launch, to
sandbox that one process.)

The Project Layer is the top-level, user-facing artifact: host-stored source in
the project directory (the cwd at `ai create`) — there is no bind mount or
guest copy, since nono sandboxes the host process directly against that same
directory (§7).

## 4.2 Model-Request Path (how a call flows)

```text
Agent CLI (AI Tooling, sandboxed host process via nono)
 ╎  plain host loopback → the resolved gateway address
nginx (aip-proxy, host :18787, location /v1 → LiteLLM directly)  (SOLE host entry)
 ↓
LiteLLM (Model Layer, aip-litellm:4000, internal-only)
 ↓  pre_call headroom guardrail: POST messages to aip-headroom:8787/v1/compress, swap in compressed input
 ↓  enabled guardrails (§15): secret-masking (Presidio), hide-secrets, tool firewall — when selected
 ↓  route to provider (local — omlx via host.docker.internal — or cloud); real provider key from LiteLLM's store (cloud only)
Provider · Caveman steers output                         (Context Optimization)
```

Context Optimization sits *between* the agent and the model (Caveman steers the
agent's output; Headroom compresses the input). The path: the sandboxed agent
process sends to the **nginx gateway** at a plain host address (loopback by
default, §29.2 — there is no VM boundary to cross), nginx forwards
`/v1` **directly to LiteLLM**, which compresses the input via its in-process
`headroom` guardrail (§10), runs the other enabled guardrails (§15), and attaches the
real provider key from its own store on cloud calls (keys-in-LiteLLM, §17). The
agent process holds only a scoped LiteLLM virtual key, injected by nono from
the OS keychain. See Sections 8–9 (context
optimization), 15 (LiteLLM + guardrails), 17 (keys-in-LiteLLM), and **29 (the full
per-component networking model + the nono egress policy)** for detail.

---

# 5. Host Layer

The Host Layer provides all persistent infrastructure.

Supported hosts:

* macOS (Apple Silicon or Intel)
* Linux

Responsibilities:

* Project storage
* Shared AI resources
* Headroom deployment (shared container; LiteLLM's `pre_call` input-compression guardrail backend)
* LiteLLM deployment
* Presidio deployment (analyzer + anonymizer, backing LiteLLM's secret-masking guardrail when enabled)
* Local-inference backend — host-side omlx (probed, not containerized); the service tier reaches it via `host.docker.internal` (§16)
* nono — the host-native kernel-capability sandboxing CLI (§7), installed via `ai setup`/`brew install nono`/`curl -fsSL https://nono.sh/install.sh | sh`
* Platform state

There is no per-workspace OS image or Dockerfile anymore (`internal/envimage`
and the per-OS Dockerfile templates it composed were removed along with
Microsandbox) — a workspace runs on whatever toolchains already exist on the
host.

---

## Host Directory Structure

```text
~/.ai-platform/
```

Contains (`internal/layout.Ensure`):

```text
~/.ai-platform/
├── agents/
├── audit/
├── cache/          # re-fetchable caches: catalog.yaml (models.dev)
├── config/         # global settings + projects index (no per-project state)
├── logs/
├── overlays/       # vestigial — the per-workspace VM-disk overlay this backed (§26 history) is gone; no code writes here anymore
├── prompts/
├── skills/
├── templates/      # vestigial — the per-OS Dockerfile template composer (internal/templates) that used this is gone; no code writes here anymore
├── venv/           # platform-managed host Python venv (omlx)
├── volumes/        # host data volumes only (litellm-db, models store)
└── tools/
```

`~/.ai-platform/` holds **only global settings and host artifacts** — there is
no per-project state here. Per-project state lives in the project itself (see
below). (The `bin/` directory that used to hold the platform-managed,
sha256-verified `msb` CLI binary is gone with Microsandbox — nono is installed
onto the regular host PATH, e.g. via Homebrew or its own installer, not
platform-managed inside `~/.ai-platform`.)

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
├── config.yaml       # tracked — project config
├── project.yaml      # tracked — { name, os, created }
├── nono-profile-<cli>.json / nono-profile-shell.json  # gitignored — regenerated on every launch (§7)
├── {agents,skills,prompts,projects}/  # shared resource pool — dirs scaffolded empty;
│                     #   populated at workspace start (Caveman install + Graphify +
│                     #   symlinks). Caveman is NOT platform-seeded/git-tracked (§9).
├── .gitignore        # ignores run/
└── run/              # gitignored — host-local runtime state
    └── workspaces/<session-id>.yaml
```

There is no `Dockerfile` and no `profile.yaml` anymore — a workspace has no
per-project OS image or software-stack list to define (§24, §25 history).

Rules:

* **tracked** files (`config.yaml`, `project.yaml`) are the config
  definition — committable so the project is reproducible from git. The
  generated `nono-profile-*.json` files are regenerated on every launch from
  `config.yaml` (so a config change takes effect on the next launch with no
  restart step) and are not meaningful to commit.
* **`run/`** holds host-local runtime handles (the nono session id, status)
  and is **gitignored** — machine-specific, never committed
* per-entity sharding (one file per session under `run/`)
  avoids write contention
* **all state writes are atomic**: write to a temp file in the same directory,
  then `rename()` over the target — a crash mid-write never corrupts state
* `ai state repair` reconstructs `run/` by reading the project + nono/git;
  project discovery uses the global `projects.yaml` index

There is no per-workspace **overlay** anymore (§26 history) — the project
directory itself, on the host, is the only persistent state a workspace has.

---

## Host Services Control Plane

The platform's host services — Headroom, LiteLLM (+ its Postgres), Presidio
(analyzer + anonymizer), and the CoreDNS egress-audit resolver (there are no
optional host services) — plus the local-inference backend (host-side omlx,
§16) and the host-native **nono** sandboxing CLI (§7) are installed,
configured, and supervised by the `ai` CLI. The CLI is the **single control
plane**: the user never invokes `docker compose`, `nono`, `launchctl`, or
`systemctl` directly. The service-tier containers share a private docker network
(`aip-net`) and are reconciled in order: network → DNS → Presidio →
Valkey (+ RedisInsight) → Headroom → LiteLLM (+ DB) → nginx proxy (last).
**omlx** is a host-side backend, not a container in this order — the platform
probes it **non-fatally** during reconcile (an unreachable omlx is a hint, not a
setup failure — §16); Headroom now **precedes** LiteLLM because LiteLLM's
`headroom` compression guardrail calls it.
(The destructive-tool-call firewall and the `hide-secrets` detector are in-process
in LiteLLM — they need no companion container; only Presidio, which backs the
secret-masking guardrail, runs as its own analyzer + anonymizer containers, and it
is reconciled only when the `secret-masking` guardrail is selected. §15.
The in-process prompt-injection detector was removed — §15.)
(Headroom is a shared host container that LiteLLM calls as a `pre_call` guardrail,
INTERNAL-ONLY on `aip-net`; the `aip-proxy` nginx reverse proxy is the sole gateway
entry on host :18787 — see §10.)

**`aip-proxy` (nginx) is the SOLE host entry point to the service tier.** Every
other service container is INTERNAL-ONLY on `aip-net` (reached by name) — none
publishes a port to the host. Only nginx publishes, and it publishes a **single
port**: host **:18787**. Everything is served on that one port, split by route and
by `server_name` — the default server (the model path + the `/llm`
management surface) plus the two Host-based UI vhosts (`litellm.<domain>` → LiteLLM
admin UI, `valkey.<domain>` → RedisInsight). The full route table, the UI vhosts, and
the standalone `/etc/hosts` vs server-mode DNS operator contract are specified in §10.
(There is no in-workspace app layer anymore — `internal/apps`, which ran opt-in
containerized apps like Open WebUI inside the old microVM, was removed along
with Microsandbox; see §7 history. Odysseus had already been removed from the
platform entirely, independent of this migration.)

TLS terminates **per-vhost at nginx** (out of scope now): the blocks are structured
so a per-vhost `listen 443 ssl;` can be added later, and once HTTPS is configured the
`:80`/http listener on the single :18787 entry **MUST** `return 301
https://$host$request_uri;` (no redirect is emitted today because there is no https
listener yet — a 301 with no :443 would break every plain-http caller). nginx is
reconciled LAST so its upstreams are up first. (Only `aip-dns` stays
loopback-published — DNS must stay `127.0.0.1:15353`, used for the egress-audit
log (§29.7); `aip-litellm-db` has no host port at all, internal-only on
`aip-net`.) The live end-to-end routing through these nginx routes — the
litellm UI vhost and the sudo `/etc/hosts` write — is a **hardware bring-up**
verification item (`docs/HARDWARE-BRINGUP.md`).

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
| nginx proxy | container (via Runtime) `aip-proxy` (`nginx:stable-alpine3.23-slim`) | the SOLE host ENTRY to the service tier: publishes ONLY :18787 — the default server (`/`+`/v1`→LiteLLM directly, `/llm`→LiteLLM) plus the two Host-based UI vhosts on the same port (`litellm.<domain>` → LiteLLM admin UI, `valkey.<domain>` → RedisInsight); HTTPS termination point later (§10) |
| Headroom | container (via Runtime) `aip-headroom` (`ghcr.io/headroomlabs-ai/headroom:latest`) | LiteLLM's `pre_call` input-compression guardrail backend, called at `aip-headroom:8787/v1/compress`; INTERNAL-ONLY on :8787 on aip-net (no host publish, nginx never routes to it); carries only `HEADROOM_TELEMETRY=off`; HTTP only (§10) |
| LiteLLM | container (via Runtime) `aip-litellm` (image tag `latest`) (+ `aip-litellm-db` Postgres, surfaced as its own `postgres` status line) | INTERNAL-ONLY: no host publish, reached by name (`aip-litellm:4000`) by nginx's model path + `/llm` route; it calls Headroom in-process; HTTP only; no host privileges |
| Presidio | two containers (via Runtime) `aip-presidio-analyzer` + `aip-presidio-anonymizer` | back LiteLLM's secret-masking guardrail; started ONLY when `secret-masking` is selected (§15); internal-only, not published |
| omlx (local backend) | **host-side, ONE shared process** (a single `omlx serve` answering for every locally-served model — NOT per-model processes, NOT containers) | the platform's sole local-inference backend, macOS + Apple Silicon only; probed at `http://127.0.0.1:8100/v1/models`, reached by the LiteLLM/nginx containers at `host.docker.internal:8100/v1`; ensured NON-fatally in reconcile — an unreachable omlx is a hint, not a setup failure. Install/serve mechanics, model management (entirely via omlx's own admin panel), and the hardware-bring-up caveats are §16 |
| DNS audit resolver | container (via Runtime) `aip-dns` (CoreDNS) | egress-audit resolver: a fixed host loopback nameserver so attempted names are logged for `ai network log`; published to host loopback only; audit, not enforcement (§29.7) |
| nono | host-process sandboxing CLI, invoked per agent-CLI launch | sandboxes one host process per `nono run --detached`/`nono shell` invocation (Landlock on Linux, Seatbelt on macOS); no daemon to supervise (§7) |

**docker compose is not used.** Container-tier services are managed directly
through the runtime abstraction (§6), so docker and podman remain
interchangeable and there is no second orchestration mechanism. The whole
service tier runs as containers — there is no native OS service to register.
nono is **not** a long-running service — the `ai` CLI invokes it directly, per
agent-CLI launch, to start/attach/stop a sandboxed session (§7); `ai setup`
only verifies the `nono` binary is installed (and best-effort installs it when
absent, §7) — there is no virtualization capability to verify, unlike the
Microsandbox runtime this replaced.

### Configuration by Reconciliation

The platform `config.yaml` (§27) is the single source of truth. The CLI
**renders** each service's native config from it and reconciles desired vs.
actual state; `ai setup` means "make reality match config" and is safe
to re-run. Pinned versions/digests live in `config/versions.yaml`. Rendered
service configs live under `config/<service>/`.

### Install + Configure Summary

* **install**: container images are pulled by pinned **image + tag** (default
  `latest`; no digests — they are platform/arch specific — `config/versions.yaml`,
  §27); the `nono` CLI is installed onto the regular host PATH (`brew install
  nono` when Homebrew is available, else `curl -fsSL https://nono.sh/install.sh
  | sh`), best-effort and Detect-gated (§7); omlx is `pip`-installed into the
  platform-managed venv `~/.ai-platform/venv` by `ai setup` (or a manual
  `ai services start omlx`, which retries the same install) (§16); the
  claude-code/codex/opencode agent CLIs can likewise be host-installed by `ai
  setup` via each CLI's own official installer (§12) when selected
* **configure**: rendered from platform config — the LiteLLM config carries no
  model list (models are DB-backed, §14–15) and no key references; the real
  provider credentials live in the LiteLLM gateway, managed via `ai keys`
  (keys-in-LiteLLM, §17) — never on platform disk;
  nono's per-project profile (filesystem/network capability grant) is
  regenerated from `config.yaml` on every agent/shell launch (§7) — there is no
  image, mount, or resource-limit to configure ahead of time; local models are
  registered as DB-backed LiteLLM models automatically, reconciled against
  omlx's own live model list (`litellm.SyncOmlxModels`, also triggerable on
  demand via `ai models refresh`) — omlx
  (host-side, one shared `omlx serve` process) is the sole local backend (§14, §16)
* **startup ordering**: container runtime verified (nono's presence is checked
  independently and does not gate the service-tier reconcile) → container tier
  reconciled in order
  `aip-net` network → DNS (CoreDNS) → Presidio (analyzer + anonymizer,
  only when `secret-masking` is selected) → Valkey (+ RedisInsight) → Headroom →
  LiteLLM (+ Postgres) →
  **nginx proxy (last** — its upstreams must be up first since nginx resolves
  literal `proxy_pass` hosts at config-load) → verify
  (nono sessions are created on demand, per `ai agent`/`ai shell`, not at setup)

### Secrets Boundary

Real provider keys live **only in the LiteLLM gateway**, never on platform disk and
never in the workspace; the workspace agent holds only a scoped LiteLLM **virtual
key** (the gateway key), so no plaintext provider credential reaches the workspace,
the rendered config, or a backup (keys-in-LiteLLM, §17).

---

# 6. Runtime Layer

The platform uses two runtimes for two purposes.

## 6.1 Container Runtime (service tier)

Used for the container-tier services — the nginx proxy (`aip-proxy`, the sole
host entry), Headroom, LiteLLM (+ its Postgres), Presidio (analyzer +
anonymizer), and the CoreDNS egress-audit resolver — which
share a private docker network (`aip-net`). There are no optional host services.
The local-inference backend (omlx) is **not** in this
tier — it runs host-side and the container tier reaches it via
`host.docker.internal` (§16).

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

## 6.2 Sandboxing Runtime (workspaces) — nono

Workspaces run as **nono-sandboxed host processes** (https://nono.sh), not
containers and not VMs. This runtime is driven directly by the `ai` CLI,
shelling out to the `nono` binary per agent-CLI invocation (`internal/nono`,
`RealRunner`); an injectable `nono.Runner` interface (`RealRunner`/`FakeRunner`)
keeps the orchestration code unit-testable without the real binary (§6.1's
runtime-abstraction convention applies here too). There is no daemon and no
host Docker socket involved (§7, §30).

nono sandboxes using the host kernel's own capability-confinement facilities:

* **Linux:** Landlock LSM
* **macOS:** Seatbelt (`sandbox-exec`)

Host requirements: **none beyond the `nono` binary itself** — there is no
virtualization capability to verify (no KVM, no Apple Hypervisor requirement),
and no Apple-Silicon-only restriction. Any host the `nono` binary runs on can
sandbox workspaces (§3, Host Agnostic). `ai setup`/`ai doctor` simply check
that `nono` is on PATH (`nono.Detect`) and best-effort install it when absent
(§7).

The platform consumes **no OCI images for workspaces** — there is nothing to
build. A workspace is ready to use the moment its project is registered
(`ai create`); the first `ai agent <cli>`/`ai shell` generates that invocation's
nono profile on the fly (§7).

## Delivery Phasing

Runtime support is delivered incrementally (see `02-implementation-roadmap.md`):

* Slice 1: Docker (service tier) + nono (workspaces), rootless by default.
* Slice 6: Podman support + full container-runtime abstraction on Linux.

Acceptance tests that exercise Podman or Docker/Podman equivalence apply from
Slice 6 onward; the rootless test applies from Slice 1. Each test in
`05-acceptance-tests.md` is tagged with the slice from which it is expected to
pass. (nono has no host-OS-specific slice gate — unlike the Microsandbox
runtime it replaced, it needs no Apple-Silicon/KVM capability check — though
this spec does not revise the CI/acceptance-suite config itself; see
`docs/HARDWARE-BRINGUP.md`/CI workflow for whether the self-hosted
Apple-Silicon-only runner constraint is still warranted now that the workspace
runtime has no virtualization requirement.)

---

# 7. Sandbox Layer (nono)

**nono** (https://nono.sh) provides:

* workspace isolation (kernel-capability sandboxing — Landlock LSM on Linux,
  Seatbelt/`sandbox-exec` on macOS — confining a single host process per
  invocation, NOT hardware/VM isolation; see §3 Isolation for the explicit
  trade-off this replaces)
* session lifecycle (one `nono` session per agent-CLI/shell invocation)

There is no VM, no OS image, no guest kernel, and no persistent boot. A
"workspace" is simply a **registered project directory** (tracked in
`internal/project`, the same `.ai-platform/` state layout, minus the old
Dockerfile/profile.yaml/apps pieces) plus a **generated nono profile**
(`internal/nono`, a JSON capability grant written to
`<project>/.ai-platform/nono-profile-<cli>.json`, regenerated on every
launch). The runtime has **no daemon**: the `ai` CLI shells out to the `nono`
binary directly, once per launch (`internal/nono.RealRunner`), via `nono run
--detached` / `nono attach` / `nono shell` / `nono ps --json` / `nono inspect
--json` / `nono stop`.

---

## Workspace Types

### Project Workspace

**One project directory, potentially several concurrent nono sessions.**
Unlike the old one-microVM-per-project model, every `ai agent <cli>`/`ai
shell` launch is its **own** nono session, sandboxed against the same project
root: an anonymous session for `ai shell`, one named for the CLI for
`ai agent <cli>`. `ai agent <cli> [name]` reattaches that CLI's
already-running session if one exists, else it mints a fresh scoped key,
writes that CLI's config, regenerates the profile, and launches a new
detached session before attaching to it interactively. There are still no
per-agent *workspaces* — multiple simultaneous sessions share the one project
directory, and any source isolation a user wants between them (git branches,
worktrees, separate checkouts) is the in-workspace agent CLI's concern (§20),
unchanged from before.

---

## Workspace Lifecycle

nono sessions have two live states — **running** and **stopped/exited** — not
the five-state model a microVM lifecycle needed:

```text
(project registered)
 ↓
Running   (a nono session is live: `ai agent`/`ai shell`)
 ↓
Stopped   (`ai stop` / `nono stop`; the session exited or was killed)
```

The platform's `state.Workspace` type (`internal/state`) still carries its
original five-value `WorkspaceStatus` enum (`created`/`started`/`stopped`/
`archived`/`destroyed`) for continuity, but only `started` and `stopped` are
actually written by current code (`Manager.Agent`/`Manager.StopAll`) — the
others are unused legacy values.

A session is disposable — stopping or killing it destroys nothing but that
one sandboxed process. Project data is not: it is the project directory on
the host, untouched by any session's lifecycle.

---

## nono Integration Mapping

The platform drives nono through the `nono.Runner` interface
(`internal/nono`, real implementation shells out to the `nono` binary). The
mapping below is normative.

### Naming

```text
sandbox-facing name:  aip-<project>
```

Kept for continuity with the platform's prior naming/display even though it
no longer names a microVM — `Name(project)` is used as the `nono run --name`
prefix for agent sessions and in log/UI text.

### Profile (replaces "Image")

There is no OCI image, no Dockerfile, and no build step. Instead, each launch
generates a **nono profile** — a small JSON capability grant
(`internal/nono.Profile`) describing exactly what that one sandboxed process
may touch:

* `filesystem` — path-level allow/read/write grants (today: `--allow-cwd`
  grants the project root at launch; the profile itself carries no extra
  filesystem block beyond what the base pack/`default` profile already grants)
* `network` — `block` (true under the `deny` egress mode, §29.6) plus
  `allow_domain`/`deny_domain` lists mirrored straight from the project's
  `config.yaml` `network` block
* `environment.set_vars` — plain (non-secret) environment variables, used only
  for gemini's gateway base URL (gemini has no config-file base-URL setting)
* `extends` — a base profile this one inherits from: for opencode/claude-code/
  codex, a **nono registry pack** (`nolabs-ai/opencode`, `nolabs-ai/claude`,
  `nolabs-ai/codex` respectively — official, battle-tested filesystem/
  OAuth-flow grants for that CLI, best-effort `nono pull`led before first use);
  every other CLI (gemini/omp/hermes) and plain `ai shell` fall back to nono's
  own built-in `"default"` profile

The profile is cheap to regenerate (a few lines of JSON) and is rewritten on
**every** launch, so a `config.yaml` network change (`ai network allow/deny`)
takes effect on the very next `ai agent`/`ai shell` — there is no restart step
and no persistent sandbox to recreate.

### Credential delivery

The scoped LiteLLM virtual key never touches the agent process's disk or a
plain environment variable the process could leak via `/proc`/`ps`. It is
stored in the host OS keystore (macOS Keychain via `security`, Linux via
`secret-tool`/Secret Service — `internal/nono.StoreCredential`) under a
per-project name (`aip-gateway-<project>`), and nono itself injects it into
the sandboxed process's environment at launch via `--env-credential-map
<keychain-ref> <ENV_VAR>` — e.g. `AIP_GATEWAY_KEY` for opencode/omp/hermes/
codex, `ANTHROPIC_AUTH_TOKEN` for claude-code, `GEMINI_API_KEY` for gemini.
This is the direct replacement for the old in-VM agent-env-file mechanism —
same end state (the real secret never lands in a config file or on disk
outside the OS keystore), delivered through nono's own credential-injection
flag instead of VM isolation. `ai delete` best-effort revokes the project's
LiteLLM key and deletes its keystore entry (`Manager.DeleteCredentials`).

**OAuth-mode agents** (claude-code/codex/gemini can still be set to `oauth`
instead of `api-key` at `ai create`, exactly as before) skip the
key-minting/credential-map step entirely — the CLI's native login talks
directly to the provider, bypassing the gateway (the same bypass-the-firewall
caveat as before, now a same-kernel sandboxed process talking out rather than
a separate VM).

### Per-CLI config files — now on real host paths

Each CLI's provider config is written at **that CLI's own default project
location**, but now directly on the **host filesystem** — there is no guest
path to translate, since nono sandboxes the host process in place:

* opencode → `<project>/.opencode/opencode.json`
* claude-code → `<project>/.claude/settings.json`
* codex → `<project>/.codex/config.toml`, **plus** a GLOBAL,
  shared-across-ALL-projects `~/.codex/config.toml` trust-entry append (codex's
  trust file is no longer per-VM-isolated, so the platform idempotently
  APPENDS one `[projects."<path>"]` entry per project rather than overwriting
  the whole file)
* gemini → env-only, no config file (`GOOGLE_GEMINI_BASE_URL` set via the nono
  profile's `environment.set_vars`, no `/v1` suffix)
* omp → `~/.omp/agent/models.yml` (global) + `<project>/.omp/config.yml`
* hermes → global `~/.hermes/config.yaml`

All are still keyless — the real key is delivered only via nono's credential
injection above, never written into these files.

### Ports

There is no VM network boundary and no port-publishing concept anymore — a
nono-sandboxed process runs directly on the host, so it reaches the model
gateway the same way any other host process would: a plain host address
(loopback by default), resolved once by `internal/runtime` and threaded into
each CLI's config/env (§29.2). There is no `AI_PLATFORM_HOST` guest↔host
translation to perform and nothing analogous to the old `host:<port>` → VM →
container port chain the in-VM apps layer used — that whole app layer
(`internal/apps`, Open WebUI, agent-CLI dashboards, published app ports) was
removed along with Microsandbox; in-VM apps are simply out of scope now, not
replaced.

### Logs

There is no persistent per-workspace log stream and no microVM to tail —
`nono ps --json`/`nono inspect --json` report a session's live state, and
`ai sessions [name]` surfaces it (ID/NAME/STATUS/ATTACHED/STARTED). The `ai ui`
TUI's old Logs and Metrics sub-tabs are gone for the same reason — its
per-workspace sub-tabs are now exactly Workspace/Network/Context/Shell: there
is no persistent VM log and no per-VM metrics API to poll — just a
short-lived sandboxed process you attach to or don't.

### Lifecycle verbs

* **`ai agent <cli> [name]`** reattaches that CLI's already-running session if
  one exists, else mints a fresh scoped key, writes the CLI's config,
  regenerates the nono profile, and launches + attaches a new detached
  session — all in-process, no subprocess shell-out dance and nothing to
  "boot".
* **`ai shell [name]`** launches (or reattaches) an anonymous interactive nono
  shell session sandboxed to the project root — no specific agent CLI.
* **`ai start [name]`** launches (or reattaches) the project's **default
  agent CLI** (`config.yaml` `agent.default_tool`) the same way `ai agent`
  does — it is interactive (suspends into the real terminal) since there is no
  detached VM boot to run non-interactively anymore.
* **`ai stop [name]`** stops **every** live session for the project,
  in-process (`Manager.StopAll` — a fast host-side `nono stop` per session, not
  a VM shutdown).
* **`ai restart [name]`** stops every live session, then relaunches the
  default agent CLI fresh.
* **`ai delete`** (alias `ai destroy`) is still the single permanent removal
  verb: it now ALSO best-effort stops any live sessions for the project and
  revokes/deletes its OS-keychain credential, before removing the project's
  `.ai-platform` directory and de-registering it from `config/projects.yaml`.
  The user's OTHER files are kept unless `--purge` is given (which removes the
  whole project directory). There is still **no** separate "tear down but
  keep the project" verb beyond `ai stop`.
* **`ai exec`** is **gone** — there is no persistent sandbox process to exec
  into anymore. Use `ai agent <cli>` or `ai shell` instead.

---

# 8. Context Optimization Layer

The platform includes a mandatory context optimization layer.

Purpose:

* reduce token usage
* improve context quality
* reduce costs
* improve scalability

Components:

* Headroom — compresses input context (a shared host container LiteLLM calls as a
  `pre_call` guardrail, §10)
* Caveman — compresses agent output (per-project, in-workspace skill, §9)

These bound token usage on both sides of every model request: Headroom
compresses the *input* sent to the model, and Caveman compresses the *output*
the agent emits. Per-project Headroom tuning still applies — the platform keeps a
per-project strategy and rides its knobs in the request body so they reach the
`headroom` guardrail LiteLLM runs (§10).

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

Caveman is meant to integrate as an agent skill — installed per-project, not
baked into anything platform-managed, not git-tracked. `ai context caveman
<project> <level>` sets a level (`lite`/`full`/`ultra`/`wenyan`) in
`config.yaml` (`contextopt.SetCavemanLevel`), and `ai create`'s AI-tools
multi-select records whether Caveman is enabled for the project
(`context.caveman_enabled`, default ON).

**Status note (post-nono-migration):** in the prior Microsandbox architecture
this toggle drove a real install step, `workspace.registerCaveman`, that ran
Caveman's own upstream installer inside the workspace at start, giving each
detected CLI its native skill/agent/command files. That function (and its
equivalents for Graphify, code-review-graph, codebase-memory-mcp, OpenViking,
tokensave, agentmemory, ponytail, and agent-skills — §12) **does not exist in
the current `internal/workspace` package** — the config-level toggle and the
`ai tools`/wizard UI around it are present and functional, but nothing in the
current code actually runs an installer or writes skill files on the host.
This section is therefore describing the **intended** integration point, not
a currently-wired behavior; treat it as an open item for this migration cycle
rather than working functionality until `internal/workspace` (or a successor)
gains a host-side equivalent of the old per-tool `register*` functions.

---

## Context Optimization Workflow

Headroom and Caveman are the two halves of context optimization, though only
Headroom's half is currently wired end-to-end (see the status note above for
Caveman). **Headroom** is a **shared host container** (`aip-headroom`, §10)
that **LiteLLM calls as a `pre_call` compression guardrail** — its tuning is
per-project: the platform maps a per-project strategy onto Headroom's
request-body knobs (`headroom_keep_turns`/`headroom_output_buffer_tokens`,
§10) baked into the opencode config at every `ai agent opencode` launch
(`internal/workspace/agentconfig.go` `writeOpenCodeConfig` →
`headroomParams`), so each project's compression behavior reaches the
guardrail. Separately, the host-installed `headroom` CLI can wrap an
agent-CLI invocation directly (`headroom wrap <cli>`, §7) when found on PATH,
for the three CLIs it supports.

```text
   sandboxed agent process (nono)              │ host
   ─────────────────────────────               │
  Agent ─ full context ──────┼──────────────► nginx ─────────► LiteLLM ─► Model
    ▲  (per-project knobs    │  (:18787,         │  ▲ pre_call headroom guardrail  │
    │   in request body)     │   /v1)            │  └─► aip-headroom:8787/v1/compress
    └── compact output (Caveman-steered, when wired) ─────────────────────────────┘
```

---

# 10. Headroom

Headroom manages context budgets.

Headroom runs as a **shared host container** (`aip-headroom`, image
`ghcr.io/headroomlabs-ai/headroom:latest` — pulled, never built, running the image's
default `headroom proxy --host 0.0.0.0 --port 8787` CMD on `:8787`). It is the
backend for **LiteLLM's `pre_call` input-compression guardrail**: LiteLLM's
`headroom` guardrail (guardrail_name `headroom-compression`, mode `pre_call`,
`api_base: http://aip-headroom:8787`) POSTs the request messages to
`http://aip-headroom:8787/v1/compress` and swaps in the compressed result before
dispatch (requires LiteLLM v1.92.x+; the litellm image now tracks `latest`, which
satisfies that). Headroom is
**no longer an nginx proxy in front of LiteLLM** — nginx routes to it for nothing.
It carries **only `HEADROOM_TELEMETRY=off`** (the old `OPENAI_TARGET_API_URL` is
dropped, which also prevents a litellm→headroom→litellm loop). Headroom is
**INTERNAL-ONLY** on `aip-net` (no host publish), reached only by LiteLLM by name.

The gateway ENTRY is the **`aip-proxy` nginx reverse proxy**
(`nginx:stable-alpine3.23-slim`), the **SOLE host entry point** to the whole service
tier. The model path is **sandboxed agent process (host) → nginx (:18787, `location /v1`) → LiteLLM
(:4000)** — nginx forwards `/v1` **directly to LiteLLM**, which runs the `headroom`
compression guardrail in-process; this is what every agent CLI's
`base_url=…/v1` hits, and the nginx config disables response buffering and uses long
timeouts so streamed (SSE) LLM responses flush promptly. On the **same :18787**,
nginx additionally fronts the LiteLLM admin/management surface on **`location /llm`**
(→ `aip-litellm:4000`, prefix stripped: `/model/info`, `/v1/models`, `/health*`,
`/key*`, `/credentials`, …) — the specific `/llm` prefix
matches before the catch-all `/` (the default route, also → LiteLLM directly). The
web UIs are served as **Host-based vhosts on the SAME :18787**, NOT separate
host ports (data-driven from `services.UIVhosts()`): `litellm.<domain>` →
`aip-litellm:4000` (admin UI at `/ui`) and `valkey.<domain>` →
`aip-redisinsight:5540` (the RedisInsight GUI), both always on and carrying
WebSocket upgrade headers. `<domain>`
is the resolved platform base domain (`runtime.yaml` `domain`, default `aip.local`;
`ai domain`). Standalone points those names at `127.0.0.1` via an `/etc/hosts`
managed block written by `ai setup` (consent + sudo, else a manual block);
server mode does NOT edit `/etc/hosts` and instead prints the DNS (`*.<domain>` →
this server) + TLS (cert terminated at nginx) operator contract. Because nginx is
the only publisher, **every other service container is
INTERNAL-ONLY on `aip-net`** (LiteLLM, Presidio, Headroom do not
publish to the host); only `aip-dns` stays loopback-published
(`127.0.0.1:15353`) — `aip-litellm-db` has no host port at all, reached only
by name on `aip-net`. (The local-inference backend — host-side omlx — is
not on `aip-net` at all: it runs host-side and the container
tier reaches it via `host.docker.internal`, §16.) The gateway address resolves to a
plain host loopback address now that agent sessions are sandboxed host processes
rather than microVMs (§29.2), and nginx still lets TLS be terminated later,
per-vhost (in server mode it binds `0.0.0.0:18787` — the eventual public HTTPS
endpoint). The host CLI reaches LiteLLM (admin via `…/llm`, the chat test via
`…/v1`) through nginx, never the backend directly. Separately from the
container-tier Headroom, a user-installed `headroom` CLI on the host PATH can
also opportunistically wrap an agent-CLI invocation (`headroom wrap <cli>`, §7)
for the three CLIs it supports (claude-code, codex, opencode) — the platform
does not install this CLI itself (the old plan to bake `headroom-ai[proxy]`
into the workspace image no longer applies, since there is no workspace image).
The live end-to-end routing through the nginx routes — the litellm UI vhost
and the sudo `/etc/hosts` write — is verified at hardware bring-up. (There is
no in-workspace app layer anymore — see §7 history; Odysseus has been removed
from the platform.)

The Github repository is found at:

[https://github.com/chopratejas/headroom](https://github.com/chopratejas/headroom)

Responsibilities:

* token accounting
* summarization
* compression
* budget enforcement

---

## Headroom Rules

By default every model request passes through the Headroom compression guardrail
(`headroom` is the only default-on guardrail, §15).

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
reaches LiteLLM's `headroom` guardrail. (Go: `internal/contextopt.HeadroomParams`.)

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

**This whole layer changed shape with the nono migration.** There is no
workspace image and no Dockerfile anymore, so there is nothing for the
platform to "bake in" ahead of time — base tooling is whatever already
exists on the host, agent CLIs are installed directly onto the host, and
the per-project AI tools (Caveman, Graphify, …) are config-level toggles
whose actual host-side installation logic is, as of this writing, **not yet
reimplemented** — see the status note under "Per-project AI tools" below.

## Base tooling (host-provided, not platform-managed)

There is no base-tooling layer anymore: a workspace is sandboxed directly
against the host, so whatever language runtimes, shells, and CLIs already
exist on the host are what the agent sees — the platform does not install
Git, a shell, Node.js, Python, or `uv` on the user's behalf the way the old
per-OS Dockerfile templates did. The **interactive shell** `ai shell` launches
is still a first-class per-project choice (`--shell`/the wizard,
`config.Workspace.Shell`, default `bash`, the alternative `zsh`) — nono simply
runs that shell binary as the sandboxed process; it does not install or
configure it.

## Agent CLIs — installed directly on the host

Three of the six supported agent CLIs can be installed directly onto the host
by the platform (`internal/setup/agentcli_host.go`, `SupportedHostAgentCLIs`),
each via its own official installer, Detect-gated (left untouched if already
on PATH) and strictly best-effort (a failed install is logged and swallowed —
it never fails `ai setup`):

| Agent CLI | Install command |
|---|---|
| claude-code | `curl -fsSL https://claude.ai/install.sh \| bash` |
| codex | `curl -fsSL https://chatgpt.com/codex/install.sh \| sh` |
| opencode | `curl -fsSL https://opencode.ai/install \| bash` |

`ai setup`'s `--host-agents <cli,...>` flag (or its TTY picker) selects which
of these three to install; `omp`, `gemini`, and `hermes` have no
platform-managed host installer — the user installs them separately (or they
are already present). Whichever CLIs a project selects at `ai create`
(`--agents`) are recorded in `config.yaml` regardless of whether they are
actually on the host yet — that is not re-validated until the first
`ai agent <cli>` launch, which fails with
an actionable error (exit code mapped from `workspace.ErrUnknownAgentCLI`/a
generic not-installed error) if the binary is missing.

## Per-project AI tools — selection exists; host-side installation does not (yet)

**Status note (post-nono-migration):** `ai create`'s AI-tools multi-select and
`ai tools list|enable|disable` are fully wired — they read/write the
`context.*_enabled` booleans in `config.yaml` via `create.SupportedAITools()` /
`create.SplitAITools()` / `config.ContextConfig.ToolEnabled()` — but **no code
in the current `internal/workspace` package actually installs or registers any
of these tools on the host**. The elaborate per-tool `register*` functions the
prior (Microsandbox) architecture had — `registerCaveman`, `registerGraphify`
(+ its `git init`/MCP-server/model-routing wiring), `registerCodeReviewGraph`,
`registerCodebaseMemory`, `registerOpenViking`, `registerTokenSave`,
`registerAgentMemory`, `registerPonytail`, and the MCP-injection mechanism
(`agentcfg.EnabledMCPServers`) that fed codex/hermes/omp — do not exist in the
current codebase (`EnabledMCPServers` itself is still defined in
`internal/agentcfg` but has **no caller** anywhere). So today:

* a user can select/toggle any of the nine tools below and the choice is
  durably recorded;
* nothing currently installs the tool, writes its skill/agent/command files,
  registers it as an MCP server, or wires Graphify's model routing
  (`agent.graphify_model` is likewise stored but unconsumed — `OmlxModelHandle`
  has no caller either).

This is tracked here as an explicit gap for this migration cycle, not
described as working. The nine tools, with their intended purpose
(`create.AIToolDescription`), are:

* **caveman** (default ON) — compresses agent output into a terser style to
  cut tokens (lite/full/ultra/wenyan levels, §9).
* **graphify** (default ON) — builds a knowledge graph of the codebase for
  faster, cheaper code navigation and queries; the wizard still offers a
  Graphify-model picker sourced from omlx's live model list (§16) whenever
  `graphify` is selected, storing the choice as `agent.graphify_model` (not
  currently consumed — see the status note above).
* **code-review-graph** (default ON) — code-review knowledge-graph MCP
  server; generates a visual D3 dependency graph.
* **codebase-memory-mcp** (default OFF) — persistent codebase-memory MCP
  server, with an optional 3D graph UI.
* **openviking** (default OFF) — local context database server for agents
  (openviking.ai).
* **tokensave** (default OFF) — code-graph MCP server for 34 languages —
  semantic queries instead of grepping/reading files, cutting token use.
* **agentmemory** (default OFF) — persistent cross-session memory — captures
  and recalls prior work so agents stop re-learning the codebase.
* **ponytail** (default OFF) — nags the agent to check
  YAGNI/stdlib/existing code before writing anything new.
* **agent-skills** (default OFF) — curated senior-engineer skills (spec,
  TDD, review, security, shipping, …) from github.com/addyosmani/agent-skills.
* **serena** (default OFF) — LSP-backed semantic code navigation/editing MCP
server, installed into the project Python environment (`.venv`).

(`create.DefaultAITools()` returns caveman + graphify + code-review-graph ON,
the other seven OFF — `create.SplitAITools` maps the multi-select onto the ten
`project.Spec` bools, which `Scaffold` writes straight into `config.yaml`.)

There is no `rtk`/Headroom-CLI/Caveman per-user install step anymore either —
those used to be baked into the OS base image; with no image, they are simply
not installed by the platform (a user who wants `rtk` installs it themselves
following its own `install.sh`).

## Agent CLIs (selected per project)

The AI coding-agent CLIs are **not** all baked in. One or more are chosen at
environment setup (`ai create`, CLI §3.1) from the supported list:

* **OpenCode** — the default; pre-selected and the default agent
* **omp** ("Oh My Pi", a Pi fork) — selectable; when chosen, wired to LiteLLM like OpenCode
* **Claude Code**
* **Codex**
* **Gemini CLI**
* **Hermes** — selectable; wired to LiteLLM (gateway/api-key) like OpenCode

Selection is **multi-select**: install any subset (at least one), with **OpenCode**
pre-selected by default. The chosen CLIs are written into the project's
`config.yaml` (`agent.tools`) at creation — this records which CLIs the
project is *wired* to use, independent of whether they happen to be
host-installed yet (see "Agent CLIs" above). The **default agent** — which
CLI `ai start`/`ai restart` launches — is recorded as `agent.default_tool`
(repo-layout §12.4) and must be one of the selected CLIs (default OpenCode).

**Per-agent auth mode** (`agent.auth_modes`, chosen at `ai create`): each gateway-capable
CLI records `api-key` (DEFAULT — route through the gateway with a scoped LiteLLM virtual
key, so the tool firewall + secret masking apply) or, for the three CLIs with a first-party
subscription login (**Claude Code, Codex, Gemini** — `config.OAuthCapableCLIs()`), `oauth`
(the CLI's own subscription login, talking DIRECTLY to the provider, **bypassing the gateway
and all guardrails**). OpenCode/omp/Hermes have no subscription and are ALWAYS
gateway/api-key. `config.ForcedOAuthCLIs()` is a generic mechanism for a forced-OAuth CLI
(gateway-incapable, authenticates natively to its own provider, never offered an auth-mode
choice) — it currently returns an empty set (no supported CLI needs it). `ai create` WARNS
that an oauth agent's traffic bypasses the firewall/masking/egress audit.

Context optimization (§8–10):

* **Headroom** — input compression via LiteLLM's `pre_call` `headroom` guardrail;
  per-project tuning rides in the opencode config's request body (§8, §10). A
  host-installed `headroom` CLI can additionally wrap claude-code/codex/opencode
  sessions directly (`headroom wrap <cli>`, §7) when present on PATH.
* **Caveman** — output compression, selectable at `ai create` and toggleable via
  `ai tools`; the actual host-side install step is not currently wired (§9
  status note).

MCP servers are configured and run by the agent itself (the platform does not
manage MCP).

---

# 13. Tool Provider Layer

Supported providers (selected per environment, §12):

* OpenCode — default
* omp ("Oh My Pi", a Pi fork)
* Claude Code
* Codex
* Gemini CLI
* Hermes

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

All model access flows through LiteLLM. On the full path the agent sends to the
nginx gateway, which routes `/v1` directly to LiteLLM; LiteLLM compresses the input
via its `pre_call` `headroom` guardrail (§10), applies any other enabled guardrails
(§15), and attaches the real provider key held in the gateway (§17). The full
per-component networking path is in §29.3.

```text
Agent CLI (sandboxed host process, nono)
 ╎  plain host address (loopback by default, §29.2)
nginx (aip-proxy, host :18787, /v1 → LiteLLM directly)
 ↓
LiteLLM
 ↓  (pre_call headroom guardrail → aip-headroom:8787/v1/compress; input compression)
 ↓  (enabled guardrails: secret-masking / hide-secrets / tool-firewall when selected, §15)
 ↓  (real provider key from LiteLLM's own store; agent holds only a virtual key)
Provider
```

---

## Supported Providers

Cloud providers are **not** a fixed, platform-maintained allow-list — they are
whichever [models.dev](https://models.dev) catalog providers map to a
LiteLLM-routable prefix (`internal/litellm.LiteLLMPrefix`; the mapping is
verbatim in most cases, with a small number of known exceptions — e.g. the
catalog's `google` provider routes under LiteLLM's `gemini` prefix). Commonly
used examples:

* OpenAI
* Anthropic
* Google Gemini
* Groq
* OpenRouter

plus the local backend:

* omlx (host-side, §16)

A provider becomes usable the moment its API key is added via `ai keys add`
(§17), which syncs that provider's catalog models into LiteLLM's DB
(`litellm.SyncModels`).

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
that provider's catalog models into the DB via `litellm.SyncModels`; the matching
local (omlx) registration/unregistration is a separate, automatic sync (below,
§16) — not a user-invoked pull/rm. The catalog id is the public `model_name` verbatim; the catalog-id→LiteLLM-prefix map
(e.g. `google` → `gemini`) supplies each model's routing prefix. In an agentic
workflow the agent names a model on each request and LiteLLM routes it to the
provider (attaching its own stored key for cloud, none for a local backend). A
model is usable when it is (a) registered in the DB and (b) actually available: a
local model must already be **served by omlx**; a cloud model needs its provider
key present in the gateway (§17).

**The local-inference backend** (omlx, §16) feeds the DB-backed store:

* **omlx** (host-side, single shared server) → public handle
  `omlx/<id>` (`<id>` exactly as omlx's own live `GET /v1/models` reports it — no
  alias derivation), routed to `openai/<id>` with `api_base` set to
  the one shared omlx endpoint (no credential).

omlx models are managed entirely through **omlx's own admin panel**
(`ai services console omlx`) — there is no host-side `ai models pull`/`rm`
equivalent. The platform's only job is to keep LiteLLM's `omlx/*` registrations
in sync with omlx's live model list (`litellm.SyncOmlxModels`; when this runs and
how it reconciles is §16, Registration). A cloud-key catalog resync
(`litellm.SyncModels`) shields `omlx/*` handles from its delete pass
(`localModelPrefixes` = `["omlx/"]`).

There is no default model: an unqualified request is the agent's responsibility.
The local backend (omlx, needing no credential) is available for whichever
models omlx itself is currently serving.

---

# 15. LiteLLM

Runs on the host as a **thin shared gateway**.

Purpose:

* unified provider endpoint for all tools (and omlx)
* single point that holds the real provider keys (keys-in-LiteLLM, §17)
* the DB-backed served-model catalogue (synced from models.dev by `ai keys`, §14)
* failover
* monitoring

It does not make model-selection decisions on the agent's behalf.

LiteLLM runs as container `aip-litellm` (image `ghcr.io/berriai/litellm`, tag
`latest`, which satisfies the `headroom` guardrail's LiteLLM v1.92.x+ requirement
— `:4000`) on the `aip-net` network — **INTERNAL-ONLY** (no host
publish; reached by name `aip-litellm:4000` by nginx's model path + `/llm` route +
`litellm.<domain>` vhost; it in turn calls Headroom at `aip-headroom:8787/v1/compress`).
Its DB-backed admin UI / virtual keys require
PostgreSQL: container `aip-litellm-db` (image `postgres:18.4-alpine3.23`, the data
dir **HOST-BIND-MOUNTED** from `~/.ai-platform/volumes/litellm-db` at
`/var/lib/postgresql` — NOT a Docker named volume — `trust` auth on the private
network, INTERNAL-ONLY with no host port). This Postgres is the one stateful
piece of the service tier. It is reconciled by `ensureLiteLLMDB` as part of litellm
and is surfaced as its **own display-only `postgres` status line** in `ai services` /
the TUI (positioned immediately after the litellm line): State `running` when the
container is up else `stopped`, Mode `container`, no host endpoint (internal-only,
no host port). It is managed WITH litellm — there is no separate
start/stop/restart verb (`ai services <action> litellm-db` = "unknown service").

**All host-persisted SYSTEM data volumes live under `~/.ai-platform/volumes/<name>`**
— a single discoverable home, so `ai uninstall --purge` (which `RemoveAll`s
`~/.ai-platform`) removes them all (no scattered Docker named volumes). Today:
`volumes/litellm-db` (the Postgres data dir above) and `volumes/models` (the omlx
model store, under `volumes/models/omlx`) — `volumes/` is ONLY true host data.
**Re-fetchable caches live under
`~/.ai-platform/cache/<name>` instead** (`paths.CacheDir`): `cache/catalog.yaml` (the
models.dev catalog, fetched as JSON but persisted as YAML; a legacy `catalog.json` or
the older `volumes/catalog.json` is one-shot converted).
This is a re-downloadable copy, not SYSTEM data, and is likewise removed by
`ai uninstall --purge`. Config files (the LiteLLM/DNS/nginx configs under `config/`)
are NOT system volumes and stay where they are. (There is no per-project
workspace overlay anymore to carve out an exception for — §26 history.)
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

**Persisting the secrets.** Two paths write **`~/.ai-platform/.ai-platform.env`**
— an **0600** file of `export KEY='VALUE'` lines that the `ai` CLI **auto-loads at
startup** (into its own process env, where the container env-passthrough launches
pick them up) — so secrets persist across restarts WITHOUT the user editing their
shell rc. Precedence is "existing env wins": the file only fills gaps, so a value
already exported in the shell is never clobbered.

* **Unconditional (infra keys):** every `ensureLiteLLM` reconcile persists the
  `LITELLM_MASTER_KEY` + `LITELLM_SALT_KEY` pair (`persistLiteLLMInfraKeys`), so
  they survive a full container-down + fresh-process relaunch (preserving them off
  the running container only works while it is still inspectable). This is a
  correctness requirement — a rotated **salt key** orphans every stored provider
  credential, and a stable **master key** keeps workspace scoped-key minting
  working across recreates — so it is NOT opt-in.
* **Opt-in (UI password):** `ai setup` (server) and `ai litellm password` OFFER
  (on a TTY) to additionally save `UI_PASSWORD`; on decline / non-TTY the manual
  `export …` block is printed instead.

The container
also
carries `DATABASE_URL` (inline; it carries no secret) and the Presidio endpoints
`PRESIDIO_ANALYZER_API_BASE=http://aip-presidio-analyzer:3000` /
`PRESIDIO_ANONYMIZER_API_BASE=http://aip-presidio-anonymizer:3000`.

---

## Guardrails (user-selectable; enabled ones render `default_on`)

Guardrails are **user-selectable**, not blanket "always-on". `ai setup` presents a
multi-select guardrail **picker** (shown on a TTY; under `--json`/no-TTY the
`--guardrails` flag drives it), and the choice is persisted machine-wide in
`runtime.yaml` under `guardrails:` (`runtime.Info.Guardrails`; nil = the default
Headroom-only set; a non-nil set — including an explicit empty one — is honoured
verbatim). The catalog (`litellm.Guardrails`) is:

* **`headroom`** — "Input compression (Headroom)" — **DEFAULT ON** (the only
  default-on guardrail out of the box).
* **`secret-masking`** — "Secret masking (Presidio)" — opt-in; expands to the
  Presidio input+output pair below.
* **`hide-secrets`** — "API-key / token detection (detect-secrets)" — opt-in.
* **`tool-firewall`** — "Destructive-command tool firewall" — opt-in (below).

`litellm.DefaultGuardrails()` = Headroom only; `litellm.GuardrailKeys()` = all four.
The `--guardrails` flag takes a comma-separated subset (or the literal `none` to
disable all) and seeds the checkbox on a TTY; a bad name is exit 2.
`setup.ResolveGuardrails(options, persisted)` sets precedence: this run's
picker/flag > persisted `runtime.yaml` > `litellm.DefaultGuardrails()`
(`reconcileGuardrails()` reads `runtime.yaml` with the same fallback).

`litellm.buildGuardrails(enabled)` renders **only the enabled guardrails** into the
config's top-level `guardrails:` block — an unselected one is **omitted entirely**
(so the config never references a backend that isn't running); a nil/empty set
renders no guardrails. Each **rendered** guardrail is `default_on: true`, so a client
**cannot opt out of an ENABLED guardrail**; because **every route — including cloud
providers — traverses the LiteLLM proxy**, cloud calls can't bypass an enabled
guardrail either. Headroom is the only default-on guardrail; the security guardrails
(secret-masking, hide-secrets, tool-firewall) are opt-in.

The masking is deliberately scoped to **secrets and credentials, NOT general PII**: a
coding agent's prompts legitimately contain names, places, paths and emails, and
masking those corrupts the prompt before the model sees it (e.g. "capital of France"
→ "capital of `<LOCATION>`"), so general PII masking was removed. (An in-process
prompt-injection callback was removed — see below.) When **`secret-masking`** is
selected, Presidio runs as two internal-only host containers (not published to the
host) — `aip-presidio-analyzer` and `aip-presidio-anonymizer`, both listening on
`:3000` internally — which LiteLLM reaches via the `PRESIDIO_*_API_BASE` env above;
when it is not selected those containers are not reconciled (§below).

* **`headroom-compression`** — `guardrail: headroom`, `mode: pre_call`,
  `api_base: http://aip-headroom:8787` — POSTs the request messages to
  `/v1/compress` and swaps in the compressed input before dispatch (§10). Default-on.
* **`presidio-secrets-input`** — `guardrail: presidio`, `mode: pre_call`,
  `presidio_filter_scope: input` — masks financial/identity **secrets**
  (CREDIT_CARD, US_SSN, US_BANK_NUMBER, IBAN_CODE, CRYPTO; `secretEntities`; action
  MASK; score threshold default 0.6) out of the prompt before the model sees it.
* **`presidio-secrets-output`** — `guardrail: presidio`, `mode: post_call`,
  `presidio_filter_scope: output` — masks the same secret entities out of the
  response.
* **`hide-secrets`** — `guardrail: hide-secrets`, `mode: pre_call` — LiteLLM's
  in-process secret detector (bundled detect-secrets) strips API keys/tokens/
  credentials from the prompt. No external server.

The example below shows a config in which the user enabled every guardrail; a set
with fewer selected simply omits the unselected entries (Headroom alone is the
out-of-the-box default). List order below matches the actual render order from
`litellm.buildGuardrails` — secret-masking pair, then hide-secrets, then
tool-firewall, then headroom-compression **last** (the order is an append sequence,
not alphabetical or catalog order):

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
        - { id: deny-destructive-command-array, tool_name: "<shell-tool regex>", decision: deny,
            allowed_param_patterns: { command[]: "<destructive-command regex>" } }
        - { id: deny-destructive-cmd, tool_name: "<shell-tool regex>", decision: deny,
            allowed_param_patterns: { cmd: "<destructive-command regex>" } }
  - guardrail_name: headroom-compression
    litellm_params: { guardrail: headroom, mode: pre_call, api_base: http://aip-headroom:8787, default_on: true }
```

### Presidio containers gated on `secret-masking`

The Presidio containers (`aip-presidio-analyzer`, `aip-presidio-anonymizer`) are
pulled AND started **only when the `secret-masking` guardrail is selected**.
`presidioSelected(guardrails)` gates it across `requiredImages` / `PullImages` /
`UpdateImages` (presidio images are not pulled when off), the reconcile's
`ensurePresidio`, and the Control "all" path — while an explicit
`ai services start presidio` STILL forces it (bypassing the gate). Presidio remains
a **core** service (it is now merely conditionally reconciled), and `ai services
status` reports it **`disabled`** when `secret-masking` is off (listed but not
probed, so it stays discoverable).

### Tool firewall (destructive command tool-calls)

For sandboxed coding agents the bigger risk is destructive **tool execution**, not
prompt content — `git push --force`, `git reset --hard`, `terraform destroy`,
`kubectl delete`, `rm -rf` matter far more than "ignore previous instructions". The
`tool-firewall` guardrail (`guardrail: tool_permission`, `mode: post_call`,
in-process — no external service) inspects the model's **tool-calls** at the gateway
and **denies** ones whose shell command matches a destructive pattern
(`destructiveCommandPatterns` in `internal/litellm`): default-allow, with one deny
rule per tool-call argument path that can carry a shell command — `command`,
`command[]`, `cmd` (`commandParamPaths`) — each matching a shell-tool-name regex
against that path; `arguments.command` is deliberately NOT used, since LiteLLM's
guardrail evaluates the parsed arguments object and a nested `arguments.` prefix
matches nothing. `on_disallowed_action: block` rejects the response. Because every agent
(opencode/claude-code) routes model calls through LiteLLM, this is tool-agnostic.

It is **defence-in-depth**, not the only control: it catches the model's tool-calls
(the agent path), but nono's same-kernel sandboxing (§3, §7) + the per-project
egress policy (§29.6 — which a project can re-lock to `deny` to block
network-dependent destructive commands like push/terraform/kubectl) remain the
other layers — note nono's isolation is capability confinement, not a VM
boundary (§3), so this is a weaker backstop than the platform's former
Microsandbox design for anything the sandbox profile's filesystem/network
grants don't themselves prevent. The exact shell-tool name/param
paths per agent CLI are a `hardware bring-up` verification item
(`docs/HARDWARE-BRINGUP.md`).

### Prompt-injection scanning — REMOVED

The platform does **not** scan prompt content for injection. The in-process
`detect_prompt_injection` callback was removed: it is a crude local heuristic
(similarity to known attack strings) that **false-positives on ordinary coding
traffic** — including normal local-model requests, which it rejected with
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
`litellm.SyncModels`; removing it unregisters them; the platform's automatic
omlx sync (§16) registers/unregisters the matching `omlx/<id>`. Registering a
cloud model does **not** install it — a cloud model still
needs its provider key present in the gateway (§17); an omlx model is already
"installed" the moment omlx itself is serving it (model download/management
lives entirely in omlx's own admin panel, §16). The catalog id is the public
`model_name` verbatim; the catalog-id→LiteLLM-prefix map (e.g. `google` → `gemini`)
supplies the routing prefix. There is no default model.

Local (omlx) models are registered with the public `model_name` `omlx/<id>` and
`litellm_params.model` = `openai/<id>` (`OmlxRoutedModel`), against the single
shared `omlx serve` endpoint. They are owned by omlx's own admin panel; the
authentication (real key vs. placeholder) and the sync mechanics (the same
pure add/delete diff as the cloud-key sync, not a full rebuild) are covered in
§16 (Registration).

### Per-CLI agent provider config — keyless host-path configs, key delivered by nono

All **six** supported agent CLIs (§12) route through the gateway **by default** — subject
to per-agent auth mode: an `oauth`-mode Claude
Code/Codex/Gemini goes direct to its provider and gets no gateway config (see auth modes
above). Each CLI's provider
config is written at **that CLI's own default per-project location**, now directly on
the **real host filesystem** (there is no guest path to translate — nono sandboxes the
host process against the project directory in place, §7), (re)generated on **every
`ai agent <cli>`/`ai shell` launch** by `Manager.writeAgentConfig`
(`internal/workspace/agentconfig.go`, over `internal/agentcfg`).
Every on-disk config is **KEYLESS** — the scoped virtual key is referenced by env
interpolation, never written to disk — and opencode's/claude-code's existing files are
**deep/JSON-merged** so the dynamic managed block wins while the user's other keys survive:

* **opencode** → `<project>/.opencode/opencode.json` (`apiKey: "{env:AIP_GATEWAY_KEY}"`);
  it carries the per-request Headroom knobs (`headroom_keep_turns` /
  `headroom_output_buffer_tokens`) on every model, and the served-model picker (below).
* **claude-code** → `<project>/.claude/settings.json` — an `env` block setting only
  `ANTHROPIC_BASE_URL` (the gateway root, no `/v1`); the bearer token is injected
  directly into the sandboxed process's environment by nono (settings.json has no
  `${VAR}` interpolation), so the file is keyless.
* **codex** → `<project>/.codex/config.toml` — a keyless `[model_providers.aip-gateway]`
  block with `wire_api = "responses"` and `env_key = "AIP_GATEWAY_KEY"`, plus a **global**
  (shared across every project on the host — no VM isolation to separate them anymore)
  `~/.codex/config.toml` trust-entry APPEND (`appendCodexTrust`, idempotent — it never
  rewrites the whole file, since that would destroy other projects' trust entries):
  `[projects."<project-root>"] trust_level = "trusted"` so codex loads the keyless
  project config.
* **gemini** → **env-only** (no settings key for a base URL exists): `GOOGLE_GEMINI_BASE_URL`
  is set via the launch's nono profile (`environment.set_vars`, §7) and `GEMINI_API_KEY` is
  injected by nono's credential map — there is no gemini config file at all.
* **omp** → global `~/.omp/agent/models.yml` + `<project>/.omp/config.yml`.
* **hermes** → global `~/.hermes/config.yaml`.

The **scoped virtual key is NEVER on host disk**. It lives ONLY in the host OS keystore
(macOS Keychain / Linux Secret Service, §7) and is injected directly into the sandboxed
process's environment by `nono run --env-credential-map` at launch — `AIP_GATEWAY_KEY`
(opencode/codex/omp/hermes), `ANTHROPIC_AUTH_TOKEN` (claude-code), `GEMINI_API_KEY`
(gemini). There is no shell-sourced env file anymore (the old in-VM
`~/.config/aip/agent-env.sh` is gone with the VM); nono's own credential-injection flag
is the direct replacement.

**Not currently wired** (tracked as a gap, not a hardware-bring-up item — this is a
missing feature, not an unverified-on-hardware one): Graphify's `agent.graphify_model`
routing through `OPENAI_BASE_URL`/`OPENAI_API_KEY`/`OPENAI_MODEL=omlx/<model>` env vars,
and the shared `<project>/.ai-platform/{agents,skills,prompts,projects}` resource pool +
its per-CLI symlinks into `.opencode/skills`, `.claude/skills`, etc. — neither has a
caller in the current `internal/workspace` package (§9, §12, §18).

### Served-model picker

For discoverability, opencode's config is seeded with a **picker** built at every
launch (`Manager.pickerModels`, `internal/workspace/agentconfig.go`): it is **exactly
the set of models the LiteLLM gateway currently serves** — its live DB-backed models —
sourced via the injected `Manager.Served` → `litellm.KeyManager.ListModels`, deduped
with empty names dropped. If the gateway is unreachable the picker **degrades to an
empty list** (never fatal — it must never fail a launch), and `MergeOpenCodeConfig`
only REPLACES the `provider.<id>.models` map when the fetched list is non-empty —
an empty (outage) list falls back to the ordinary deep-merge, so the existing on-disk
list survives a transient outage, same guarantee as before the migration. There is
**no "default model" seeding anymore at all** — `writeOpenCodeConfig` always calls
`MergeOpenCodeConfig` with an empty `defaultModel`, which actively drops any
previously-seeded top-level `model` key, so opencode's own persisted last-used
selection is what always wins; the old seed-then-remember policy (seeding
`agent.graphify_model` as the default on a first start) has no equivalent in the
current code, since `agent.graphify_model` is not consumed anywhere (see the status
note above). There is also no `refresh-models` command anymore — simplified rather
than lost, since a plain host install means opencode's own state directory
(`~/.local/share/opencode`) is already permanent host-side state with no
VM-restart/overlay-symlink concern, and every `ai agent opencode` launch already
regenerates the config fresh from whatever LiteLLM reports at that moment, so there
is nothing a separate in-session refresh command would add. There is no host-side
"browse available local models" surface — no curated in-code list, no TUI Local
Models tab. Adding/removing/tuning a local model is done entirely through **omlx's
own admin panel** (`ai services console omlx`, §16); the platform only keeps the
gateway's `omlx/*` registrations synced to whatever omlx is currently serving, which
is what feeds this picker.

---

# 16. Local Inference (omlx)

**omlx** (https://github.com/jundot/omlx) is the platform's **sole** local-inference
backend. It runs **host-side** (a host process, **NOT** a Docker container): there is
no local-inference container in the reconcile. LiteLLM routes local model traffic to
it across the host boundary. omlx ships prebuilt wheels for **macOS + Apple Silicon
only** — unlike the per-model vLLM design it replaced (which also served Linux/CUDA),
there is no Linux build — so local inference is a macOS-only feature of an otherwise
host-agnostic platform (§3, §6.2 still apply to sandboxing + the service tier).

## Serving model

omlx runs **ONE shared host-side process** for every locally-served model —
`omlx serve --model-dir <dir> --port 8100` (port fixed at `services.OmlxPort`, no
per-model port allocation) — a single OpenAI-compatible endpoint on the host loopback
that scans its own model directory (`~/.ai-platform/volumes/models/omlx`) and serves
whatever it finds there. This replaces the old vLLM design's one-process-per-model
approach: there is no per-model port, no max-concurrent cap, no LRU eviction, and no
per-model resource-cap (`--gpu-memory-utilization`/`--max-model-len`) recording on the
platform side anymore — model tuning is now entirely omlx's own concern. The platform
probes it at `http://127.0.0.1:8100/v1/models`, and the LiteLLM/nginx containers reach
it at `http://host.docker.internal:8100/v1` (launched with
`--add-host=host.docker.internal:host-gateway`, harmless on Docker Desktop, required
on Linux).

In `Reconcile`, omlx is ensured **non-fatally** — an unreachable omlx is a hint, not a
setup failure. Installing it is **wired**: `omlx.Install` resolves the **latest**
`github.com/jundot/omlx` GitHub release, picks the **newest-Python-version** prebuilt
`.whl` asset among the release (one universal2 wheel per Python version — no
core+plugin pairing dance, unlike the old vLLM-Metal install), pins the platform venv
to that exact Python version (`pyenv.EnsureVersion`, recreating a mismatched venv), and
pip-installs it; `ai setup` best-effort auto-installs omlx into the platform-managed
venv (`~/.ai-platform/venv`) the same way when absent, and a manual
`ai services start omlx` retries the same install. `RealRunner.Start`/`Stop`
(`internal/omlx/runner.go`) spawn/stop the real, detached `omlx serve` process (its own
process group, log redirected to a sibling log file); `Stop` is an unqualified
`pkill -f "omlx serve"` since there is exactly one such process. A **cross-process
flock** (`internal/omlx/lock.go`, on a lock file sibling to the model store) serializes
`EnsureServed` across separate `ai` invocations — `ai` is daemonless, so without it two
racing invocations could each see the port free (the server's own FastAPI/MLX startup
takes real time before it actually binds the socket) and both spawn `omlx serve`,
crashing the second with "address already in use" (the same daemonless race the old
vLLM per-model design also had to guard against). What remains is the live
network install/wheel-resolution and a real serve + gateway round-trip on provisioned
hardware — a **hardware-bring-up** verification item (`docs/HARDWARE-BRINGUP.md` §2.9).

## Model management (omlx's own admin panel)

Downloading, adding, removing, and tuning models is **not a platform concern**: it
lives **entirely inside omlx's own admin panel**, reached via **`ai services console
omlx`** (opens `http://127.0.0.1:8100/admin`). There is no `ai models
pull`/`rm`/`configure`/`enable`/`disable`/`list`/`popular`/`install-vllm` equivalent —
that whole CLI surface, the per-model `~/.ai-platform/config/model-runtimes.yaml`
runtime-choice store, and the curated Hugging-Face-repo picker were all retired along
with the per-model vLLM design. `ai models` now has three, read-only,
gateway-inspection subcommands: `status` (LiteLLM health/providers/routing plus omlx
connectivity), `test [model]` (probe a served model through the gateway), and
`refresh` (re-sync — the same pure add/delete diff as the automatic sync — LiteLLM's
`omlx/*` registrations against omlx's live model list on demand — e.g. right after
adding/removing a model through the
admin panel, without a full `ai services restart omlx`; TUI mirror: the `m` key in the
omlx Service Detail pane, only shown/offered for that service).

## Registration

omlx models register automatically in LiteLLM's DB-backed store (§14): the platform
syncs LiteLLM's `omlx/*` registrations against omlx's own live `GET /v1/models`
response (`litellm.SyncOmlxModels`/`DesiredOmlxModels`) using the SAME pure
add/delete `Reconcile`/`ApplyPlan` diff as the cloud-key sync — a model already
registered under an unchanged name is left ALONE, so any settings a user
hand-edited on it in LiteLLM's admin UI (rate limits, tags, custom pricing, ...)
survive a refresh untouched; only a model omlx newly reports gets added, and only
one that disappeared from omlx's list gets deleted — this sync runs automatically at
`ai setup` and at `ai services start|restart omlx`, and on demand via
`ai models refresh` (or the TUI's `m` key); there is no other user-facing sync
command. The public handle is
`omlx/<id>` (`litellm.OmlxModelName`) where `<id>` is **exactly** what omlx's own
`/v1/models` reports — no alias derivation, unlike the old vLLM design's
Hugging-Face-repo-id-derived alias — routed to `openai/<id>` (`OmlxRoutedModel`) with
`api_base` pointing at the **single shared** omlx endpoint, authenticated with the REAL
key when the user has configured one on omlx's OWN side (`internal/omlx.APIKey` — env
`OMLX_API_KEY` else omlx's own `~/.omlx/settings.json` `auth.api_key`; omlx validates
no key by default, so an empty result is the common case) else the non-empty
placeholder credential `"EMPTY"` (LiteLLM's `openai/` provider requires a non-empty key
to construct its client regardless). The same `internal/omlx.APIKey` also
authenticates the platform's own probes (`ListModels`/`DefaultProbe`/`omlxHTTPGet`) as
a Bearer `Authorization` header, so a key-protected server still reports healthy.
**Every omlx model
shares the same `api_base`** — a structural simplification versus the old per-model
vLLM `api_base`. A cloud-key catalog resync (`litellm.SyncModels`) shields `omlx/*`
handles from its delete pass (`localModelPrefixes` = `["omlx/"]`); local models are
owned only by omlx's own admin panel plus the automatic sync above.

`ai services` / `ai doctor` show omlx with Mode **"host"** (state from an HTTP health
probe against `GET /v1/models`, no container to `inspect`/`stats`) and
`Optional: true` (a down omlx is a warning, not a hard failure). It is
start/stop/restart-controllable as a host process. There is no per-model list in
`ai services status`/the TUI Services detail pane anymore — just one running/stopped
summary line — and its Logs sub-tab reads omlx's own captured log file directly (one
process, so no per-model concatenation is needed).

All access occurs through LiteLLM; LiteLLM routes local model calls to omlx via
`host.docker.internal` (no credential needed for local) and applies
whichever guardrails are enabled (§15) on these requests as on any other. Live
end-to-end routing to the host backend is a hardware-bring-up verification item.
Workspace egress to all of this is governed by the project's nono egress policy
(§29.4).

---

# 17. Secrets Layer

Provider credentials live **in the LiteLLM gateway** — never on platform disk and
never in the sandboxed agent process. This is the **keys-in-LiteLLM** model:

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
* **egress enforcement** — the per-project **nono egress policy** (default
  posture `public`: open internet allowed, private ranges blocked; re-lockable
  to `deny`), configured by `ai network` (§29.4, §29.6); there is **no egress
  proxy**;
* **secret masking / audit** — LiteLLM's enabled guardrails (§15), which run on
  every request and every route (each enabled guardrail can't be opted out of).

---

## The sandboxed agent process holds only a virtual key

The agent CLI, sandboxed by nono, never holds a provider secret on disk or in
a plain env var it could leak. It is given a scoped **LiteLLM virtual key**
(the gateway key), injected directly into its process environment by nono's
own credential-injection mechanism at launch (`--env-credential-map`, §7),
and sends all model calls to the gateway path (nginx :18787 → LiteLLM
directly, §29). LiteLLM authenticates the virtual key, then uses the *real*
provider key from its own store to reach the upstream provider:

```text
Sandboxed agent process (nono)  (holds only the LiteLLM virtual key, injected by nono)
 ↓  plain host address → nginx (:18787, /v1) → LiteLLM (:4000, calls Headroom :8787/v1/compress)
LiteLLM  (authenticates the virtual key; attaches the real provider key)
 ↓
Provider (cloud) / omlx (local, no key)
```

Because the real key lives only in the gateway, a compromise of the sandboxed
agent process cannot exfiltrate a provider secret — at worst it can spend
against the scoped virtual key, which the gateway can revoke or rate-limit
independently. (Unlike the old microVM boundary, this is a same-kernel
sandbox — see §3, Isolation — so the relevant containment is the
capability grant nono's profile makes, not a hardware wall.)

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

Real provider keys never reach the sandboxed agent process's filesystem — they
live only in the LiteLLM gateway, and the sandboxed process holds only the scoped
virtual key, injected into its environment by nono at launch.

---

## Egress and audit (cross-references)

The wire-level controls that used to be bundled with the secret store are now
their own mechanisms:

* **egress enforcement** is the per-project nono profile's network policy,
  declared via `ai network` — the default outbound posture (`public` by
  default: open internet allowed, private ranges blocked; re-lockable to `deny`)
  plus an allow/deny domain list (§29.4, §29.6). The policy is rendered into the
  profile's `network` block (`internal/workspace.ensureProfile`) and regenerated
  on every `ai agent`/`ai shell` launch — there are no published ports to declare
  anymore (nono has no guest-network boundary to publish a port from).
* **secret masking** is LiteLLM's enabled guardrails (§15): because each
  rendered guardrail is `default_on: true` and **every** route — cloud included —
  traverses the LiteLLM proxy, nothing bypasses an ENABLED guardrail (secret masking
  is opt-in, so it applies only when `secret-masking` is selected). A platform
  audit log that would record model-configuration/secret-access events (§31) is
  deferred — not yet implemented.

---

## Supported Secret Types

* provider API keys (OpenAI, Anthropic, Gemini, Groq, …)
* the LiteLLM master key and admin-UI password (§15)
* LiteLLM virtual keys issued to project workspaces

Credentials a tool inside the sandboxed agent process needs that are **not**
model-provider keys (e.g. a git token or an MCP server's credential) are the
user's / agent's own concern — the platform manages only the model-path
credentials described here, and confines all other egress with nono's network
policy (§29.4).

---

# 18. Shared Resources

The platform reserves global, host-managed shared-resource directories under:

```text
~/.ai-platform/
```

Including:

```text
agents/
skills/
prompts/
templates/
```

**Status: deferred.** There is no platform-wide skill/agent/prompt pool shared
across projects today, global or per-project. The global directories above are
pre-created empty by `internal/layout.Ensure` but nothing populates or mounts
them. The **per-project** shared pool,
`<project>/.ai-platform/{agents,skills,prompts,projects}`, that the prior
(Microsandbox) architecture symlinked into each installed agent CLI's own
directories at workspace start is likewise **not currently wired** — the
`internal/workspace` package has no symlink-setup step anymore (§9, §12, §15
status notes). Both the global and the per-project resource-pool layers are
open items for this migration cycle, not working features.

---

# 19. Project Architecture

Host location (the project directory — the cwd at `ai create`, not a fixed root):

```text
<project dir>/.ai-platform/
```

There is no separate guest mount location anymore — nono sandboxes the agent
process directly against this same host directory (§7); a nono-sandboxed
process's working directory IS the project directory, with no translation.

---

## Generated Structure

```text
.ai-platform/
  config.yaml        # tracked
  project.yaml       # tracked — { name, os, created }
  nono-profile-<cli>.json / nono-profile-shell.json  # gitignored — regenerated every launch (§7)
  # shared resource pool directories exist in the on-disk layout but nothing
  # populates or symlinks them currently (§9, §12, §18 status notes):
  agents/
  skills/
  prompts/
  projects/          #   reserved
  .gitignore         # ignores run/
  run/               # gitignored — workspace/agent runtime state

# per-CLI provider configs (keyless, (re)written on every `ai agent`/`ai shell` launch; §15):
.opencode/opencode.json
.claude/settings.json
.codex/config.toml

docs/

scripts/

src/

tests/

README.md
```

`<project>/.ai-platform/` holds the project's config and gitignored runtime
state — no platform-managed memory (delegated to the agent, §11), and no
Dockerfile or profile.yaml anymore (§24, §25 history). The per-CLI provider
configs live at each CLI's own default location under the project root (all
keyless; the scoped key is delivered only via nono's credential injection,
§7/§15/§17). MCP is configured by the agent, not the platform. This matches
the canonical project layout in `03-repository-layout.md` §2.1 (which may
still describe the pre-migration Dockerfile/profile.yaml layout — verify
against `03-repository-layout.md` directly if precise cross-reference
accuracy matters, since updating that file is outside this task's scope).

---

## Identifier Rules

Project names become directory names and the sandbox-facing name (§7),
so they are validated:

```text
project name:  ^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$
```

* lowercase alphanumeric and hyphens; 2–40 chars; no leading/trailing hyphen
* a name that fails validation exits `2`
* project names are unique across the platform
* the derived sandbox-facing name is `aip-<project>` (§7) — used as the
  `nono run --name` prefix, not an OCI image or microVM name

---

# 20. Multi-Agent System — Removed (not a platform concern)

The platform provides **one workspace per project**. Running multiple AI agents
on a project — and any source isolation that needs (git branches, worktrees,
separate checkouts) — is the **in-workspace agent CLI's** job, not the platform's
(consistent with §11: the platform doesn't manage the agent's concerns). The
platform never creates per-agent workspaces, branches, or worktrees.

---

# 21. Git Workflow

Version control is **out of scope** — there is no git package and `ai create`
runs no git. `ai create` writes only the `.ai-platform/` config into the
current directory and leaves existing files (including an existing repo)
untouched; it does not clone, branch, commit, or merge. The prior
(Microsandbox) architecture ran one internal `git init` at every workspace
start (so the Graphify git hook always had a repo to attach to) — that step
has **no equivalent in the current code** (`internal/workspace` has no git
invocation at all post-migration; `grep -rn "git init"` over `internal/`
returns nothing). Everything git — init, clone, branches, commits, rebases,
merges, conflict resolution, worktrees, remotes, and pull requests — is now
entirely the user's and the agent CLI's job, with no platform involvement of
any kind.

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

Default — one nono-sandboxed host process per agent-CLI/shell invocation:

```text
Project directory (host)
 └─ nono session (Landlock on Linux / Seatbelt on macOS)
     └─ sandboxed agent-CLI or shell process
```

Each launch is its own sandboxed process, confined to the project directory by
a generated nono profile (§7) — not hardware isolation, same-kernel capability
confinement. There is no per-workspace overlay or disk image to keep separate
(§26 history): the project directory on the host IS the persistent state, so
"isolated and persisting independently" now just means "is a separate host
directory," not "is a separate VM disk." The container runtime (Docker/Podman)
is used only for the service tier (§6.1), never for a workspace.

---

# 25. Project Environment (Dockerfile) — Removed

**This whole section is obsolete and describes a removed feature.** The prior
(Microsandbox) architecture gave every project a `.ai-platform/Dockerfile`,
seeded from a per-OS template (`alma`/`debian-trixie`/`ubuntu`) the user chose
at `ai create`, extended with selected agent CLIs and a multi-select of
software stacks (`go`/`rust`/`java`/`maven`/`deno`), and built into the
workspace's OCI image. nono sandboxes the **host process directly** — there is
no image to build, so there is no Dockerfile, no OS choice, no `--os` flag, and
no software-stacks selection at all anymore:

* `internal/envimage` (the OCI image builder) — **deleted**, entirely unused.
* `internal/templates` (the OS/Dockerfile-snippet composer) — **deleted**.
* The `--os`/`--stacks` create flags and wizard steps — **removed**; `ai
  create`'s current flag/wizard surface (§12, CLI §3.1) has no OS or stack
  input at all.
* A workspace now runs on **whatever toolchains already exist on the host** —
  the platform does not provision per-project language runtimes. If a project
  needs `go`/`rust`/`java`/etc., the user installs it on the host themselves
  (or it's already there); there is no per-project override.

`config.Config.OS` and `state.Project.OS` fields still exist and are still
populated — but now with the **host's own `runtime.GOOS`** (`darwin`/`linux`,
set by `project.Scaffold`), an informational record of what platform the
project was created on, not a selectable Dockerfile template choice.

---

# 26. Overlay System (Persistence) — Removed

**This whole section is obsolete.** It described `internal/overlay`, the
host-bind-mounted VM-disk overlay (`~/.ai-platform/overlays/<workspace-id>` →
microVM `/persist`) that made a disposable microVM root persist installed
programs and agent state across restarts. `internal/overlay` is **deleted** —
there is no VM disk to overlay anymore.

Persistence is now simply: **the project directory, on the host, is the only
persistent state a workspace has.** A nono-sandboxed process runs directly
against that directory with no copy-on-write layer in between — whatever it
writes inside the project directory is immediately and permanently there, the
same as any other host process. There is nothing analogous to the old
"installed programs survive restart via the overlay" claim, because there is
no separate VM filesystem for installs to land in outside the project
directory in the first place — a `apt`/`npm i -g`/etc. run by the sandboxed
process affects the **host's own** installed-program state (scoped by
whatever nono's profile permits it to write), not a disposable VM root.
Likewise, an in-workspace agent's own state (session, config, `AGENTS.md`,
history) persists exactly where that agent CLI already keeps it on a normal
host install — there is no platform-managed persistence layer to maintain.

`~/.ai-platform/overlays/` still exists as a directory on disk
(`internal/layout.Ensure` still creates it) but is vestigial — no current code
writes to or reads from it.

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
is no separate workspace layer — a workspace sandboxes the project directory
directly, so the project config *is* the workspace config — and no
snapshot-defaults/environment-image layer at all anymore, §25 history.)

---

# 28. Resource Limits — Removed

**This whole section is obsolete.** It described per-workspace `cpu_limit`/
`memory_limit`/`disk_limit` config fields and `ai resize`, which sized and
later adjusted a microVM's vCPU count, memory ceiling, and writable-rootfs
disk size (capped against host RAM/CPU via `config.UsableHostMemoryMiB`, etc.).
None of that applies to a nono-sandboxed host process:

* `config.WorkspaceConfig` no longer has `CPULimit`/`MemoryLimit`/`DiskLimit`
  fields — it carries only `Shell` (§7, §12).
* **`ai resize` is deleted entirely.** There is no VM to size — a sandboxed
  process uses host resources directly, the same as any other process the
  user runs; there is nothing for the platform to cap or resize.
* A runaway agent is bounded by whatever limits the user's own host/OS
  imposes on any process, not a platform-managed ceiling.

---

# 29. Networking (how the workspace reaches every component)

A workspace is now a **sandboxed host process**, not a VM and not a container,
so there is no separate guest network boundary, no virtual NIC, and no
`host.docker.internal`-style guest↔host translation to speak of. The agent CLI
sits directly on the host network stack; nono's sandboxing confines what it may
reach via a per-launch capability profile rather than a virtual network device.

## 29.1 Transport — nono's network capability grant

Each launch's nono profile carries a `network` block
(`internal/nono.NetworkCapabilities`) with three pieces: `block` (true under
the `deny` egress mode — blocks all network access), and `allow_domain`/
`deny_domain` lists (mirrored from the project's `config.yaml`). How nono
itself mediates a sandboxed process's traffic against these domain lists
(Landlock network rules on Linux, a Seatbelt network profile on macOS, or an
internal proxy — nono's own implementation detail) is outside this platform's
code and is not asserted here beyond what nono's own profile schema
documents; verify against nono's own docs
(https://nono.sh/docs) rather than this spec for enforcement internals.
There is **no egress proxy operated by this platform** and no virtual NIC —
confinement is whatever nono's own sandboxing enforces from the declared
profile.

Because there is no VM, there is also no separate hypervisor/NIC privilege
plane to keep in user space (§29.1's prior "every plane stays in user space"
table, keyed to the Microsandbox/libkrun design, no longer applies — nono has
no hypervisor entitlement requirement at all, §3/§6.2).

## 29.2 Host address — a plain host address, not a guest↔host translation

There is no `AI_PLATFORM_HOST` guest-network boundary to cross anymore — the
sandboxed agent process already runs on the host, so it reaches the gateway at
a **plain host address**. `runtime.DefaultGatewayHost` is now **`127.0.0.1`**
(plain loopback), not a Microsandbox-specific guest→host DNS name — there is
nothing to pin against a hypervisor backend, since there is no hypervisor.
`runtime.HostGateway(_)` always returns `(DefaultGatewayHost, true)` regardless
of GOOS — the parameter is kept only for call-site compatibility with the
pre-migration signature.

The trusted platform gateway is reached at `<host>:18787` — the **nginx
proxy** (the sole host entry), which routes `/v1` **directly to LiteLLM**
(`aip-litellm:4000`); LiteLLM compresses input via its `headroom` guardrail (calling
`aip-headroom:8787/v1/compress`, internal-only). All other egress is
governed by the project's nono network policy (§29.4). Reaching *other*
host-local services — a developer's database or message broker — remains a
separate, explicitly allow-listed concern (§29.6), though the published-port
mechanism that used to accompany it is gone (§7 history — there is no guest
port to publish from).

**Gateway resolution → base URL + egress allow.** The model-gateway endpoint
every agent-CLI launch on a host routes through is derived from
`Info.HostAddress()` (`internal/runtime`): the `ai_platform_host` field if set
(machine-wide, configured by `ai setup --mode client --server` or `ai gateway
set` — §CLI 10.5), else the resolved `host_gateway` (now plain `127.0.0.1`).
The field holds a **bare host or `host:port`** (NOT a URL); `runtime.ResolveGateway`
applies: empty → `127.0.0.1:18787` (standalone/local); `host` → that host on
the default nginx-gateway port `18787`; `host:port` → that host and port. The
result drives the per-CLI provider configs' `base_url`/`http://<host>:<port>/v1`
at every `ai agent`/`ai shell` launch (§15). So in standalone mode every
sandboxed process reaches the local LiteLLM via plain loopback, and in client
mode every sandboxed process on the machine routes through the configured
remote server's gateway instead.

## 29.3 Reaching each component

```text
         sandboxed agent process (nono, host)
       ┌─────────────────────────────┐
       │ agent CLI                    │   egress = nono's per-launch network profile
       └───────────────│─────────────┘   (default `public`; `deny` blocks all network)
        trusted gateway │ (<host>:18787, plain loopback by default)
                        ▼
        nginx (host :18787) ─────────► LiteLLM ──────► provider (cloud)
        (sole host entry,             (routing +   │   (real key from
         /v1 → LiteLLM directly)       enabled     │    LiteLLM's store)
                                       guardrails;  ▼
                                       keys-in-   omlx (local)
                                       LiteLLM)
                                          │ pre_call headroom guardrail
                                          ▼
                             Headroom (aip-headroom:8787, /v1/compress — input compression)
```

* **Model requests** — the sandboxed agent process sends the request to the
  **nginx gateway** at a plain host address (the sole host entry), which routes
  `/v1` **directly to LiteLLM** (routing + enabled guardrails, §15). LiteLLM
  compresses the input via its `pre_call` `headroom` guardrail (calling the
  shared Headroom container at `aip-headroom:8787/v1/compress`, internal-only),
  then reaches **both** the local omlx backend **and** cloud providers,
  attaching the real provider key from **its own store** on cloud calls
  (keys-in-LiteLLM, §17). These hops are host-side; the sandboxed process holds
  only the scoped LiteLLM virtual key, injected by nono (§7).
* **omlx** — the local model backend (§16); never reached directly by the
  sandboxed process. **LiteLLM** routes local model calls to it (no credential
  needed) and applies whichever guardrails are enabled (§15) as on any other
  request.
* **All other egress** (git push, MCP servers, arbitrary web) is governed by
  nono's per-launch network profile (§29.4): the default `public` posture
  allows the open internet (the `deny` mode blocks all network instead), with
  an allow/deny domain list layered on top via `ai network`. There is no
  egress proxy and no wire-level credential injection; any non-model-path
  credential a tool needs is the agent's own.

## 29.4 Egress confinement (the nono network policy)

Each launch's nono profile carries the project's declared egress posture
(§29.6). There are only **two** modes now (the old third mode,
`unrestricted`, is gone):

* **`public`** (default) — nono's own default: unrestricted outbound, with the
  project's `allow_domain`/`deny_domain` lists layered on top (`deny_domain`
  entries are blocked even under `public`).
* **`deny`** — `network.block = true` in the generated profile: no network
  access at all for that sandboxed process, except whatever carve-out
  `allow_domain` declares.

**The project's `config.yaml` `network` block is the single declared-policy
source**, compiled straight into the nono profile's own `network` section on
every launch (`internal/workspace.ensureProfile`) — there is no separate
runtime net-rules format to translate into (contrast the old
`egress.MsbNetworkArgs` msb-rules renderer, which no longer exists). Off-disk
provider credentials are a separate concern handled by keys-in-LiteLLM (§17);
secret masking and audit are LiteLLM's enabled guardrails (§15).

This must work identically across all supported hosts — nono's own Landlock/
Seatbelt backends are expected to honor the same declared profile shape on
Linux and macOS respectively; cross-platform enforcement equivalence is
nono's own concern, not re-implemented by this platform.

## 29.5 Delivery phasing

The policy is **applied today**: `ai network` edits the project's `network`
block in `config.yaml`, and it is compiled into that launch's nono profile on
the very next `ai agent`/`ai shell` — there is no persistent sandbox to
restart, so a policy change is live immediately (contrast the old
Microsandbox design, which needed `ai start`/create to apply a net-rule
change). The host-gateway address (§29.2) is now simply `127.0.0.1` — nothing
to pin against a hypervisor backend.

What is **not verified** for this migration cycle: a live end-to-end
reachability check — on a provisioned host — that (1) `deny` actually blocks
a non-gateway destination under real nono enforcement, (2) `allow_domain`
actually carves out reachability for a listed domain, and (3) the gateway path
itself stays reachable under `deny`. This is a smaller verification surface
than the old Microsandbox design's spike (no published-port check is needed —
there is nothing to publish, §7 history) but is still unconfirmed against a
live `nono` binary in this review; treat it as an open item pending a
hardware-equivalent (host) verification pass, same spirit as the prior
`docs/HARDWARE-BRINGUP.md` tracking (though nono needs no special
hardware/virtualization capability to verify — any host suffices, §6.2).

## 29.6 Host-local services and the allow/deny domain list

There is no NAT boundary anymore — a nono-sandboxed process sits directly on
the host network, so reaching a host-resident service (Postgres, Redis, Kafka)
is no different from any other host process reaching it, **subject to**
whatever the active profile's `network.block`/`allow_domain`/`deny_domain`
permits. There is also no "publish a guest port to the host" direction to
configure anymore (§7 history) — a dev server the sandboxed process starts is
already bound on the host's own network interface like any other host
process; nothing needs publishing.

`network.egress` (`config.NetworkConfig.Egress`, `config.EgressModes`) is now
just:

* **`public`** (**default**) — nono's own default: unrestricted outbound,
  minus anything on the `deny_domain` list. This is a deliberate
  security-posture choice: usable out of the box, re-lockable per project.
  Empty `network.egress` resolves to `public`.
* **`deny`** — blocks all network for that sandboxed process (`network.block
  = true`), except whatever is explicitly on `allow_domain`.

A target in `ai network allow`/`disallow` is now a **bare domain**, not a
`host:port` pair — there is no port concept anymore (no `host:port` allow-list
entries, no `gateway` token special case, no `ai network publish`/`unpublish`
command, since there are no ports to publish without the old in-VM apps
layer, §7 history). `ai network allow <domain>` accepts a hostname/IP/domain
or a `*.suffix` wildcard (e.g. `*.npmjs.org`, requiring at least two labels);
`ai network disallow <domain>` clears it from BOTH the allow and deny lists.
A domain can be moved between allow/deny/neither, never both at once
(`internal/egress.Allow`/`Deny` each remove the entry from the other list).

All of this is configured entirely via the **`ai network`** commands
(`ai network show|egress|allow|disallow`, CLI §10.4) — no manual file editing
is required, though the project `network` block (repo-layout §12.4) can still
be edited by hand:

```yaml
network:
  egress: public              # public (default) | deny
  allow_domain:                # reachable even under deny
    - api.github.com
    - "*.npmjs.org"
  deny_domain:                 # blocked even under public
    - example-blocked.com
```

`ai network` manages the **declaration** in `config.yaml`. The actual
enforcement is nono's own: `internal/workspace.ensureProfile` compiles this
block straight into the launch's nono profile's `network` section — there is
no separate net-rules renderer and no always-on DNS-allow-pair carve-out to
emit (that was specific to msb's L3/L4 rule engine needing help resolving
names under default-deny; nono's own domain-based allow/deny list has no
equivalent need, since it operates on names in the first place, not
post-resolution IPs — though exactly how nono resolves a domain allow/deny
rule at the network layer is nono's own implementation, not this platform's).
App-data connections (DB/Kafka/HTTP) go **direct** under this policy — they do
not pass through the model gateway.

## 29.7 Attempted-egress-by-name audit (`aip-dns`) — status unclear post-migration

The service tier (§5) still includes **`aip-dns`**, a CoreDNS resolver, and
`ai network log` (CLI §10.4) still reads its query log and renders the
attempted-egress-**by-name** audit — this container and command are
unchanged by the nono migration (they are part of the Docker service tier,
which the brief for this migration explicitly says is untouched).

**What is unclear, and flagged here rather than asserted either way:** the
old mechanism that made this audit meaningful was every Microsandbox microVM
booting with `--dns-nameserver` pointed at `aip-dns` (`127.0.0.1:15353`), so
*every* workspace's DNS resolution was forced through it. A nono-sandboxed
host process has no such boot flag — `internal/nono.Profile`'s `network`
block has no DNS-nameserver-override field, and no code in
`internal/workspace`/`internal/nono` sets `network_profile` or otherwise
routes a sandboxed process's DNS queries at `aip-dns`. So, as far as this
review of the current code can tell, **nothing currently points a
nono-sandboxed agent's DNS resolution at `aip-dns`** — the container and `ai
network log` command still work exactly as documented below, but it is not
verified whether they still see real agent-process traffic, or whether they
are now effectively auditing nothing. This needs a live check (does a
nono-sandboxed process's DNS actually arrive at `aip-dns`, or does it go
straight to the host's normal resolver?) that is outside what static code
reading can answer — flagged as an open question for whoever next touches
this path, not resolved by this spec edit.

Assuming it IS still wired (unverified):

* **Enforcement stays on nono's own network policy** (§29.4). A resolver
  *answer* cannot create reachability — a name resolving to an address nono's
  own rules deny is still blocked. The resolver has no authority over what a
  sandboxed process may reach.
* **The resolver is the audit source.** It sees the *names* a process tried
  to resolve, which domain-based allow/deny rules do not surface as a
  human-readable list on their own.

**v1 scope / caveats** (made explicit in `ai network log`'s output):

* **Host-wide, not per-project.** If still wired, every sandboxed process
  would forward to the one resolver, so the audit would be host-wide DNS, not
  attributed per project. `ai network log [project]` accepts the argument for
  forward compatibility but does not filter on it.
* **Names only — not connection verdicts, not direct-IP egress.** It would be
  a record of attempted *resolutions*, not of allowed/blocked connections, and
  traffic to a literal IP never touches DNS so never appears here.

---

# 30. Security Model

Requirements:

* **keys-in-LiteLLM** — real provider keys live only in the LiteLLM gateway
  (§17); the sandboxed agent process holds only a scoped LiteLLM virtual key,
  injected by nono at launch, never written to disk
* no plaintext secrets on platform disk
* no host Docker socket by default (the service tier runs rootless; nono
  sandboxing never touches the Docker socket at all)
* **workspace isolation is same-kernel, not hardware** — each agent-CLI/shell
  invocation is a single sandboxed **host process**, confined by nono's
  kernel-capability mechanism (Landlock LSM on Linux, Seatbelt on macOS), NOT
  a VM boundary. This is a deliberate, accepted trade-off against the
  platform's former Microsandbox microVM runtime (§3, Isolation): a sandbox
  escape here is a Landlock/Seatbelt bypass at the shared host kernel, not a
  hypervisor breakout. What nono's profile DOES guarantee is a filesystem
  capability grant (only the project directory + explicitly granted paths)
  and a network capability grant (only what the profile's egress policy
  allows) — not an isolated kernel.
* project isolation (separate project directories; no shared sandbox state
  between projects)
* agent isolation (each agent CLI/shell launch is its own nono session,
  independently started/stopped/attached)
* **egress confinement** — each launch's nono profile carries the project's
  declared posture: the default `public` posture allows the open internet
  (minus any `deny_domain` entries); `deny` blocks all network for that
  process except `allow_domain` carve-outs. A project can re-lock to `deny`
  via `ai network` (§29.4, §29.6), with the change taking effect on the very
  next launch — no restart step, since there is no persistent sandbox to
  restart.
* **enforced secret protection when enabled** — each rendered LiteLLM guardrail is
  `default_on` on every request and every route (cloud included), so no model traffic
  can bypass an ENABLED guardrail; secret masking is opt-in (`secret-masking`, §15)
* **least host privilege** — nono's sandboxing needs no hypervisor entitlement,
  no kext, no NetworkExtension, and no admin networking prompt on either
  supported OS (§6.2); the platform's only elevated-privilege host actions
  remain the ones unrelated to workspace sandboxing (the service tier's
  container runtime, and the opt-in rootful in-VM container runtime that no
  longer exists post-migration, §7 history).

There is no hypervisor/virtualization-capability gate for workspace sandboxing
anymore (§6.2) — `ai doctor` simply checks that the `nono` binary is on PATH
rather than verifying KVM/Apple-Hypervisor availability. (The host-side
local-inference backend, omlx, is still macOS/Apple-Silicon-only, §16 — that
restriction is unrelated to workspace sandboxing.)

---

# 31. Audit Logging

**Status: deferred.** No event log of this kind is implemented today — there is no
`internal/audit` package and no code writes workspace-creation/deletion,
secret-access, or model-configuration events anywhere. The only thing that exists
on disk is the empty `~/.ai-platform/audit/` directory that `ai setup` pre-creates
(`internal/layout`) — reserved for this future layer, currently unused. The
unrelated DNS-name egress audit (`aip-dns` / `ai network log`, §29.7) is a
different mechanism entirely (attempted-egress hostnames, not platform
lifecycle/secret events) — do not confuse the two; note also that §29.7 flags
whether that mechanism still captures real nono-sandboxed-agent traffic
post-migration as itself unverified.

Location (once implemented):

```text
~/.ai-platform/audit/
```

Events (once implemented):

* workspace creation
* workspace deletion
* secret-access events (that a credential was used — never the value)
* model configuration changes

Secret values must never be logged. LiteLLM owns the authoritative model-request
log (spend, virtual-key usage, guardrail actions) in its Postgres-backed store;
the platform audit log would record only that a secret-access or
model-configuration event occurred, with no secret material.

---

# 32. Backup and Recovery — Removed

The platform has no backup/restore feature. It isn't needed:

* **project source** lives in the project directory (the cwd at `ai create`) and
  is the user's own git repo, if any (backed up by pushing to a remote) —
  unlike before, the platform runs no git of its own, so this is entirely the
  user's responsibility (§21)
* **platform state** is reconstructable from the filesystem via `ai state repair`
* **provider keys** live in the LiteLLM gateway (§17), not on platform disk
* **installed programs** live directly on the host — there is no VM disk or
  overlay to lose anymore (§26 history); a nono-sandboxed process's installs
  land exactly where any other host process's would

There is no per-project environment image to reproduce from anymore (§25
history) — there is nothing bespoke to back up beyond the project directory
itself.

---

# 33. Monitoring and Diagnostics

The platform must support:

* health checks
* diagnostics
* dependency verification
* provider verification

Monitoring is required for:

* nono (host-installed presence, plus a project's live session count when
  resolvable — `ai doctor [name]`, §7)
* nginx proxy (`aip-proxy` — the host entry; readiness probed end-to-end through
  the gateway, §10)
* LiteLLM
* Presidio
* omlx
* DNS audit resolver (`aip-dns`, §29.7 — see that section's status-unclear note)
* Docker
* Podman
* Caveman
* Headroom
* Valkey (+ RedisInsight)

(`ai doctor` reports every service-tier service — dns, omlx, presidio, valkey,
redisinsight, litellm, headroom, proxy — §5 — plus a top-level "nono runtime"
check and, when inside/naming a project, "workspace runtime" + "workspace
sessions" checks, replacing the old workspace-microVM/exec/log/
rootless-virtualization checks §6.2 no longer needs, §7.)

---

# 34. Success Criteria

The architecture is considered successful when a user can:

```bash
ai setup

ai create my-project   # interactive wizard: name/location/shell/agent CLIs/AI tools — scaffolds .ai-platform/
ai agent opencode       # (or: ai start) — mints a scoped key, generates a nono profile, launches a sandboxed session
```

and immediately receive:

* a sandboxed host process per agent-CLI/shell session (nono — Landlock on
  Linux, Seatbelt on macOS; same-kernel capability confinement, §3/§7 — not
  hardware isolation, a deliberate trade-off against the platform's former
  Microsandbox design), with no OS image, no build step, and no boot delay
* host-backed source code (the project directory IS the only persistent
  state, §26 history)
* rootless container runtime for the service tier (Docker/Podman, §6.1)
* LiteLLM integration (provider keys held in the gateway, §17)
* user-selectable LiteLLM guardrails on every model request — Headroom input
  compression default-on; secret-masking / hide-secrets / tool-firewall opt-in (§15)
* a per-project nono egress policy — default `public` (open internet
  reachable, `deny_domain` entries still blocked), re-lockable to `deny`
  (`ai network`, §29.4), taking effect on the very next launch
* Headroom input compression
* Caveman output compression (selectable/toggleable today; the host-side
  install step itself is not yet reimplemented post-migration, §9)
* zero manual nono configuration (profiles are generated automatically)
* project persistence = the project directory on the host, with nothing else
  to manage (§26 history)
    
