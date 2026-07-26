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
├── cache/         # re-fetchable caches: catalog.yaml (models.dev), ollama-models.yaml (ollama.com)
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

`~/.ai-platform/.ai-platform.env` is a **mode-0600** plain-text file of
`export KEY='VALUE'` lines (NOT YAML) that the `ai` CLI loads at startup so
platform secrets persist across restarts without the user editing a shell rc; it
lives **beside** `~/.ai-platform/`, not inside it. The `LITELLM_MASTER_KEY` +
`LITELLM_SALT_KEY` pair is written **automatically** on every LiteLLM reconcile
(`persistLiteLLMInfraKeys`) — a correctness requirement, since a rotated salt key
orphans stored provider credentials and a stable master key keeps workspace
scoped-key minting working across container recreates; the `UI_PASSWORD` is an
**opt-in** addition (`ai setup` server / `ai litellm password`). Precedence is
"existing env wins" — load only fills gaps. It is the
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
catalog.yaml          # models.dev catalog (fetched as JSON, persisted as YAML; legacy catalog.json one-shot converted)
ollama-models.yaml    # ollama.com installable-library list, scraped (name/size/context/input), YAML
models/
downloads/
temp/
```

Rules:

* fully disposable — all entries are **re-fetchable** copies, not SYSTEM data
* may be rebuilt at any time (`catalog.yaml` / `ollama-models.yaml` are re-downloaded
  on next use, falling back to the cached copy only while the source is unreachable)
* `catalog.yaml` is the models.dev catalog fetched as JSON and **persisted as YAML**;
  a legacy `catalog.json` (or the older `volumes/catalog.json`) is **one-shot
  converted** to YAML (`catalog.Path`); `volumes/` is now ONLY true host data
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
~/.ai-platform/templates/agentclis/
~/.ai-platform/templates/tools/
```

One Dockerfile template per supported OS key, one install snippet per software
stack, per agent CLI, and per opt-in dev tool (architecture §25):

```text id="h10"
dockerfiles/alma/Dockerfile
dockerfiles/debian-trixie/Dockerfile
dockerfiles/debian-bookworm/Dockerfile
dockerfiles/ubuntu/Dockerfile

stacks/go/Dockerfile.snippet
stacks/rust/Dockerfile.snippet
stacks/java/Dockerfile.snippet
stacks/maven/Dockerfile.snippet
stacks/deno/Dockerfile.snippet

# opt-in AI tools — appended to the project Dockerfile only when selected at create (--tools)
tools/graphify/Dockerfile.snippet
tools/code-review-graph/Dockerfile.snippet
tools/codebase-memory-mcp/Dockerfile.snippet
```

Rules:

* the OS template **seeds** a new workspace's `.ai-platform/Dockerfile` at
  `ai create`; the selected **stack snippets** (wizard step 5) are
  composed into it after the base tooling
* every OS base template installs, beyond the base tooling, a **rootful in-VM OCI
  container runtime** (containerd + nerdctl + runc + CNI + buildkit, from the
  pinned `nerdctl-full` tarball into `/usr/local`, arch-aware) plus its CNI deps
  (`iptables`, `iproute`); the runtime is started at workspace start (arch §7)
* every OS base template also bakes in **Node.js** (pinned Node 24 LTS,
  system-wide — so the agent-CLI snippets only `npm install -g` their CLI), the
  latest **Python 3** (system-wide),
  and **uv** (Astral's Python package/tool manager, installed for the workspace user
  onto `~/.local/bin`). **Graphify** (PyPI `graphifyy`, CLI `graphify`) is NO LONGER baked
  into the base — it is a selectable AI tool (`--tools graphify`, default on) installed by a
  CONDITIONAL `tools/graphify/Dockerfile.snippet`
  (`uv tool install "graphifyy[pdf,office,video,postgres,google,svg,sql,terraform,ollama,openai,gemini,anthropic,mcp]"`,
  all optional extras except the region/DB/niche-specific `chinese,azure,bedrock,falkordb,neo4j,leiden,dm,pascal`)
  appended only when selected. When selected, Graphify is then registered with each agent CLI
  at WORKSPACE START (not in the Dockerfile), ONCE per project (guarded by a
  `.ai-platform/.graphify-installed` marker) — `graphify install --project [--platform <cli>]`
  run in `~/project` (`Manager.registerGraphify`, gated on `context.graphify_enabled`) —
  because `--project` writes into the bind-mounted project dir, which only exists at runtime
  (arch §12). REGARDLESS of the Graphify selection, when the project is not already a valid
  git working tree (detected with `git rev-parse --is-inside-work-tree`, dropping a dangling
  `.git` gitlink file first) `registerGraphify` runs `git init` so every workspace is
  git-backed; when Graphify is selected it then runs `graphify hook install`
  on EVERY start for a valid repo (the hook is idempotent, so there is deliberately no
  marker, and it runs even for an omp/hermes-only project). Graphify's headless LLM
  backend is an Ollama model chosen at `ai create` (`agent.graphify_model`, §12.4),
  routed through the gateway as `ollama/<model>`. **Neither Python nor Node is
  a `--stacks` option** — both are baked into the base (the no-op `python`/`node`
  stack snippets were removed entirely), so the selectable stacks are `go`, `rust`,
  `java`, `maven`, `deno`, and the agent-CLI snippets now only `npm install` their
  CLI (Node itself is in the base).
* the stack list is **extensible** — adding `stacks/<name>/Dockerfile.snippet`
  makes `<name>` selectable
* after creation the project owns its Dockerfile; templates/snippets are no
  longer consulted
* there is no snapshot versioning, storage, or pinning

---

## 1.6 MCP Registry — Removed

There is no platform MCP registry. Most MCP servers are configured and run by the
in-workspace agent (architecture §12). The one exception: for the agent configs the
platform rewrites whole on every start (codex/hermes/omp), it INJECTS the
enabled AI tools' MCP servers (code-review-graph/codebase-memory-mcp/graphify) directly
into that render so the rewrite doesn't clobber them (architecture §12); the CLIs whose
configs the platform does not own get those same servers via the tools' own native install.
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
subdomains (`litellm.<domain>` → LiteLLM admin UI and `valkey.<domain>` →
RedisInsight; Open WebUI is now a per-workspace in-VM app and Odysseus was
removed) to `127.0.0.1`, so the nginx
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
.ai-platform/
  Dockerfile           # tracked — environment (architecture §25)
  config.yaml          # tracked — project config (§12.4)
  profile.yaml         # tracked — project profile (language/toolchain)
  project.yaml         # tracked — { name, os, created } (§12.1)
  agents/  skills/  prompts/  projects/   # shared resource pool (§12.1c) — dirs
                       #   scaffolded empty; populated at workspace start (Caveman
                       #   install + Graphify + symlinks into each CLI's dir).
                       #   Caveman is NOT platform-seeded/git-tracked (architecture §9)
  .gitignore           # ignores run/
  run/                 # gitignored — host-local runtime state
    workspaces/<workspace-id>.json   # (§12.2)

# per-CLI provider configs — KEYLESS, written at workspace start (§12.1c, architecture §15):
.opencode/opencode.json         # opencode provider config; apiKey "{env:AIP_GATEWAY_KEY}"
.claude/settings.json           # claude-code env block (base URL only; token via env)
.codex/config.toml              # codex provider block (key via env_key)
.omp/config.yml                 # omp provider order + default model (models.yml is GLOBAL in-VM ~/.omp/agent/models.yml)
.omp/mcp.json                   # omp MCP servers — injected AI-tool MCP servers, rewritten whole at start
.hermes/…                       # hermes: config is the GLOBAL in-VM ~/.hermes/config.yaml (keyless)
# (gemini is ENV-only — no on-disk provider file; copilot manages its own ~/.copilot)
```

### 12.1c Per-CLI project configs + shared resource pool

The old `<project>/.ai-platform/agents/` keyless-template layer is **retired**. Instead,
at workspace start the platform writes each agent CLI's provider config at **that CLI's
own default per-project location** under the project root (in the bind-mounted project
dir = host disk, so the project is self-describing and portable). Every on-disk config is
**KEYLESS** — the scoped virtual key is env-supplied and never written to disk — and any
existing file is deep-merged so the managed block wins while the user's other keys survive:

* **opencode** → `.opencode/opencode.json` (`apiKey: "{env:AIP_GATEWAY_KEY}"`); the in-VM
  agent env file exports `OPENCODE_CONFIG` to point opencode at it.
* **claude-code** → `.claude/settings.json` — an `env` block with `ANTHROPIC_BASE_URL`;
  the token stays in the exported `ANTHROPIC_AUTH_TOKEN` env var (no key in the file).
* **codex** → `.codex/config.toml` (keyless, `env_key = "AIP_GATEWAY_KEY"`,
  `wire_api = "responses"`) + a global in-VM `~/.codex/config.toml` trust entry so codex
  loads the project config.
* **gemini** → env-only (`GOOGLE_GEMINI_BASE_URL` + `GEMINI_API_KEY` in the in-VM agent
  env file; no settings key for a base URL exists).
* **omp** (a Pi fork) → the GLOBAL in-VM `~/.omp/agent/models.yml` (YAML, keyless — the
  provider `apiKey` names the `AIP_GATEWAY_KEY` env var, `openai-models-list` discovery)
  + `.omp/config.yml` (provider order + seed-then-remember `modelRoles.default`). These
  use the `.yml` extension because omp documents those paths that way (the one external-tool
  exception to the platform's `.yaml` rule).
* **copilot** (GitHub Copilot CLI) → NO platform-written config (forced-OAuth /
  gateway-incapable; it manages its own `~/.copilot`).

**Injected MCP servers.** The three configs the platform rewrites whole on every start —
codex's `.codex/config.toml`, hermes's global `~/.hermes/config.yaml`, and omp's
`.omp/mcp.json` — carry the enabled AI tools'
(code-review-graph/codebase-memory-mcp/graphify) MCP servers, injected into the render so
the start-time rewrite can't clobber them (architecture §12). CLIs whose configs the
platform does not own (claude-code/opencode/gemini/copilot) get those servers via the
tools' own native `install --platform`/auto-detect.

The scoped virtual key lives **only** in the in-VM agent env file
`~/.config/aip/agent-env.sh` (off host disk), sourced by every shell + agent session
(architecture §15). A user's edits to the on-disk configs survive restart (present files
are deep-merged, never clobbered).

**Shared resource pool.** `<project>/.ai-platform/{agents,skills,prompts,projects}` holds
ONE copy of the project's agents / skills / prompts. At workspace start (BEFORE Graphify
registration) each pool is symlinked (relative) into each **installed** CLI's real
per-project dir: `skills` → `.opencode/skills`/`.claude/skills`; `agents` →
`.opencode/agents`/`.claude/agents`; `prompts` →
`.opencode/commands`/`.claude/commands`/`.gemini/commands`. Kinds a CLI has
no concept for are skipped (codex/gemini have no skills/agents). Caveman
(`skills/caveman/SKILL.md`) is thereby shared to every skills-capable client. *(hardware
bring-up: the symlinks resolving in-VM, and each CLI honouring its project config, are not
yet verified live.)*

The **Caveman** agent skill is **not** seeded at project creation and **not**
git-tracked. It is installed into the shared `.ai-platform/skills` pool at
workspace **start** by its own upstream installer (`registerCaveman`,
once-guarded, network-bound, best-effort — `Scaffold` writes no caveman files),
so `skills/caveman/` appears only after the first workspace start.

* the tracked files define the environment + config
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
~/project/   (= /home/workspace/project)
```

---

## 3.1 Workspace Structure

```text id="w2"
~/project/   (= /home/workspace/project)
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
~/project   (= /home/workspace/project)
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
stacks: [go, rust]   # any subset of the available stack snippets (§1.5)
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
  tools: [opencode]        # installed agent CLIs (any subset of: opencode, omp, claude-code, codex, gemini, copilot, hermes); opencode by default
  default_tool: opencode   # default agent CLI; must be one of agent.tools
  graphify_model: qwen2.5-coder:7b  # optional: Ollama model Graphify uses (chosen at `ai create`, routed through the gateway as ollama/<model>); omitted = none
context:
  strategy: balanced       # Headroom input compression: conservative | balanced | aggressive
                           # (mapped to Headroom per-request knobs keep_turns/output_buffer_tokens)
  caveman_level: full      # Caveman output compression: lite | full | ultra | wenyan
  caveman_enabled: true    # AI tools — ONE `--tools` multi-select at create (§1.5); the create-default
                           # selection is caveman + graphify + code-review-graph ON, codebase-memory OFF
  graphify_enabled: true   # install Graphify (conditional Dockerfile snippet, §1.5) + register it per CLI at start
  code_review_graph_enabled: true    # install code-review-graph (code-review-graph.com) + register it
                                     # as an MCP server with each installed agent CLI at start
  codebase_memory_enabled: false     # OPT-IN: install codebase-memory-mcp (DeusData/codebase-memory-mcp) +
                                     # register it as an MCP server with each installed agent CLI at start
workspace:
  cpu_limit: 4             # microVM resource limits wired into `msb create --cpus/--memory`
  memory_limit: 8          # memory in GB (a plain number; a 512M/4G suffix still works); empty falls back to the microVM default (4G)
  disk_limit: 16           # writable rootfs (OCI overlay upper) size in GiB — sizes the in-VM
                           # disk holding containerd's image store (so multi-GB in-VM app images
                           # fit); applied at create via the SDK's WithOCIUpperSize, changeable
                           # later with `ai resize`; empty falls back to the workspace default
  shell: bash              # default interactive shell (bash | zsh), chosen at `ai create --shell`; applied at every start
microsandbox:
  idle_timeout: 24h        # `msb create --idle-timeout`; default set by `ai create`, editable later
network:                   # workspace networking (arch §29.6); all fields managed via `ai network`
                           # enforced as a Microsandbox NetworkPolicy (no egress proxy); DNS-audited
  egress: public           # public (DEFAULT, allow-outbound; private ranges still blocked) | deny | unrestricted
                           # empty == "public" (the model gateway is always reachable in every mode)
  allow_host_services:     # external destinations the workspace may reach (host/IP/domain or "gateway")
    - { host: gateway, port: 5432 }
  publish_ports:           # host → workspace port maps
    - { guest: 3000, host: 3000 }
apps:                      # opt-in in-VM AI apps (arch §7), chosen via `ai create --apps`/`--app-port` / `ai apps add`
                           # each runs as a rootful nerdctl container in the workspace microVM, gateway-routed,
                           # published on a unique host port (stable across restarts; seeded to the app's
                           # familiar container port — Open WebUI 8080 — when free, else
                           # auto-allocated from the 21000–21999 window — internal/apps/ports.go)
  - { key: openwebui, port: 8080 }    # key one of: openwebui
agent_dashboards:          # agent-CLI web dashboards (currently only hermes — `hermes dashboard`, default 9119);
                           # prompted for a host port at create when the CLI is selected (--app-port <cli>=<port>),
                           # published from the microVM the same way as apps (host==guest); reuses the AppEntry shape
  - { key: hermes, port: 9119 }
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
* `domain`: the platform base domain the nginx UI subdomains hang off
  (`litellm.<domain>` and `valkey.<domain>`); empty resolves to the default
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
    "valkey":       { "mode": "container", "image": "valkey/valkey", "tag": "9.1.0-alpine" },
    "redisinsight": { "mode": "container", "image": "redis/redisinsight", "tag": "latest" },
    "proxy":        { "mode": "container", "image": "nginx", "tag": "stable-alpine3.23-slim" },
    "dns":          { "mode": "container", "image": "coredns/coredns", "tag": "latest" }
  }
}
```

* `mode`: `container` | `native`
* This file is the **source of truth** for the service-tier image references:
  container services are pinned by **image + tag** — most use the `latest` tag
  (e.g. `litellm` → `ghcr.io/berriai/litellm:latest`, which satisfies the in-process
  `headroom` compression guardrail's LiteLLM v1.92.x+ requirement), while
  some are intentionally pinned to a specific tag
  (`litellm-db` → `postgres:18.4-alpine3.23`, `proxy` →
  `nginx:stable-alpine3.23-slim`, `valkey` → `valkey/valkey:9.1.0-alpine`) — **not by
  digest** (digests are platform/arch specific, so a digest pin breaks
  cross-platform pulls).
* The **native microsandbox runtime is NOT pinned here**: it is not an image the
  platform pulls, so it is deliberately excluded from this image pin set (it keeps a
  registry slot only for its log scope). It is instead **platform-managed and pinned
  in code** — `internal/workspace/msb.go` pins the `msb` CLI to `microsandboxVersion`
  (matched to the go.mod Microsandbox SDK pin) and `MsbBinary` downloads the matching
  fork binary (sha256-verified against `msbAssets`) into `~/.ai-platform/bin/msb`
  (`paths.BinDir`) on first use, so it is NOT a user-installed PATH prerequisite and
  `sandbox.Detect` counts the platform-managed binary as installed. Native pins
  (`version` + `sha256`) in this file are reserved for any future genuinely-pinned
  native component managed through `versions.yaml`.
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
