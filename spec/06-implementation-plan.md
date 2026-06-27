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

---

# 1. Tech Stack

* **Language: Go.** The `ai` CLI, installers, state management, Microsandbox
  orchestration, runtime abstraction, and diagnostics are all
  Go, shipped as a **single static binary** per host.
* **Thin launchers only** (bash/zsh) bootstrap the binary; no platform logic in
  shell. Supported hosts are macOS (Apple Silicon) and Linux — there is no
  Windows/PowerShell path.
* **Declarative config/templates** in YAML/JSON; never executable logic.
* External components are invoked as subprocesses or over HTTP, never
  reimplemented: Microsandbox (the `msb` CLI), LiteLLM (host service over HTTP),
  docker / podman (subprocess). Version control is out of scope — no `git`/`gh`.

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
│   ├── paths/                   # ~/.ai-platform layout helpers (workspaces live in the cwd, not a fixed ~/projects)
│   ├── conffile/                # atomic YAML read/write (temp file + rename; rejects unknown fields)
│   ├── state/                   # project-local state (<project>/.ai-platform/run) + projects index; atomic writes
│   ├── config/                  # config load/merge (project > global)
│   ├── versions/                # service-tier image refs (image+tag, no digest) — source of truth for setup
│   ├── envfile/                 # ~/.ai-platform/.ai-platform.env (opt-in 0600 secrets passthrough: UI password, master key)
│   ├── runtime/                 # docker/podman detect + rootless verify + role/domain/gateway resolution (service tier)
│   ├── sandbox/                 # Microsandbox SDK wrapper: naming, mounts/volumes, microVM lifecycle
│   ├── services/               # service-tier topology registry (names, ports, UI subdomains, gateway paths)
│   ├── hostsfile/              # managed /etc/hosts block writer (delimited, idempotent)
│   ├── uihosts/                # UI-vhost logic: the litellm.<domain> host vhost + its /etc/hosts entry (composes services + hostsfile)
│   ├── console/                # host-display endpoint registry (UI subdomains + gateway paths off the nginx port)
│   ├── litellm/                 # host lifecycle, config gen, health, routing, guardrails, virtual-key + credential KeyManager (keys-in-LiteLLM; fronted by `ai keys` in cli/keys.go)
│   ├── ollama/                  # required local-model service + bundled Ollama catalogue (models.yaml)
│   ├── agentcfg/                # in-VM agent provider config (base_url→nginx gateway, virtual key, picker models, refresh-models)
│   ├── contextopt/              # per-project Headroom strategy (→ host Headroom proxy per-request knobs) + in-workspace Caveman skill
│   ├── envimage/                # compose .ai-platform/Dockerfile (OS template + stack snippets + agent CLIs) + build OCI image
│   ├── workspace/               # workspace lifecycle + tmux-transparent sessions (Builder/Sandbox/Manager)
│   ├── apps/                     # opt-in in-VM AI apps (Open WebUI / AnythingLLM) — declarative manifests + per-(workspace,app) lifecycle over nerdctl; unique host-port allocation
│   ├── egress/                  # per-project egress policy → msb net-rules (MsbNetworkArgs)
│   ├── overlay/                 # per-workspace persistent overlay
│   ├── audit/                   # append-only audit log (no secrets)
│   ├── ui/ + tui/               # theme registry + the K9s-style `ai ui` management TUI
│   ├── templates/               # embedded source templates + installer into ~/.ai-platform/templates
│   │   └── files/
│   │       ├── dockerfiles/<os>/Dockerfile        # one base Dockerfile per OS key (alma, debian-trixie, debian-bookworm, ubuntu)
│   │       ├── stacks/<stack>/Dockerfile.snippet  # one install snippet per stack (java, maven, node, deno, go, python, rust)
│   │       └── agentclis/                          # per-agent-CLI install snippets
│   ├── uninstall/               # native `ai uninstall` teardown
│   └── doctor/                  # consolidated health checks → repair suggestions
├── installers/                  # install.sh (+ install-local.sh) thin launchers (macOS/Linux)
├── test/acceptance/             # Go acceptance harness + fixtures (AT §1.6) — [S1] stub
│   └── fixtures/                # sample-app, large-repo gen, mock-provider
├── go.mod
├── Makefile
└── .github/workflows/ci.yml
```

Principle: `cli/` stays thin (parse flags → call a package → render via
`output/`). All logic is testable without the CLI.

`ai setup` installs the templates embedded under
`internal/templates/files/` (OS `dockerfiles/<os>/Dockerfile`, software-stack
`stacks/<stack>/Dockerfile.snippet`, and agent-CLI snippets) into
`~/.ai-platform/templates/` (the runtime paths `ai create` composes from —
repo-layout §1.5).

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
* `ai state repair`: reconcile `run/` from filesystem + Microsandbox (no git —
  version control is out of scope)

## 3.3 Config (`config/`)

* load + merge two layers with precedence `project > global`
* one `Config` struct matching repo-layout §12.4; missing layers skipped

## 3.4 Runtime Abstraction (`runtime/`, `sandbox/`)

* `runtime/` (service tier): detect docker/podman; verify rootless; write
  `config/runtime.yaml`; `Runtime` interface so docker/podman are interchangeable
  (Podman impl in S6)
* `sandbox/` (workspaces): drive Microsandbox via the **`msb` CLI**; verify the
  microVM runtime + host virtualization (Apple Silicon / KVM); no daemon to
  supervise. The adapter creates each workspace microVM **and applies its egress
  network policy as `msb` net-rules at create**. The enforcement primitive is
  msb's deny fallthrough (`--net-default-egress deny`) plus explicit allow rules;
  the **per-project default mode is now `public`**, which keeps that deny
  fallthrough but adds a broad `allow:egress@public` rule (open internet; private
  ranges still blocked) so a fresh workspace can pull in-VM container images and
  reach the internet. `deny` mode drops the broad rule and allows only the trusted
  host service ports + published ports — that locked-down posture is what AT §16.3
  asserts (it configures `deny` explicitly via the egress fixture). The model
  gateway is always reachable in every mode. The per-project egress policy itself
  (mode `deny`/`public`/`unrestricted`, allowed host services, published ports —
  repo-layout §12.4) is configured by the `ai network` command and rendered into
  the `msb create` net-rule fragment by `egress.MsbNetworkArgs`.

## 3.5 External Adapters & Service Control Plane

The `ai` CLI is the **single control plane** for host services (architecture §5,
"Host Services Control Plane"). All host services run in the **container tier**
behind uniform `ai services` verbs — **no docker compose**:

* **container tier**, reconciled in order network → DNS → Ollama → Presidio →
  LiteLLM(+DB) → Headroom → nginx proxy: the `aip-dns` CoreDNS
  egress-audit resolver, the containerized Ollama `aip-ollama`, the Presidio
  secret-masking pair `aip-presidio-analyzer`/`aip-presidio-anonymizer`, LiteLLM
  `aip-litellm` + its Postgres `aip-litellm-db`, the Headroom proxy
  `aip-headroom`, and the `aip-proxy` nginx gateway: managed directly via the
  `runtime/` abstraction (run by image+tag, restart policy, health poll) on the
  private `aip-net` network, so docker and podman stay interchangeable. **Only the
  `aip-proxy` nginx gateway is host-published** (host port `18787`, the sole
  entry); every other service is internal-only on `aip-net` and reached through
  it (Postgres + DNS stay loopback). `ai services update` re-pulls moved tags and
  recreates affected containers. (Open WebUI is now an opt-in **in-VM** app and
  Odysseus was removed, so the host tier has no optional services; the optional
  mechanism is retained for future host services.)

The Microsandbox runtime is **not** a managed service: its `msb` binary is
pinned into `tools/` and invoked on demand via `sandbox/` to create and drive
workspace microVMs; `ai setup` only verifies it is installed and the host
supports virtualization.

Each service's config is **rendered** from the platform config into
`config/<service>/` (including the nginx vhost map for the `litellm.<domain>` UI
subdomain — the only host UI vhost — and the `/v1`, `/ollama`, `/llm` gateway
paths); real provider keys stay only in the LiteLLM gateway (keys-in-LiteLLM).
Service-tier image refs are pinned by **image+tag** (no digest — digests are
platform/arch specific) in `config/versions.yaml`. (The in-VM apps pin their own
images in `internal/apps`, not in the host `versions.yaml`.)

| Adapter | Integration | Run mode | First slice |
|---|---|---|---|
| `sandbox/` | Microsandbox Go SDK / `msb`; names `aip-<project>[-<agent>]`; microVM lifecycle map (arch §7) | microVM runtime (no daemon) | S1 |
| `litellm/` | container via `runtime/`; config rendered from routing; `/health` poll; `KeyManager` mints scoped virtual keys + stores provider credentials (keys-in-LiteLLM, fronted by `ai keys`) | container | S1 |
| `contextopt/` | per-project Headroom strategy (drives the host `aip-headroom` proxy per-request) + Caveman skill installed in the workspace | container (Headroom) / workspace (Caveman) | S2 |

---

# 4. Slice 1 — MVP Build Sequence

Slice 1 target (roadmap §2): macOS (Apple Silicon), Docker rootless service tier,
Microsandbox microVM workspaces, debian-trixie, single agent, LiteLLM,
keys-in-LiteLLM credentials (agent holds a scoped virtual key), zero manual config.
Commands (exactly the `[S1]`-tested surface): `ai setup`,
`ai create` (interactive wizard), `ai delete`,
`ai start|stop|destroy|exec`, `ai services status`,
`ai keys add|list|remove`, `ai models status|test`, `ai state show|repair`,
`ai doctor`, `ai logs`.
(No snapshot/upgrade/rollback commands — the environment is the project's
`.ai-platform/Dockerfile`, arch §25; overlay persistence is `[S4]`.)

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
  Tests: AT §11.1.
* **M3 — `ai setup` + services.** Preflight (exit 3 on missing deps),
  init `~/.ai-platform/`, install/configure/start the container service tier
  (DNS resolver, Ollama, Presidio pair, LiteLLM + its DB, Headroom, nginx gateway)
  with the role-driven bind host (server 0.0.0.0,
  standalone/client loopback) + verify the Microsandbox runtime; provider keys
  live in the LiteLLM gateway (keys-in-LiteLLM, §8.2); render the per-project
  network policy template (default mode `public`, §8.2); `ai services status`;
  **idempotent**.
  Tests: AT §2.1, §2.2, §2.3, §12.1 (macOS install).
* **M4 — Model layer + secrets.** LiteLLM config gen + routing default
  (the generated config also renders **always-on guardrails**, all
  `default_on: true` so no request — cloud included — can bypass them: Presidio
  scoped to financial/identity **secrets** input+output, `hide-secrets`, the
  `detect_prompt_injection` callback, and the `tool_permission` tool firewall;
  general-PII masking and the unmaintained LLM Guard were removed);
  `ai keys add/remove`; `ai models status`, `ai models test` against the mock
  provider. Tests: AT §7.1, §7.2.
* **M5 — debian-trixie image + Microsandbox.** Seed `.ai-platform/Dockerfile` from the
  `debian-trixie` template, build the workspace OCI image from it; create/start
  the microVM (virtio-net + gvproxy, the per-project network policy **applied as
  `msb` net-rules at create** — deny fallthrough + allow rules, default mode
  `public`, §3.4); mounts/volumes;
  `ai exec`; write the in-VM agent provider config so the agent reaches the host
  nginx gateway (`http://<gateway>:18787/v1` → Headroom → LiteLLM, resolved via
  `runtime.ResolveGateway`) with its scoped virtual key (arch §17). Tests: AT §6.1,
  harness workspace-start threshold, AT §16.2, AT §16.3.
* **M6 — `ai create` wizard + delete.** In-process `charmbracelet/huh` wizard
  (CLI §3.1) with steps for
  name/OS/agent-CLIs/default-agent/**software-stacks**/**in-VM apps**, each with a
  presented default, checkbox multi-select for CLIs + stacks + apps,
  arrow/space navigation, Back + Abort. Every input also has a flag
  (`--name`/`--os`/`--agents`/`--stacks`/`--apps`) that **pre-seeds** the wizard
  on a TTY
  (the wizard always shows); under `--json`/no-TTY the spec is built straight from
  the flags with no prompt and `--os` is **required** (missing `--os` → exit 2).
  Then, in the current directory (no git — VCS is out of scope), write
  `.ai-platform/` (Dockerfile = OS template + selected stack snippets + selected
  CLIs / config incl. `agent.tools`+`default_tool` + any selected `apps` (with an
  allocated unique host port each) / `profile.yaml` incl. `stacks` / project.yaml /
  .gitignore) + index in `config/projects.yaml`.
  `create` is scaffold-only — the workspace OCI image build, the microVM, and the
  agent's scoped LiteLLM virtual key are created on demand by `ai start`
  (or by `create`'s attach path when the cwd is already a project), not on first
  creation. `ai delete` tears down the workspace microVM before removing
  project state.
  Tests: AT §3.1 (incl. no-TTY + abort), §6.3 (CLI selection), §6.4 (stack
  selection), §3.3, §9.1, §9.2.
* **M7 — `ai doctor`.** All S1 dependency/health checks with actionable output.
  Tests: AT §10.1, §14.1.
* **M8 — Acceptance harness.** Go harness (ephemeral `$AIP_TEST_HOME`, fixtures,
  mock-provider, credential sentinel) with all `[S1]` tests green. Tests: AT
  §1.6 plus every `[S1]`-tagged case, AT §16.1.

Slice 1 is complete only when every `[S1]` test passes with no manual config.

---

# 5. Later Slices (sequencing)

* **S2 Context Optimization.** `contextopt/`: host Headroom proxy (`aip-headroom`)
  on the request path in front of LiteLLM, driven per-request by the per-project
  strategy; per-project Caveman skill installed into
  `<project>/.ai-platform/skills/`; `ai context status|strategy|caveman`. (No
  platform memory — agent owns it.) Tests `[S2]`.
* **S3 — Removed.** Multi-agent ORCHESTRATION and git worktrees are the
  in-workspace agent CLI's concern, not the platform's: one workspace per project,
  no platform-managed agent identities/branches/worktrees (arch §20–22). The flat
  `ai agent` / `ai attach` / `ai sessions` commands that exist are tmux session
  launchers, not orchestration (CLI §4.5b). The `[S3]` tag is retired.
* **S4 Overlay persistence.** `overlay/`: per-workspace persistent overlay so
  installs + agent state survive restart/recreation (arch §26). No snapshot
  versioning/upgrade/rollback and no backup (arch §25, §32). Tests `[S4]`.
* **S5 Extended OS.** Add `alma`, `debian-bookworm`, `ubuntu` Dockerfile
  templates (S1 ships `debian-trixie`); OS-equivalence test. Tests `[S5]`.
* **S6 Linux + Podman.** Podman `Runtime` impl; Linux launcher; abstraction
  equivalence. Tests `[S6]`.
* **S7 — Removed.** The `[S7]` tag is retired (no separate slice).

Each slice must not break prior slices (roadmap §1).

**In-VM AI apps (post-S1, cross-cutting).** `apps/`: the opt-in in-VM
applications (Open WebUI, AnythingLLM) that run as **rootful nerdctl containers
inside the workspace microVM**, pointed at the same model gateway as the agent
CLIs, with data persisted on the workspace overlay and reachable from the host on
a per-(workspace, app) unique published port. Surfaced by `ai apps
<list|add|remove|update|start|stop|restart>` and pre-seeded at create via
`ai create --apps`; installed apps are recorded in `config.yaml` (`apps:`,
repo-layout §12.4). The host **versions.yaml** no longer pins Open WebUI — the
in-VM apps pin their own images in `internal/apps`. (Live `nerdctl`/containerd
operation inside a booted microVM is a `hardware bring-up` seam.)

**Status.** The full surface is implemented — milestones M0–M8 plus slices S1,
S2, S4–S6 (S3 and S7 retired) — host-side and, on a provisioned Apple Silicon
host, verified end-to-end against the live external tools. The narrow seams that
still depend on further bring-up (live service/microVM log capture, verifying the
tool-firewall regexes against live agent tool schemas, the nginx readiness probe)
are grep-able (`hardware bring-up`) and tracked in `docs/HARDWARE-BRINGUP.md`.

---

# 6. Testing & CI

* **Unit tests** per package (state atomicity, config merge precedence, runtime
  detection, sandbox name/lifecycle mapping, output envelope).
* **Acceptance harness** (`test/acceptance/`) implements AT §1.6: ephemeral HOME,
  fixtures, mock provider that records injected credentials, non-interactive
  confirmation via `--yes`. Tests assert the CLI §19 JSON envelope and CLI §18
  exit codes — never scraped text.
* **CI** (`ci.yml`), split by hardware needs:
  * **hosted runners** (every push): build the binary, `go vet`/`golangci-lint`,
    and the full unit-test suite. No virtualization required.
  * **self-hosted Apple Silicon macOS runner** (for `[S1]` acceptance): the only
    place that can run the full stack — Microsandbox needs the Apple Hypervisor
    (Apple Silicon) for workspace microVMs *and* Docker for the rootless service
    tier, which hosted macOS runners can't reliably provide. Runs the
    `[S1]`-tagged acceptance suite.
  * Slice tags gate which acceptance tests run per environment; later slices add
    a Linux (KVM) self-hosted runner for `[S6]`.
* **Cross-compile** matrix (darwin/arm64, linux/amd64, linux/arm64). (darwin/amd64
  is dropped — Intel Macs are unsupported, §6.2.)

---

# 7. Host Prerequisites (pre-flight)

`ai setup` fails fast (exit 3) with actionable output if missing:

* supported OS (S1: macOS on Apple Silicon)
* container runtime (S1: Docker) + rootless capability (service tier)
* Microsandbox runtime + host virtualization (Apple Hypervisor entitlement on macOS — the only elevated facility, §29.1)
* no admin networking (egress is a userspace Microsandbox NetworkPolicy — msb's deny fallthrough plus allow rules, default mode `public`; no `utun`/NetworkExtension/admin networking, §8.2; version control is out of scope, so no git prereq)
* provider credentials are loaded into the LiteLLM gateway via `ai keys`
  (encrypted in the LiteLLM Postgres DB; not a pre-flight hard-fail — `setup`
  warns if a routed provider has no key; a model call fails only when its
  credential is actually absent)
* free ports + resolvable `AI_PLATFORM_HOST`
* sufficient disk/CPU/RAM for image builds and workspace microVMs

(See architecture §5–6, §29.)

---

# 8. Open Decisions / Risks

1. **Service provisioning — RESOLVED.** `ai setup` installs and manages
   all host services itself (LiteLLM + its Postgres, the containerized Ollama,
   the Presidio PII-guardrail pair, and the Headroom proxy) as the single control
   plane, and verifies the Microsandbox workspace runtime;
   the user pre-installs only the container runtime. No docker compose:
   container-tier services run via the runtime abstraction, and
   workspace microVMs via Microsandbox (no daemon). See architecture §5.
2. **Egress model — RESOLVED (Microsandbox NetworkPolicy).** Workspace egress is a
   per-project **Microsandbox NetworkPolicy** built on msb's deny fallthrough plus
   explicit allow rules; there is **no egress proxy**:
   * the workspace gets a virtio-net NIC via **gvproxy** (userspace). The
     **per-project default mode is now `public`** (allow-outbound to the open
     internet; private ranges still blocked by the deny fallthrough), so a fresh
     workspace can pull in-VM container images and reach the internet — egress is
     still DNS-audited (`ai network log`) and re-lockable. `deny` mode permits only
     the trusted host service ports + published ports declared via `ai network`;
     the model gateway is always reachable in every mode. All planes are userspace;
     the only elevated facility is the macOS hypervisor entitlement (arch §29.1,
     §30). Under `deny`, a non-allow-listed destination is denied — tested by
     AT §16.3 (which configures `deny` via the egress fixture).
   * the per-project `ai network` declarations are rendered into `msb` net-rules
     and applied at workspace create (`egress.MsbNetworkArgs` → `msb create`, §3.4).
     **Gate (spike on hardware):** on a clean Apple Silicon Mac, prove (a) the
     `com.apple.security.hypervisor` entitlement works under Developer ID +
     notarization for a downloaded binary, and (b) the `msb` net-rules apply the
     deny-fallthrough + allow-rule model with no `utun`/NetworkExtension/admin
     prompt/kext.
3. **Headroom placement (decided).** Headroom runs as a **host service-tier
   container** (`aip-headroom`, pulled image `ghcr.io/chopratejas/headroom:slim`,
   internal :8787) — an input-compression proxy **in front of LiteLLM**, no longer
   baked into the workspace image, and now **internal-only on `aip-net` behind the
   nginx gateway** (no host publish). The per-project strategy
   (`ai context strategy`) maps to Headroom per-request knobs
   (`keep_turns`/`output_buffer_tokens` via `contextopt.HeadroomParams`). Caveman
   remains the symmetric in-workspace output-compression skill.
4. **Image build.** The workspace OCI image is built from `.ai-platform/Dockerfile`
   with the detected container runtime and booted as a Microsandbox microVM;
   confirm rootless build works for all OS templates and that each image boots
   cleanly as a microVM.
5. **Caveman packaging.** Installed per project into `.ai-platform/skills/caveman/`
   (not the image); validate its output-compression integration with each tool
   provider in S2.

---

# 9. Definition of Done (per slice)

* all slice commands implemented with `--json` + exit codes
* all slice-tagged acceptance tests green in CI
* no manual configuration or filesystem intervention required
* prior slices still pass (regression)
