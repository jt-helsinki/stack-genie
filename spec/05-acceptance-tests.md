# 05-acceptance-tests.md

# AI Development Platform

## Acceptance Test Specification

Version: 1.0

---

# 0. Purpose

This document defines the formal acceptance tests for the AI Development Platform.

A feature is only considered complete when it passes the corresponding acceptance tests.

These tests validate:

* installation correctness
* project lifecycle
* agent system behavior
* workspace sandboxing (nono) + session lifecycle
* model routing
* keys-in-LiteLLM integration
* context optimization
* cross-platform support

---

# 1. Global Test Principles

## 1.1 Deterministic Results

All tests must produce deterministic outcomes given the same input state.

---

## 1.2 No Manual Intervention

All tests must run:

* without manual file edits
* without external debugging steps
* without hidden configuration

---

## 1.3 State Isolation

Each test must run in a clean environment:

* fresh state directory
* no cached projects
* no pre-existing workspaces

---

## 1.4 Project Creation (non-interactive)

`ai create` has a flag for every choice (`--name`/`--agents`/`--location`/
`--graphify-model`/`--shell`/`--auth-mode`/`--tools`/`--dry-run`). On a TTY
those flags **pre-seed** an interactive wizard, but under `--json` (or no TTY)
the wizard is **disabled** and the spec is built straight from the flags (the
programmatic contract — every input has a flag, no prompts; CLI §1.3/§3.1). The
harness always runs with `--json`, so it drives creation with explicit flags
rather than a pseudo-terminal:

```text
harness.CreateProject(name)                   →  ai create <name> --json
harness.CreateProjectWithAgents(name, agents) →  ai create <name> --agents <a,b> --json
```

* `ai create` is **scaffold-only** — it writes the project's tracked
  `.ai-platform/` state (`config.yaml`, `project.yaml`, `.gitignore`) and
  registers the project in the global index; it does **not** build or launch
  anything. There is no OS choice, no Dockerfile, and no image any more — a
  workspace is just a registered host project directory plus a generated nono
  profile (written at `ai start`/`ai agent`/`ai shell` time, not here). Those
  commands launch (or reattach to) a sandboxed nono session directly on the
  host — there is nothing to boot.
* the default agent CLI is `opencode` (used when `--agents` is omitted)
* **no flag is required** on this non-interactive path: the workspace name
  defaults to the `[name]` positional/`--location`'s directory basename, and
  `--agents` defaults to `[opencode]`. A value that is not a known option (an
  unknown `--agents`/`--tools`/`--shell`/`--auth-mode` choice) exits `2`, as
  does an invalid name/location
* the result is the `--json` envelope on stdout (envelope `command: project.create`)

A `--json`/no-TTY `create` exits `2` **only** when a provided value is invalid
(there is no required input to be missing any more) — not merely because there
is no TTY (§3.1).

---

## 1.5 Slice Tagging

Each test is tagged with the slice from which it must pass (see
`02-implementation-roadmap.md`). A test does not apply before its slice.

```text
[S1] Slice 1   [S2] Slice 2   [S3] Slice 3 (RETIRED — see §4)
[S4] Slice 4   [S5] Slice 5   [S6] Slice 6
```

The `[S3]` tag is retired (agent-CLI behavior is the CLI's own concern, not a
platform-level test — see "Agent System Tests — Removed", §4); no test in this
document carries it.

---

## 1.6 Test Harness

All tests run unattended against a clean, ephemeral environment. Every command —
including `ai create` — is non-interactive: the harness always appends `--json`,
which disables the interactive wizard and drives creation with explicit flags
(§1.4). No PTY is used to drive `ai create`. (The `creack/pty` harness exists only
for the rare TTY-only human-wizard cases and is not used here.)

### Fixtures

* **HOME**: a throwaway `$AIP_TEST_HOME`; `~/.ai-platform` resolves under it.
  Removed on teardown.
* **sample dir**: a throwaway working directory (`t.TempDir()`) used as the
  location for `ai create` (the project is created in place). There is **no**
  `fixtures/` tree on disk — the harness builds everything it needs inline.
* **large repo / performance**: the >1M-LOC perf thresholds (§8.2/§15.1) are `[S2]`
  and **not yet implemented**; there is no large-repo fixture today.
* **mock provider**: an **in-process** HTTPS server (`httptest`, `mockprovider_test.go`)
  emulating an OpenAI-compatible endpoint with deterministic responses that records
  the provider credential on each inbound request (to assert LiteLLM attached the real
  key). It serves TLS with a test cert the harness trusts (arch §17); the provider
  config is written **inline** (`writeProviderConfig`/`writeCertPEM`), not from a file
  fixture. LiteLLM holds the real provider key (keys-in-LiteLLM) and attaches it on the
  upstream call. Today its only consumer is §7.2's `TestModelTestOnHardware` (a
  `models test` round-trip through the gateway); the session-level credential-injection
  test this fixture was originally built for (§9.1) has no automated coverage any more
  — see §9.1 for why.
* **egress policy**: a project's egress mode is now one of exactly two values,
  `deny` or `public` (the old third mode `unrestricted` is gone), defaulting to
  `public` (allow-outbound — private ranges still blocked), set/read with
  `ai network egress|show` and compiled straight into the project's generated
  **nono profile** (`internal/nono.NetworkCapabilities` — `Block`/
  `AllowDomain`/`DenyDomain`, bare domains, no ports, no "gateway" special
  case) at the next `ai agent`/`ai shell`/`ai start` — there is no restart step
  and no Microsandbox NetworkPolicy/`msb` net-rules any more (nono sandboxes a
  host process directly; enforcement is nono's own Landlock/Seatbelt network
  filtering, not a VM boundary). There is no `TestEgressConfinementOnHardware`
  any more (removed along with the rest of `s1_hardware_test.go` in the nono
  migration, not replaced) — see §16.3.
* **`$AIP_TEST_SENTINEL`** (used in §9.1/§16.1's test blocks): a documentation
  placeholder only — no current fixture defines it and no current test
  instantiates it (the `ai keys add`/OS-keychain flow those sections describe
  is not automated). It is kept in those sections' example scripts to show
  what a future PTY-driving-harness test would assert, not as a claim that it
  exists today.

### Setup / Teardown

`HOME` is repointed at the throwaway dir so `~/.ai-platform` and `~/projects`
resolve under it. The mock provider is brought up and wired into LiteLLM's
model list before §7.2's model-test runs so the round-trip succeeds. (Provider
credentials are **not** a `ai setup` hard-fail — plan §7; a missing credential is
warned by `ai doctor` and only fails the actual model call.)

```text
setup:
  export AIP_TEST_HOME=$(mktemp -d)
  export HOME="$AIP_TEST_HOME"                 # ~/.ai-platform, ~/projects resolve here
  export MOCK_PROVIDER_URL=$(start in-process httptest mock)   # in-process HTTPS OpenAI-compatible endpoint (mockprovider_test.go), TLS cert written to disk
  ai setup --json --provider-config <inline model_list pointing gpt-5 at the mock>   # config written inline by the harness (writeProviderConfig), not a checked-in fixture; api_key is a virtual-key placeholder, not a real sentinel

teardown:
  ai delete <each> --purge --yes                # best effort
  (the in-process mock stops with the test process)
  rm -rf "$AIP_TEST_HOME"
```

The mock provider's inline `model_list` points LiteLLM's `gpt-5` alias at the local
endpoint, so `ai models test gpt-5` exercises the gateway round-trip end-to-end without a
real provider. There is no sentinel-credential/`ai keys add` step in this flow today —
see §9.1 for the separate (currently uncovered) credential-injection scenario this
fixture was originally built to support.

### Assertions

* **every command MUST be run with `--json`** (even where a test below shows the
  bare command for brevity); assertions are made against the §19 envelope
  (`ok`, `data`, `error.code`), never by scraping human text
* exit code is asserted against §18
* filesystem/state assertions check concrete paths from
  `03-repository-layout.md`
* where a secret-leak assertion is described (§9, §16), it is an exact-match
  `grep` against a known sentinel value, not a fuzzy "looks like a secret"
  check — but note that no test in `test/acceptance/` or `test/integration/`
  currently performs one (see §9.1/§9.2/§16.1/§16.2)

### Non-Interactive Confirmation

Destructive commands (`ai delete`, and its alias `ai destroy`) are confirmed
non-interactively with `--yes` (CLI §20). `ai destroy` is a **cobra alias** of
`ai delete` — the same destructive command (§4.4), so it gates identically
(pausing a workspace's sessions with `ai stop` is non-destructive and needs no
confirmation). There is no AI-approval flow.

### Thresholds

| Metric | Threshold |
|---|---|
| `ai models test` round-trip (mock provider) | < 2000 ms |
| Headroom input reduction on large-repo context | ≥ 50% tokens under the project's strategy |
| Caveman output reduction (full level) | ≥ 40% output tokens |
| `ai create` scaffold (no image, no boot) | < 1 s |

---

# 2. Installation Tests

## 2.1 Platform Bootstrap `[S1]`

### Test

```bash id="t2"
ai setup --json
```

### Expected Result

* CLI installed (`ai` on PATH)
* `~/.ai-platform/config/` created (incl. `projects.yaml` index)
* **Docker detected, rootless** (Podman is `[S6]`; Slice 1 service tier is Docker-only)
* **nono runtime verified** (the `nono` CLI is installed — no Apple-Silicon-only
  restriction and no separate "host virtualization" check any more: nono
  sandboxes a host process directly via Landlock (Linux) or Seatbelt (macOS),
  not a hypervisor, so it runs on any supported host the binary runs on)
* the service tier started as containers, reconciled in order (network → DNS →
  Presidio → Valkey(+RedisInsight) → Headroom → LiteLLM(+DB) → nginx
  proxy): `aip-dns`, the
  `aip-presidio-analyzer`/`aip-presidio-anonymizer` pair, `aip-valkey`
  (+ `aip-redisinsight`), `aip-headroom`,
  `aip-litellm` (+ `aip-litellm-db` Postgres), and `aip-proxy` (the nginx gateway)
  — all on `aip-net` (Headroom PRECEDES LiteLLM because LiteLLM calls it in-process
  as a `pre_call` compression guardrail)
* the **inference tier is host-native omlx** (macOS/Apple-Silicon only), not
  containerized: ONE shared `omlx serve --model-dir <dir> --port 8100` host
  process serves every locally-served model (there is **no `aip-*` inference
  container** and no per-model process/port). `ai setup` best-effort installs
  omlx into the platform venv, starts the shared server, and automatically
  syncs LiteLLM's `omlx/<id>` registrations against the server's own live
  `GET /v1/models` (the LiteLLM container reaches the single endpoint at
  `host.docker.internal:8100`); model download/add/remove/tuning happens
  entirely in omlx's own admin panel (`ai services console omlx`), not through
  any `ai models` subcommand
* in **standalone** (default) the shared services bind **127.0.0.1**; the nginx
  gateway (`aip-proxy`) is the SOLE host entry on `:18787`, with `aip-headroom`
  now INTERNAL-ONLY (reached only by LiteLLM by name, no host publish); the host
  tier has **no optional services** (Open WebUI and Odysseus have both been
  removed from the platform entirely — there is no in-VM-app mechanism any
  more, per-workspace or otherwise)
* command exits `0`

**Automated coverage:** `TestSetupBringsServiceTierUpOnHardware`
(`test/acceptance/s1_remaining_test.go`) drives this §2.1 flow (and the §2.2
idempotency case below) on a provisioned host, asserting `ai setup` exits `0`
and at least one service reports `running` in the envelope, then confirming via
`ai services status`. It does not assert the specific container list, reconcile
order, or bind address above — those are architectural facts, not per-test
assertions. `TestSetupSucceedsOnHardware` (`test/acceptance/acceptance_test.go`)
is a lighter smoke variant of the same flow.

---

## 2.2 Idempotency Test `[S1]`

### Test

```bash id="t3"
ai setup
ai setup
```

### Expected Result

* second run does not duplicate resources
* no errors
* system state unchanged

**Automated coverage:** see `TestSetupBringsServiceTierUpOnHardware` above —
its second `ai setup` run is exactly this idempotency check.

---

## 2.3 Upgrade Test `[S1]`

### Test

```bash id="t4"
ai setup --upgrade
```

### Expected Result

* existing state preserved
* no project loss
* projects index intact

**Automated coverage:** `TestSetupUpgradePreservesStateOnHardware`
(`test/acceptance/s1_remaining_test.go`) creates a project, runs
`ai setup --upgrade`, and confirms the project still appears in `ai list`.

---

# 3. Project Lifecycle Tests

## 3.1 Project Creation `[S1]`

### Test

```bash id="t5"
ai create test-project --json   # flag-driven, non-interactive (§1.4); no OS/stacks to choose any more
```

### Expected Result

`ai create` is scaffold-only — it writes state and indexes the project; it does
**not** build or launch anything (launching a session is `ai start`/`ai agent`/
`ai shell`, and there is nothing to boot).

* the envelope is `command: project.create`, `ok == true`, exit `0`
* `data.name == test-project`
* `data.tools` is `[opencode]` (the default agent CLI; `--agents` was omitted)
* project scaffolded in the current directory; `data.root` points at it, and the
  project is indexed in `config/projects.yaml`
* tracked state written under `<root>/.ai-platform/`: `config.yaml`,
  `project.yaml`, `.gitignore` — there is no `Dockerfile` and no `profile.yaml`
  any more (no OS, no image). A per-CLI nono profile (e.g.
  `.ai-platform/nono-profile-opencode.json`) is generated later, at
  `ai start`/`ai agent`/`ai shell` time — not by `create`

### Negative case

* `ai create demo --agents not-a-real-cli --json` exits `2` — under `--json`
  the wizard is disabled, so an unknown `--agents` value is rejected directly
  rather than falling back to a prompt (§1.4/§3.1). The same holds for an
  unknown `--tools`/`--shell`/`--auth-mode` value. There is no `--os` flag any
  more, so there is no "missing required input" case to test — every input is
  optional and defaults sensibly

**Automated coverage:** the negative case is `TestProjectCreateNoTTYExits2`
(despite its name, it asserts the invalid-value behavior above, not TTY
absence); the positive scaffold-only flow is `TestProjectLifecycle`
(create/list/delete) — both in `test/acceptance/acceptance_test.go`. The
default-tools case (`--agents` omitted → `[opencode]`) is exercised implicitly
by `TestProjectLifecycle`'s use of the `CreateProject` harness helper
(`test/acceptance/pty_test.go`); there is no dedicated test asserting it in
isolation any more (the old `TestAgentCLIDefaultSelection` is gone).

---

## 3.2 Create In Existing Directory `[S1]`

### Test

```bash id="t6"
# A directory with pre-existing files (ai create runs no git and leaves them untouched).
mkdir -p "$AIP_TEST_HOME/work/app" && echo hi > "$AIP_TEST_HOME/work/app/README.md"
cd "$AIP_TEST_HOME/work/app" && ai create app --json
```

### Expected Result

* the project is created in the current directory (`root` == that directory)
* pre-existing files are left untouched (`README.md` still present)
* no `.git` is created by the platform
* re-running `ai create` in the same directory **attaches** to the existing
  workspace instead of erroring

**Unverified:** the attach behavior is implemented (`internal/cli/project.go`,
`attachWorkspace`, gated on the `--location`-omitted + already-a-workspace
case), but no test in `test/acceptance/` or `test/integration/` re-runs
`ai create` in an existing workspace directory to exercise it end-to-end — this
scenario is not covered by an automated test today.

---

## 3.3 Workspace Stop/Start Recovery `[S1]`

There is no microVM and no overlay any more: a workspace is a registered host
project directory, and "stopping"/"starting" it means stopping and relaunching
its **nono sessions**, not pausing/resuming a VM. `ai start`/`ai restart` are
also no longer scriptable the way `ai stop` is — see below.

### Test

```bash id="t7"
ai stop test-project --json
ai start test-project          # NOTE: cannot be driven with --json (see below)
```

### Expected Result

* `ai stop test-project --json` stops **every live nono session** for the
  project, in-process (no subprocess, no VM shutdown) — `Manager.StopAll`. No
  `--yes` needed — stop is non-destructive (CLI §20); pausing a workspace is
  `ai stop`, not `ai delete`/`ai destroy` (which are the same destructive
  removal). The project directory, config, and files are untouched regardless
  — there is nothing VM-side to "keep" any more
* `ai start test-project` launches (or reattaches to) the workspace's default
  agent CLI session — but it is **interactive-only**: under `--json` or any
  non-TTY invocation it exits `2` ("ai start is interactive and needs a
  terminal"), because there is no detached VM to boot any more — the command's
  entire job is to suspend into a real terminal session. The same holds for
  `ai restart`. This is a **behavior change** from the old microVM model, where
  both verbs ran fully detached and were `--json`-scriptable

**Automated coverage:** `ai stop`'s non-interactive path is covered at the unit
level by `TestStopAllStopsOnlyRunningSessions` (`internal/workspace/workspace_test.go`,
against a fake nono runner — stops only the running sessions, skips already-exited
ones). There is **no** test anywhere (`test/acceptance/`, `test/integration/`) that
drives a full `ai stop` + `ai start` CLI round trip end-to-end — and, unlike before,
this is not merely an oversight: `ai start`/`ai restart` cannot be driven through the
JSON-envelope harness at all now (they require a terminal), so proving this
end-to-end needs a PTY-driving harness for nono sessions, which this suite does
not yet have (`test/integration/workspace_test.go`'s own doc comment calls this
out explicitly as a hardware-bring-up gap). The old `TestOverlayPersistsAcrossRecreation`
and `TestRestartStopsThenStartsExistingMicroVM` tests this section used to cite are
both gone (the overlay and the microVM `Restart` they exercised no longer exist).

---

## 3.4 Project Delete `[S1]`

### Test

```bash id="t7b"
ai create del-test --json

# (a) KNOWN project, missing confirmation → exit 2 (isolates the missing-`--yes` cause)
ai delete del-test --json

# (b) UNKNOWN project, WITH --yes → exit 2 (isolates the unknown-project cause)
ai delete no-such-project --yes --json

# (c) successful default delete (keeps host source)
ai delete del-test --yes --json
```

### Expected Result

The two exit-`2` cases are deliberately separated so the test never conflates an
unknown project with a missing confirmation:

* **(a)** exits `2` **because confirmation is missing** — the project provably
  exists, and `--yes` is the only thing absent; nothing is removed and `del-test`
  still appears in `config/projects.yaml`
* **(b)** exits `2` **because the project is unknown** — `--yes` is present, so a
  missing confirmation cannot be the cause; nothing is removed
* **(c)** default delete: any live nono sessions for the project are
  best-effort stopped, its scoped gateway credential is best-effort revoked
  from the OS keychain (`Manager.StopAll`/`Manager.DeleteCredentials`), and the
  project's **whole `<project>/.ai-platform/` directory** removed
  (`config.yaml` / `project.yaml` / `.gitignore` / any generated
  `nono-profile-*.json` / `run/` all gone — there is no Dockerfile or
  profile.yaml any more); `config/projects.yaml` no longer lists `del-test`;
  the user's **OTHER** files in the directory are **kept**
* `--purge` (e.g. `ai delete <p> --purge --yes` on a separately created
  disposable project) additionally removes the project directory entirely

**Automated coverage:** cases (a) and (c) are `TestProjectLifecycle`
(`test/acceptance/acceptance_test.go`). Case (b) — unknown project + `--yes` →
exit 2 — is not exercised by `test/acceptance/`; it is verified at the CLI-unit
level by `TestNewDeleteCmdUnknownProject`
(`internal/cli/project_helpers_test.go`) and black-box by
`TestWorkspaceCreateTeardown/delete_unknown_errors`
(`test/integration/workspace_lifecycle_test.go` — unlike the old Microsandbox
model, `ai create`/`ai delete` have NO external dependency at all here, not
even installed templates (`internal/templates` is gone), so these run
unconditionally fast and everywhere, not just on a provisioned host).
`--purge` removing the whole project directory is also covered by
`TestWorkspaceCreateTeardown/delete_purge_removes_project_dir` and the unit
test `TestDeletePurgeRemovesSource` (`internal/project/project_test.go`).

---

# 4. Agent System Tests — Removed

There is no platform multi-agent ORCHESTRATION abstraction — no platform-managed
agent identities, branches, or worktrees (architecture §20, CLI §5) — so there are
no platform-level orchestration tests. (The flat `ai agent` / `ai attach` /
`ai sessions` commands DO exist and are exercised with the other workspace-session
commands, CLI §4.5b; they are session launchers, not orchestration.) The `[S3]`
tag is retired.

---

# 5. Git Workflow Tests — Removed

The platform runs no git **workflow** operations (architecture §21) — the only git it
runs is an internal `git init` at workspace start to seat the Graphify hook — so there
are no platform-level git-workflow tests. Clone, branching, merging, and rebasing are
the user's and the in-workspace agent's job.

---

# 6. Environment (Dockerfile) Tests — Removed

The nono migration deleted the OS-image/Dockerfile concept entirely
(`internal/envimage` and `internal/templates` are both gone; there is no OS
choice, no software-stack choice, and no per-project `Dockerfile` or
`profile.yaml` any more). A workspace now runs directly on whatever OS/toolchain
the HOST actually has — "the environment" is just the host, not a
platform-built artifact — so there is no snapshot/versioning/rollback concept
to test and no environment-reproducibility guarantee for this section to make.

The four tests this section used to define are all **removed, not replaced**:

* **6.1 Project Dockerfile Created** — there is no Dockerfile. The former
  automated coverage, `TestProjectDockerfileReflectsOS`
  (`test/acceptance/s1_remaining_test.go`), no longer exists.
* **6.2 Environment Change Via Dockerfile** — there is no Dockerfile to edit
  and no image to rebuild; `ai start` no longer builds or recreates anything.
* **6.3 Agent CLI Selection** — the *selection* itself is unchanged and still
  tested (`config.yaml` records the chosen `--agents`, covered by
  `TestWorkspaceCreateTeardown/flags_persist_to_config` in
  `test/integration/workspace_lifecycle_test.go` and by `TestProjectLifecycle`/
  `TestProjectCreateNoTTYExits2` in `test/acceptance/acceptance_test.go`), but
  the in-workspace `command -v opencode`/`command -v codex` probe this test
  specified has no replacement: `ai exec` — the command it used to reach inside
  the workspace — is **deleted entirely** (CLI §4). The only way to run a
  command inside a sandboxed session now is `ai agent <cli>`/`ai shell`, which
  are interactive-only (no envelope) and need a PTY-driving harness this suite
  does not yet have.
* **6.4 Software Stack Selection** — the `--stacks` flag and the whole software-
  stacks concept are gone; whatever toolchains exist on the host are what a
  session sees. There is nothing left to select or probe.

Agent-CLI and AI-tool *selection* round-tripping to `config.yaml` remains
covered — see §3.1 and `test/integration/workspace_lifecycle_test.go`'s
`flags_persist_to_config` subtest — but probing what a live session can
actually execute is now out of this suite's reach without a PTY harness.

---

# 7. Model Layer Tests

## 7.1 LiteLLM Health `[S1]`

### Test

```bash id="t15"
ai models status --json
```

### Expected Result

* LiteLLM reachable
* providers listed
* routing active

**Automated coverage:** `TestModelStatusOnHardware`
(`test/acceptance/s1_remaining_test.go`), hardware-gated. It asserts gateway
health and a non-empty provider list; it deliberately does **not** assert a
non-empty `default` — the model set is catalog-driven/DB-backed and
`litellm.DefaultRouting()` returns the zero `Routing`, so `data.default` is
intentionally always empty.

---

## 7.2 Model Invocation Test `[S1]`

### Test

```bash id="t16"
ai models test gpt-5 --json
```

### Expected Result

* response received
* latency within acceptable bounds

**Automated coverage:** `TestModelTestOnHardware`
(`test/acceptance/s1_remaining_test.go`), hardware-gated — it points the
gateway at the TLS mock-provider fixture (§1.6) and probes the `gpt-5` alias,
so it exercises this without a real provider key.

---

## 7.3 Local Inference Runtime (omlx, live integration)

Covered by the **live integration suite**, not the `[Sx]` acceptance harness:
group 7 of `make test-integration` (`//go:build integration`,
`TestGroup07InferenceRuntime`; self-skips without a running stack). The `[Sx]`
tags in this document apply to the `test/acceptance` harness; the integration
groups are separate.

The group-7 source file is `test/integration/omlx_runtime_test.go`
(`TestGroup07InferenceRuntime`), rewritten for the vLLM→omlx refactor: it
asserts `ai services status`/`ai doctor` list the host-native `omlx` service
(and fail if a removed `vllm`/`ollama` name is still surfaced), and exercises
`ai models refresh` on both the omlx-unavailable negative path (must fail)
and the omlx-running positive sync path.

### Test

```bash
ai services status --json                              # lists the host-native omlx backend
ai doctor --json                                       # same host-native omlx backend
ai models refresh --json                                # re-sync LiteLLM's omlx/* registrations from omlx's live model list
ai models test omlx/<id> --json                         # positive routing path, <id> exactly as omlx's admin panel reports it
```

### Expected Result

* `ai services status` and `ai doctor` both surface the host-native `omlx`
  backend as a single service (no `aip-*` inference container, no per-model
  entries — omlx runs one shared server for every model it serves)
* `ai models refresh` re-syncs LiteLLM's `omlx/<id>` registrations against the
  server's live `GET /v1/models` and never errors when nothing changed
* the positive `omlx/<id>` routing round-trip **self-skips** when the omlx
  server is not running on the host (matching the suite's `hardware bring-up`
  self-skip convention)
* there is no `ai models pull`/`configure`/`enable`/`disable`/`rm` — local
  model management (download, remove, tune) happens entirely in omlx's own
  admin panel (`ai services console omlx`)

---

# 8. Context Optimization Tests

## 8.1 Caveman Output Compression `[S2]`

### Test

```bash id="t17"
ai context caveman test-project full --json
```

### Expected Result

* the Caveman skill is present at `<project>/.ai-platform/skills/caveman/`
  (installed at workspace start by its own upstream installer, not seeded at create)
* Caveman level applied
* agent output tokens reduced
* technical accuracy preserved (code, URLs, facts unchanged)

**Automated coverage:** `TestContextOptimizationFlow`
(`test/acceptance/acceptance_test.go`) covers only the platform-owned plumbing
— the default level is seeded (`full`), `ai context caveman`/`ai context
strategy` round-trip through the project config, and the skill is correctly
reported as not-yet-installed right after `create` (it is installed later, at
workspace start, by Caveman's own upstream installer). The actual output-token
reduction and technical-accuracy bullets above are Caveman's own behavior (an
external, upstream-owned toolkit) and are not measured by any platform test.

---

## 8.2 Headroom Compression `[S2]`

### Test

* with `fixtures/large-repo` open, issue a model call with a large multi-turn
  context, under the project's Headroom strategy (CLI `ai context strategy`)

### Expected Result

* Headroom reduces input tokens by ≥ 50% (harness threshold, §1.6) — the
  strategy maps to Headroom's per-request `keep_turns`/`output_buffer_tokens`
  knobs (there is no `context.max_tokens` field; arch §10)
* `ai context status --json` reports the active strategy + reduction
* command exits `0`

---

## 8.3 OpenViking Memory/Context Database `[S2]`

### Test

```bash
ai create ov-test --tools caveman,graphify,code-review-graph,openviking --json
```

### Expected Result

* OpenViking (https://docs.openviking.ai — "the context database for AI
  agents") is one of the eight `--tools`/AI-tools-wizard selections
  (`SupportedAITools()`: caveman, graphify, code-review-graph,
  codebase-memory-mcp, openviking, tokensave, agentmemory, ponytail); like
  every other AI tool it is OPT-IN — `DefaultAITools()` returns an empty set
  (no tool is pre-selected), so it is installed only when explicitly named
* `<project>/.ai-platform/config.yaml` records `context.openviking_enabled:
  true`. There is no project Dockerfile any more (`internal/templates` and
  `internal/envimage` were both deleted in the nono migration), so the old
  "conditional Dockerfile snippet" install mechanism no longer applies — the
  platform's opt-in AI tools are now meant to install directly on the HOST via
  plain `os/exec` at the next `ai agent`/`ai shell` (CLI §4), not at any build
  step
* **Uncertain:** the exact current registration mechanism (starting the
  OpenViking server, where its index persists, and its per-CLI plugin
  registration) could not be confirmed against `internal/workspace` — the
  `registerOpenViking` function these bullets used to name does not exist
  there, nor does `registerCaveman`/`registerGraphify`/any other per-tool
  registration function; only the config-flag plumbing
  (`internal/create.SplitAITools`) is wired today. The old claim that its
  index persists under `/persist/openviking` is definitely stale
  (`internal/overlay`, the VM-disk persistence layer it referenced, is
  deleted), but what — if anything — replaces it is not visible in the current
  source, so treat this as **not yet wired up under the nono model** rather
  than a confirmed behavior
* the install-detached/once-guarded semantics described for the old model are
  likewise unconfirmed under the new one

**Automated coverage:** `TestSplitAITools`/`TestDefaultAITools`
(`internal/create/options_test.go`) cover the `--tools` selection plumbing (no
AI tool, OpenViking included, is selected by default) — this part is
unaffected by the nono migration. No platform acceptance test exercises any
server start / plugin-registration behavior — like Caveman (§8.1), it is an
external, upstream-owned toolkit whose runtime behavior is not measured here,
and (per the note above) its current wiring under the host-process model is
itself unconfirmed.

---

# 9. Secrets Management Tests

## 9.1 Credentialed Outbound Request `[S1]`

Validates the keys-in-LiteLLM model (architecture §17): the agent never holds the
real provider secret, yet the upstream request is delivered with the real key
that LiteLLM holds in its own store. **The credential-delivery mechanism changed**
with the nono migration: there is no in-VM env file any more. A project's scoped
LiteLLM virtual key is minted per agent-CLI launch, stored in the **OS keychain**
(macOS Keychain via `security`, Linux via `secret-tool` — `internal/nono`'s
`CredentialStore`) under a name like `aip-gateway-<project>`, and referenced — not
copied as plaintext — into the launched nono session via `nono run
--env-credential-map <keychain-ref> <ENV_VAR>` (e.g. `AIP_GATEWAY_KEY` for
opencode/omp/hermes/codex, `ANTHROPIC_AUTH_TOKEN` for claude-code, `GEMINI_API_KEY`
for gemini). The real upstream provider key (the sentinel, in this fixture) still
lives only in LiteLLM's own credential store and is attached by LiteLLM on the
upstream call — that part of the architecture (keys-in-LiteLLM) is unchanged.

### Test

```bash id="t18"
ai create test-project --json
ai agent opencode test-project          # NOTE: interactive-only, cannot be driven with --json
# inside the launched session, an agent request would route through the gateway
# to the mock provider, which should record the real provider key on the wire.
```

### Expected Result

* the scoped LiteLLM virtual key is minted and stored only via the OS keychain
  (`CredentialStore.StoreCredential`), referenced by name in the nono run's
  `EnvCredentialMap` — never written as plaintext to the session's environment
  by platform code, and never written to project/config files on disk
* the **mock provider's recorded request contains `$AIP_TEST_SENTINEL`**
  (proves LiteLLM attached the real provider key — held in its own store — on
  the upstream call), over HTTPS (§1.6)
* no credential file (`.env` or otherwise) exists anywhere under the project

**Automated coverage: NONE at the hardware/end-to-end level.** The hardware-gated
test this scenario used to cite, `TestCredentialedRequestOnHardware`
(`test/acceptance/s1_hardware_test.go`), was removed during the nono migration and
**not replaced** — proving a live nono session's outbound request carries the real
provider key needs a PTY-driving harness for nono sessions that this suite does
not have yet (`ai agent`/`ai shell` are interactive-only — no JSON envelope — and
`test/integration/workspace_test.go`'s own doc comment flags exactly this gap as
`hardware bring-up`). The closest automated coverage is the **mocked unit test**
`TestAgentAPIKeyModeMintsAndStoresKey` (`internal/workspace/workspace_test.go`),
which proves the scoped key is minted, stored ONLY via the injected
`CredentialStore`, and referenced by name (never plaintext) in the nono run's
`EnvCredentialMap` — but it fakes both nono and LiteLLM, so it does not prove the
real upstream request carries the real provider key end-to-end.

---

## 9.2 Secret Isolation `[S1]`

The old mechanism for this test drove `ai exec <project> -- env`/`grep` inside the
workspace to prove the sentinel never appeared there. **`ai exec` is deleted
entirely** (CLI §4) — there is no non-interactive verb left that can run an
arbitrary probe command inside a sandboxed session at all. `ai agent`/`ai
shell`/`ai attach` are interactive-only (no envelope, real terminal), so this
test's shape — capture a command's stdout from inside the workspace via `--json`
and grep it on the host — **cannot be reproduced** against the current CLI
surface without a PTY-driving harness (see §9.1).

### Test

**Removed** — no non-interactive mechanism exists to reach inside a sandboxed
session any more. The intent (the real provider secret never enters the
workspace's process environment or filesystem) still holds by construction — per
§9.1, the platform-side code path never places the real key anywhere accessible
to the session; only a keychain *reference* is passed via
`--env-credential-map`, and nono (not platform code) resolves it into the
process's actual environment at launch. But this intent is not independently
verified end-to-end by any test today.

### Expected Result (intent, unverified)

* the workspace process's environment should contain only the scoped LiteLLM
  virtual key (or, for oauth-mode agents, no key at all — §9.1), never the real
  provider secret
* the real provider key should never be reachable from the workspace — it lives
  only in the LiteLLM gateway (keys-in-LiteLLM, arch §17); non-allow-listed
  egress from a session is a matter of the project's `ai network` egress policy,
  compiled into its nono profile (§16.3), not a VM boundary

**Automated coverage:** none at the acceptance/integration level (mechanism
removed, see above). Related unit-level coverage: `TestAgentAPIKeyModeMintsAndStoresKey`
(credential stored only via `CredentialStore`, referenced by name in
`EnvCredentialMap` — never placed in platform-written config/env files) and
`TestAgentOAuthModeSkipsKeyMinting` (an oauth-mode agent gets no key minted, no
credential stored, and no `EnvCredentialMap` entry at all), both in
`internal/workspace/workspace_test.go`.

---

# 10. MCP — Not a Platform Concern

The platform does not manage MCP, so there are no MCP acceptance tests. MCP
servers are configured and run by the in-workspace agent (architecture §12), and
any credentials those servers need are the agent's own concern — the platform
manages only the model-path provider keys (in the LiteLLM gateway, §9) and
confines all other workspace egress via the project's `ai network` policy,
compiled into its generated nono profile (§16.3) — there is no Microsandbox
NetworkPolicy any more.

---

# 11. Runtime Tests (Container + Workspace Sandbox)

## 11.1 Rootless Service Tier + Workspace Sandbox Test `[S1]`

### Test

```bash
ai doctor test-project --json
```

### Expected Result

`ai doctor` is consolidated: given a project name it adds a WORKSPACE-runtime
section to the report. It always runs to completion and exits `0` (per-check
status conveys health, §14.1) — a runtime shortfall is folded into the checks,
not a non-zero exit. Unlike the Microsandbox microVM runtime this replaced,
there is no container-runtime/rootless/virtualization posture to probe for a
workspace — nono sandboxes a host process directly (Landlock on Linux, Seatbelt
on macOS), so the section is just: is `nono` installed, and how many live nono
sessions does the project have.

* the envelope is `command: doctor`, `ok == true`, exit `0`
* on a provisioned host the workspace posture checks pass: the `workspace
  runtime` check reports status `ok` (nono is installed), and `workspace
  sessions` reports `ok`/`warn` depending on whether any session is currently
  running (never an error unless listing sessions itself fails)
* off-hardware (`nono` not installed) `workspace runtime` reports a non-`ok`
  status — the command still exits `0`

**Automated coverage:** `TestWorkspaceDoctorOnHardware`
(`test/acceptance/acceptance_test.go`), hardware-gated, asserts `workspace
runtime == ok`. `TestGroup04WorkspaceLifecycle`'s "doctor reports nono runtime
ok" subtest (`test/integration/workspace_test.go`) covers the same assertion
against the live integration stack. Off-hardware, `TestDoctorRunsAndReports`
(`test/acceptance/acceptance_test.go`) confirms `ai doctor` still exits `0`
with a non-empty report (it does not assert the workspace-runtime checks'
non-`ok` status specifically).

---

## 11.2 Runtime Abstraction Test `[S6]`

### Test

* run the identical project-creation + agent-session-launch flow once under
  Docker and once under Podman (force via `config/runtime.yaml`) — this
  exercises the service-tier container runtime only; workspace sandboxing
  (nono) is independent of which container runtime backs the service tier

### Expected Result

* identical `--json` `data` for both runtimes (modulo `runtime` field and ids)
* no workflow changes required

**Unverified:** no test in `test/acceptance/` or `test/integration/` runs this
Docker-vs-Podman comparison; Podman parity is not exercised by an automated
test today.

---

# 12. Cross-Platform Tests

## 12.1 macOS Test `[S1]`

### Test

```bash id="t20"
ai setup --json
```

### Expected Result

* full installation succeeds (`ok == true`) on Apple Silicon
* Docker-based rootless service tier works
* the `nono` workspace sandbox works (Seatbelt) — unlike the Microsandbox
  microVM runtime this replaced, nono has **no Apple-Silicon-only
  restriction**; this test still targets Apple Silicon because the
  **inference tier** (host-native omlx) is macOS-Apple-Silicon-only, a
  restriction that is unrelated to the workspace runtime and must not be
  conflated with it

**Automated coverage:** this is the same flow as §2.1 —
`TestSetupBringsServiceTierUpOnHardware`/`TestSetupSucceedsOnHardware`.
`.github/workflows/ci.yml` runs `make test-acceptance` on a
`workflow_dispatch`-triggered, self-hosted `[macOS, ARM64]` runner (the only
CI host that can run the full stack); the workflow file itself does not set
`AIP_HARDWARE_TESTS=1`, so whether the hardware-gated tests actually execute
there (vs. self-skip) depends on that variable being exported in the runner's
own environment — unverifiable from the repo alone. **Note:** this Apple-
Silicon-only CI constraint was originally driven by the Microsandbox
hypervisor requirement; now that the workspace sandbox (nono) itself has no
such restriction, the constraint is revisitable for workspace-only test runs
(a Linux runner could exercise §11/§16's workspace-sandbox checks) — but the
service tier's omlx inference check would still need Apple Silicon. No new CI
configuration is proposed here; this is a note for future consideration.

---

## 12.2 Linux Test `[S6]`

### Test

* run full installation on Linux

### Expected Result

* Docker or Podman auto-detected (`config/runtime.yaml.detected` set)
* the `nono` workspace sandbox works via Landlock LSM (no KVM/virtualization
  needed — nono sandboxes a host process directly, not a VM)
* no manual configuration required

**Unverified:** there is no Linux self-hosted runner in
`.github/workflows/ci.yml` yet (it is future work per that file's own comment)
and no test in `test/acceptance/` or `test/integration/` exercises this on
Linux — the suite's hardware-gated tests only ever run on the Apple Silicon
runner.

---

## 12.3 OS Equivalence Test — Removed

There is no OS choice any more: `ai create` has no `--os` flag, there is no
per-project Dockerfile/base-image, and a workspace simply runs on whatever OS
the host already has (§6). "OS equivalence" is therefore not a meaningful
acceptance criterion under the nono model — a workspace IS the host, by
definition identical to itself. The test this section used to define
(creating a workspace per OS key and diffing tooling across them) and its
automated coverage, `TestOSEquivalenceOnHardware`
(`test/acceptance/acceptance_test.go`), are both gone, not replaced.

---

# 13. Persistence Tests — Removed (guarantee is now trivial, not platform-specific)

(There is no backup/restore feature — architecture §32.) The old persistence
guarantee was the **overlay**: a Microsandbox microVM's writable disk layer,
which had to specifically survive a `--replace` recreate for installed
programs/agent state to still be there. `internal/overlay` is **deleted** —
there is no VM disk any more. A workspace's project directory **is** a host
directory; whatever a sandboxed nono session writes there (or, subject to its
profile's filesystem grants, anywhere else on the host) simply persists the
way any file on disk persists, because there is no VM boundary to survive in
the first place. This is no longer a distinct, independently-failable platform
behavior — it is a property of the host filesystem, not something `ai`
implements or could break — so it is not a meaningful acceptance criterion on
its own any more.

The two tests this section used to define are both **removed, not replaced**:

## 13.1 Writes Persist Across Stop/Start `[S4]` — Removed

The old test wrote a file via `ai exec` into the VM overlay, cycled `ai stop` +
`ai start`, and re-read it. `ai exec` is deleted entirely (§9.2/CLI §4), and
`ai start` is now interactive-only (§3.3), so this exact shape cannot be driven
through the `--json` harness at all. More fundamentally, there is no overlay
left to prove survives anything — the file would just be sitting in the
project directory on host disk the whole time, stop or no stop. The automated
test this section cited, `TestOverlayPersistsAcrossRecreation`
(`test/acceptance/acceptance_test.go`), is gone and has no replacement — there
is nothing overlay-shaped left to test.

## 13.2 Agent State Persists Across Restart `[S4]` — Removed

Same reasoning as §13.1: the "outside the project mount" framing assumed a
VM-guest/host-mount split that no longer exists (nono is not a VM; there is no
second filesystem view to mount anything into). Whatever directory an agent
writes to on the host is just... a directory on the host. No automated test
covers this (or needs to) any more.

---

# 14. Diagnostics Tests

## 14.1 Doctor Command `[S1]`

### Test

```bash id="t23"
ai doctor --json
```

### Expected Result

`ai doctor` is consolidated — one command covers the platform dependencies, every
managed service, and (when a project name is given, §11.1) the workspace runtime.
It always exits `0`; per-check status conveys health.

* the envelope is `command: doctor`, `ok == true`, exit `0`, with a non-empty
  `data.checks`
* the platform-dependency checks are always present: `container runtime` and
  `nono runtime` (the old `microsandbox runtime`/`host virtualization` checks
  are gone — nono has no separate "virtualization available" check; it
  sandboxes a host process directly, so the only question is whether the
  `nono` binary is installed)
* the SERVICES section lists every managed service — the host-native inference
  backend `omlx`, plus `presidio`, `valkey`, `redisinsight`,
  `litellm`, `headroom`, `proxy`, `dns` (the names appear even when stopped
  off-hardware). `presidio` reads **`disabled`** when the `secret-masking`
  guardrail is off (listed but not probed). The host tier has no optional services
  (Open WebUI and Odysseus have both been removed from the platform entirely —
  there is no in-VM-app mechanism any more).
  (`ai services status` additionally surfaces a **display-only `postgres` line**
  immediately after `litellm` for the LiteLLM Postgres `aip-litellm-db`, which has
  no independent lifecycle verb.)
* `ai doctor ghost` (unknown name) still exits `0` with a report — a shortfall is
  folded into the checks, not a non-zero exit

**Automated coverage:** `TestDoctorRunsAndReports` (basic exit-0-with-checks
smoke), `TestDoctorAlwaysReportsExit0` (the `ai doctor ghost` case plus the
platform-dependency checks), and `TestDoctorListsAllServices` (the SERVICES
section names, including that the removed `open-webui`/`odysseus` services are
absent) — all in `test/acceptance/acceptance_test.go`.

---

## 14.2 State Repair `[S1]`

### Test

```bash id="t24"
ai state repair --json
```

### Expected Result

* missing state reconstructed
* inconsistencies resolved

**Automated coverage:** `TestStateRepairThenShow`
(`test/acceptance/acceptance_test.go`) — it also exercises the companion `ai
state show`, asserting the envelope's `data` carries `projects`/`config`/
`runtime` keys (not documented elsewhere in this spec).

---

## 14.3 Log Command

Not previously documented in this spec: `ai logs` streams/tails a service
container's log (CLI §4 family). Two behaviors are exercised directly:

### Test

```bash
ai logs --json                        # no service tier running / no filter
ai logs --service bogus --json        # unknown service name
```

### Expected Result

* with no service tier reachable, `ai logs` still returns a clean, `ok`
  envelope (`command: logs`) rather than erroring
* `--service <unknown>` is invalid input and exits `2`

**Automated coverage:** `TestLogsEmptyReturnsCleanEnvelope` and
`TestLogsUnknownServiceExits2`, both in `test/acceptance/acceptance_test.go`.

---

## 14.4 CLI Baseline

Not previously documented in this spec: two general CLI-contract checks that
apply platform-wide, independent of any project or service.

### Test

```bash
ai --version --json
ai bogus --json
```

### Expected Result

* `ai --version` reports `data.name == "ai"` and a non-empty `data.version`,
  exit `0`
* an unknown subcommand (`ai bogus`) exits `2`

**Automated coverage:** `TestVersion` and `TestUnknownCommandExits2`, both in
`test/acceptance/acceptance_test.go`.

---

# 15. Performance Tests

## 15.1 Large Repository Context `[S2]`

### Test

* with `fixtures/large-repo` (>1M LOC), issue a model call with a large raw
  multi-turn context under the project's Headroom strategy

### Expected Result

* Headroom reduces the request's input tokens by ≥ 50% (§1.6 threshold) via its
  per-request `keep_turns`/`output_buffer_tokens` knobs
* no model overload; command exits `0`

---

# 16. Security Tests

## 16.1 Secret Leakage Test `[S1]`

### Test

```bash
# exact-match search for the sentinel across host state, logs, and projects
grep -rIF "$AIP_TEST_SENTINEL" "$AIP_TEST_HOME/.ai-platform" "$AIP_TEST_HOME/projects"  # expect no match
find "$AIP_TEST_HOME" -name '.env' -print                                               # expect empty
```

### Expected Result

* the sentinel grep produces **zero matches** (exit non-zero) across state,
  logs, audit, and project files
* the `find` for `.env` returns nothing
* audit log records secret-access events without values (arch §31)

**Unverified:** no test in `test/acceptance/` or `test/integration/` runs this
host-state-wide sentinel grep or the `.env` search. The narrower per-session
check this note used to point at, `TestCredentialedRequestOnHardware`, no
longer exists (§9.1) — there is currently no automated secret-leak scan at any
level beyond the mocked unit tests described in §9.1/§9.2.

---

## 16.2 Workspace Isolation Test — Removed (claim no longer holds; corrected)

**This section's premise is gone and must not be restated as-is.** The old test
assumed a Microsandbox microVM: a separate guest kernel, so the host filesystem
and the host's Docker socket were categorically unreachable from inside it —
a hardware/VM boundary. **nono provides a fundamentally weaker guarantee**:
same-kernel, kernel-capability sandboxing (Landlock LSM on Linux, Seatbelt on
macOS). It restricts a sandboxed process's *filesystem paths and network
access* per its profile; it does **not** hide the host kernel, its process
table, or any path the profile doesn't explicitly deny. A host-only marker
file placed outside the project directory is **not** categorically
unreachable the way it was under a microVM — whether a session can read it
depends entirely on what the active nono profile's filesystem grants allow,
not on a kernel boundary. The "no Docker socket inside the workspace" check
is also meaningless now: there is no separate guest filesystem for a socket
to be absent from — if `/var/run/docker.sock` exists on the host and the
profile doesn't block it, it is simply there.

Compounding this, the platform's own generated nono profile
(`internal/workspace/profile.go`'s `ensureProfile`) does **not configure any
filesystem restrictions at all** — it sets only `Network` (block/allow/deny
domains) and, for gemini, an environment variable. Filesystem confinement, if
any, comes entirely from whatever base/registry-pack profile is extended
(`nolabs-ai/opencode`, `nolabs-ai/claude`, `nolabs-ai/codex`, or nono's own
built-in `default`) — a third party's defaults, not something this platform
declares or could assert a specific acceptance criterion about today.

The old automated coverage, `TestWorkspaceIsolationOnHardware`
(`test/acceptance/s1_hardware_test.go`), is gone and has **no replacement** —
there is no test anywhere in `test/acceptance/` or `test/integration/` that
makes any filesystem-isolation claim about a workspace, and per the above, one
should not be written against this platform's own profile-generation code
until it actually declares filesystem capabilities itself. Any future
isolation test must be scoped to "kernel-capability sandboxing restricts
certain paths/syscalls" (Landlock/Seatbelt semantics), never "the host
filesystem is unreachable."

---

## 16.3 Egress Confinement Test `[S1]`

### Test

A project's egress mode is `deny` or `public` (default `public` — allow-
outbound, only private ranges blocked), compiled into the project's generated
nono profile's `Network` capabilities (`Block`/`AllowDomain`/`DenyDomain` —
bare domains, no ports) at the next `ai agent`/`ai shell`/`ai start` — there is
no Microsandbox NetworkPolicy/`msb` net-rules any more, no
`host.microsandbox.internal` (the default gateway host is now plain
`127.0.0.1`), and no in-workspace `$AI_PLATFORM_HOST`/`$LITELLM_PORT` env vars
(those are host-CLI-process env overrides, not something injected into a
sandboxed session). Enforcement is nono's own Landlock/Seatbelt network
filtering per the compiled profile, not a VM boundary or an egress proxy.

**There is no hardware-gated test of this any more.** The old
`TestEgressConfinementOnHardware` (`test/acceptance/s1_hardware_test.go`) —
which probed `http://$AI_PLATFORM_HOST:$LITELLM_PORT/health` and
`https://example.com` from inside a workspace via `ai exec` — is gone,
**not replaced**: `ai exec` no longer exists, and there is no PTY-driving
harness to launch an interactive `ai agent`/`ai shell` session and probe its
actual network reachability. The only remaining coverage is a **config
round-trip**, not a runtime-enforcement test:

```bash
ai network egress deny   <project> --json
ai network show          <project> --json   # expect: egress == "deny"
ai network egress public <project> --json
ai network show          <project> --json   # expect: egress == "public"
```

### Expected Result

* `ai network egress <mode>` persists the mode to `config.yaml`; `ai network
  show` reports it back exactly (`ResolvedEgress()` — `deny` or `public`,
  defaulting empty to `public`)
* no restart step is needed — the change takes effect on the next session
  launch (the profile is regenerated on every launch)
* **not asserted by any current test:** that a `deny`-mode session actually
  cannot reach a non-allow-listed destination, or that a `public`-mode session
  can reach the open internet but not a private/link-local address — both
  need a live, probeable nono session, which requires the not-yet-built
  PTY-driving harness (§9.1)

**Automated coverage:** `TestGroup04WorkspaceLifecycle`'s "egress deny/public
round-trips" subtest (`test/integration/workspace_test.go`) exercises exactly
the config round-trip above against the live integration stack. There is no
acceptance-level (`test/acceptance/`) coverage, and no test anywhere asserts
actual network enforcement.

---

# 17. Final System Acceptance Criteria

The system is considered fully implemented when:

* all CLI commands pass tests
* all workflows execute without manual setup
* agents operate independently
* workspace sessions are sandboxed consistently (nono) across every supported
  host, with no OS/image to keep reproducible any more (§6)
* secrets are never exposed
* cross-platform behavior is identical
* AI-assisted workflows function reliably
