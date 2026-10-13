# 02-implementation-roadmap.md

# AI Development Platform

> **Workspace runtime migration.** Everything below was delivered against
> **Microsandbox** hardware-isolated microVMs (the links below are kept for
> historical reference). That runtime has since been **replaced end-to-end by
> [nono](https://nono.sh)**, a kernel-capability sandboxing CLI (Landlock LSM
> on Linux, Seatbelt on macOS) that wraps a single host process per
> invocation — there is no VM, no OS image, no boot step, and no hypervisor
> anymore. This is a deliberate, accepted security-model change: nono is
> same-kernel capability sandboxing, not hardware/VM isolation. The slices
> below (S1–S6) remain the accurate historical record of delivery order; each
> is annotated in place where its *mechanism* changed post-migration, and
> marked moot where it no longer applies at all (OS image selection, in-VM
> apps, VM resize/mounts). Unrelated systems — the Docker-based service tier,
> LiteLLM, the guardrail set, omlx — are unaffected by this migration and are
> left as originally written.

The Microsandbox Github repo can be found at:
[https://github.com/microsandbox/microsandbox](https://github.com/microsandbox/microsandbox)
(superseded; no longer used by this platform)

The Microsandbox documentation is found at:
[https://docs.microsandbox.dev/](https://docs.microsandbox.dev/)
(superseded)

The Go SDK previously used to drive workspace microVMs was found at:
[https://github.com/microsandbox/microsandbox/tree/main/sdk/go](https://github.com/microsandbox/microsandbox/tree/main/sdk/go)
(removed from the codebase — workspaces no longer link this SDK)

The current workspace runtime, nono, is documented at [https://nono.sh](https://nono.sh).

## Implementation Roadmap

Version: 1.0

---

# 0. Overview

This document defines the phased implementation plan for the AI Development Platform.

It translates the full architecture into incremental delivery slices.

Each slice must be independently testable and fully working before proceeding to the next.

> **Status:** the full surface is implemented — slices S1, S2, S4–S6 (S3 and S7
> are retired) — host-side and, on a provisioned Apple Silicon host, verified
> end-to-end against the live external tools. S3 (multi-agent orchestration)
> was retired because that is the in-workspace agent CLI's concern, not the
> platform's (§4); S7 (Windows/WSL host support) was retired when Windows
> support was dropped from the platform entirely — the supported hosts are
> macOS and Linux only (the workspace runtime itself, nono, has no
> Apple-Silicon/virtualization requirement on either; the local-inference
> backend, omlx, is separately macOS/Apple-Silicon-only). The slices below describe the delivery
> order; the few seams that still depend on live hardware bring-up are
> grep-able as `hardware bring-up` and tracked in `docs/HARDWARE-BRINGUP.md`.

---

# 1. Implementation Philosophy

## Incremental Delivery

The system must be built in small, functional slices.

Each slice must:

* produce a working system
* pass acceptance tests
* not depend on future features

---

## OS selection — MOOT (removed post-migration)

```text id="sl0a1"
There is no OS selection anymore: a workspace runs on whatever OS the host
actually is. There is no Dockerfile, no OCI image build, and no `--os` flag.
```

This entire mechanism — the `ai create` wizard's OS step, the `--os` flag, and
its exit-2-if-missing validation — existed only because a workspace used to be
a Microsandbox microVM booted from a per-OS Dockerfile template. Under nono,
an agent CLI runs directly as a host process; there is no guest OS to choose.
`project.Spec`/`config.Config` still record an `OS` field, but it is now just
the detected host `runtime.GOOS` (`internal/project/project.go`), informational
only — never a user choice, never a build input. Slice 1's `debian-trixie`
template and Slice 5's `alma`/`ubuntu` templates (§6 below) no longer exist in
the codebase.

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

> **Post-migration note.** Slice 1 originally delivered this MVP on
> Microsandbox microVMs; the bullets below describe what Slice 1 settled into
> **today**, after the nono migration replaced the workspace runtime
> end-to-end (the delivery order and acceptance bar are unchanged — only the
> workspace mechanism is). Where a bullet below says "gone"/"moot" it names a
> Slice-1-era mechanism that no longer exists in the codebase at all.

---

## Scope

* macOS or Linux host support — nono (Landlock LSM on Linux, Seatbelt on
  macOS) has no Apple-Silicon-only / virtualization requirement, unlike the
  Microsandbox microVM runtime it replaced (the local-inference backend,
  omlx, is separately macOS/Apple-Silicon-only, for an unrelated reason)
* Docker runtime for the service tier (Podman deferred to Slice 6); **nono**
  for workspace agent sessions (no microVM runtime anymore)
* rootless by default for the service tier (see 01-architecture-spec.md §6)
* **OS selection is gone** — there is no Dockerfile template, no OS choice at
  `ai create`, and no `--os` flag; a workspace runs on whatever OS the host
  actually is (see "OS selection — MOOT" above)
* Single-agent system
* LiteLLM integration
* keys-in-LiteLLM credentials (real provider keys live in the gateway; the
  agent's scoped virtual key is minted per project and stored ONLY in the OS
  keychain — macOS Keychain / Linux `secret-tool` via `internal/nono` — never
  on platform disk)
* nono session launch (host-process sandbox, per agent-CLI invocation) in
  place of Microsandbox workspace (microVM) creation

---

## Components

### Host

* macOS/Linux bootstrap launcher (thin shell script that fetches/execs the Go binary)
* Docker installation detection (service tier) + **nono CLI** detection
  (`nono.Detect`, `internal/nono`) — a simple PATH check, since nono has no
  virtualization-capability gate to probe (unlike the Microsandbox
  hypervisor/KVM check it replaced)
* LiteLLM container startup
* keys-in-LiteLLM credentials: real provider keys live in the LiteLLM gateway
  (env passthrough at launch / its Postgres-backed store), never on platform disk
  or in the workspace; the agent authenticates to the gateway with a scoped
  virtual key, injected into the sandboxed process by nono's
  `--env-credential-map <keychain-ref> <ENV_VAR>` at launch (never written to a
  config file or disk). The secret-injection acceptance test depends on this
  credential path being fully functional.

---

### Runtime

* Docker (service tier) + **nono** (workspace agent sessions)
* rootless by default for the service tier (fail `doctor` if rootless is unavailable; no rooted fallback)
* fail `doctor` if **nono itself is not installed**; there is no
  virtualization-capability check anymore (nono sandboxes a host process, not
  a VM) and no degraded "non-sandboxed" fallback

---

### Workspace System

* `ai create` scaffolds `.ai-platform/config.yaml` + `project.yaml` +
  `.gitignore` and registers the project — there is no Dockerfile and no
  image build; a workspace is ready to use the moment it is scaffolded
* a workspace is now just **a registered project directory** (`internal/project`)
  plus a generated **nono profile** (`internal/nono`, one JSON file per agent
  CLI at `<project>/.ai-platform/nono-profile-<cli>.json`, regenerated on every
  launch so an `ai network` change takes effect immediately); there is nothing
  to "boot"
* single workspace per directory
* the **project directory on host IS the persistent state** — there is no VM
  disk, so nothing is "host-mounted" into a guest (see Slice 4's note below)

---

### Model Layer

* LiteLLM configured
* models are **DB-backed** (`general_settings.store_model_in_db: true`): the
  rendered `config.yaml` carries **no `model_list`, no named aliases, no
  per-provider wildcards, and no default model**. Served models are managed via
  `ai keys add <provider>` (registers that provider's models.dev catalog models).
  Local inference is **host-native omlx** (macOS + Apple Silicon only — the sole
  local-inference backend; no inference container runs on `aip-net`): ONE shared
  `omlx serve --model-dir <dir> --port 8100` process serves every model omlx finds.
  Model download/add/remove/tune happens entirely in omlx's OWN admin panel
  (`http://127.0.0.1:8100/admin`, reached via `ai services console omlx`) — there
  is no `ai models pull` equivalent and no per-model resource-cap tracking on this
  platform's side. omlx's live `GET /v1/models` list is synced into LiteLLM
  automatically (public name `omlx/<id>`, routed `openai/<id>`) at `ai setup` and
  `ai services start|restart omlx`, and on demand via `ai models refresh`.
  The served set itself stays DB-backed:

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
ai list
ai delete            # `ai destroy` is an alias of this
ai start|stop|restart
ai agent <cli> [name]
ai shell|attach|sessions
ai services status
ai keys add|list|remove
ai models status|test|refresh
ai state show|repair
ai doctor
ai logs
```

**`ai exec` is gone** — there is no persistent process to exec into anymore;
`ai agent <cli>`/`ai shell` launch (or reattach) a sandboxed session directly.
There is no snapshot/upgrade/rollback or backup machinery — a workspace's
persistent state is simply its project directory on host, and overlay
persistence (originally Slice 4's VM-disk overlay) is also superseded, see
Slice 4 below.

---

## Acceptance Criteria

* platform installs on macOS or Linux (no Apple-Silicon/virtualization
  requirement for the workspace runtime itself)
* project is created successfully
* an agent CLI runs sandboxed by nono (no microVM — a host process per
  invocation)
* LiteLLM responds
* secrets resolved via the LiteLLM credential store (the agent's scoped
  virtual key is stored only in the OS keychain, injected by nono at launch)
* no manual configuration required

---

# 3. Slice 2 — Context Optimization Layer

## Goal

Introduce Headroom + Caveman.

---

## Scope

* Headroom context management (input compression) — a LiteLLM `pre_call`
  **guardrail**, NOT an nginx proxy. The `aip-headroom` service (pulled image
  `ghcr.io/headroomlabs-ai/headroom:latest`, internal port :8787, **internal-only on
  `aip-net`**, no host publish) is called BY LiteLLM: LiteLLM's `headroom`
  guardrail POSTs the request messages to `http://aip-headroom:8787/v1/compress`
  and swaps in the compressed result before dispatch (requires LiteLLM v1.92.x+;
  the litellm image now tracks `latest`, which satisfies that). **Post-migration:**
  there is no workspace image anymore to bake a `uv tool install
  "headroom-ai[proxy]"` into, so the in-session `headroom wrap <cli>` path is now
  purely opportunistic — `workspace.Manager.Agent` (`internal/workspace/workspace.go`)
  checks `exec.LookPath("headroom")` on the HOST at every launch and, if found,
  wraps the command (`agentcfg.HeadroomWrapName` — claude-code/codex/opencode
  only; omp/gemini/hermes are not wrappable) regardless of api-key/OAuth mode;
  the platform never installs the `headroom` CLI itself, so this only engages
  when the user happens to have it on their own host PATH. Per-project strategy
  still maps to Headroom per-request knobs fed to the gateway guardrail's
  compress call.
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
commands that DO exist are thin **nono session** launchers/reattachers for an
agent CLI running directly on the host — a session surface, not orchestration.
**Post-migration note:** these were originally tmux sessions multiplexed inside
a Microsandbox microVM; nono has its own native session concept
(`nono run --detached` / `nono attach <id>` / `nono ps --json` / `nono stop
<id>`, wrapped by `internal/nono`), so there is no tmux involved anymore —
each `ai agent`/`ai shell` launch is its own nono session, anonymous for `ai
shell` and named for the CLI for `ai agent`.)

---

# 5. Slice 4 — Overlay Persistence — MOOT (mechanism no longer applies)

## Goal (historical)

Make installed programs and agent state persist across workspace restart and
recreation, via the per-workspace overlay. (Environments were defined by the
project's `.ai-platform/Dockerfile` from Slice 1 — there was no snapshot
versioning/upgrade/rollback.)

> **Post-migration: this entire slice is moot.** `internal/overlay` (the
> per-workspace writable VM-disk overlay) has been **deleted** — there is no
> VM disk to overlay anymore, since nono sandboxes a host process rather than
> booting a guest. The persistence problem Slice 4 solved simply does not
> exist under the new model: the **project directory on host IS the
> persistent state** — anything an agent writes under the project root is
> already a normal file on the host filesystem, with nothing to "survive
> recreation" because there is no recreation step. Host-global per-CLI state
> (e.g. `~/.claude`, `~/.codex`, `~/.omp`, `~/.hermes`) is likewise just
> ordinary host-home-directory state now, not something symlinked into a
> guest overlay. The one piece of state that still needs an explicit,
> durable store is the project's scoped LiteLLM gateway credential — that is
> handled by `internal/nono`'s OS-keychain wrapper (macOS Keychain /
> Linux `secret-tool`), not by anything overlay-shaped.

---

## Scope (historical — no longer implemented)

* ~~per-workspace persistent writable overlay (architecture §26)~~
* ~~overlay mounted over the image built from `.ai-platform/Dockerfile`~~
* ~~overlay survives stop/start (`ai stop` → `ai start`/`ai restart` re-mount)~~
* ~~overlay removed only when its workspace is permanently removed~~

---

## Commands

No new commands — persistence was (and still is) automatic, now simply
because there is no VM boundary for state to cross.

---

## Acceptance Criteria (historical — superseded)

The following no longer apply as written (there is no recreation step, no
read-only image, and no overlay to mutate into):

* ~~a program installed in the workspace is still present after recreation~~
* ~~agent state written outside the project mount persists across recreation~~
* ~~the read-only image is never mutated; all changes land in the overlay~~

Current equivalent: a program/dependency installed by an agent session is a
normal write to the host project directory or the host's own package
managers — it was never going to disappear, because there is no guest
filesystem for it to live in.

---

# 6. Slice 5 — Extended OS Support — MOOT (mechanism no longer applies)

## Goal (historical)

Add the remaining OS Dockerfile templates (Slice 1 shipped the debian-trixie template).

> **Post-migration: this slice is moot.** `internal/templates` (the embedded
> OS-base-Dockerfile + stack/agent-CLI/tool snippet composer) and
> `internal/envimage` (the OCI image builder that consumed it) have been
> **deleted** — there is no per-workspace image, so there is nothing to template
> per OS. A workspace simply runs on whatever OS the host already is
> (`project.Spec.OS`/`config.Config.OS` just record the detected
> `runtime.GOOS`). The `alma`/`debian-trixie`/`ubuntu` templates this slice
> added no longer exist in the codebase.

---

## Scope (historical — no longer implemented)

* ~~`alma` template (Alma 10)~~
* ~~`debian-trixie` template (`debian:trixie-slim`)~~
* ~~`ubuntu` template (`ubuntu:24.04`)~~

---

## Acceptance Criteria (historical — superseded)

* ~~all three OS templates selectable in the `ai create` wizard (or via `--os`)~~
* ~~the user always selects the OS — no OS is applied silently~~
* ~~all OS images expose an identical **base** tooling surface (agent CLIs are whatever the user selected)~~

Current equivalent: whatever toolchains exist on the host are what's
available — the platform no longer provisions per-workspace language
runtimes/stacks at all (the `go`/`rust`/`java`/`maven`/`deno` stack picker from
Slice 1/`internal/create` is likewise gone, see §9 below).

---

# 7. Slice 6 — Linux Host Support

## Goal

Add Linux host compatibility.

> **Post-migration note.** Slice 6 originally gated Linux workspace support on
> KVM virtualization, because Microsandbox microVMs needed it. nono has no such
> requirement — on Linux it sandboxes via **Landlock LSM**, a same-kernel
> capability restriction, not a hypervisor. So the bullets below describe what
> actually gates Linux support today: just the `nono` binary being installed
> (`nono.Detect`), nothing virtualization-related.

---

## Scope

* Linux bootstrap (nono sandboxes via Landlock LSM — no KVM/virtualization
  requirement for workspaces; KVM was a Microsandbox-era constraint)
* Podman support (service tier)
* runtime abstraction improvements

---

## Acceptance Criteria

* `ai setup` works on Linux (no virtualization capability required for
  workspaces — only `nono` installed)
* Docker or Podman auto-detected for the service tier
* agent CLIs run sandboxed by nono via Landlock (no KVM/microVM involved)
* no workflow differences vs macOS

---

> **Slice 7 — Removed.** Slice 7 originally added native Windows host support
> (later narrowed to WSL2-only); Windows support was then dropped from the
> platform entirely, so there is no Slice 7 section here — the supported hosts
> are macOS (Apple Silicon) and Linux only — and the `[S7]` acceptance tag is
> retired with no replacement slice.

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
  catalog when a provider key is added (`ai keys add`), and from the host-native
  omlx backend (macOS + Apple Silicon only) whenever its live model list changes
  (synced automatically as `omlx/<id>`, or on demand via `ai models refresh`); the
  rendered config has no `model_list`, no wildcards, and no named aliases
* **user-selectable guardrails** rendered into the generated LiteLLM config.
  `ai setup` presents a guardrail multi-select (+ `--guardrails` flag,
  comma-separated subset or `none`); the choice is persisted machine-wide in
  `runtime.yaml` `guardrails:`. Only ENABLED guardrails are rendered — an
  unselected one is omitted entirely (never references a backend that isn't
  running). Each RENDERED guardrail is `default_on: true`, so a client can't opt
  out of an ENABLED guardrail; since every route (cloud included) traverses the
  proxy, cloud calls can't bypass an enabled guardrail. Headroom is the ONLY
  default-on guardrail out of the box; the security guardrails below are opt-in.
  The set is scoped to **secrets/credentials, not general PII** (masking general
  PII was removed — it corrupted ordinary coding prompts):
  * `headroom` — input compression (see Slice 2) — the only default-on guardrail
  * `secret-masking` — the `presidio-secrets-input` (pre_call) +
    `presidio-secrets-output` (post_call) pair as ONE option, masking only
    financial/identity secrets (CREDIT_CARD, US_SSN, US_BANK_NUMBER, IBAN_CODE,
    CRYPTO; MASK action, 0.6 score threshold), backed by the
    `aip-presidio-analyzer` + `aip-presidio-anonymizer` service-tier containers
    (which are pulled + started ONLY when this guardrail is selected)
  * `hide-secrets` (LiteLLM's in-process detect-secrets) for API keys/tokens
  * (the in-process `detect_prompt_injection` callback was REMOVED — it
    false-positived on ordinary coding/local-inference traffic)
  * a `tool-firewall` (`tool_permission`, post_call) that DENIES destructive
    command tool-calls (`git push --force`, `rm -rf`, `terraform destroy`,
    `kubectl delete`, …) — for coding agents the bigger risk is destructive tool
    execution, not prompt content
* (The unmaintained LLM Guard was removed; Guardrails AI remains deliberately
  deferred.)

---

## 8.2 Credentials & Egress

* **keys-in-LiteLLM**: real provider keys live in the LiteLLM gateway (env
  passthrough at launch / its Postgres-backed store); the workspace agent holds
  only a scoped LiteLLM virtual key, never a real provider secret. **Post-migration:**
  that scoped key is minted per project (`workspace.Manager.ensureScopedKey`),
  stored ONLY in the **OS keychain** (macOS Keychain via `security` / Linux
  `secret-tool`, `internal/nono.StoreCredential`) — not written into any
  in-VM env file (there is no VM) — and injected into the sandboxed process at
  launch via nono's own `--env-credential-map <keychain-ref> <ENV_VAR>`
* `ai keys` (add|list|remove) manages the per-provider keys, stored **encrypted
  in the LiteLLM Postgres DB** (`LITELLM_SALT_KEY`); no .env files, no keys in
  `config.yaml`, none on platform disk or in the workspace
* **egress**: a per-project policy declared in `config.yaml`
  (`network.egress`/`allow_domain`/`deny_domain`) and compiled straight into the
  project's **nono profile**, configured via `ai network show|egress|allow|disallow`
  (modes now just `deny`/`public` — the old third mode `unrestricted` is gone —
  plus bare-domain, not `host:port`, allow/deny entries); **default mode
  `public`** (nono's own unrestricted-outbound default), with `deny` the
  locked-down posture; no egress proxy, and no restart needed — a change
  applies on the very next `ai agent`/`ai shell`/`ai start`
* **secret masking / audit**: LiteLLM's user-selectable guardrails, applied on
  every request once enabled (the opt-in `secret-masking` Presidio pair scoped to
  financial/identity secrets + `hide-secrets` + the `tool-firewall`; the
  `detect_prompt_injection` callback was removed as a false-positive source),
  which cloud routes cannot bypass once enabled (§8.1);
  plus per-domain DNS egress audit via the `aip-dns`
  CoreDNS resolver, surfaced by `ai network log`

---

## 8.3 LiteLLM routing

* thin gateway only (unified endpoint, aliasing, failover, provider-key injection)
* no per-task routing policy — the agent selects its model
* the in-workspace agent's model picker is **exactly the models the LiteLLM
  gateway currently serves** (its live DB-backed model set), read at every
  agent launch via the injected `workspace.ServedModels` source
  (`litellm.KeyManager.ListModels`) and built by `workspace.Manager.pickerModels`.
  It is **not** a union of aliases + local models + a cloud seed — there are no aliases
  and no `cloud_models.yaml`. **Post-migration:** the picker/config writers
  (`writeOpenCodeConfig`/`OmpConfig`/etc., `internal/workspace/agentconfig.go`)
  now run at every `ai agent`/`ai shell` launch rather than at a persistent
  workspace's start — there is no persistent process to refresh config on
  later, since each launch is its own fresh nono session. `agent.graphify_model`
  (picked from omlx's live list at `ai create`, stored in `config.yaml`) is
  still captured, but every config writer currently checked passes an EMPTY
  default model (`OmpConfig("")`, `MergeOpenCodeConfig(..., "", ...)`) — the
  pre-migration "seed-then-remember" default-model behavior and the
  `/persist`-overlay-backed state-dir symlinking it depended on (overlay is
  deleted, see Slice 4) do not currently appear wired into the launch path;
  this is flagged as **unverified/possibly-regressed**, not asserted as
  working, pending confirmation.

(MCP is not a platform concern — the agent manages it.)

---

## 8.4 Host service control commands

* `ai domain [name]` — show/set the platform base domain (default `aip.local`)
* `ai gateway show|set|clear` — machine-wide gateway address selection
* `ai litellm password` — set/rotate the LiteLLM admin UI password (env
  passthrough; offers to persist to `~/.ai-platform/.ai-platform.env`)
* `ai services start|stop|restart|update [service|all]` — manage the platform
  container set; `update` re-pulls moved tags (e.g. `latest`) and recreates
  affected containers
* `ai services enable|disable <service>` — toggle an optional service
* `ai services console <service>` — open a service's admin console/dashboard
* `ai services compose` — write a debug-only `docker-compose.yaml` mirroring
  the service-tier topology (not the launcher)
* `ai network ... log` — per-domain DNS egress audit via the `aip-dns` resolver

---

## 8.5 Sandbox Strategy

Default:

```text id="d1"
one nono-sandboxed host process per agent-CLI session (kernel-capability
isolation — Landlock LSM on Linux, Seatbelt on macOS; NOT a VM)
```

**Post-migration note.** This was originally "one Microsandbox microVM per
workspace (hardware isolation, libkrun)" — hardware/VM-level isolation. That
runtime is gone; nono sandboxes each agent CLI invocation directly on the
host kernel. This is an explicit, accepted trade-off: same-kernel
capability sandboxing, not hardware isolation.

The container runtime (Docker/Podman) is used only for the service tier
(the `aip-dns` CoreDNS egress-audit resolver, the Presidio secret-masking pair,
LiteLLM + its Postgres, the Headroom input-compression guardrail service, the
`aip-valkey` cache (+ its `aip-redisinsight` GUI), and the `aip-proxy` nginx
gateway), never to run a workspace (workspace agent sessions are nono-sandboxed
host processes, not containers either). The **inference tier is host-native omlx**
(macOS + Apple Silicon only) — ONE shared `omlx serve` host process serving every
locally-served model (there is **no `aip-*` inference container**); `ai setup`
best-effort installs omlx into the platform venv and reconciles the omlx host
server (the LiteLLM container reaches it at `host.docker.internal:8100`). The
host tier has no optional services (Open WebUI is now a
per-workspace **in-VM** app and Odysseus was removed). All service-tier containers
share the private `aip-net` network, and **only the `aip-proxy` nginx gateway is
host-published** (the host port `18787`); every other service is internal-only on
`aip-net` and reached through it (Postgres + DNS stay loopback for admin access).

---

## 8.6 Networking

* the **`aip-proxy` nginx gateway is the SOLE host entry** to the service tier,
  on host port `18787`. It binds **127.0.0.1** in standalone/client roles and
  **0.0.0.0** in server role (`internal/setup` `currentBindHost()`). It forwards
  the model path DIRECTLY to LiteLLM (`aip-litellm:4000`), which runs its
  guardrail chain (Headroom, §3/§8.1) in-process — nginx no longer routes to
  Headroom at all — and serves the Host-based UI subdomains
  (`litellm.<domain>` → LiteLLM admin UI, `valkey.<domain>` → RedisInsight) plus host-CLI gateway paths
  (`/v1` model path, `/llm`). The platform base domain is set by
  `ai domain` (default `aip.local`; host-CLI URLs render under `localhost:18787`).
  TLS/HTTPS termination at nginx is still deferred.
* **role-based UI auth**: the server role secures the exposed UI (LiteLLM admin
  via `ai litellm password`); standalone/client are open. Passwords/master key are
  passed by **env passthrough** and can be persisted out of argv/disk in
  `~/.ai-platform/.ai-platform.env` (`internal/envfile`).
* **post-migration:** there is no microVM, so there is no virtual NIC, no
  `host.docker.internal`-style guest DNS name, and no host Docker socket to
  reason about. A nono-sandboxed agent runs directly on the host network stack
  — the gateway is reached at plain loopback, exactly like any other host
  process would reach it
* machine-wide gateway selection via `ai gateway show|set|clear` →
  `ai_platform_host` in `runtime.yaml`; `runtime.ResolveGateway` derives the
  `http://<host>:<port>/v1` agent base URL (bare host or `host:port`, default
  port `18787`; empty → **`127.0.0.1:18787`** for standalone/local —
  `runtime.DefaultGatewayHost`/`DefaultGatewayPort` — replacing the old guest-only
  `host.microsandbox.internal:18787` name, since there is no guest network to
  translate through anymore; `runtime.HostGateway` always returns plain loopback)
* the host-native omlx inference backend (macOS + Apple Silicon only) is reached
  only via LiteLLM, never directly by the workspace
* egress is a per-project policy declared in `config.yaml`
  (`NetworkConfig{Egress, AllowDomain, DenyDomain}`) and compiled directly into
  the project's **nono profile** `network` section (`internal/nono.Profile`) —
  there is no Microsandbox NetworkPolicy and no `msb net-rules` anymore. Modes
  are now just `deny` (nono's `--block-net`) or **`public`** (nono's own
  default — unrestricted outbound; `unrestricted` as a distinct third mode is
  gone). `ai network allow|disallow <domain>` edits bare-domain (or
  `*.suffix`-wildcard) allow/deny-list entries — meaningful under `deny`, where
  they carve exceptions into an otherwise-blocked default — not `host:port`
  pairs, and there is no more "gateway"-token special case and no
  `ai network publish/unpublish` (there are no ports to publish without
  in-VM apps). Changes take effect on the **very next launch** automatically,
  since the profile is regenerated on every `ai agent`/`ai shell`/`ai start` —
  there is no "restart the workspace to apply" step anymore.

---

## 8.7 Audit Logging

* lifecycle tracking
* no secret logging

---

# 9. Global Acceptance Criteria

**Post-migration note:** the `ai create` wizard below no longer has OS,
software-stack, resource (vCPU/memory/disk/ports/idle-timeout), or in-VM-app
steps — those mechanisms are gone (§2/§6 above). It is now:
Location → Name → Shell → Agent CLIs → Default agent → per-OAuth-agent
auth-mode steps → AI tools → Graphify model (shown only if graphify is
selected).

System is complete when:

```bash id="g1"
ai setup
ai create --name my-project   # interactive wizard: shell (bash|zsh); agent CLIs
                               # (opencode default, omp/claude-code/codex/gemini/
                               # hermes also selectable); default agent CLI;
                               # per-agent auth mode for OAuth-capable CLIs; AI
                               # tools (caveman/graphify/code-review-graph/
                               # codebase-memory-mcp/openviking/tokensave/
                               # agentmemory/ponytail/agent-skills — ALL opt-in,
                               # none selected by default — see
                               # create.SupportedAITools()); Graphify model
                               # (picked from omlx's live served list)
```

produces (for the options the user selected):

* a registered workspace (project directory + per-agent-CLI nono profile —
  no OS image, no Dockerfile)
* working LiteLLM
* keys-in-LiteLLM secrets (the agent's scoped virtual key is minted per
  project and held only in the OS keychain, injected by nono at launch)
* Headroom input compression (gateway guardrail; in-session `headroom wrap`
  only when the `headroom` CLI happens to be on the host's own PATH)
* selected AI tools (caveman/graphify/code-review-graph/codebase-memory-mcp/
  openviking/tokensave/agentmemory/ponytail/agent-skills — see
  `create.SupportedAITools()`) installed on the host and applied at the next
  `ai agent`/`ai start` — **none are on by default**, including Caveman and
  Graphify (a change from the pre-migration default of both ON)
* reproducible environments — now meaning "whatever is actually on the host",
  since there is no OS image/stack provisioning to make reproducible across
  hosts anymore
* zero manual configuration

---

# 10. Implementation Rule

No slice is considered complete until:

* all commands work
* all acceptance tests pass
* no manual configuration is required
