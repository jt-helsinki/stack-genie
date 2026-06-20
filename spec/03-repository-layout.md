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
├── cache/
├── config/        # global settings + projects index (no per-project state)
├── logs/
├── overlays/
├── prompts/
├── skills/
├── templates/     # OS Dockerfile templates + shared templates
└── tools/
```

Global only — **no per-project state here**. Per-project state lives in
`<project>/.ai-platform/` (see §2).

---

## 1.2 State

State is split between a small **global index** and **project-local** state.

Global (`~/.ai-platform/config/`):

```text id="h3"
config/projects.json     # index: project name → absolute path (create/delete only)
```

Project-local (`<project>/.ai-platform/`, see §2) holds everything specific to
one project — tracked definition files plus a gitignored `run/` for host-local
runtime state. There is **no `~/.ai-platform/state/` directory**.

Rules:

* the global `projects.json` index is low-write (create/delete only), so no
  write contention
* per-project runtime state is sharded per entity under `<project>/.ai-platform/run/`
  (one file per workspace) — gitignored, machine-specific
* **all state writes are atomic**: write to a temp file in the same directory,
  then `rename()` over the target, so a crash mid-write can never produce a
  partial or corrupt state file
* `ai state repair` rebuilds `run/` from the project + Microsandbox/git; discovery
  uses `projects.json`

---

## 1.3 Cache Layer

```text id="h5"
~/.ai-platform/cache/
```

```text id="h6"
models/
downloads/
temp/
```

Rules:

* fully disposable
* may be rebuilt at any time
* never contains secrets

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

* the OS template **seeds** a new project's `.ai-platform/Dockerfile` at
  `ai project create`; the selected **stack snippets** (wizard step 5) are
  composed into it after the base tooling
* the stack list is **extensible** — adding `stacks/<name>/Dockerfile.snippet`
  makes `<name>` selectable
* after creation the project owns its Dockerfile; templates/snippets are no
  longer consulted
* there is no snapshot versioning, storage, or pinning

---

## 1.6 MCP Registry — Removed

The platform does not manage MCP. MCP servers are configured and run by the
in-workspace agent (architecture §12). There is no platform MCP registry.
(ClawPatrol still brokers any credentials those MCP servers need — architecture
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
runtime.json             # detected runtime, platform-global (§12.5)
versions.json            # pinned versions/digests of host services (§12.6)
projects.json            # index: project name → path (§12.7)
litellm/                 # rendered LiteLLM config (placeholders only)
microsandbox/            # rendered Microsandbox workspace defaults (image, mounts, limits)
clawpatrol/              # platform-rendered ClawPatrol bits (NOT the credential store).
                         # The operational gateway config lives in ClawPatrol's own
                         # home: ~/.clawpatrol/gateway.hcl (seeded by `ai setup`).
ollama/                  # rendered Ollama config (required local model backend)
```

Rules:

* every `<service>/` config is **rendered** by the CLI from the platform
  config; not hand-edited (architecture §5, Host Services Control Plane)
* contains **no secrets** — only placeholders; real credentials live in
  ClawPatrol's own SQLite store, never here
* native-service binaries live under `~/.ai-platform/tools/`, not here

## 1.9 Tools

```text id="h18"
~/.ai-platform/tools/<name>/<version>/
```

Pinned, checksum-verified host binaries (e.g. `clawpatrol` and the Microsandbox
`msb` runtime). Versions are tracked in `config/versions.json`. (Ollama is no
longer a native binary — it runs as a container-tier service; see architecture
§16 and `config/versions.json` §12.6.)

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
project.json         # tracked — { name, os, created } (§12.1)
skills/caveman/      # tracked — platform-seeded Caveman skill (architecture §9)
.gitignore           # ignores run/
run/                 # gitignored — host-local runtime state
  workspaces/<workspace-id>.json   # (§12.2)
```

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

* `config/projects.json` (global) → `ai project create` / `delete` (index)
* `<project>/.ai-platform/project.json` → `ai project create`
* `<project>/.ai-platform/run/workspaces/*.json` → `ai workspace`

---

# 8. Security Constraints

* no secrets stored anywhere in repository
* no secrets in workspace
* no secrets in cache
* only ClawPatrol injects secrets at runtime

---

# 9. Design Constraints

* deterministic directory structure
* no runtime-generated unknown paths
* per-project state lives in the project (`<project>/.ai-platform/`); a global
  `config/projects.json` index records project locations
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
* agents can be rebuilt from worktrees + branches
* the `.ai-platform/Dockerfile` recreates the environment
* no manual directory setup is required

---

# 12. State & Config Schemas

These schemas are normative. All examples use JSON for state files and YAML for
config files. Unknown fields must be rejected. All timestamps are RFC 3339 UTC.
`schema_version` is required on every state file so the CLI can migrate.

## 12.1 `<project>/.ai-platform/project.json` (tracked)

```json id="sc1"
{
  "schema_version": 1,
  "name": "my-app",
  "os": "debian-trixie",
  "created": "2026-06-18T10:00:00Z"
}
```

* tracked in git; the project's path is recorded in the global
  `config/projects.json` index (§12.7), not here
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

The service-tier runtime is **auto-detected** (into `runtime.json`, §12.5), not a
config field. Git branches/worktrees/merges and multi-agent lifecycle are the
in-workspace agent CLI's concern (arch §20–22), so there are no `git`/`agents`
config blocks.

```yaml id="sc6"
os: alma                   # alma | debian-trixie | debian-bookworm | ubuntu
agent:
  tools: [opencode]        # installed agent CLIs (any subset of: opencode, claude-code, codex, gemini-cli)
  default_tool: opencode   # default agent CLI; must be one of agent.tools
context:
  strategy: balanced       # Headroom input compression: conservative | balanced | aggressive
  caveman_level: full      # Caveman output compression: lite | full | ultra | wenyan
workspace:
  cpu_limit: 4             # microVM resource limits (applied at workspace start)
  memory_limit: 8G
network:                   # workspace networking (arch §29.6)
  egress_proxy: clawpatrol
  allow_host_services:     # plain-TCP host services the workspace may reach
    - { host: gateway, port: 5432 }
  publish_ports:           # host → workspace port maps
    - { guest: 3000, host: 3000 }
```

## 12.5 `config/runtime.json` (platform-global, non-project)

```json id="sc7"
{
  "schema_version": 1,
  "detected": "docker",
  "rootless": true,
  "microsandbox": { "available": true, "virtualization": "hvf" },
  "ai_platform_host": "host.local",
  "detected_at": "2026-06-18T09:59:00Z"
}
```

## 12.6 `config/versions.json` (pinned host-service versions)

```json id="sc9"
{
  "schema_version": 1,
  "services": {
    "microsandbox": { "mode": "native",    "version": "v0.x", "sha256": "..." },
    "clawpatrol":   { "mode": "native",    "version": "v0.x", "sha256": "..." },
    "litellm":      { "mode": "container", "image": "ghcr.io/berriai/litellm", "digest": "sha256:..." },
    "headroom":     { "mode": "workspace", "version": "..." },
    "ollama":       { "mode": "container", "image": "docker.io/ollama/ollama", "digest": "sha256:..." }
  }
}
```

* `mode`: `container` | `native`
* `ai setup` verifies installed services match these pins;
  `--upgrade` updates them and re-reconciles

## 12.7 `config/projects.json` (global projects index)

```json id="sc11"
{
  "schema_version": 1,
  "projects": {
    "my-app":   { "path": "/Users/me/projects/my-app" },
    "other-app":{ "path": "/Users/me/work/other-app" }
  }
}
```

* maps project name → absolute path; written only on `ai project create` /
  `ai project delete` (low-write)
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
