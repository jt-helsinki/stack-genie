# 06-implementation-plan.md

# AI Development Platform

## Implementation Plan

Version: 1.0

---

# 0. Purpose

This document is the engineering plan for building the platform. It complements
the other specs:

* `01-architecture-spec.md` — end-state architecture (the *what*)
* `02-implementation-roadmap.md` — product slices S1–S7 (the *when*)
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
* **Thin launchers only** (bash/zsh/PowerShell) bootstrap the binary; no
  platform logic in shell.
* **Declarative config/templates** in YAML/JSON; never executable logic.
* External components are invoked via Go SDK or subprocess, never reimplemented:
  Microsandbox (Go SDK / `msb`), LiteLLM (host service over HTTP),
  ClawPatrol (gateway binary + config), git / docker / podman / gh (subprocess).

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
│   ├── paths/                   # ~/.ai-platform + ~/projects layout helpers
│   ├── state/                   # project-local state (<project>/.ai-platform/run) + projects index; atomic writes
│   ├── config/                  # config load/merge (project > global)
│   ├── runtime/                 # docker/podman detect + rootless verify + abstraction (service tier)
│   ├── sandbox/                 # Microsandbox SDK wrapper: naming, mounts/volumes, microVM lifecycle
│   ├── litellm/                 # host lifecycle, config gen, health, routing
│   ├── clawpatrol/              # gateway lifecycle, credential brokering, placeholders
│   ├── contextopt/              # per-project Headroom strategy + Caveman skill (both in-workspace)
│   ├── git/                     # project init/clone only (no platform branch/worktree/merge)
│   ├── envimage/                # compose .ai-platform/Dockerfile (OS template + stack snippets + agent CLIs) + build OCI image
│   ├── overlay/                 # per-workspace persistent overlay
│   ├── audit/                   # append-only audit log (no secrets)
│   └── doctor/                  # health checks → repair suggestions
├── dockerfiles/                 # one base Dockerfile per OS key
│   ├── alma/Dockerfile
│   ├── debian-trixie/Dockerfile
│   ├── debian-bookworm/Dockerfile
│   └── ubuntu/Dockerfile
├── stacks/                      # one install snippet per software stack (java, maven, node, deno, go, python, rust, …)
├── installers/                  # install.sh, install.ps1 (thin launchers)
├── test/acceptance/             # Go acceptance harness + fixtures (AT §1.6)
│   └── fixtures/                # sample-app, large-repo gen, mock-provider
├── go.mod
├── Makefile
└── .github/workflows/ci.yml
```

Principle: `cli/` stays thin (parse flags → call a package → render via
`output/`). All logic is testable without the CLI.

`ai setup` installs the source `dockerfiles/<os>/Dockerfile` and
`stacks/<stack>/` templates into `~/.ai-platform/templates/` (the runtime paths
`ai project create` composes from — repo-layout §1.5).

---

# 3. Cross-Cutting Foundations

These underpin every slice and are built first.

## 3.1 Output & Exit Codes (`output/`)

* one `Result{ ok, command, data, error, warnings }` type → CLI §19 envelope
* `--json` emits envelope to stdout; human renderer otherwise; logs to stderr
* central exit-code mapping (CLI §18)

## 3.2 State Store (`state/`)

* project-local state under `<project>/.ai-platform/run/` (per-entity files);
  global `config/projects.json` index of project name → path
* **atomic writes**: temp file + `rename()`; never partial state
* typed load/save for each schema (§12 of repo-layout); reject unknown fields
* `ai state repair`: reconcile `run/` from filesystem + Microsandbox + git

## 3.3 Config (`config/`)

* load + merge two layers with precedence `project > global`
* one `Config` struct matching repo-layout §12.4; missing layers skipped

## 3.4 Runtime Abstraction (`runtime/`, `sandbox/`)

* `runtime/` (service tier): detect docker/podman; verify rootless; write
  `config/runtime.json`; `Runtime` interface so docker/podman are interchangeable
  (Podman impl in S6)
* `sandbox/` (workspaces): wrap the **Microsandbox Go SDK** (`msb` only as a
  fallback); verify the microVM runtime + host virtualization (Apple Silicon /
  KVM / WSL2 nested-virt); no daemon to supervise. The adapter creates each
  workspace microVM **and applies its egress network policy via the Go SDK** —
  the default-deny + allow-rule model (deny by default; allow exactly the
  ClawPatrol proxy + trusted host service ports), which is what AT §16.3 asserts.
  (The Go SDK exposes the same network-policy core model as the other SDKs; pin
  the exact symbol against the SDK version at build time.)

## 3.5 External Adapters & Service Control Plane

The `ai` CLI is the **single control plane** for host services (architecture §5,
"Host Services Control Plane"). It manages two run modes behind uniform
`ai services` verbs — **no docker compose**:

* **container tier** (LiteLLM, optional Ollama): managed directly via
  the `runtime/` abstraction (run by digest, restart policy, health poll), so
  docker and podman stay interchangeable
* **native tier** (ClawPatrol): downloaded as a pinned checksum-verified binary
  into `tools/`, registered with the OS service manager (launchd / systemd /
  Windows)

The Microsandbox runtime is **not** a managed service: its `msb` binary is
pinned into `tools/` and invoked on demand via `sandbox/` to create and drive
workspace microVMs; `ai setup` only verifies it is installed and the host
supports virtualization.

Each service's config is **rendered** from the platform config into
`config/<service>/`; real secrets stay only in ClawPatrol. Versions pinned in
`config/versions.json`.

| Adapter | Integration | Run mode | First slice |
|---|---|---|---|
| `sandbox/` | Microsandbox Go SDK / `msb`; names `aip-<project>[-<agent>]`; microVM lifecycle map (arch §7) | microVM runtime (no daemon) | S1 |
| `litellm/` | container via `runtime/`; config rendered from routing; `/health` poll | container | S1 |
| `clawpatrol/` | native gateway; register creds; inject placeholders into workspace env | native | S1 |
| `contextopt/` | per-project Headroom strategy + Caveman skill; both installed in the workspace (Headroom in the image, Caveman as a skill) | workspace | S2 |
| `git/` | subprocess; project init/clone only | n/a | S1 |

---

# 4. Slice 1 — MVP Build Sequence

Slice 1 target (roadmap §2): macOS (Apple Silicon), Docker rootless service tier,
Microsandbox microVM workspaces, debian-trixie, single agent, LiteLLM, ClawPatrol
credential brokering, zero manual config.
Commands (exactly the `[S1]`-tested surface): `ai setup`,
`ai project create` (interactive wizard), `ai project delete`,
`ai workspace start|stop|destroy|exec`, `ai services status`,
`ai secrets set|map|list`, `ai models status|test`, `ai state show|repair`,
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
  `config/runtime.json`; fail (exit 4) if rootless or virtualization unavailable.
  Tests: AT §11.1.
* **M3 — `ai setup` + services.** Preflight (exit 3 on missing deps),
  init `~/.ai-platform/`, install/configure/start LiteLLM (container) + ClawPatrol
  gateway (native, credential brokering, **forward-proxy egress** — S1 model,
  §8.2) + verify the Microsandbox runtime; **ensure the ClawPatrol TLS-interception
  CA exists** (generate on first run; arch §17); render the ClawPatrol default-deny
  allowlist; `ai services status`; **idempotent**. Tests: AT §2.1, §2.2, §2.3,
  §12.1 (macOS install).
* **M4 — Model layer + secrets.** LiteLLM config gen + routing default;
  `ai secrets set/map`; `ai models status`, `ai models test` against the mock
  provider. Tests: AT §7.1, §7.2.
* **M5 — debian-trixie image + Microsandbox.** Seed `.ai-platform/Dockerfile` from the
  `debian-trixie` template, build the workspace OCI image from it; create/start
  the microVM (virtio-net + gvproxy, default-deny network policy **applied via the
  Go SDK**, §3.4); mounts/volumes;
  `ai workspace exec`; inject `AI_PLATFORM_HOST` + `HTTPS_PROXY` (ClawPatrol);
  **install the ClawPatrol CA root into the workspace trust store** so HTTPS
  injection works (arch §17). Tests: AT §6.1, harness workspace-start threshold,
  AT §16.2, AT §16.3.
* **M6 — `ai project create` wizard + delete.** Interactive PTY wizard (CLI §3.1)
  with steps for name/OS/agent-CLIs/default-agent/**software-stacks**,
  each with a presented default, checkbox multi-select for CLIs + stacks,
  arrow/space navigation, Back + Abort; no `--os`/per-choice flags; no TTY → exit
  2. Then project dir + git init (or `--clone`) + write `.ai-platform/` (Dockerfile
  = OS template + selected stack snippets + selected CLIs / config incl.
  `agent.tools`+`default_tool` / `profile.yaml` incl. `stacks` / project.json /
  .gitignore) + index in `config/projects.json` + workspace + ClawPatrol
  placeholders + pre-create N agents; `ai project delete`. Tests: AT §3.1 (incl.
  no-TTY + abort), §6.3 (CLI selection), §6.4 (stack selection), §3.2 (clone),
  §3.3, §9.1, §9.2.
* **M7 — `ai doctor`.** All S1 dependency/health checks with actionable output.
  Tests: AT §10.1, §14.1.
* **M8 — Acceptance harness.** Go harness (ephemeral `$AIP_TEST_HOME`, fixtures,
  bare-git clone fixture, mock-provider, credential sentinel) with all `[S1]`
  tests green. Tests: AT §1.6 plus every `[S1]`-tagged case, AT §16.1.

Slice 1 is complete only when every `[S1]` test passes with no manual config.

---

# 5. Later Slices (sequencing)

* **S2 Context Optimization.** `contextopt/`: in-workspace Headroom proxy on the request
  path; per-project Caveman skill installed into `<project>/.ai-platform/skills/`;
  `ai context status|strategy|caveman`. (No platform memory — agent owns it.)
  Tests `[S2]`.
* **S3 — Removed.** Multi-agent and git worktrees are the in-workspace agent
  CLI's concern, not the platform's: one workspace per project, no `ai agent`
  commands, no platform-managed branches/worktrees (arch §20–22). The `[S3]` tag
  is retired.
* **S4 Overlay persistence.** `overlay/`: per-workspace persistent overlay so
  installs + agent state survive restart/recreation (arch §26). No snapshot
  versioning/upgrade/rollback and no backup (arch §25, §32). Tests `[S4]`.
* **S5 Extended OS.** Add `alma`, `debian-bookworm`, `ubuntu` Dockerfile
  templates (S1 ships `debian-trixie`); OS-equivalence test. Tests `[S5]`.
* **S6 Linux + Podman.** Podman `Runtime` impl; Linux launcher; abstraction
  equivalence. Tests `[S6]`.
* **S7 Windows + WSL.** Windows launcher (WSL2 + nested virtualization for
  Microsandbox — at-risk), path normalization, networking. Tests `[S7]`.

Each slice must not break prior slices (roadmap §1).

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
* **Cross-compile** matrix (darwin/arm64, linux/amd64, linux/arm64,
  windows/amd64) once S6/S7 land. (darwin/amd64 is dropped — Intel Macs are
  unsupported, §6.2.)

---

# 7. Host Prerequisites (pre-flight)

`ai setup` fails fast (exit 3) with actionable output if missing:

* supported OS (S1: macOS on Apple Silicon)
* container runtime (S1: Docker) + rootless capability (service tier)
* Microsandbox runtime + host virtualization (Apple Hypervisor entitlement on macOS — the only elevated facility, §29.1)
* git; a free port for ClawPatrol's forward proxy (S1 egress model — no `utun`/NetworkExtension/admin networking; WireGuard is a later slice, §8.2)
* provider credentials are loaded into ClawPatrol via `ai secrets` (not a
  pre-flight hard-fail — `setup` warns if the configured routing has no
  credential; a model call fails only when its credential is actually absent)
* free ports + resolvable `AI_PLATFORM_HOST`
* sufficient disk/CPU/RAM for image builds and workspace microVMs

(See architecture §5–6, §29.)

---

# 8. Open Decisions / Risks

1. **Service provisioning — RESOLVED.** `ai setup` installs and manages
   all host services itself (LiteLLM, ClawPatrol, optional Ollama) as
   the single control plane, and verifies the Microsandbox workspace runtime;
   the user pre-installs only the container runtime and grants the OS privileges
   ClawPatrol needs. No docker compose: container-tier services run via the
   runtime abstraction, the native tier via the OS service manager, and
   workspace microVMs via Microsandbox (no daemon). See architecture §5.
2. **Egress model — S1 RESOLVED (forward proxy); WireGuard is the deferred
   target.** WireGuard L3 capture is the **long-term** egress model (arch §29
   end-state), but it is **not in S1**. The biggest correctness risk was assuming
   userspace WireGuard termination is feasible *with ClawPatrol* in S1, so S1
   sidesteps it entirely:
   * **S1 model (no WireGuard):** the workspace gets a virtio-net NIC via
     **gvproxy** (userspace), a **Microsandbox default-deny network policy**
     permits only the ClawPatrol port + trusted host service ports, and
     **ClawPatrol runs as an explicit forward proxy** (`HTTPS_PROXY`/`HTTP_PROXY`
     in the workspace env). All planes are userspace; the only elevated facility
     is the macOS hypervisor entitlement (arch §29.1, §30). Known limit:
     proxy-unaware tools can't reach the internet (fail closed) rather than being
     credential-injected — acceptable for S1, tested by AT §16.3.
   * **Deferred WireGuard slice (target):** swap the default route to a WireGuard
     tunnel to the ClawPatrol gateway for transparent L3 capture of *all* tools.
     **Gate (spike before that slice):** on a clean Apple Silicon Mac, prove
     (a) the `com.apple.security.hypervisor` entitlement works under Developer ID
     + notarization for a downloaded binary; (b) a guest→host **userspace**-WG
     tunnel terminates **with no `utun`/NetworkExtension/admin prompt/kext**; and
     critically (c) **whether `denoland/clawpatrol` can terminate WG in user
     space** — if not, front it with a **userspace-WG sidecar** (WG → local
     plaintext socket → gateway). If the spike fails outright, the S1 forward-proxy
     model remains the fallback. This keeps the S1 milestones free of the
     highest-uncertainty integration.
3. **Headroom placement (decided).** Headroom runs **per project inside the
   workspace** (installed in the workspace image) as a local proxy wrapping the
   agent CLI; it forwards to LiteLLM on the host via `AI_PLATFORM_HOST` (arch §10).
   It is not a host service. This compresses at the source and is symmetric with
   the in-workspace Caveman skill.
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
