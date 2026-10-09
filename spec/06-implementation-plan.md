# 06-implementation-plan.md

# AI Development Platform

## Implementation Plan

Version: 1.0

---

# 0. Purpose

This document is the engineering plan for building the platform. It complements
the other specs:

* `01-architecture-spec.md` — end-state architecture (the *what*)
* `02-implementation-roadmap.md` — product slices S1–S6 (the *when*)
* `03-repository-layout.md` — runtime directory + state/config schemas
* `04-cli-specification.md` — CLI contracts
* `05-acceptance-tests.md` — executable acceptance tests (the *done* bar)

This plan defines the *how*: language, repo structure, package boundaries,
external-tool integration approach, the Slice 1 build sequence, and CI/testing.

> **Workspace runtime migration.** This plan was originally built around
> **Microsandbox** hardware-isolated microVMs as the workspace runtime. That
> runtime has since been replaced end-to-end by **[nono](https://nono.sh)**, a
> kernel-capability sandboxing CLI (Landlock LSM on Linux, Seatbelt on macOS)
> that sandboxes a single host process per agent-CLI invocation — there is no
> VM, no OS image, no boot step, and no hypervisor anymore. This is a
> deliberate, accepted security-model change: same-kernel capability
> sandboxing, not hardware/VM isolation. The milestones below (M0–M8) remain
> the accurate historical build order; each is annotated in place where its
> mechanism changed, and marked moot where it no longer applies (OS image
> composition, in-VM apps, workspace resize/mounts). The Docker-based service
> tier, LiteLLM, the guardrail set, and omlx are unaffected by this migration.

---

# 1. Tech Stack

* **Language: Go (1.26), module `github.com/jt-helsinki/stack-genie`.** The `ai` CLI, installers, state management,
  nono session orchestration, runtime abstraction, and diagnostics are all Go,
  shipped as a **single self-contained binary** per host. **Post-migration:**
  `internal/nono` wraps the real `nono` CLI by shelling out via plain
  `os/exec` (`RealRunner`) — there is no cgo binding involved in the
  workspace-runtime path anymore, unlike the Microsandbox Go SDK it replaced
  (a cgo binding that dlopened an embedded FFI library). **Known stale build
  config (not yet cleaned up):** `go.mod` still lists
  `github.com/superradcompany/microsandbox/sdk/go` and the `Makefile` still
  forces `CGO_ENABLED=1` "because it links the Microsandbox Go SDK" — but no
  `.go` file in the repository imports that module anymore, so this appears to
  be an unremoved leftover from before the migration rather than a still-true
  constraint; it has not been re-evaluated here.
* **Thin launchers only** (bash/zsh) bootstrap the binary; no platform logic in
  shell. Supported hosts are macOS and Linux — nono itself has no
  Apple-Silicon-only or virtualization requirement (Landlock LSM on Linux,
  Seatbelt on macOS); the local-inference backend, omlx, remains
  macOS/Apple-Silicon-only for an unrelated reason.
* **Declarative config/templates** in YAML/JSON; never executable logic.
* External components are invoked as subprocesses or over HTTP, never
  reimplemented: **nono** (the `nono` CLI, replacing Microsandbox's `msb` CLI),
  LiteLLM (host service over HTTP), docker / podman (subprocess). Version
  control is largely out of scope — no `gh`, no clone/remote, and no `git` HOST
  prerequisite. **Post-migration:** the in-VM `git init` at workspace start (to
  seat the Graphify hook) described here was a Microsandbox-era mechanism; no
  equivalent git-init/Graphify-hook call site was found in the current
  `internal/workspace` package (which is now just three files:
  `workspace.go`/`agentconfig.go`/`profile.go`) — this is flagged as
  **unverified/possibly not yet re-wired** rather than asserted as still
  happening.

Key libraries: `cobra` (commands), `viper`-free hand-rolled config merge (to
keep the precedence rules in arch §27 explicit), `slog` (structured logs), stdlib
`encoding/json` + `gopkg.in/yaml.v3`.

---

# 2. Repository Layout (source)

```text
/
├── cmd/ai/                      # main(): single binary entrypoint
├── internal/
│   ├── cli/                     # cobra commands → thin; delegate to packages
│   ├── output/                  # JSON envelope (CLI §19) + human renderer + exit codes (CLI §18)
│   ├── version/                 # binary version string (injected via -ldflags)
│   ├── paths/                   # ~/.ai-platform layout helpers (workspaces live in the cwd, not a fixed ~/projects)
│   ├── layout/                  # creates the global ~/.ai-platform directory tree (idempotent)
│   ├── conffile/                # atomic YAML read/write (temp file + rename; rejects unknown fields)
│   ├── state/                   # project-local state (<project>/.ai-platform/run) + projects index; atomic writes
│   ├── project/                 # project entry/spec types + projects-index resolution
│   ├── config/                  # config load/merge (project > global); OS/Agent/Context/Network blocks — POST-MIGRATION: no isolated_dirs/shared_mounts anymore (`ai mounts` is gone, see §3.3)
│   ├── catalog/                 # models.dev model catalog (fetch as JSON, persist as YAML cache)
│   ├── sysinfo/                 # host CPU/RAM inspection (resource caps for `ai create`)
│   ├── versions/                # service-tier image refs (image+tag, no digest) — source of truth for setup
│   ├── envfile/                 # ~/.ai-platform/.ai-platform.env (opt-in 0600 secrets passthrough: UI password, master key)
│   ├── runtime/                 # docker/podman detect + rootless verify + role/domain/gateway resolution (service tier)
│   ├── nono/                    # POST-MIGRATION (replaces the deleted `sandbox/` package): wraps the `nono` CLI (https://nono.sh) via an injectable Runner (RealRunner shells out; FakeRunner for tests) — RunDetached/Attach/Shell/ListSessions/Inspect/Stop, profile JSON (WriteProfile), registry-pack pulls (PullPack), and OS-keychain credential storage (StoreCredential/DeleteCredential — macOS Keychain via `security`, Linux via `secret-tool`); just a PATH presence check, no virtualization-capability probe
│   ├── services/               # service-tier topology registry (names, ports, UI subdomains, gateway paths)
│   ├── setup/                  # service-tier reconcile orchestrator (network→DNS→Presidio→Valkey→RedisInsight→Headroom→LiteLLM+DB→nginx) + host-native omlx inference reconcile (the one shared omlx server in omlx_host.go; best-effort omlx install into the platform venv, live-model sync into LiteLLM) + `ai setup`/uninstall service control
│   ├── hostsfile/              # managed /etc/hosts block writer (delimited, idempotent)
│   ├── uihosts/                # UI-vhost logic: the litellm.<domain> host vhost + its /etc/hosts entry (composes services + hostsfile)
│   ├── console/                # host-display endpoint registry (UI subdomains + gateway paths off the nginx port)
│   ├── litellm/                 # host lifecycle, config gen, health, routing, guardrails, virtual-key + credential KeyManager (keys-in-LiteLLM; fronted by `ai keys` in cli/keys.go)
│   ├── pyenv/                    # platform-managed HOST Python venv (~/.ai-platform/venv): Ensure (newest Python, general/Linux) / EnsureVersion (exact-pin + recreate) / PipInstall
│   ├── omlx/                     # host-native omlx backend (macOS/Apple-Silicon only): ONE shared `omlx serve --model-dir <dir> --port 8100` Manager (single OpenAI endpoint serving every model omlx finds, health-polled); StoreDir volumes/models/omlx; model add/remove/tune lives entirely in omlx's own admin panel (`ai services console omlx`), not this package (live server fork + admin-panel round-trip are `hardware bring-up` stubs)
│   ├── agentcfg/                # in-VM agent provider config (base_url→nginx gateway, virtual key, picker models, refresh-models)
│   ├── contextopt/              # per-project Headroom strategy (→ per-request knobs fed to LiteLLM's headroom compress-guardrail call) + in-workspace Caveman skill
│   ├── create/                   # shared `ai create` logic: scaffold `.ai-platform/config.yaml`+`project.yaml`+`.gitignore`, seed context defaults — used by CLI + TUI wizard. POST-MIGRATION: no resource validate/cap step anymore (nothing to size — see §4 M6); `internal/envimage` (OCI image compose+build) is DELETED
│   ├── workspace/                # POST-MIGRATION, 3 files only (workspace.go/agentconfig.go/profile.go) — no Builder/Sandbox interfaces, no microVM, no tmux. `Manager` coordinates per-project **nono sessions**: `Agent`/`Shell` launch a detached `nono run` session wired to the LiteLLM gateway (mint + OS-keychain-store a scoped key, write the CLI's keyless host-side config, generate its nono profile); `Attach`/`ListSessions`/`Stop`/`StopAll` drive nono's own session lifecycle; `DeleteCredentials` revokes the OS-keychain entry
│   ├── egress/                   # per-project egress policy, compiled directly into the project's **nono profile** `network` section (no more `msb` net-rules; `internal/apps` and `internal/overlay` are both DELETED — no in-VM apps, no VM-disk overlay)
│   ├── logs/                    # file-backed log reader (backs `ai logs`)
│   ├── ui/ + tui/               # theme registry + the K9s-style `ai ui` management TUI (POST-MIGRATION: no Metrics/Logs/Mounts/Apps per-workspace sub-tabs — see §4 M6)
│   ├── uninstall/               # native `ai uninstall` teardown (stops the running `omlx serve` process; the omlx model store is kept unless `--purge`)
│   └── doctor/                  # consolidated health checks → repair suggestions
├── installers/                  # install.sh (+ install-local.sh) thin launchers (macOS/Linux)
├── test/acceptance/             # Go acceptance harness (AT §1.6); in-process mock
│                                #   provider (httptest) + inline provider config —
│                                #   NO fixtures/ dir (mock is code, not files)
├── test/integration/           # live black-box suite (//go:build integration,
│                                #   `make test-integration`) against a real stack
├── go.mod
├── Makefile
└── .github/workflows/
    ├── ci.yml                    # hosted (every push): fmt-check/vet/lint/test/build; self-hosted Apple Silicon (manual workflow_dispatch only): [S1] acceptance
    └── release.yml               # autobump semver on merge to main (Conventional Commits) → cross-compiled release
```

Principle: `cli/` stays thin (parse flags → call a package → render via
`output/`). All logic is testable without the CLI.

**Post-migration: `internal/templates` is deleted.** There is no OS
Dockerfile, software-stack snippet, or per-agent-CLI image-install snippet
anymore for `ai setup`/`ai create` to compose or install — a workspace runs
directly on the host, and each agent CLI is either installed onto the host by
`ai setup` (`internal/setup/agentcli_host.go`, for claude-code/codex/opencode,
via each tool's own official installer) or expected to already be on the
host's PATH.

---

# 3. Cross-Cutting Foundations

These underpin every slice and are built first.

## 3.1 Output & Exit Codes (`output/`)

* one `Result{ ok, command, data, error, warnings }` type → CLI §19 envelope
* `--json` emits envelope to stdout; human renderer otherwise; logs to stderr
* central exit-code mapping (CLI §18)

## 3.2 State Store (`state/`)

* project-local state under `<project>/.ai-platform/run/` (per-entity files);
  global `config/projects.yaml` index of project name → path
* **atomic writes**: temp file + `rename()`; never partial state
* typed load/save for each schema (§12 of repo-layout); reject unknown fields
* `ai state repair`: reconcile `run/` from filesystem (drops stale
  projects-index entries, ensures `run/` shard dirs exist); a forward-looking
  "+ Microsandbox" per-workspace-handle reconciliation step was planned here
  but never implemented in `internal/state/repair.go`, and remains unimplemented
  post-migration too — live session state is queried directly from `nono ps`
  (`workspace.Manager.ListSessions`), not reconciled by `ai state repair` (no
  git — version control is out of scope)

## 3.3 Config (`config/`)

* load + merge two layers with precedence `project > global`
* one `Config` struct matching repo-layout §12.4; missing layers skipped
* **`ai mounts` is GONE (post-migration).** `workspace.isolated_dirs`
  (guest-private named volumes) and `workspace.shared_mounts` (extra
  host↔guest bind mounts beyond the base project mount) only ever made sense
  because a workspace was a separate guest filesystem with its own view to
  carve mounts into. nono sandboxes a host process directly — there is no
  second "guest view" to mount into at all — so `internal/cli/mounts.go` and
  both config fields are deleted, with no replacement mechanism; the project
  directory on host is simply the one filesystem view there is. The surviving
  `WorkspaceConfig` field is just `Shell` — which interactive shell `ai shell`
  launches (`bash` default or `zsh`) — there is no vCPU/memory/disk to size
  and no idle-timeout concept, since a nono session just runs until it exits
  or is stopped

## 3.4 Runtime Abstraction (`runtime/`, `nono/`)

**Post-migration: `internal/sandbox` is deleted** — the detection it did
(Microsandbox/virtualization) is replaced by the much smaller `nono.Detect`
(`internal/nono`), and `workspace/`'s `Sandbox` interface (SDK-backed vs.
`msb`-CLI-backed) is gone along with it.

* `runtime/` (service tier): detect docker/podman; verify rootless; write
  `config/runtime.yaml`; `Runtime` interface so docker/podman are interchangeable
  (Podman impl in S6)
* `nono/` (workspace agent sessions): DETECTION only — `nono.Detect` is a PATH
  check for the `nono` binary, nothing more (`internal/setup/nono_host.go`'s
  comment: "nono has no virtualization-capability gate — any host the binary
  runs on can use it", unlike Microsandbox's Apple Silicon HVF / Linux KVM
  check). No daemon to supervise, no session lifecycle of its own beyond
  wrapping `nono`'s own subcommands. Creating/driving each agent session is
  `workspace/`'s job (`Manager.Agent`/`Shell`/`Attach`/`Stop`), which builds a
  **nono profile** per CLI invocation (`internal/workspace/profile.go`'s
  `ensureProfile`, regenerated on every launch) and applies the workspace's
  egress policy directly into that profile's `network` section — **there is
  no `msb create`/net-rule step anymore**. The enforcement primitive is now
  nono's own `network.block` flag (`true` under `deny`) plus
  `network.allow_domain`/`deny_domain` lists, not a Microsandbox deny-fallthrough
  net-rule chain. The **per-project default mode is `public`** (nono's own
  default — unrestricted outbound); `deny` sets `network.block: true` and
  relies on the allow-list to carve out exceptions. The
  `unrestricted`-as-a-third-mode concept, the `TestEgressConfinementOnHardware`
  known-stale-assertion caveat, and published-port/host-service allow-list
  entries (AT §16.3, repo-layout §12.4) were all specific to the Microsandbox
  net-rule model and no longer apply as written — the per-project egress
  policy today is just `config.NetworkConfig{Egress, AllowDomain, DenyDomain}`
  (modes `deny`/`public` only), configured by `ai network show|egress|allow|disallow`
  (`internal/egress`) and compiled by `workspace/profile.go` into the nono
  profile at every launch, not at a one-time `create`.

## 3.5 External Adapters & Service Control Plane

The `ai` CLI is the **single control plane** for host services (architecture §5,
"Host Services Control Plane"). All host services run in the **container tier**
behind uniform `ai services` verbs — **no docker compose**:

* **container tier**, reconciled in order network → DNS → Presidio →
  Valkey(+RedisInsight) → Headroom → LiteLLM(+DB) → nginx proxy: the `aip-dns`
  CoreDNS egress-audit resolver, the
  Presidio secret-masking pair `aip-presidio-analyzer`/`aip-presidio-anonymizer`,
  the Headroom compression service `aip-headroom` (which LiteLLM CALLS as a
  `pre_call` guardrail, so it PRECEDES LiteLLM), LiteLLM `aip-litellm` + its
  Postgres `aip-litellm-db`, and the `aip-proxy` nginx gateway: managed directly via the
  `runtime/` abstraction (run by image+tag, restart policy, health poll) on the
  private `aip-net` network, so docker and podman stay interchangeable. **Only the
  `aip-proxy` nginx gateway is host-published** (host port `18787`, the sole
  entry); every other service is internal-only on `aip-net` and reached through
  it (Postgres + DNS stay loopback). `ai services update` re-pulls moved tags and
  recreates affected containers. (Open WebUI is now an opt-in **in-VM** app and
  Odysseus was removed, so the host tier has no optional services; the optional
  mechanism is retained for future host services.)
* **host-native inference tier** (NOT containerized, macOS/Apple-Silicon only):
  omlx is the sole local-inference backend, running as ONE shared
  `omlx serve --model-dir <dir> --port 8100` host process that serves every
  model it finds — there is **no `aip-*` inference container** and no
  per-model process/port. `ai setup` best-effort installs omlx into the
  platform venv, starts the shared server (`internal/setup/omlx_host.go` over
  `internal/omlx`), and then automatically syncs LiteLLM's `omlx/<id>`
  registrations against the server's own live `GET /v1/models`; the LiteLLM
  container reaches the single endpoint at `host.docker.internal:8100`. Local
  model download/add/remove/tuning is not a platform command — it lives
  entirely in omlx's own admin panel (`http://127.0.0.1:8100/admin`, reached
  via `ai services console omlx`); `ai models refresh` re-triggers the LiteLLM
  sync on demand.

**Post-migration:** the nono runtime is likewise **not** a managed service —
the `nono` binary is installed onto the host directly (`ai setup`'s
`EnsureNonoInstalled`, `brew install nono` else nono's own curl installer) and
invoked on demand by `workspace/` (`internal/nono.RealRunner`, plain
`os/exec`) to launch and drive agent-CLI sessions; `nono/` itself only detects
that the `nono` binary is installed, which `ai setup`/`ai doctor` use to
verify prerequisites — there is no virtualization check anymore (the old
`sandbox/` package and the Microsandbox `msb` binary pin under `tools/` are
both gone).

Each service's config is **rendered** from the platform config into
`config/<service>/` (including the nginx vhost map for the `litellm.<domain>` and
`valkey.<domain>` UI subdomains and the `/v1`, `/llm` gateway
paths); real provider keys stay only in the LiteLLM gateway (keys-in-LiteLLM).
Service-tier image refs are pinned by **image+tag** (no digest — digests are
platform/arch specific) in `config/versions.yaml`. (**Post-migration:** the
in-VM apps that used to pin their own images in `internal/apps` are gone — that
package has been deleted along with the in-VM-app concept entirely.)

| Adapter | Integration | Run mode | First slice |
|---|---|---|---|
| `workspace/` (detect: `nono/`) | `nono` CLI via `os/exec` (`internal/nono.RealRunner`); sessions named by agent CLI, per project (`workspace.Name` = `aip-<project>`); session lifecycle map is `nono run --detached`/`attach`/`ps`/`inspect`/`stop`; `nono/` itself only detects the binary is on PATH — POST-MIGRATION replacement for the Microsandbox-Go-SDK-/`msb`-backed row this table originally had | nono-sandboxed host process (no daemon, no VM) | S1 |
| `litellm/` | container via `runtime/`; config rendered from routing; `/health` poll; `KeyManager` mints scoped virtual keys + stores provider credentials (keys-in-LiteLLM, fronted by `ai keys`) | container | S1 |
| `contextopt/` | per-project Headroom strategy (drives per-request knobs on LiteLLM's headroom compress-guardrail call to `aip-headroom`) + Caveman skill, selected as one of the opt-in AI tools at `ai create`/`ai tools enable` and intended to install on the host at the next `ai agent`/`ai start` (POST-MIGRATION: the exact install call site was not found in the current `internal/workspace` package — flagged as unverified) | container (Headroom) / host (Caveman) | S2 |

---

# 4. Slice 1 — MVP Build Sequence

Slice 1 target (roadmap §2), as originally delivered: macOS (Apple Silicon),
Docker rootless service tier, Microsandbox microVM workspaces, debian-trixie,
single agent, LiteLLM, keys-in-LiteLLM credentials (agent holds a scoped
virtual key), zero manual config.

> **Post-migration note.** Slice 1's milestones (M0–M8) below are left as the
> historical build order; each is annotated in place where the workspace
> runtime it describes has since changed. In today's code: macOS or Linux (no
> Apple-Silicon/virtualization requirement for workspaces — nono has none),
> Docker rootless service tier (unchanged), **nono**-sandboxed host-process
> agent sessions (no microVM, no debian-trixie image), LiteLLM, keys-in-LiteLLM
> credentials (the agent's scoped virtual key is held in the OS keychain, not
> an in-VM env file), zero manual config.

Commands (exactly the `[S1]`-tested surface, as originally specified): `ai setup`,
`ai create` (interactive wizard), `ai delete` (alias `ai destroy`),
`ai start|stop|restart|exec`, `ai services status`,
`ai keys add|list|remove`, `ai models status|test|refresh`, `ai state show|repair`,
`ai doctor`, `ai logs`. **Post-migration: `ai exec` is gone** — there is no
persistent process to exec into; the equivalent surface today is `ai agent
<cli> [name]` / `ai shell` / `ai attach` / `ai sessions`.
(No snapshot/upgrade/rollback commands — the environment is now simply the
project directory on host, see `[S4]` below, which this slice's original
`.ai-platform/Dockerfile` reference no longer applies to.)

Ordered milestones (each ends green against its acceptance tests). Test
references below are to `05-acceptance-tests.md` ("AT §"); "CLI §" and "arch §"
refer to the CLI spec and architecture spec respectively.

* **M0 — Bootstrap.** go.mod, cobra skeleton, `output/`, `paths/`, `install.sh`
  launcher, CI. Deliverable: `ai --version --json`
  and `ai --help` work.
* **M1 — State + config.** Sharded store, atomic writes, schemas, config merge,
  `ai state show` / `ai state repair`. Tests: AT §14.2.
* **M2 — Runtime detect.** Docker detection + rootless verify (service tier) +
  Microsandbox runtime / host-virtualization verify (workspaces) →
  `config/runtime.yaml`; fail (exit 4) if rootless or virtualization unavailable.
  **Post-migration:** the workspace-side check is now just `nono.Detect`
  (`internal/nono`) — a PATH presence check, nothing virtualization-related
  (`runtime.Verify` today enforces only rootless; a missing `nono` is recorded
  on `Info.Nono` for `ai doctor`/`ai setup` to act on, not a hard exit-4
  failure — a server-only host legitimately has no `nono` at all).
  Tests: AT §11.1.
* **M3 — `ai setup` + services.** Preflight (exit 3 on missing deps),
  init `~/.ai-platform/`, install/configure/start the container service tier
  (DNS resolver, Presidio pair, Valkey (+ RedisInsight), Headroom, LiteLLM + its DB, nginx gateway)
  plus the host-native omlx inference tier (best-effort install omlx into the
  platform venv, start the one shared omlx server, sync its live model list into
  LiteLLM — no `aip-*` inference container)
  with the role-driven bind host (server 0.0.0.0,
  standalone/client loopback) + verify the Microsandbox runtime; provider keys
  live in the LiteLLM gateway (keys-in-LiteLLM, §8.2); render the per-project
  network policy template (default mode `public`, §8.2); `ai services status`;
  **idempotent**. **Post-migration:** "verify the Microsandbox runtime" is
  replaced by a best-effort `EnsureNonoInstalled` (`internal/setup/nono_host.go`
  — `brew install nono` else nono's curl installer, Detect-gated, never fails
  `ai setup`); the per-project network policy is no longer rendered as an `msb`
  net-rule template but compiled straight into each agent's nono profile at
  launch (§3.4).
  Tests: AT §2.1, §2.2, §2.3, §12.1 (macOS install).
* **M4 — Model layer + secrets.** LiteLLM config gen + routing default
  (the generated config renders **user-selectable guardrails** chosen at
  `ai setup` — a picker + `--guardrails` flag, persisted in `runtime.yaml`
  `guardrails:`; only ENABLED guardrails are rendered and each rendered one is
  `default_on: true`, so a client can't opt out of an ENABLED one — cloud included.
  Headroom is the only default-on guardrail; the security guardrails are opt-in:
  `secret-masking` (the Presidio pair scoped to financial/identity **secrets**
  input+output — its containers are pulled/started only when selected),
  `hide-secrets`, and the `tool-firewall`; general-PII masking, the unmaintained
  LLM Guard, and the false-positive-prone `detect_prompt_injection` callback were
  removed);
  `ai keys add/remove`; `ai models status`, `ai models test` against the mock
  provider. Local-model management is **not** a platform command: omlx (the
  sole local-inference backend, macOS/Apple-Silicon only) manages its models
  entirely through its own admin panel (`ai services console omlx`); the
  platform's job is limited to installing/running the one shared omlx server
  and keeping LiteLLM's `omlx/<id>` registrations synced with its live
  `GET /v1/models` — automatically at `ai setup`/`ai services start|restart
  omlx`, or on demand via `ai models refresh`. Tests: AT §7.1, §7.2, §7.3.
* **M5 — debian-trixie image + Microsandbox — MOOT, superseded by nono session
  launch.** Originally: seed `.ai-platform/Dockerfile` from the
  `debian-trixie` template, build the workspace OCI image from it; create/start
  the microVM (virtio-net + gvproxy, the per-project network policy **applied as
  `msb` net-rules at create** — deny fallthrough + allow rules, default mode
  `public`, §3.4); mounts/volumes;
  `ai exec`; write the in-VM agent provider config so the agent reaches the host
  nginx gateway (`http://<gateway>:18787/v1` → LiteLLM, which calls Headroom
  in-process as a `pre_call` guardrail; resolved via
  `runtime.ResolveGateway`) with its scoped virtual key (arch §17).
  **Post-migration, what survives of this milestone's goal:** there is no
  image, no build, no microVM, no mounts/volumes, and no `ai exec`. "The agent
  reaches the gateway with its scoped key" is now delivered by
  `workspace.Manager.Agent` at **every** `ai agent`/`ai shell` launch (not a
  one-time create/start step): mint the scoped key (unless OAuth mode), store
  it in the OS keychain, write the CLI's keyless config pointing at
  `http://127.0.0.1:18787/v1` (`runtime.ResolveGateway`,
  `runtime.DefaultGatewayHost`/`DefaultGatewayPort`), generate that CLI's nono
  profile (`internal/workspace/profile.go`), and launch a detached `nono run`
  session with the key injected via `--env-credential-map`.
  Tests: AT §6.1, the harness's workspace-start threshold, AT §16.2, AT §16.3
  as originally written describe the now-superseded microVM path; whether/how
  they were updated for the nono path is a `05-acceptance-tests.md` concern,
  out of scope here.
* **M6 — `ai create` wizard + delete.** Originally: in-process
  `charmbracelet/huh` wizard (CLI §3.1) with steps for
  name/OS/agent-CLIs/default-agent/**software-stacks**/**AI tools**/**in-VM apps**
  (plus a dynamic per-selected-app host-port phase), with `--os` **required**
  under `--json`/no-TTY (missing `--os` → exit 2), and `create` scaffold-only
  — the workspace OCI image build, the microVM, and the agent's scoped
  LiteLLM virtual key created on demand by `ai start`. `ai delete` tore down
  the workspace microVM before removing project state.
  **Post-migration, the wizard today** (`internal/cli/project.go`'s
  `runCreateWizard`) has these groups, in order: **Name** + **Location** +
  **Shell** (bash|zsh) → **Agent CLIs** (multi-select, ≥1 required) →
  **Default agent CLI** → a conditional **auth-mode** group per OAuth-capable
  CLI selected (claude-code/codex/gemini — api-key default, oauth the
  alternative) → **AI tools** (ONE multi-select: caveman, graphify,
  code-review-graph, codebase-memory-mcp, openviking, tokensave, agentmemory,
  ponytail, agent-skills → `context.*_enabled` bools; `create.DefaultAITools()`
  returns nil, so **none** is pre-selected, including caveman/graphify — a
  change from the pre-migration default of both ON) → **Graphify model**
  (a picker over omlx's live served-model list, shown only when graphify was
  selected above; no free-text entry, no download). **There is no OS step, no
  software-stacks step, no in-VM-apps step, and no resource
  (vCPU/memory/disk/ports/idle-timeout) step** — those mechanisms are gone.
  The current flag set is `--name`/`--agents`/`--location`/`--shell`/`--auth-mode`/`--tools`/`--graphify-model`
  (`createFlags`, same file) — **no `--os`, `--stacks`, `--apps`, `--app-port`,
  `--cpus`, `--memory`, `--disk`, `--ports`, or `--idle-timeout`** — each
  pre-seeds the wizard on a TTY (the wizard always shows); under `--json`/no-TTY
  the spec is built straight from the flags with no prompt, defaulting to
  `opencode` if `--agents` is omitted — **there is no required flag anymore**
  (no `--os`-style exit-2-if-missing case). `ai create` then scaffolds, in the
  target directory, `.ai-platform/config.yaml` (agent tools + default_tool +
  auth_modes + context `*_enabled` flags + network defaults — **no
  Dockerfile, no `apps:` block, no `profile.yaml` stacks field**) +
  `project.yaml` + `.gitignore`, and registers the project in
  `config/projects.yaml`. **`create` remains scaffold-only**, but what it
  defers to the first `ai agent`/`ai shell` launch has changed: there is no
  image/microVM to build — the first launch mints the scoped virtual key,
  writes the CLI's keyless config, and generates that CLI's nono profile
  (§4 M5 above). `ai delete` no longer "tears down the workspace microVM" —
  it stops any of the project's live nono sessions (`Manager.StopAll`) and
  revokes its OS-keychain credential (`Manager.DeleteCredentials`) before
  removing the project's `.ai-platform` state.
  Tests: AT §3.1 (incl. no-TTY + abort), §6.3 (CLI selection), §6.4 (stack
  selection — moot, stacks are gone), §3.3, §9.1, §9.2.
* **M7 — `ai doctor`.** All S1 dependency/health checks with actionable output.
  Tests: AT §11.1, §14.1.
* **M8 — Acceptance harness.** Go harness (ephemeral `$AIP_TEST_HOME`, in-process
  mock provider, credential sentinel) with all `[S1]` tests green. Tests: AT
  §1.6 plus every `[S1]`-tagged case, AT §16.1.

Slice 1 is complete only when every `[S1]` test passes with no manual config.

---

# 5. Later Slices (sequencing)

* **S2 Context Optimization.** `contextopt/`: the Headroom compression service
  (`aip-headroom`), which LiteLLM calls as a `pre_call` guardrail
  (`/v1/compress`), driven per-request by the per-project strategy; per-project
  Caveman skill installed into
  `<project>/.ai-platform/skills/`; `ai context status|strategy|caveman`. (No
  platform memory — agent owns it.) Tests `[S2]`.
  **Post-migration:** Caveman is now one of the opt-in AI tools selected at
  `ai create`/`ai tools enable` (not scaffolded by `create`), intended to
  install on the host at the next `ai agent`/`ai start` — the exact installer
  call site was not found in the current `internal/workspace` package, so
  treat this as **unverified** rather than confirmed-working. `ai context
  status|strategy|caveman` are unchanged (Docker-tier Headroom + the
  `ContextConfig.CavemanLevel` setting).
* **S3 — Removed.** Multi-agent ORCHESTRATION and git worktrees are the
  in-workspace agent CLI's concern, not the platform's: one workspace per project,
  no platform-managed agent identities/branches/worktrees (arch §20–22). The flat
  `ai agent` / `ai attach` / `ai sessions` commands that exist are **nono
  session** launchers (post-migration — originally tmux session launchers
  inside a Microsandbox microVM), not orchestration (CLI §4.5b). The `[S3]`
  tag is retired.
* **S4 Overlay persistence — MOOT, mechanism deleted.** `overlay/`:
  per-workspace persistent overlay so installs + agent state survive
  restart/recreation (arch §26). No snapshot versioning/upgrade/rollback and
  no backup (arch §25, §32). Tests `[S4]`. **Post-migration:** `internal/overlay`
  is deleted — there is no VM disk to overlay. The project directory on host
  IS the persistent state now (roadmap §5 has the full explanation); the one
  piece of state needing a durable store, the per-project gateway credential,
  is handled by the OS keychain via `internal/nono`, not anything overlay-shaped.
* **S5 Extended OS — MOOT, mechanism deleted.** Add `alma`, `ubuntu` Dockerfile
  templates (S1 ships `debian-trixie`); OS-equivalence test. Tests `[S5]`.
  **Post-migration:** `internal/templates`/`internal/envimage` are deleted —
  there is no per-OS Dockerfile template of any kind anymore, so there is
  nothing left to extend (roadmap §6).
* **S6 Linux + Podman.** Podman `Runtime` impl; Linux launcher; abstraction
  equivalence. Tests `[S6]`. Podman support for the **service tier** is
  unaffected by the workspace-runtime migration. **Post-migration:** the
  original "Linux launcher" bullet implied KVM virtualization for Microsandbox
  workspaces (roadmap §7) — nono needs no such thing on Linux (Landlock LSM
  instead), so Linux workspace support no longer depends on KVM at all.
* **S7 — Removed.** Slice 7 originally added native Windows (later narrowed to
  WSL2-only) host support; Windows support was subsequently dropped from the
  platform entirely — the supported hosts are macOS and Linux only (§1; the
  "Apple Silicon" qualifier on macOS was a Microsandbox-era constraint the
  workspace runtime no longer has, though omlx local inference remains
  macOS/Apple-Silicon-only for an unrelated reason). The `[S7]` tag is retired
  with no replacement slice.

Each slice must not break prior slices (roadmap §1).

**In-VM AI apps — REMOVED ENTIRELY (post-migration), not merely moot.**
Originally a post-S1 cross-cutting feature: `apps/`, opt-in in-VM applications
(Open WebUI) running as rootful nerdctl containers inside the workspace
microVM, pointed at the same model gateway as the agent CLIs, with data
persisted on the workspace overlay and reachable from the host on a
per-(workspace, app) unique published port; surfaced by `ai apps
<list|add|remove|update|start|stop|restart>`; agent-CLI web dashboards
(hermes) got the same treatment. **`internal/apps` has been deleted along with
the entire concept** — there is no in-VM container runtime anymore (no guest
to run nerdctl/containerd inside), so there is no `ai apps` command, no
`config.yaml` `apps:`/`agent_dashboards:` blocks, and no published app ports.
This was not replaced by anything; in-VM apps are simply out of scope now. A
user who wants Open WebUI or a similar tool runs it on the host directly,
outside the platform's management.

**Status.** The full surface is implemented — milestones M0–M8 plus slices S1,
S2, S4–S6 (S3 and S7 retired) — host-side and, on a provisioned Apple Silicon
host, verified end-to-end against the live external tools, as of the
Microsandbox-based implementation. **The workspace runtime has since been
migrated end-to-end from Microsandbox microVMs to nono** (host-process
kernel-capability sandboxing); this plan's narrative has been updated in place
to describe that migration, but a fresh live-hardware verification pass
against the **nono**-based workspace path (mirroring the one already done for
Microsandbox) has not been documented here. Service log capture
(`ai logs`/`ai services status`) and the nginx readiness probe are Docker-tier
and unaffected. The pre-existing `hardware bring-up` seams this status
paragraph used to list — the real omlx install and shared `omlx serve`
bring-up + LiteLLM live-model-sync round-trip, live confirmation that the
tool-firewall blocks destructive tool-calls end-to-end against a running
agent, and live end-to-end confirmation of the nginx→LiteLLM(→Headroom)
forward chain — are all Docker/service-tier concerns, still open, and still
grep-able (`hardware bring-up`). The one bring-up item this status paragraph
used to list that is now **moot rather than open** is "the CLI-backend
(non-default) microVM log follow" — there is no microVM, no CLI-backend
fallback, and no `msb` log follow of any kind anymore. `docs/HARDWARE-BRINGUP.md`
itself still describes the old msb/microVM bring-up checklist nearly
verbatim and has not been updated for this migration — treat it as stale
until it is.

---

# 6. Testing & CI

* **Unit tests** per package (state atomicity, config merge precedence, runtime
  detection, output envelope). **Post-migration:** "sandbox name/lifecycle
  mapping" is now nono-**session** name/lifecycle mapping (`internal/workspace`'s
  `Name`/`Agent`/`Shell`/`Attach`/`ListSessions`/`Stop` over a fake
  `nono.Runner`) — `internal/sandbox` (and its microVM naming tests) no longer
  exists.
* **Acceptance harness** (`test/acceptance/`) implements AT §1.6: ephemeral HOME,
  an **in-process** mock provider (`httptest`) that records injected credentials,
  provider config written inline (no `fixtures/` dir), non-interactive confirmation
  via `--yes`. Tests assert the CLI §19 JSON envelope and CLI §18 exit codes — never
  scraped text.
* **Integration suite** (`test/integration/`, `//go:build integration`, `make
  test-integration`) — a live black-box suite that drives the real service stack,
  organized as `TestGroupNN` functions including 01 setup/health, 02 services, 03
  models & keys, 04 workspace lifecycle, 05 gateway inference, 06 uninstall, and
  07 the host-native omlx local-inference runtime — there is no per-model-runtime
  concept anymore (Group 7 explicitly asserts omlx is the sole backend).
  Group 7 (`TestGroup07InferenceRuntime`, in `test/integration/omlx_runtime_test.go`)
  asserts `services`/`doctor` surface the host-native `omlx` backend (and
  explicitly fail if a stale `vllm`/`ollama` entry is still reported), exercises
  `ai models refresh`, and self-skips the positive `omlx/<id>` routing round-trip
  when the omlx server is not up. Runs only where a full stack is available
  (Apple Silicon, for omlx — not for the workspace runtime, which no longer
  needs it), separate from the hosted unit lane. **Post-migration:** Group 04
  (`test/integration/workspace_lifecycle_test.go`) now exercises nono-sandboxed
  agent sessions, not microVM create/start/stop; this plan has not independently
  re-verified its current assertions against the nono path.
* **CI** (`ci.yml`, as it exists today — unchanged by this migration; noted
  here only because its stated *rationale* has gone stale), split by hardware
  needs:
  * **hosted runners** (every push): build the binary, `go vet`/`golangci-lint`,
    and the full unit-test suite. No virtualization required.
  * **self-hosted Apple Silicon macOS runner** (for `[S1]` acceptance): originally
    justified as "the only place that can run the full stack — Microsandbox
    needs the Apple Hypervisor (Apple Silicon) for workspace microVMs *and*
    Docker for the rootless service tier". **Post-migration, that specific
    justification is stale** — nono has no Apple-Silicon/virtualization
    requirement, so the workspace half of this reasoning no longer holds; the
    remaining reasons to still require Apple Silicon here are Docker's
    rootless service tier (unaffected, not Apple-Silicon-specific) and
    exercising the macOS/Apple-Silicon-only omlx backend end-to-end.
    Whether this self-hosted-macOS-only constraint should be relaxed (e.g. to
    allow a Linux self-hosted runner to cover the workspace half of `[S1]`) is
    **flagged as worth revisiting, not actioned here** — `ci.yml` itself is
    unchanged. It still runs the `[S1]`-tagged acceptance suite only on manual
    `workflow_dispatch` (not on every push/PR like the hosted lane).
  * Slice tags gate which acceptance tests run per environment; a prior plan
    for later slices to add a Linux **(KVM)** self-hosted runner for `[S6]` is
    itself stale for the same reason — a Linux self-hosted runner for the
    workspace half of acceptance would need only `nono` installed, not KVM.
* **Release** (`release.yml`): on merge to `main` it autobumps the semver tag
  from Conventional Commits (`mathieudutour/github-tag-action`; feat → minor,
  fix → patch), then cross-compiles a **matrix** (darwin/arm64, linux/amd64,
  linux/arm64 — linux/arm64 built with the aarch64 GNU C cross toolchain for the
  cgo build; darwin/amd64 is dropped, Intel Macs are unsupported, §6.2) and
  publishes a GitHub Release with the per-target binaries, `SHA256SUMS`, and the
  `installers/install.sh` launcher. **Post-migration note:** this still
  reflects the repo's current `release.yml` and `Makefile` (`CGO_ENABLED=1`,
  darwin/amd64 excluded) verbatim, but per §1 above, no `.go` file imports the
  Microsandbox SDK anymore — the cgo requirement this matrix's cross-compile
  approach is built around may itself be a stale leftover; not re-evaluated
  here. Locally, `make release` builds the release
  binary for the host platform into `dist/ai-<os>-<arch>`; `make check`
  (= `fmt-check vet vet-tagged lint test build`) is the pre-commit gate.

---

# 7. Host Prerequisites (pre-flight)

`ai setup` fails fast (exit 3) with actionable output if missing:

* supported OS (originally S1: macOS on Apple Silicon — **post-migration**: the
  workspace runtime itself needs only macOS or Linux, no Apple Silicon/virtualization;
  Apple Silicon is still needed for the separate, unrelated omlx
  local-inference backend)
* container runtime (S1: Docker) + rootless capability (service tier)
* **post-migration:** the `nono` CLI installed (`nono.Detect`) — a plain PATH
  check, not a hard pre-flight gate (a missing `nono` is reported, not a
  blocking failure, since a server-only host legitimately has none); there is
  no host-virtualization/Apple-Hypervisor-entitlement check anymore (that was
  Microsandbox-specific, §29.1)
* **post-migration:** no admin networking in a different sense than before —
  egress is now a per-project policy compiled into each agent's **nono
  profile** (`network.block`/`allow_domain`/`deny_domain`), not a userspace
  Microsandbox NetworkPolicy/`msb` net-rule set; still no `utun`/NetworkExtension/admin
  networking involved (§8.2); version control is out of scope, so no git
  prereq
* provider credentials are loaded into the LiteLLM gateway via `ai keys`
  (encrypted in the LiteLLM Postgres DB; not a pre-flight hard-fail — `setup`
  warns if a routed provider has no key; a model call fails only when its
  credential is actually absent)
* free ports + resolvable `AI_PLATFORM_HOST`
* **post-migration:** sufficient disk/CPU/RAM for the Docker service tier —
  there is no image build and no workspace microVM to size for anymore (a
  nono session is just a sandboxed invocation of whatever the agent CLI binary
  already needs)

(See architecture §5–6, §29.)

---

# 8. Open Decisions / Risks

1. **Service provisioning — RESOLVED.** `ai setup` installs and manages
   all host services itself (LiteLLM + its Postgres, the Presidio secret-masking
   pair, and Headroom the input-compression guardrail service) as the single
   control plane, reconciles the host-native omlx inference backend
   (not containerized, macOS/Apple-Silicon only), and verifies the Microsandbox
   workspace runtime;
   the user pre-installs only the container runtime. No docker compose:
   container-tier services run via the runtime abstraction, and
   workspace microVMs via Microsandbox (no daemon). See architecture §5.
   **Post-migration:** "verifies the Microsandbox workspace runtime" is now
   "best-effort installs/verifies **nono**" (`EnsureNonoInstalled`, a PATH
   check + `brew`/curl installer, never a hard setup failure) and "workspace
   microVMs via Microsandbox (no daemon)" is now "agent sessions sandboxed by
   **nono** (no daemon, no VM at all)". The container-runtime-abstraction half
   of this decision (service tier via docker/podman, no compose) is unchanged.
2. **Egress model — RESOLVED, mechanism replaced (nono profile, not
   Microsandbox NetworkPolicy).** Originally: workspace egress was a
   per-project **Microsandbox NetworkPolicy** built on msb's deny fallthrough
   plus explicit allow rules, with the workspace reaching the network over a
   virtio-net NIC via gvproxy (userspace), rendered into `msb` net-rules and
   applied at workspace create (`egress.MsbNetworkArgs` → `msb create`); the
   only elevated facility was the macOS hypervisor entitlement (arch §29.1,
   §30), with a hardware spike gate to prove that entitlement plus the
   net-rule model worked with no `utun`/NetworkExtension/admin prompt/kext.
   **Post-migration:** there is **no egress proxy and no virtual NIC at all**
   — a nono-sandboxed agent shares the host's own network stack directly.
   Egress is `config.NetworkConfig{Egress, AllowDomain, DenyDomain}` compiled
   straight into the project's **nono profile** `network` section
   (`network.block` for `deny`, `allow_domain`/`deny_domain` lists) by
   `internal/workspace/profile.go`'s `ensureProfile`, regenerated on **every**
   `ai agent`/`ai shell`/`ai start` launch rather than applied once at create.
   The **default mode is still `public`** (nono's own unrestricted-outbound
   default), matching the pre-migration default; `TestEgressConfinementOnHardware`
   (AT §16.3)'s known-stale-assertion caveat was specific to the `msb`
   net-rule model and needs re-evaluating against the nono path (not done
   here). The macOS-hypervisor-entitlement hardware-spike gate is now **moot**
   — there is no hypervisor entitlement to prove, since there is no VM; the
   equivalent open question for nono would be confirming its Landlock/Seatbelt
   enforcement actually blocks a denied domain on each supported host, which
   has not been spiked here.
3. **Headroom placement (decided).** Headroom is a host service-tier container
   (image ref + full mechanism: architecture §10) called BY LiteLLM as an
   in-process `pre_call` guardrail, NOT an nginx proxy in front of it (§3.5, §5
   S2). It is ALSO installed inside each workspace image
   (`uv tool install "headroom-ai[proxy]"`) for a future in-VM `headroom wrap`.
   Caveman remains the symmetric in-workspace output-compression skill.
   **Post-migration:** "installed inside each workspace image" no longer
   applies — there is no image. The in-session `headroom wrap <cli>` path is
   now purely opportunistic: `workspace.Manager.Agent` checks
   `exec.LookPath("headroom")` on the **host** at every launch and wraps
   claude-code/codex/opencode when found (`agentcfg.HeadroomWrapName`); the
   platform does not install the `headroom` CLI onto the host itself. Caveman
   remains the symmetric compression skill, now installed on the host as one
   of the opt-in AI tools (see item 5 below).
4. **Image build — MOOT, no longer applicable.** Originally: the workspace OCI
   image was built from `.ai-platform/Dockerfile` with the detected container
   runtime and booted as a Microsandbox microVM; confirm rootless build works
   for all OS templates and that each image boots cleanly as a microVM.
   **Post-migration:** there is no `.ai-platform/Dockerfile`, no OCI image, and
   no microVM boot — `internal/envimage` and `internal/templates` are deleted.
   This item has no current equivalent; it is simply retired.
5. **Caveman packaging.** Installed per project into `.ai-platform/skills/caveman/`
   (not the image); validate its output-compression integration with each tool
   provider in S2. **Post-migration:** Caveman is now one of the opt-in AI
   tools (`create.AIToolCaveman`), no longer pre-selected by default, and
   intended to install directly on the **host** at the next `ai agent`/`ai
   start` rather than being baked into an image — the exact installer call
   site was not found in the current `internal/workspace` package during this
   review, so this is flagged as **unverified**, not confirmed-working.

---

# 9. Definition of Done (per slice)

* all slice commands implemented with `--json` + exit codes
* all slice-tagged acceptance tests green in CI
* no manual configuration or filesystem intervention required
* prior slices still pass (regression)
