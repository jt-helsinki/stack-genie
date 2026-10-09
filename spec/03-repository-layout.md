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
├── agents/        # reserved — created eagerly, not written to by any code path today
├── audit/         # reserved for audit logs (see §1.4) — created eagerly, not written to today
├── bin/           # vestigial — no code path creates or writes into it today (see §1.9)
├── cache/         # re-fetchable caches: catalog.yaml (models.dev); see §1.3
├── config/        # global settings + projects index (no per-project state)
├── logs/          # per-service-container log snapshots (see §6)
├── overlays/      # reserved — created eagerly, not written to by any code path today (see §1.10)
├── prompts/       # reserved — created eagerly, not written to by any code path today
├── skills/        # reserved — created eagerly, not written to by any code path today
├── templates/     # reserved — created eagerly, not written to by any code path today (see §1.5)
├── tools/         # reserved — created eagerly, not written to by any code path today
├── venv/          # platform-managed host Python venv (omlx; see §1.9/§12.6a) — created on first use
└── volumes/       # ALL host-persisted SYSTEM data volumes — created on first use
    ├── litellm-db/  # LiteLLM Postgres data dir (bind-mounted → aip-litellm-db:/var/lib/postgresql)
    └── models/      # local-model store
        └── omlx/    # host-native omlx weights (MLX-format model dirs, macOS + Apple Silicon only)

~/.ai-platform/.ai-platform.env   # OPT-IN, 0600 sibling file (NOT under ~/.ai-platform/)
```

`agents/`, `audit/`, `prompts/`, `skills/`, and `tools/` are created eagerly by
`ai setup` (`internal/layout`), but as of this writing no code path writes into
them — they are reserved placeholders, not live state. `overlays/` and
`templates/{dockerfiles,stacks}` are ALSO created eagerly by `internal/layout`
but are now equally dead: the packages that used to populate them
(`internal/overlay`, the per-workspace persistent VM-disk layer; and
`internal/envimage`/`internal/templates`, the OCI-image/Dockerfile-template
build) were deleted when workspaces moved from Microsandbox microVMs to **nono**
(https://nono.sh) host-process sandboxing — there is no VM disk to overlay and
no image to build anymore, so `internal/layout`'s eager-create list simply
hasn't been pruned of their directories yet. `venv/` and `volumes/` are, by
contrast, real and populated, created **on demand** by their own consumers
(`internal/pyenv` and the omlx/LiteLLM-DB writers respectively) rather than
eagerly by `ai setup`. `bin/` is not even in `internal/layout`'s eager-create
list anymore (see §1.9) — on a fresh install nothing creates it at all; it
survives only as a legacy artifact on a host upgraded from a pre-nono install,
and `ai uninstall` still best-effort checks it for a leftover `msb` binary to
report. There is no longer a real per-project shared resource pool either: the
old `<project>/.ai-platform/{agents,skills,prompts}` pool and its
start-time symlinking into each CLI's config dir are gone along with the
`internal/workspace` code that used to create them — see §2.2.

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
(config files — the LiteLLM/DNS/nginx configs under `config/` — are NOT system
volumes and stay where they are). Today there are two:

- `~/.ai-platform/volumes/litellm-db/` — the LiteLLM **Postgres data dir**,
  **HOST-BIND-MOUNTED** into `aip-litellm-db` at `/var/lib/postgresql` (NOT a
  Docker named volume). This is the one stateful service-tier piece.
- `~/.ai-platform/volumes/models/` — the **persistent local-model store** for the
  host-side **omlx** backend (macOS + Apple Silicon only, the sole local-inference
  runtime). Model download/add/remove/tune happens entirely through omlx's OWN
  admin panel (`http://127.0.0.1:8100/admin`, reached via `ai services console
  omlx`); models persist under `~/.ai-platform/volumes/models/omlx/`
  (`omlx.StoreDir`, MLX-format model directories). It persists under the
  standardized system-volume home and is removed by `ai uninstall --purge`
  (distinct from the disposable `cache/models/` in §1.3).

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
* `ai state repair` (`internal/state.Repair`) drops `projects.yaml` index
  entries whose project no longer exists on disk and ensures each remaining
  project's `run/` shard directories exist; discovery uses `projects.yaml`
  (VCS is out of scope — no git is consulted). It does not reconcile
  individual workspace/session handles against a live runtime today.

---

## 1.3 Cache Layer

```text id="h5"
~/.ai-platform/cache/
```

```text id="h6"
catalog.yaml          # models.dev catalog (fetched as JSON, persisted as YAML; legacy catalog.json one-shot converted) — the only entry the current code creates
models/                # reserved — not created by any code path today
downloads/             # reserved — not created by any code path today
temp/                  # reserved — not created by any code path today
```

Rules:

* fully disposable — all entries are **re-fetchable** copies, not SYSTEM data
* may be rebuilt at any time (`catalog.yaml` is re-downloaded
  on next use, falling back to the cached copy only while the source is unreachable)
* `catalog.yaml` is the models.dev catalog fetched as JSON and **persisted as YAML**;
  a legacy `catalog.json` (or the older `volumes/catalog.json`) is **one-shot
  converted** to YAML (`catalog.Path`); `volumes/` is now ONLY true host data
* `models/`, `downloads/`, and `temp/` are reserved for a possible future
  model-library cache; today local-model management lives entirely in omlx's own
  admin panel (no curated list, no `ai models pull` equivalent on this platform's
  side), so nothing writes here yet
* never contains secrets
* removed by `ai uninstall --purge` (which `RemoveAll`s `~/.ai-platform`)

---

## 1.4 Audit Logs

```text id="h7"
~/.ai-platform/audit/
```

Intended to contain immutable logs (e.g.):

```text id="h8"
workspace-create.log
workspace-destroy.log
agent-events.log
security-events.log
```

**Not yet implemented as of this writing:** the directory is created (empty) by
`ai setup`/`internal/layout`, but no code path writes into it — there is no
audit-log writer today, so the filenames above describe intent, not observed
behavior. (Point-in-time service **log capture** is a separate, real mechanism —
see §6 — and is not audit logging in this sense.)

Rules (once implemented):

* append-only
* no secret material stored
* used for debugging and compliance

---

## 1.5 OS Dockerfile + Software-Stack Templates — Removed

There is no per-project image anymore, so there is nothing to template. This
whole layer (`internal/templates`, the per-OS Dockerfile skeletons, the
per-software-stack snippets, the per-agent-CLI install snippets, and the
opt-in-tool Dockerfile snippets) was **deleted** when workspaces moved from
Microsandbox microVMs (one OCI image built per project, run as a guest kernel)
to **nono** (https://nono.sh) host-process sandboxing: an agent CLI now runs
directly as a sandboxed process on the HOST, using whatever toolchains are
already installed there — there is no OS choice at `ai create`, no `--stacks`
selection, no per-workspace language-runtime provisioning, and no rootful
in-VM container runtime (containerd/nerdctl/buildkit) to bootstrap.

What replaced it:

* **Agent CLIs install directly onto the host.** `internal/setup/agentcli_host.go`
  (`EnsureHostAgentCLIsInstalled`, run at `ai setup`) installs claude-code,
  codex, and opencode via each project's own official installer script when
  the binary is not already on `PATH` (`curl -fsSL https://claude.ai/install.sh
  | bash`, `curl -fsSL https://chatgpt.com/codex/install.sh | sh`,
  `curl -fsSL https://opencode.ai/install | bash`) — Detect-gated and
  best-effort, never failing setup. omp/gemini/hermes are expected to already be
  on `PATH` (no installer is wired for them).
* **nono itself installs the same way** — `internal/setup/nono_host.go`
  (`EnsureNonoInstalled`) runs `brew install nono` when Homebrew is present,
  else nono's own `curl -fsSL https://nono.sh/install.sh | sh` — see §1.9.
* **AI tools are config toggles, not Dockerfile snippets.** `ai create --tools`/
  `ai tools enable|disable` set the `context.*_enabled` flags in §12.4's
  `config.yaml`; **as of this writing no verified code path in
  `internal/workspace` actually installs or registers caveman, graphify,
  code-review-graph, codebase-memory-mcp, openviking, tokensave, agentmemory,
  ponytail, or agent-skills** — the install/registration steps that used to run
  at workspace start in the Microsandbox era (`registerCaveman`,
  `registerGraphify`, the MCP-injection wiring) were not found wired into the
  current `internal/workspace` package (which now only launches/attaches/stops
  nono sessions and writes each CLI's keyless provider config — see §2.2). The
  toggles persist and round-trip through `ai tools list`, but whether flipping
  one currently has any runtime effect beyond that could not be confirmed from
  the code.

---

## 1.6 MCP Registry — Removed

There is no platform MCP registry; MCP servers are the in-workspace agent's own
concern. `internal/agentcfg` still carries the MCP-injection helpers built for
the Microsandbox era (`EnabledMCPServers`, `AppendCodexMCP`, `InjectHermesMCP` —
each still unit-tested in `agentcfg/mcp_test.go`), but a grep of
`internal/workspace`'s current config writers (`writeCodexConfig`,
`writeHermesConfig`, `writeOmpConfig`) shows none of them call
`EnabledMCPServers` — the per-tool MCP injection into codex/hermes/omp's
rewritten-whole configs is **not currently wired up**. Any provider
credentials an MCP server needs would in any case be resolved through the
keys-in-LiteLLM credential store, never stored on platform disk (architecture
§17).

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
ui.yaml                  # TUI theme + mouse-capture preference
litellm/                 # rendered LiteLLM config.yaml (placeholders only; real keys live in the gateway)
proxy/                   # rendered nginx.conf for the aip-proxy gateway (the litellm.<domain> UI vhost + gateway paths)
dns/                     # rendered CoreDNS config for the aip-dns egress-audit resolver
<service>/               # one rendered-config dir per service-tier service (created at reconcile)
```

Rules:

* a config dir is created **per service-tier service** at reconcile (e.g.
  `litellm/`, `proxy/`, `dns/`, `presidio-analyzer/`, …); the ones
  that have a rendered file today are LiteLLM (`config.yaml`), the nginx gateway
  (`proxy/nginx.conf`), and the CoreDNS resolver (`dns/`). omlx is host-native
  (one shared `omlx serve` process, macOS + Apple Silicon only, no `aip-*`
  inference container) and has no rendered service-config dir here
* every `<service>/` config is **rendered** by the CLI from the platform
  config; not hand-edited (architecture §5, Host Services Control Plane)
* contains **no secrets** — only placeholders; real provider credentials live in
  the LiteLLM gateway (env passthrough / its Postgres-backed store), never here
* native-service binaries live under `~/.ai-platform/tools/`, not here

In **standalone** mode `ai setup` also writes an AI-platform-owned block to
`/etc/hosts` (a privileged write, outside `~/.ai-platform/`) mapping the UI
subdomains (`litellm.<domain>` → LiteLLM admin UI and `valkey.<domain>` →
RedisInsight — there is no Open WebUI vhost anymore: it was an in-VM app under
the now-deleted `internal/apps`, and Odysseus was removed outright) to
`127.0.0.1`, so the nginx
gateway's vhost resolves locally. Only the platform's delimited block is touched;
`ai uninstall` removes it. (`internal/hostsfile` writes the managed block;
`internal/uihosts` computes the entries from the service registry.)

## 1.9 Tools

There is no platform-managed, checksum-verified binary pin anymore. The
Microsandbox `msb` CLI this section used to describe
(`~/.ai-platform/bin/msb`, pinned in `internal/workspace/msb.go` and
sha256-verified on download) is **gone** along with the microVM runtime it
drove — `internal/workspace/msb.go` and `internal/sandbox` were both deleted.
In its place, workspaces are sandboxed by **nono** (https://nono.sh), which the
platform expects as a plain `PATH` binary:

* `internal/setup/nono_host.go` (`EnsureNonoInstalled`, run at `ai setup`)
  installs it when absent via `brew install nono` (preferred, when Homebrew is
  present) or nono's own official installer (`curl -fsSL
  https://nono.sh/install.sh | sh`) — wherever either of those put it (e.g.
  Homebrew's prefix), NOT into `~/.ai-platform/bin/`. Detect-gated and
  best-effort; a manual re-run of `ai setup` retries a failed/skipped install.
* `internal/nono.Detect` just does an `exec.LookPath("nono")` — there is no
  version pin, no checksum verification, and no platform-managed copy to keep
  in lockstep with anything (unlike the old `msb`/Go-SDK version-matching
  requirement).

`~/.ai-platform/bin/` is consequently vestigial: it is not even in
`internal/layout`'s eager-create directory list anymore, and nothing in the
current codebase writes into it. It survives only as a leftover on a host
upgraded from a pre-nono install, and `ai uninstall` still best-effort looks
for `~/.ai-platform/bin/msb` there to report/clean up (`internal/uninstall`'s
`msbBinary`/legacy-cleanup path), purely for that upgrade case. (omlx
continues to run from the platform Python venv `~/.ai-platform/venv` — see
§1.1 and §12.6a — which is unrelated to this and unchanged.) The top-level
`~/.ai-platform/tools/` directory named in §1.1 is created by `ai setup` but,
as of this writing, is not written to by any code path (reserved).

## 1.10 Overlays — Removed

There is no per-workspace persistent writable layer anymore. nono sandboxes a
single host process per invocation (Landlock on Linux, Seatbelt on macOS) —
there is no VM disk, no guest/host split, and therefore nothing to overlay:
the project directory on the host **is** the persistent state, written to
directly by whatever nono session runs there. The `internal/overlay` package
(which used to manage `~/.ai-platform/overlays/<workspace-id>/`) was deleted
in full. The `overlays/` directory named in §1.1 is still eagerly created by
`internal/layout` (not yet pruned from its list) but nothing writes into it —
it is dead weight, not live state. §12.8's `overlay.json` record is likewise
fully retired, not merely aspirational.

---

# 2. Project Layout (Host)

A project lives wherever the user chooses. `ai create`'s **location** input
(`--location`, or the wizard's location field) — **defaulting to the current
working directory** — creates the project there if it does not already exist;
a location may not be, or be nested inside, an existing project. There is no
fixed project root: `~/projects/<name>` (`project.RootPath`,
`paths.ProjectsDir`) survives in code only as a fallback used when a spec's
`Root` is left unset, but `ai create` always sets `Root` explicitly to the
chosen location, so that fallback is not exercised on the real create path.

```text id="p1"
<chosen-location>/<project-name>/     # e.g. ~/projects/my-project/, or any other directory
```

---

## 2.1 Project Root

```text id="p2"
my-project/                 # wherever the user chose to create it
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
  config.yaml          # tracked — project config (§12.4)
  project.yaml         # tracked — { schema_version, name, os, created } (§12.1)
  .gitignore           # ignores run/
  run/                 # gitignored — host-local runtime state
    workspaces/<session-id>.yaml   # one file per launched agent/shell session (§12.2)
  nono-profile-<cli>.json   # host-local, regenerated on every launch — NOT covered by .gitignore (see below)

  # NOT present: there is no Dockerfile (nothing to build), no profile.yaml
  # (no software-stack selection), and no shared agents/skills/prompts/projects
  # resource pool (the code that used to create and symlink it was removed —
  # see §1.5).

# per-CLI provider configs — KEYLESS, written at workspace start (§12.1c):
.opencode/opencode.json         # opencode provider config; apiKey "{env:AIP_GATEWAY_KEY}"
.claude/settings.json           # claude-code env block (base URL only; token via env)
.codex/config.toml               # codex provider block (key via env_key); oauth mode writes a gateway-less variant instead
.omp/config.yml                  # omp provider order (models.yml is GLOBAL at ~/.omp/agent/models.yml)
.hermes/…                        # hermes: config is GLOBAL at ~/.hermes/config.yaml (keyless)
# (gemini is ENV-only — no on-disk provider file)
```

`project.DockerfileTools` and `agentArtifactDirs` in `internal/project/project.go`
still enumerate `.opencode`, `.claude`, `.codex`, `.omp`, `.gemini`, `.hermes`, and
`.venv-msb` as the per-CLI dirs `ai delete` best-effort removes — `.venv-msb` is a
holdover from the Microsandbox era (a per-project Python venv created at workspace
start); no current code path creates one, but the delete cleanup still looks for it
in case an older project has one.

`<project>/.ai-platform/nono-profile-<cli>.json` is the project's generated **nono
profile** (`internal/nono.Profile`, JSON — nono requires JSON, not YAML; `cli` is the
agent CLI name, or `shell` for `ai shell`'s profile). It is (re)written by
`internal/workspace`'s `ensureProfile` on **every** `ai agent`/`ai shell` launch —
cheap (a few lines of JSON) so a `config.yaml` network change (`ai network
allow/deny`) takes effect on the very next launch, with no restart step. It declares:
the project's filesystem access (granted via nono's own `--allow-cwd`, not written
into the file itself), the `network` section mirrored straight from
`config.yaml`'s `NetworkConfig` (`egress`/`allow_domain`/`deny_domain`), and, for
gemini only, a non-secret `environment.set_vars.GOOGLE_GEMINI_BASE_URL` (gemini has
no config-file base-URL setting). For opencode/claude-code/codex, the profile's
`extends` field names an official nono registry pack (`nolabs-ai/opencode`,
`nolabs-ai/claude`, `nolabs-ai/codex` — best-effort pulled via `nono pull`, §12.1c);
other CLIs fall back to nono's own built-in `default` profile.

### 12.1c Per-CLI project configs

The old `<project>/.ai-platform/agents/` keyless-template layer is **retired**. Instead,
at workspace start (really: at each `ai agent <cli>` launch) the platform writes that
agent CLI's provider config at **that CLI's own default per-project location** under
the project root — a plain host directory now, not a bind mount into a guest. Every
on-disk config is **KEYLESS**: the scoped virtual key is injected by nono directly into
the sandboxed process's environment (`nono run --env-credential-map <keychain-ref>
<ENV_VAR>`, reading the OS keychain — never written to any config file, profile, or
plain host env file) and any existing file is deep-merged so the managed block wins
while the user's other keys survive:

* **opencode** → `.opencode/opencode.json` (`apiKey: "{env:AIP_GATEWAY_KEY}"`).
* **claude-code** → `.claude/settings.json` — an `env` block with `ANTHROPIC_BASE_URL`;
  the token is injected under `ANTHROPIC_AUTH_TOKEN` (no key in the file). In `oauth`
  auth mode the env block is emptied instead (`MergeClaudeSettingsOAuth`) and claude's
  own native login is used, bypassing the gateway.
* **codex** → `.codex/config.toml` (keyless, `env_key = "AIP_GATEWAY_KEY"`) PLUS a
  GLOBAL `~/.codex/config.toml` trust-entry append (`appendCodexTrust`) — a `[projects.
  "<root>"]` entry per project, idempotently appended (never overwritten wholesale,
  since this file is now shared across every project on the host rather than isolated
  per VM) — so codex trusts and loads the project config.
* **gemini** → env-only (`GOOGLE_GEMINI_BASE_URL` + `GEMINI_API_KEY`/credential
  injection; no settings key for a base URL exists).
* **omp** (a Pi fork) → the GLOBAL `~/.omp/agent/models.yml` (YAML, keyless — the
  provider `apiKey` names the `AIP_GATEWAY_KEY` env var, `openai-models-list`
  discovery) + `<project>/.omp/config.yml`. These use the `.yml` extension because omp
  documents those paths that way (the one external-tool exception to the platform's
  `.yaml` rule).
* **hermes** → the GLOBAL `~/.hermes/config.yaml`, pointed at the project's
  `.ai-platform/skills` dir via `external_dirs` (a path reference only — nothing
  currently populates that directory's contents, see §1.5).

Agent CLIs with no subscription login (opencode/omp/hermes) share the
`AIP_GATEWAY_KEY` env var name with codex; claude-code/gemini use their own
provider-specific env var names. The credential itself is stored under OS-keychain
entry name `aip-gateway-<project>` (`internal/nono.StoreCredential`/
`DeleteCredential` — macOS Keychain via `security`, Linux via `secret-tool`), stable
per project so re-minting a key overwrites it in place.

* the tracked files (`config.yaml`, `project.yaml`, `.gitignore`) define the project
  and are committable, so the project's platform-level configuration is reproducible
  from git — unlike the Microsandbox era there is no Dockerfile to make the actual
  *toolchain* reproducible: that now comes from whatever is installed on the host
* `run/` holds machine-specific session handles (nono session ids, status) and is
  gitignored. The written `.gitignore` contents are just `run/` (`project.Scaffold`) —
  **the regenerated `nono-profile-<cli>.json` files sit at the `.ai-platform/` top
  level and are NOT covered by that pattern**, so they would be tracked if a user
  commits `.ai-platform/`; this looks like a gap carried over from the migration
  rather than an intentional choice, since the files are purely host-local/machine-
  specific and are rewritten on every launch
* no platform-managed memory/history (delegated to the agent, architecture §11)

---

## 2.3 Worktrees — not platform-managed

The platform does not create or manage git worktrees. If a user runs multiple AI
agents inside a workspace and those agents use worktrees (e.g. under
`.worktrees/`), that is entirely the in-workspace agent CLI's doing (architecture
§20–21); the platform neither tracks nor cleans them up.

---

# 3. Workspace Layout — now identical to the Project Layout

There is no separate guest filesystem anymore, so there is no host/guest mount
to describe. A "workspace" is simply the registered project directory on the
HOST (§2), run directly by nono:

```text id="w2"
my-project/   (wherever the user chose to create it — same tree as §2.1)
├── .ai-platform/
├── src/
├── tests/
├── scripts/
└── docs/
```

`ai agent <cli>`/`ai shell` launch nono with `--allow-cwd` and `Dir` set to the
project root (`internal/workspace.Manager.Agent`/`Shell`), so the sandboxed
process's working directory — and the filesystem access its generated nono
profile grants — is this same directory nono runs from. There is nothing
analogous to the old `~/project` (`/home/workspace/project`) in-guest path: the
agent sees the real host path, not a remapped one.

Rules:

* the host project source is the single source of truth, read-write, with no
  copy or mount in between
* a nono **session is disposable** — it is a sandboxed invocation of one
  command, not a persistent machine; stopping/restarting one never touches
  project files
* all writes (source edits, installs the agent CLI itself makes to its own
  state dirs under the project, agent memory) land directly on the host
  project directory — there is no separate overlay layer to persist them
  through (see §1.10 — that concept no longer exists)

---

# 4. Environment Images — Removed

There is no image, no snapshot store, and no versioned image layout. nono does
not build or run a guest OS image — it sandboxes an already-installed host
binary directly (Landlock on Linux, Seatbelt on macOS). The per-project
`Dockerfile` and the per-OS Dockerfile templates this section used to describe
are gone (§1.5); so is `internal/envimage`, the package that used to compose
and build them. Whatever language/tool runtimes the agent CLI needs must
already be on the host.

---

# 5. Cache Layout

```text id="c1"
~/.ai-platform/cache/
```

```text id="c2"
catalog.yaml           # the only entry the current code creates (see §1.3)
models/                 # reserved — not created by any code path today
downloads/              # reserved — not created by any code path today
temp/                   # reserved — not created by any code path today
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

Point-in-time snapshots of each **running** service-tier container's recent
output, captured by `ai setup` / `ai services status`
(`setup.realServices.CaptureServiceLogs`, running `<runtime> logs --tail N
--timestamps <container>`) and read by `ai logs` / the TUI Logs view
(`internal/logs`, a pure file reader — it does no docker calls of its own).
One file per container, named by stripping the `aip-` prefix
(`logFileNameFor`):

```text id="l2"
litellm.log
litellm-db.log
headroom.log
proxy.log
dns.log
presidio-analyzer.log
presidio-anonymizer.log
valkey.log
redisinsight.log
```

(There is no `omlx.log` here — omlx is host-native, not a container; its
health is checked with a live HTTP probe, not a captured log file.)
Continuous following (`ai logs --service <name> --follow`) streams the live
container log directly (`<runtime> logs -f`) rather than reading this
snapshot.

---

# 7. CLI State Mapping

Per-project state lives in the project; the global index records where projects
are:

* `config/projects.yaml` (global) → `ai create` / `delete` (index)
* `<project>/.ai-platform/project.yaml` → `ai create`
* `<project>/.ai-platform/run/workspaces/*.yaml` → `ai start`/`stop`/…

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
* a project's platform-level configuration (installed agent CLIs, egress
  policy, enabled context/AI-tool toggles) is fully described by its
  `.ai-platform/config.yaml` — there is no environment/toolchain description to
  reconstruct beyond that, since nono runs the host's own installed tools
  directly rather than a platform-built image
* a project's nono profile(s) (`.ai-platform/nono-profile-<cli>.json`) are
  regenerated, not hand-maintained — they always reflect the current
  `config.yaml` on the next launch
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
  "os": "darwin",
  "created": "2026-06-18T10:00:00Z"
}
```

Note `os` is no longer a user choice: `project.Scaffold` always sets it to
`runtime.GOOS` (whatever platform `ai create` itself ran on) — there is no OS
selection step in the create wizard anymore (§1.5).

* tracked in git; the project's path is recorded in the global
  `config/projects.yaml` index (§12.7), not here

## 12.1a `<project>/.ai-platform/profile.yaml` — Removed

There is no `profile.yaml` and no software-stack selection anymore — see §1.5.

## 12.2 `<project>/.ai-platform/run/workspaces/<session-id>.yaml` (gitignored)

Written atomically by `internal/state.Store.SaveWorkspace` at
`filepath.Join(runDir, "workspaces", workspace.ID+".yaml")` (YAML-encoded, like
every other platform-generated state file — see the Conventions note in
AGENTS.md: everything the platform writes is `.yaml`, never `.json`):

```yaml id="sc2"
schema_version: 1
id: 3f2a9c7e-...        # the nono session id RunDetached returned, NOT "aip-<name>"
project: my-app
microsandbox_id: ""     # vestigial field name from the Microsandbox era — always empty; never set by current code
status: started
created: "2026-06-18T10:00:05Z"
last_started: "2026-06-18T10:00:05Z"
```

* `status`: `created` | `started` | `stopped` | `archived` | `destroyed`
  (`internal/state.WorkspaceStatus` still carries all five values, though
  `internal/workspace.Manager` today only ever writes `started` from `Agent`
  and `stopped` from `StopAll`)
* host-local runtime handle — gitignored
* `id` is the **nono session id** returned by `nono run --detached` (an opaque
  string nono assigns, not a platform-chosen name) — `Manager.Agent` writes a
  **new** handle file on every launch, since the filename is derived from that
  per-launch session id. This differs from the old one-handle-per-project
  model the field names (and this section's title) still suggest: nothing
  currently prunes or consolidates these files, so `run/workspaces/` can
  accumulate one `.yaml` per `ai agent` invocation over a project's lifetime.
  `StopAll` additionally writes a `stopped`-status handle with an **empty**
  `id` (so its filename is literally `.yaml`), not into the most recent
  session's file. `project.workspaceHandle` (used by `ai list`'s status
  column) separately computes `id = "aip-" + name` and looks for a handle
  matching that `id` — since real written ids are nono session ids, not
  `"aip-<name>"`, this lookup likely never matches a handle `Agent` wrote; this
  looks like an unresolved inconsistency from the migration rather than
  intentional behavior, and was not confirmed either way by running the code.

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
os: darwin                 # auto-detected runtime.GOOS at `ai create` — no longer a user choice; not os|arch-validated
agent:
  tools: [opencode]        # installed agent CLIs (any subset of: opencode, omp, claude-code, codex, gemini, hermes); opencode by default
  default_tool: opencode   # default agent CLI; must be one of agent.tools (defaults to agent.tools[0])
  graphify_model: qwen2.5-coder  # optional: local model NAME already served by omlx that Graphify uses
                           # (chosen at `ai create`, no download step; routed through the gateway as omlx/<model>); omitted = none
  auth_modes:              # per-CLI auth mode for the OAuth-capable CLIs only (claude-code, codex, gemini);
                           # "api-key" (default, routes through the gateway) | "oauth" (native login, bypasses
                           # the gateway and its guardrails); set via `ai create --auth-mode cli=mode`
    claude-code: oauth
  hermes_dashboard_password: ""  # plaintext basic-auth password for the hermes web dashboard (hermes dashboard) —
                           # a low-sensitivity LOCAL credential, not a provider/gateway secret. The field and its
                           # hashing helper (agentcfg.HermesDashboardPasswordHash) exist, but
                           # internal/workspace's writeHermesConfig currently always passes an empty password —
                           # nothing in the verified code path reads or generates this value yet.
context:
  strategy: balanced       # Headroom input compression: conservative | balanced | aggressive
                           # (mapped to Headroom per-request knobs keep_turns/output_buffer_tokens)
  caveman_level: full      # Caveman output compression: lite | full | ultra | wenyan
  caveman_enabled: false   # AI tools — ONE `--tools` multi-select at create (§1.5), 9 keys total; ALL
                           # opt-in with NO create-default selection (create.DefaultAITools() returns
                           # nil) — a vanilla `ai create` writes every context.*_enabled key false. A
                           # nil (never-written) value is the only case that still defaults true, for
                           # caveman/graphify, to preserve pre-toggle projects' original auto-install
                           # behavior (CavemanEnabledOrDefault/GraphifyEnabledOrDefault); every other
                           # key defaults false when unset. See §1.5 for the (unverified) install story.
  graphify_enabled: false  # toggle for Graphify
  code_review_graph_enabled: false   # toggle for code-review-graph (code-review-graph.com)
  codebase_memory_enabled: false     # toggle for codebase-memory-mcp (DeusData/codebase-memory-mcp)
  openviking_enabled: false          # toggle for OpenViking (docs.openviking.ai)
  tokensave_enabled: false           # toggle for tokensave (github.com/aovestdipaperino/tokensave)
  agentmemory_enabled: false         # toggle for agentmemory (github.com/rohitg00/agentmemory)
  ponytail_enabled: false            # toggle for the Ponytail skill (github.com/DietrichGebert/ponytail)
  agent_skills_enabled: false        # toggle for addyosmani/agent-skills
workspace:
  shell: bash              # default interactive shell `ai shell` launches inside the nono sandbox (bash | zsh);
                           # chosen at `ai create --shell`. There is no vCPU/memory/disk/idle-timeout/
                           # isolated_dirs/shared_mounts to configure anymore — nono sandboxes a single host
                           # process with no VM to size and no second "guest view" to mount into (§3).
network:                   # per-project nono egress policy, edited via `ai network show|egress|allow|disallow`;
                           # mirrors internal/nono.Profile's own `network` section shape directly —
                           # internal/egress compiles this straight into the generated nono profile
  egress: public           # public (DEFAULT — unrestricted outbound, nono's own default) | deny (nono --block-net)
  allow_domain:            # bare domains or "*.suffix" wildcards (nono --allow-domain); meaningful when egress
                           # is deny (carves exceptions into an otherwise-blocked default) — no port, no "gateway"
                           # special case (the model gateway is reachable regardless of egress mode)
    - api.github.com
  deny_domain: []          # bare domains or "*.suffix" wildcards (nono --deny-domain)

# NOT present: there is no microsandbox/apps/agent_dashboards block, no
# cpu_limit/memory_limit/disk_limit, no isolated_dirs/shared_mounts, no
# allow_host_services/publish_ports — all retired along with the microVM
# runtime, published ports, and in-VM apps (§1.5, §3, §4).
```

## 12.5 `config/runtime.yaml` (platform-global, non-project)

```json id="sc7"
{
  "schema_version": 1,
  "role": "standalone",
  "detected": "docker",
  "rootless": true,
  "nono": { "installed": true },
  "guardrails": ["headroom"],
  "ai_platform_host": "host.local",
  "host_gateway": "127.0.0.1",
  "domain": "aip.local",
  "detected_at": "2026-06-18T09:59:00Z"
}
```

`microsandbox: { available, virtualization }` is **replaced** by `nono: {
installed }` (`runtime.NonoInfo`) — unlike the Microsandbox slot it replaces,
there is no separate "virtualization available" probe: nono's Landlock
(Linux)/Seatbelt (macOS) enforcement works on any host the binary runs on, with
no Apple-Silicon-only (or any hardware-virtualization) restriction. `detected`/
`rootless` describe the Docker/Podman **service tier** (unchanged by this
migration — still required for LiteLLM/Headroom/etc., see AGENTS.md). Also
note `host_gateway`'s default is now plain loopback (`runtime.
DefaultGatewayHost = "127.0.0.1"`), not a guest-network DNS name — a
nono-sandboxed agent runs directly on the host, so there is no guest/host
network translation to name.

* `role`: `standalone` | `server` | `client` (chosen by `ai setup`; absent on a
  pre-role runtime.yaml). Drives the service bind host and which prereqs are
  required — `client` now only needs `nono` installed (not `msb` +
  hardware-virtualization support); `server` needs neither (no workspaces run
  there).
* `optional_services`: the opt-in HOST services enabled at setup; the optional
  mechanism is retained but there are currently **no** optional host services
  (Open WebUI was an in-VM app under the now-deleted `internal/apps`, and
  Odysseus was removed outright — in-VM apps are out of scope entirely now,
  not replaced by anything), so this field is normally omitted.
* `guardrails`: the set of LiteLLM guardrail keys enabled on the gateway,
  chosen from the picker at `ai setup` and persisted machine-wide — only
  `headroom` (input-compression) is on by default; `presidio` (secret
  masking), `detect-secrets`, and the destructive-command tool firewall are
  opt-in. A non-nil value (including an explicit empty list) means the user
  has made a choice; absent falls back to the built-in default set.
* `ai_platform_host`: the machine-wide gateway address every workspace routes
  through (`ai gateway set` / `ai setup --mode client --server`).
* `host_gateway`: the host address a nono-sandboxed agent reaches the gateway
  at, default plain loopback `127.0.0.1` (`runtime.DefaultGatewayHost`) — there
  is no guest network anymore, so (unlike the Microsandbox-era
  `host.microsandbox.internal`) this is just the literal host address.
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
    "presidio-analyzer":   { "mode": "container", "image": "mcr.microsoft.com/presidio-analyzer",   "tag": "latest" },
    "presidio-anonymizer": { "mode": "container", "image": "mcr.microsoft.com/presidio-anonymizer", "tag": "latest" },
    "valkey":       { "mode": "container", "image": "valkey/valkey", "tag": "9.1.0-alpine" },
    "redisinsight": { "mode": "container", "image": "redis/redisinsight", "tag": "3.8" },
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
* **nono (the workspace sandboxing runtime) is NOT pinned here, and is not pinned
  anywhere.** It is not an image the platform pulls, so it is excluded from this
  image pin set; unlike the Microsandbox `msb` CLI this file's pin set used to
  exclude for a different reason (that one WAS platform-managed-and-pinned, just
  not through this file — `internal/workspace/msb.go`, now deleted, pinned an
  exact version in code and downloaded a checksum-verified binary into
  `~/.ai-platform/bin/`), nono has **no platform-managed pin or download at
  all**: `internal/setup/nono_host.go` installs it via `brew install nono` or
  nono's own official curl installer (§1.9), and `internal/nono.Detect` is a
  bare `exec.LookPath("nono")` with no version or checksum check. Native pins
  (`version` + `sha256`) in this file remain reserved for any future
  genuinely-pinned native component managed through `versions.yaml`.
* `ai setup` **resolves every service-tier image from this file** (via
  `internal/setup`'s `containerImage`, falling back to the built-in
  `versions.Default()` pins when the file is absent or an entry is incomplete);
  `--upgrade` re-writes it to this binary's defaults and re-reconciles

## 12.6a `config/model-runtimes.yaml` — Removed

The old per-model vLLM serving record (`internal/config/modelruntime.go`) is gone,
along with `internal/vllm` and `internal/hf` entirely. omlx (macOS + Apple Silicon
only, the sole local-inference backend) needs no parallel model registry: it is
ONE shared `omlx serve --model-dir <dir> --port 8100` process (fixed port,
`services.OmlxPort`) that serves every model it finds under its model directory
(`~/.ai-platform/volumes/models/omlx/`, `omlx.StoreDir`) — model download, add,
remove, and tuning are handled entirely by omlx's OWN admin panel
(`http://127.0.0.1:8100/admin`, `ai services console omlx`), so there is no
per-alias/per-repo bookkeeping on this platform's side and no `ai models pull`
equivalent. LiteLLM's DB stays the sole source of truth for **what** is served:
its `omlx/*` registrations (public name `omlx/<id>`, routed `openai/<id>`) are
kept in sync with omlx's live `GET /v1/models` by `internal/litellm`'s
`SyncOmlxModels`/`DesiredOmlxModels`, driven by `internal/setup/omlx_host.go`
(the host-native probe/reconcile seam that replaced `internal/setup/vllm_host.go`)
— run automatically at `ai setup` and `ai services start|restart omlx`, and on
demand via `ai models refresh`.

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

## 12.8 `overlays/<workspace-id>/overlay.json` — Removed

Fully retired, not merely unimplemented: `internal/overlay` (which this schema
was proposed for) was deleted along with the whole overlay/persistent-VM-disk
concept — see §1.10.
