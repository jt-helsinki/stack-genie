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
* **Thin launchers only** (bash/zsh/PowerShell) bootstrap the binary; no
  platform logic in shell.
* **Declarative config/templates** in YAML/JSON; never executable logic.
* External components are invoked via Go SDK or subprocess, never reimplemented:
  Microsandbox (Go SDK / `msb`), LiteLLM (host service over HTTP),
  git / docker / podman / gh (subprocess).

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
│   ├── secrets/                 # keys-in-LiteLLM credential broker (fronts the LiteLLM credential store; virtual-key minting)
│   ├── contextopt/              # per-project Headroom strategy (→ host Headroom proxy per-request knobs) + in-workspace Caveman skill
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
  KVM); no daemon to supervise. The adapter creates each
  workspace microVM **and applies its egress network policy via the Go SDK** —
  the default-deny + allow-rule model (deny by default; allow exactly the
  trusted host service ports + published ports), which is what AT §16.3 asserts.
  The per-project egress policy itself (mode `deny`/`public`/`unrestricted`,
  allowed host services, published ports — repo-layout §12.4) is configured by
  the `ai network` command; the live Microsandbox NetworkPolicy enforcement is a
  deferred hardware bring-up seam.
  (The Go SDK exposes the same network-policy core model as the other SDKs; pin
  the exact symbol against the SDK version at build time.)

## 3.5 External Adapters & Service Control Plane

The `ai` CLI is the **single control plane** for host services (architecture §5,
"Host Services Control Plane"). All host services run in the **container tier**
behind uniform `ai services` verbs — **no docker compose**:

* **container tier** (LiteLLM + its Postgres `aip-litellm-db`, the containerized
  Ollama `aip-ollama`, the Presidio PII-guardrail pair
  `aip-presidio-analyzer`/`aip-presidio-anonymizer`, and the host Headroom proxy
  `aip-headroom`): managed directly via the `runtime/` abstraction (run by
  digest, restart policy, health poll) on the private `aip-net` network, so
  docker and podman stay interchangeable

The Microsandbox runtime is **not** a managed service: its `msb` binary is
pinned into `tools/` and invoked on demand via `sandbox/` to create and drive
workspace microVMs; `ai setup` only verifies it is installed and the host
supports virtualization.

Each service's config is **rendered** from the platform config into
`config/<service>/`; real provider keys stay only in the LiteLLM gateway
(keys-in-LiteLLM). Versions pinned in `config/versions.json`.

| Adapter | Integration | Run mode | First slice |
|---|---|---|---|
| `sandbox/` | Microsandbox Go SDK / `msb`; names `aip-<project>[-<agent>]`; microVM lifecycle map (arch §7) | microVM runtime (no daemon) | S1 |
| `litellm/` | container via `runtime/`; config rendered from routing; `/health` poll | container | S1 |
| `secrets/` | keys-in-LiteLLM credential store; mint scoped virtual key for the workspace agent | container (LiteLLM) | S1 |
| `contextopt/` | per-project Headroom strategy (drives the host `aip-headroom` proxy per-request) + Caveman skill installed in the workspace | container (Headroom) / workspace (Caveman) | S2 |

---

# 4. Slice 1 — MVP Build Sequence

Slice 1 target (roadmap §2): macOS (Apple Silicon), Docker rootless service tier,
Microsandbox microVM workspaces, debian-trixie, single agent, LiteLLM,
keys-in-LiteLLM credentials (agent holds a scoped virtual key), zero manual config.
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
  init `~/.ai-platform/`, install/configure/start the container service tier
  (Ollama, Presidio pair, LiteLLM + its DB, Headroom) + verify the Microsandbox
  runtime; provider keys live in the LiteLLM gateway (keys-in-LiteLLM, §8.2);
  render the per-project default-deny network policy template; `ai services status`;
  **idempotent**. Tests: AT §2.1, §2.2, §2.3, §12.1 (macOS install).
* **M4 — Model layer + secrets.** LiteLLM config gen + routing default
  (the generated config also renders an **always-on Presidio PII guardrail** —
  pre_call input + post_call output, both `default_on: true`, so no request,
  cloud included, can bypass it); `ai secrets set/map`; `ai models status`,
  `ai models test` against the mock provider. Tests: AT §7.1, §7.2.
* **M5 — debian-trixie image + Microsandbox.** Seed `.ai-platform/Dockerfile` from the
  `debian-trixie` template, build the workspace OCI image from it; create/start
  the microVM (virtio-net + gvproxy, default-deny network policy **applied via the
  Go SDK**, §3.4); mounts/volumes;
  `ai workspace exec`; inject `AI_PLATFORM_HOST` so the agent reaches the host
  Headroom→LiteLLM gateway with its scoped virtual key (arch §17). Tests: AT §6.1,
  harness workspace-start threshold, AT §16.2, AT §16.3.
* **M6 — `ai project create` wizard + delete.** Interactive PTY wizard (CLI §3.1)
  with steps for name/OS/agent-CLIs/default-agent/**software-stacks**,
  each with a presented default, checkbox multi-select for CLIs + stacks,
  arrow/space navigation, Back + Abort; no `--os`/per-choice flags; no TTY → exit
  2. Then, in the current directory (no git — VCS is out of scope), write
  `.ai-platform/` (Dockerfile = OS template + selected stack snippets + selected
  CLIs / config incl. `agent.tools`+`default_tool` / `profile.yaml` incl.
  `stacks` / project.json / .gitignore) + index in `config/projects.json` +
  workspace + mint the agent's scoped LiteLLM virtual key; `ai project delete`.
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
* **Cross-compile** matrix (darwin/arm64, linux/amd64, linux/arm64). (darwin/amd64
  is dropped — Intel Macs are unsupported, §6.2.)

---

# 7. Host Prerequisites (pre-flight)

`ai setup` fails fast (exit 3) with actionable output if missing:

* supported OS (S1: macOS on Apple Silicon)
* container runtime (S1: Docker) + rootless capability (service tier)
* Microsandbox runtime + host virtualization (Apple Hypervisor entitlement on macOS — the only elevated facility, §29.1)
* git (egress is a userspace default-deny Microsandbox NetworkPolicy — no `utun`/NetworkExtension/admin networking, §8.2)
* provider credentials are loaded into the LiteLLM gateway via `ai secrets` (not a
  pre-flight hard-fail — `setup` warns if the configured routing has no
  credential; a model call fails only when its credential is actually absent)
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
2. **Egress model — RESOLVED (Microsandbox NetworkPolicy).** Workspace egress is
   a **default-deny Microsandbox NetworkPolicy** per project; there is **no egress
   proxy**:
   * the workspace gets a virtio-net NIC via **gvproxy** (userspace), and the
     **Microsandbox default-deny network policy** permits only the trusted host
     service ports + published ports declared via `ai network`. All planes are
     userspace; the only elevated facility is the macOS hypervisor entitlement
     (arch §29.1, §30). A non-allow-listed destination is denied — tested by
     AT §16.3.
   * applying the per-project `ai network` declarations live as the Microsandbox
     NetworkPolicy via the Go SDK is the deferred hardware bring-up seam (§3.4).
     **Gate (spike on hardware):** on a clean Apple Silicon Mac, prove (a) the
     `com.apple.security.hypervisor` entitlement works under Developer ID +
     notarization for a downloaded binary, and (b) the Go SDK's NetworkPolicy
     applies the default-deny + allow-rule model with no `utun`/NetworkExtension/
     admin prompt/kext.
3. **Headroom placement (decided).** Headroom runs as a **host service-tier
   container** (`aip-headroom`, pulled image `ghcr.io/chopratejas/headroom:slim`,
   :8787) — an input-compression proxy **in front of LiteLLM**, no longer baked
   into the workspace image. The per-project strategy (`ai context strategy`)
   maps to Headroom per-request knobs (`keep_turns`/`output_buffer_tokens` via
   `contextopt.HeadroomParams`). Caveman remains the symmetric in-workspace
   output-compression skill.
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
