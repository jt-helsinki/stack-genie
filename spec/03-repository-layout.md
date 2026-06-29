# 03-repository-layout.md

# AI Development Platform

## Repository Layout Specification

Version: 1.0

---

# 0. Purpose

This document defines the canonical directory structures used by the AI Development Platform across:

* host system
* projects
* workspaces
* caches
* agent systems

Consistency of structure is required for reproducibility, automation, and AI-driven tooling.

---

# 1. Host Platform Layout

All platform-wide data is stored under:

```text id="h1"
~/.ai-platform/
```

---

## 1.1 Root Structure

```text id="h2"
~/.ai-platform/
├── agents/
├── audit/
├── cache/         # re-fetchable caches: catalog.json (models.dev), ollama-library.json
├── config/        # global settings + projects index (no per-project state)
├── logs/
├── overlays/
├── prompts/
├── skills/
├── templates/     # OS Dockerfile templates + shared templates
├── tools/
└── volumes/       # ALL host-persisted SYSTEM data volumes (bind-mounted into aip-* containers)
    ├── litellm-db/  # LiteLLM Postgres data dir (→ aip-litellm-db:/var/lib/postgresql)
    └── models/      # Ollama persistent model store (→ aip-ollama)

~/.ai-platform/.ai-platform.env   # OPT-IN, 0600 sibling file (NOT under ~/.ai-platform/)
```

Global only — **no per-project state here**. Per-project state lives in
`<project>/.ai-platform/` (see §2).

`~/.ai-platform/.ai-platform.env` is an **opt-in, mode-0600** plain-text file of
`export KEY='VALUE'` lines (NOT YAML) that the `ai` CLI loads at startup so
platform secrets (`UI_PASSWORD`, `LITELLM_MASTER_KEY`) persist across restarts
without the user editing a shell rc; it lives **beside** `~/.ai-platform/`, not
inside it. Precedence is "existing env wins" — load only fills gaps. It is the
ONE on-disk place secrets may live for the host's own service tier (still never
in a workspace or project); real provider keys remain in the LiteLLM gateway.

`~/.ai-platform/volumes/` is the **single home for every host-persisted SYSTEM
data volume** — keeping them in one discoverable place (rather than scattered
Docker named volumes or ad-hoc paths under the platform dir) means
`ai uninstall --purge` (which `RemoveAll`s `~/.ai-platform`) removes them all. The
rule: **all system host volumes live under `~/.ai-platform/volumes/<name>`**
(config files — the LiteLLM/DNS/nginx configs under `config/` — and the
per-project workspace overlays are NOT system volumes and stay where they are).
Today there are two:

- `~/.ai-platform/volumes/litellm-db/` — the LiteLLM **Postgres data dir**,
  **HOST-BIND-MOUNTED** into `aip-litellm-db` at `/var/lib/postgresql` (NOT a
  Docker named volume). This is the one stateful service-tier piece.
- `~/.ai-platform/volumes/models/` — the **persistent Ollama model store**,
  bind-mounted into `aip-ollama` so pulled local models survive container
  recreation (distinct from the disposable `cache/models/` in §1.3).

**Migration caveat (acceptable for this dev platform):** existing data in the old
`aip-litellm-db-data` named volume and the old `~/.ai-platform/models/` does NOT
auto-migrate — the next `ai setup` starts with fresh dirs (Postgres re-`initdb`s,
models re-pull). `ai uninstall` still best-effort `docker volume rm aip-*`s the
legacy named volume on upgrade.

---

## 1.2 State

State is split between a small **global index** and **project-local** state.

Global (`~/.ai-platform/config/`):

```text id="h3"
config/projects.yaml     # index: project name → absolute path (create/delete only)
```

Project-local (`<project>/.ai-platform/`, see §2) holds everything specific to
one project — tracked definition files plus a gitignored `run/` for host-local
runtime state. There is **no `~/.ai-platform/state/` directory**.

Rules:

* the global `projects.yaml` index is low-write (create/delete only), so no
  write contention
* per-project runtime state is sharded per entity under `<project>/.ai-platform/run/`
  (one file per workspace) — gitignored, machine-specific
* **all state writes are atomic**: write to a temp file in the same directory,
  then `rename()` over the target, so a crash mid-write can never produce a
  partial or corrupt state file
* `ai state repair` rebuilds `run/` from the project + Microsandbox; discovery
  uses `projects.yaml` (VCS is out of scope — no git is consulted)

---

## 1.3 Cache Layer

```text id="h5"
~/.ai-platform/cache/
```

```text id="h6"
catalog.json          # models.dev catalog (moved from volumes/catalog.json; one-shot migrated)
ollama-library.json   # live ollama.com installable-library list
models/
downloads/
temp/
```

Rules:

* fully disposable — all entries are **re-fetchable** copies, not SYSTEM data
* may be rebuilt at any time (`catalog.json` / `ollama-library.json` are re-downloaded
  on next use, falling back to the cached copy only while the source is unreachable)
* `catalog.json` was **moved here from the legacy `volumes/catalog.json`** with a
  one-shot lazy migration (`catalog.Path`); `volumes/` is now ONLY true host data
* never contains secrets
* removed by `ai uninstall --purge` (which `RemoveAll`s `~/.ai-platform`)

---

## 1.4 Audit Logs

```text id="h7"
~/.ai-platform/audit/
```

Contains immutable logs:

```text id="h8"
workspace-create.log
workspace-destroy.log
agent-events.log
security-events.log
```

Rules:

* append-only
* no secret material stored
* used for debugging and compliance

---

## 1.5 OS Dockerfile + Software-Stack Templates

```text id="h9"
~/.ai-platform/templates/dockerfiles/
~/.ai-platform/templates/stacks/
```

One Dockerfile template per supported OS key, and one install snippet per
software stack (architecture §25):

```text id="h10"
dockerfiles/alma/Dockerfile
dockerfiles/debian-trixie/Dockerfile
dockerfiles/debian-bookworm/Dockerfile
dockerfiles/ubuntu/Dockerfile

stacks/java/Dockerfile.snippet
stacks/maven/Dockerfile.snippet
stacks/node/Dockerfile.snippet
stacks/deno/Dockerfile.snippet
stacks/go/Dockerfile.snippet
stacks/python/Dockerfile.snippet
stacks/rust/Dockerfile.snippet
```

Rules:

* the OS template **seeds** a new workspace's `.ai-platform/Dockerfile` at
  `ai create`; the selected **stack snippets** (wizard step 5) are
  composed into it after the base tooling
* every OS base template installs, beyond the base tooling, a **rootful in-VM OCI
  container runtime** (containerd + nerdctl + runc + CNI + buildkit, from the
  pinned `nerdctl-full` tarball into `/usr/local`, arch-aware) plus its CNI deps
  (`iptables`, `iproute`); the runtime is started at workspace start (arch §7)
* the stack list is **extensible** — adding `stacks/<name>/Dockerfile.snippet`
  makes `<name>` selectable
* after creation the project owns its Dockerfile; templates/snippets are no
  longer consulted
* there is no snapshot versioning, storage, or pinning

---

## 1.6 MCP Registry — Removed

The platform does not manage MCP. MCP servers are configured and run by the
in-workspace agent (architecture §12). There is no platform MCP registry.
(Any provider credentials those MCP servers need are resolved through the
keys-in-LiteLLM credential store, not stored on platform disk — architecture
§17.)

---

## 1.7 Prompts & Skills

```text id="h15"
~/.ai-platform/prompts/
~/.ai-platform/skills/
```

Used for:

* agent templates
* reusable prompt modules
* standardized workflows

---

## 1.8 Config

```text id="h16"
~/.ai-platform/config/
```

```text id="h17"
config.yaml              # global platform config (§12.4)
runtime.yaml             # detected runtime, platform-global (§12.5)
versions.yaml            # pinned image+tag of host services (§12.6)
projects.yaml            # index: project name → path (§12.7)
litellm/                 # rendered LiteLLM config.yaml (placeholders only; real keys live in the gateway)
proxy/                   # rendered nginx.conf for the aip-proxy gateway (the litellm.<domain> UI vhost + gateway paths)
dns/                     # rendered CoreDNS config for the aip-dns egress-audit resolver
ollama/                  # rendered Ollama config (required local model backend)
<service>/               # one rendered-config dir per service-tier service (created at reconcile)
```

Rules:

* a config dir is created **per service-tier service** at reconcile (e.g.
  `litellm/`, `proxy/`, `dns/`, `ollama/`, `presidio-analyzer/`, …); the ones
  that have a rendered file today are LiteLLM (`config.yaml`), the nginx gateway
  (`proxy/nginx.conf`), and the CoreDNS resolver (`dns/`)
* every `<service>/` config is **rendered** by the CLI from the platform
  config; not hand-edited (architecture §5, Host Services Control Plane)
* contains **no secrets** — only placeholders; real provider credentials live in
  the LiteLLM gateway (env passthrough / its Postgres-backed store), never here
* native-service binaries live under `~/.ai-platform/tools/`, not here

In **standalone** mode `ai setup` also writes an AI-platform-owned block to
`/etc/hosts` (a privileged write, outside `~/.ai-platform/`) mapping the UI
subdomain (`litellm.<domain>` — the only host UI vhost; Open WebUI is now a
per-workspace in-VM app and Odysseus was removed) to `127.0.0.1`, so the nginx
gateway's vhost resolves locally. Only the platform's delimited block is touched;
`ai uninstall` removes it. (`internal/hostsfile` writes the managed block;
`internal/uihosts` computes the entries from the service registry.)

## 1.9 Tools

```text id="h18"
~/.ai-platform/tools/<name>/<version>/
```

Pinned, checksum-verified host binaries (e.g. the Microsandbox
`msb` runtime). Versions are tracked in `config/versions.yaml`. (Ollama is no
longer a native binary — it runs as a container-tier service; see architecture
§16 and `config/versions.yaml` §12.6.)

## 1.10 Overlays (Persistence)

Per-workspace persistent writable layers (architecture §26), host-backed so
installs and agent state survive workspace restart and recreation.

```text id="h19"
~/.ai-platform/overlays/<workspace-id>/
```

Each directory is the persistent writable layer for one workspace
(`aip-<project>`), mounted over the read-only image
(built from the project Dockerfile) at workspace start.

Rules:

* one overlay per workspace; the whole writable layer persists (no manifest of
  declared paths)
* local persistence, not a backup — removed only when its workspace is
  permanently removed
* never contains secrets

---

# 2. Project Layout (Host)

All projects live under:

```text id="p1"
~/projects/<project-name>/
```

---

## 2.1 Project Root

```text id="p2"
~/projects/my-project/
├── .ai-platform/
├── docs/
├── scripts/
├── src/
├── tests/
└── README.md
```

The project source is host-backed and is the single source of truth. Agent
memory, MCP config, and any agent rules files (e.g. `AGENTS.md`) are written by
the agent into this source tree — the platform does not manage them.

---

## 2.2 Project Platform Directory

```text id="p3"
<project>/.ai-platform/
```

```text id="p4"
Dockerfile           # tracked — environment (architecture §25)
config.yaml          # tracked — project config (§12.4)
profile.yaml         # tracked — project profile (language/toolchain)
project.yaml         # tracked — { name, os, created } (§12.1)
skills/caveman/      # tracked — platform-seeded Caveman skill (architecture §9)
agents/              # tracked — KEYLESS, user-editable agent CLI templates (§12.1c)
  opencode.json      #   opencode static settings + gateway base URL (no key)
  pi.json            #   pi static settings + gateway base URL (no key)
  codex.toml         #   codex gateway provider block (key via env_key, no key)
.gitignore           # ignores run/
run/                 # gitignored — host-local runtime state
  workspaces/<workspace-id>.json   # (§12.2)
```

The `agents/` templates hold each agent CLI's **static, user-editable** settings
plus the gateway **base URL** (not a secret) — but **never the scoped virtual key**.
At workspace start the platform reads each template, deep-merges in the dynamic,
key-bearing values (the freshly-minted scoped key, the served-model picker, the
Headroom knobs), and writes the **final config INTO the microVM** — so the key lives
only in the VM, never on platform disk (architecture §15). A user's edits survive
restart (a present template is merged, never clobbered with the key-bearing form);
only an absent template is re-scaffolded keyless. claude-code/codex/gemini route via
**env vars** written into the VM agent env file (sourced by every session), so all
five CLIs reach the gateway by default.

The platform seeds the **Caveman** agent skill into `skills/caveman/` at
project creation (not into the workspace image) so the in-workspace agent picks
it up for that project. Like the Dockerfile, it is **git-tracked**: it is seeded
once at creation and then committed with the project, so the skill travels with
the repo and the project stays reproducible from git alone. It is not
re-installed or auto-upgraded on workspace start (the project owns it; refresh it
by re-seeding explicitly).

* the tracked files (including `skills/caveman/`) define the environment + config
  and are committable, so the project is reproducible from git
* `run/` holds machine-specific runtime handles (Microsandbox ids, status) and is
  gitignored
* no platform-managed memory/history (delegated to the agent, architecture §11)

---

## 2.3 Worktrees — not platform-managed

The platform does not create or manage git worktrees. If a user runs multiple AI
agents inside a workspace and those agents use worktrees (e.g. under
`.worktrees/`), that is entirely the in-workspace agent CLI's doing (architecture
§20–21); the platform neither tracks nor cleans them up.

---

# 3. Workspace Layout (Microsandbox)

Inside the workspace microVM:

```text id="w1"
~/workspace/
```

---

## 3.1 Workspace Structure

```text id="w2"
~/workspace/
├── .ai-platform/
├── src/
├── tests/
├── scripts/
└── docs/
```

---

## 3.2 Mounting Rules

Host project:

```text id="w3"
~/projects/my-project
```

Mounted into:

```text id="w4"
~/workspace
```

Rules:

* the host project source is the single source of truth (mounted read-write)
* the workspace **microVM is disposable** — its identity/runtime can be
  destroyed and recreated at any time
* non-source writes (installs, agent state) are **not** lost on recreation:
  they persist via the per-workspace **overlay** (§1.10), from Slice 4 onward

---

# 4. Environment Images

There is no snapshot store or versioned image layout. A project's workspace
image is **built from `<project>/.ai-platform/Dockerfile`** (architecture §25)
as an OCI image and run as a Microsandbox microVM; image storage is handled by
Microsandbox's own OCI image store (`msb image ls`). The platform does not
maintain a bespoke `snapshots/` directory.

* the per-OS **templates** that seed a new project's Dockerfile live under
  `~/.ai-platform/templates/dockerfiles/<os>/Dockerfile` (§1.5)
* build/image caching is the runtime's responsibility (no platform-managed versions)

---

# 5. Cache Layout

```text id="c1"
~/.ai-platform/cache/
```

```text id="c2"
models/
downloads/
temp/
```

Rules:

* fully disposable
* can be rebuilt
* never stores secrets

---

# 6. Logging Layout

```text id="l1"
~/.ai-platform/logs/
```

```text id="l2"
system.log
microsandbox.log
agent.log
llm.log
```

---

# 7. CLI State Mapping

Per-project state lives in the project; the global index records where projects
are:

* `config/projects.yaml` (global) → `ai create` / `delete` (index)
* `<project>/.ai-platform/project.yaml` → `ai create`
* `<project>/.ai-platform/run/workspaces/*.json` → `ai start`/`stop`/…

---

# 8. Security Constraints

* no secrets stored anywhere in repository
* no secrets in workspace
* no secrets in cache
* real provider keys live only in the LiteLLM gateway (keys-in-LiteLLM); the
  workspace agent holds only a scoped virtual key

---

# 9. Design Constraints

* deterministic directory structure
* no runtime-generated unknown paths
* per-project state lives in the project (`<project>/.ai-platform/`); a global
  `config/projects.yaml` index records project locations
* all agent data namespaced per project

---

# 10. Consistency Rule

Every layer must respect:

* Host layout
* Project layout
* Workspace layout

No component may introduce ad-hoc directories outside this specification.

---

# 11. Success Criteria

System is valid when:

* CLI can reconstruct runtime state from each project's `.ai-platform/run/`
* a project is fully described by its `.ai-platform/` (Dockerfile + config)
* the `.ai-platform/Dockerfile` recreates the environment
* no manual directory setup is required

---

# 12. State & Config Schemas

These schemas are normative. All examples use JSON for state files and YAML for
config files. Unknown fields must be rejected. All timestamps are RFC 3339 UTC.
`schema_version` is required on every state file so the CLI can migrate.

## 12.1 `<project>/.ai-platform/project.yaml` (tracked)

```json id="sc1"
{
  "schema_version": 1,
  "name": "my-app",
  "os": "debian-trixie",
  "created": "2026-06-18T10:00:00Z"
}
```

* tracked in git; the project's path is recorded in the global
  `config/projects.yaml` index (§12.7), not here
* the selected software stacks live in `profile.yaml` (§12.1a), not here

## 12.1a `<project>/.ai-platform/profile.yaml` (tracked)

The project's language/tool profile — the **software stacks** installed in the
environment (architecture §25), chosen in the create wizard (CLI §3.1 step 5):

```yaml id="sc1a"
schema_version: 1
stacks: [node, go]   # any subset of the available stack snippets (§1.5)
```

* tracked in git so the environment is reproducible
* the stacks are composed into `.ai-platform/Dockerfile` at creation; editing the
  Dockerfile directly is also fine (the Dockerfile is the source of truth for the
  build, §25)

## 12.2 `<project>/.ai-platform/run/workspaces/<workspace-id>.json` (gitignored)

```json id="sc2"
{
  "schema_version": 1,
  "id": "aip-my-app",
  "project": "my-app",
  "microsandbox_id": "msb-9f3c...",
  "status": "started",
  "created": "2026-06-18T10:00:05Z",
  "last_started": "2026-06-18T10:00:05Z"
}
```

* `status`: `created` | `started` | `stopped` | `archived` | `destroyed`
* host-local runtime handle — gitignored
* one workspace per project (§12.1); there is no per-agent workspace

## 12.3 Agent state — Removed

There is no platform `agent.json`. The platform has no "agent" entity; running
multiple AI agents on a project is the in-workspace agent CLI's concern
(architecture §20).

## 12.4 Config (`config.yaml`)

Same shape at every level of the hierarchy (§27 of the architecture spec);
each level may set any subset and overrides the level below.

The service-tier runtime is **auto-detected** (into `runtime.yaml`, §12.5), not a
config field. Git branches/worktrees/merges and multi-agent lifecycle are the
in-workspace agent CLI's concern (arch §20–22), so there are no `git`/`agents`
config blocks.

```yaml id="sc6"
os: alma                   # alma | debian-trixie | debian-bookworm | ubuntu
agent:
  tools: [opencode, pi]    # installed agent CLIs (any subset of: opencode, pi, claude-code, codex, gemini-cli); opencode + pi by default
  default_tool: opencode   # default agent CLI; must be one of agent.tools
context:
  strategy: balanced       # Headroom input compression: conservative | balanced | aggressive
                           # (mapped to Headroom per-request knobs keep_turns/output_buffer_tokens)
  caveman_level: full      # Caveman output compression: lite | full | ultra | wenyan
workspace:
  cpu_limit: 4             # declared microVM resource limits (schema fields; NOT yet
  memory_limit: 8G         # wired into `msb create` — the microVM currently boots with a
                           # fixed 4G (`workspace_real.go` microVMMemory) and no `--cpus`)
network:                   # workspace networking (arch §29.6); all fields managed via `ai network`
                           # enforced as a Microsandbox NetworkPolicy (no egress proxy); DNS-audited
  egress: public           # public (DEFAULT, allow-outbound; private ranges still blocked) | deny | unrestricted
                           # empty == "public" (the model gateway is always reachable in every mode)
  allow_host_services:     # external destinations the workspace may reach (host/IP/domain or "gateway")
    - { host: gateway, port: 5432 }
  publish_ports:           # host → workspace port maps
    - { guest: 3000, host: 3000 }
apps:                      # opt-in in-VM AI apps (arch §7), chosen via `ai create --apps` / `ai apps add`
                           # each runs as a rootful nerdctl container in the workspace microVM, gateway-routed,
                           # published on its allocated unique host port (stable across restarts;
                           # allocated from the 21000–21999 window — internal/apps/ports.go)
  - { key: openwebui, port: 21000 }   # key one of: openwebui, anythingllm
```

## 12.5 `config/runtime.yaml` (platform-global, non-project)

```json id="sc7"
{
  "schema_version": 1,
  "role": "standalone",
  "detected": "docker",
  "rootless": true,
  "microsandbox": { "available": true, "virtualization": "hvf" },
  "ai_platform_host": "host.local",
  "host_gateway": "host.microsandbox.internal",
  "domain": "aip.local",
  "detected_at": "2026-06-18T09:59:00Z"
}
```

* `role`: `standalone` | `server` | `client` (chosen by `ai setup`; absent on a
  pre-role runtime.yaml). Drives the service bind host and which prereqs are
  required.
* `optional_services`: the opt-in HOST services enabled at setup; the optional
  mechanism is retained but there are currently **no** optional host services
  (Open WebUI moved to a per-workspace in-VM app and Odysseus was removed), so
  this field is normally omitted.
* `ai_platform_host`: the machine-wide gateway address every workspace microVM
  routes through (`ai gateway set` / `ai setup --mode client --server`).
* `host_gateway`: the guest-visible host address (arch §29.2), default
  `host.microsandbox.internal`.
* `domain`: the platform base domain the nginx UI subdomain hangs off
  (`litellm.<domain>` — the only host UI vhost); empty resolves to the default
  `aip.local` (`ai domain` shows/sets it).

## 12.6 `config/versions.yaml` (pinned host-service versions)

```json id="sc9"
{
  "schema_version": 1,
  "services": {
    "litellm":      { "mode": "container", "image": "ghcr.io/berriai/litellm", "tag": "latest" },
    "litellm-db":   { "mode": "container", "image": "postgres", "tag": "18.4-alpine3.23" },
    "headroom":     { "mode": "container", "image": "ghcr.io/chopratejas/headroom", "tag": "latest" },
    "ollama":       { "mode": "container", "image": "ollama/ollama", "tag": "latest" },
    "presidio-analyzer":   { "mode": "container", "image": "mcr.microsoft.com/presidio-analyzer",   "tag": "latest" },
    "presidio-anonymizer": { "mode": "container", "image": "mcr.microsoft.com/presidio-anonymizer", "tag": "latest" },
    "proxy":        { "mode": "container", "image": "nginx", "tag": "stable-alpine3.23-slim" },
    "dns":          { "mode": "container", "image": "coredns/coredns", "tag": "latest" }
  }
}
```

* `mode`: `container` | `native`
* This file is the **source of truth** for the service-tier image references:
  container services are pinned by **image + tag** — most use the `latest` tag, but
  some are intentionally pinned to a specific tag (e.g. `litellm-db` →
  `postgres:18.4-alpine3.23`, `proxy` → `nginx:stable-alpine3.23-slim`) — **not by
  digest** (digests are platform/arch specific, so a digest pin breaks
  cross-platform pulls).
* The **native microsandbox runtime is NOT pinned here**: it is a user-installed
  prerequisite that `ai setup`/`ai doctor` DETECT on PATH, not an image the platform
  pulls — so it is deliberately excluded from this image pin set (it keeps a
  registry slot only for its log scope). Native pins (`version` + `sha256`) are
  reserved for any future genuinely-pinned native component.
* `ai setup` **resolves every service-tier image from this file** (via
  `internal/setup`'s `containerImage`, falling back to the built-in
  `versions.Default()` pins when the file is absent or an entry is incomplete);
  `--upgrade` re-writes it to this binary's defaults and re-reconciles

## 12.7 `config/projects.yaml` (global projects index)

```json id="sc11"
{
  "schema_version": 1,
  "projects": {
    "my-app":   { "path": "/Users/me/projects/my-app" },
    "other-app":{ "path": "/Users/me/work/other-app" }
  }
}
```

* maps project name → absolute path; written only on `ai create` /
  `ai delete` (low-write)
* lets the CLI find a project's `.ai-platform/` without scanning

## 12.8 `overlays/<workspace-id>/overlay.json` (global overlay record)

A tiny record beside the persistent layer (the layer contents are not described
by a manifest — the whole writable layer persists).

```json id="sc10"
{
  "schema_version": 1,
  "workspace": "aip-my-app",
  "updated": "2026-06-18T11:00:00Z"
}
```
