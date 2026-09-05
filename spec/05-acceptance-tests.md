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
* environment (Dockerfile) + persistence
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

`ai create` has a flag for every choice (`--name`/`--os`/`--agents`/`--stacks`).
On a TTY those flags **pre-seed** an interactive wizard, but under `--json` (or no
TTY) the wizard is **disabled** and the spec is built straight from the flags (the
programmatic contract — every input has a flag, no prompts; CLI §1.3/§3.1). The
harness always runs with `--json`, so it drives creation with explicit flags
rather than a pseudo-terminal:

```text
harness.CreateProjectWithOS(name, osKey)  →  ai create <name> --os <key> --json
```

* `ai create` is **scaffold-only** — it writes the project's `.ai-platform/`
  artifacts and indexes the project; it does **not** build or boot a workspace.
  `ai start` builds the image and boots the microVM.
* the default base OS is `debian-trixie` and the default agent CLI is
  `opencode`
* `--os` is **required** on this non-interactive path; a missing or unknown value
  exits `2`
* the result is the `--json` envelope on stdout (envelope `command: project.create`)

A `--json`/no-TTY `create` exits `2` **only** when a required input (`--os`) is
missing or a value is invalid — not merely because there is no TTY (§3.1).

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
  upstream call, so the injection tests (§9.1) assert the key is present in the request
  the mock records — end-to-end over HTTPS.
* **credential sentinel**: the test credential stored in the LiteLLM gateway has
  a known, unique value `AIP_TEST_SENTINEL_<uuid>` (env: `AIP_TEST_SENTINEL`).
  Secret assertions are exact: the sentinel must appear in the mock provider's
  recorded request, and must appear **nowhere** in the workspace env, the
  workspace filesystem, `~/.ai-platform`, or `~/projects`. The workspace agent
  holds only a scoped LiteLLM **virtual key**, never the provider secret.
* **egress policy**: the per-project egress
  default is now `public` (allow-outbound), so to assert *confinement* the harness
  configures the egress controls explicitly — it sets **`deny` mode** via
  `ai network` so tests run against a *known* locked-down policy rather than ambient
  host behavior. In `deny` mode the **Microsandbox NetworkPolicy** (applied
  per-workspace as Microsandbox net-rules at workspace create via the `msb` CLI,
  plan §3.4) permits the workspace to reach only the **host gateway** (the
  always-on allow rule: nginx `aip-proxy` → LiteLLM (which calls Headroom
  in-process as a `pre_call` guardrail), default
  `host.microsandbox.internal:18787` — workspaces never reach LiteLLM directly)
  plus the allow-listed `$MOCK_PROVIDER_URL` (§ Setup), configured inline via
  `ai network allow` (parameterized by `$MOCK_PROVIDER_URL`). There is **no egress
  proxy** — confinement is the net-rules applied at workspace create (arch §29.4–29.5).

### Setup / Teardown

`HOME` is repointed at the throwaway dir so `~/.ai-platform` and `~/projects`
resolve under it. The mock provider and the sentinel credential are bootstrapped
before any model-using test runs so credentialed requests succeed. (Provider
credentials are **not** a `ai setup` hard-fail — plan §7; a missing credential is
warned by `ai doctor` and only fails the actual model call.)

```text
setup:
  export AIP_TEST_HOME=$(mktemp -d)
  export HOME="$AIP_TEST_HOME"                 # ~/.ai-platform, ~/projects resolve here
  export AIP_TEST_SENTINEL="AIP_TEST_SENTINEL_$(uuidgen)"
  export MOCK_PROVIDER_URL=$(start in-process httptest mock)   # in-process HTTPS OpenAI-compatible endpoint (mockprovider_test.go); its https:// base URL is reachable from the workspace and is the one allow-listed destination. The harness trusts its test cert.
  ai setup --json --provider-config <inline temp file>   # config written inline by the harness, not a checked-in fixture
  printf '%s' "$AIP_TEST_SENTINEL" | ai keys add openai --stdin --json   # stored encrypted in the LiteLLM DB (keys-in-LiteLLM)

teardown:
  ai delete <each> --purge --yes                # best effort
  (the in-process mock stops with the test process)
  rm -rf "$AIP_TEST_HOME"
```

The mock provider's `litellm.yaml` points LiteLLM's `gpt-5` alias at the local
endpoint, so `ai models test gpt-5` exercises injection end-to-end without a
real provider.

### Assertions

* **every command MUST be run with `--json`** (even where a test below shows the
  bare command for brevity); assertions are made against the §19 envelope
  (`ok`, `data`, `error.code`), never by scraping human text
* exit code is asserted against §18
* filesystem/state assertions check concrete paths from
  `03-repository-layout.md`
* secret assertions use the credential sentinel (exact-match `grep`), not fuzzy
  "looks like a secret" checks

### Non-Interactive Confirmation

Destructive commands (`ai delete`, and its alias `ai destroy`) are confirmed
non-interactively with `--yes` (CLI §20). `ai destroy` is a **cobra alias** of
`ai delete` — the same destructive command (§4.4), so it gates identically (the
microVM is paused with `ai stop`, which needs no confirmation). There is no
AI-approval flow.

### Thresholds

| Metric | Threshold |
|---|---|
| `ai models test` round-trip (mock provider) | < 2000 ms |
| Headroom input reduction on large-repo context | ≥ 50% tokens under the project's strategy |
| Caveman output reduction (full level) | ≥ 40% output tokens |
| workspace create → started (cached image) | < 30 s |

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
* **Microsandbox runtime + host virtualization verified** (Apple Silicon on macOS)
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
  tier has **no optional services** (Open WebUI is now a per-workspace in-VM app
  and Odysseus was removed)
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
ai create test-project --os debian-trixie --json   # flag-driven, non-interactive (§1.4)
```

### Expected Result

`ai create` is scaffold-only — it writes state and indexes the project; it does
**not** build or boot a workspace (that is `ai start`).

* the envelope is `command: project.create`, `ok == true`, exit `0`
* `data.name == test-project`, `data.os == debian-trixie`
* `data.tools` is `[opencode]` (the default agent CLI)
* project scaffolded in the current directory; `data.root` points at it, and the
  project is indexed in `config/projects.yaml`
* state written under `<root>/.ai-platform/` (Dockerfile / config.yaml /
  profile.yaml / project.yaml)

### Negative case

* `ai create test-project --json` with **no `--os`** exits `2` — under `--json`
  the wizard is disabled and `--os` is required (§1.4/§3.1). The failure is the
  missing required input, not the absence of a TTY.

**Automated coverage:** the negative case is `TestProjectCreateNoTTYExits2`; the
positive scaffold-only flow is `TestProjectLifecycle` (create/list/delete); the
default-tools case is `TestAgentCLIDefaultSelection` — all in
`test/acceptance/acceptance_test.go`/`s1_remaining_test.go`.

---

## 3.2 Create In Existing Directory `[S1]`

### Test

```bash id="t6"
# A directory with pre-existing files (ai create runs no git and leaves them untouched).
mkdir -p "$AIP_TEST_HOME/work/app" && echo hi > "$AIP_TEST_HOME/work/app/README.md"
cd "$AIP_TEST_HOME/work/app" && ai create app --os debian-trixie --json
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

### Test

```bash id="t7"
ai stop test-project --json
ai start test-project --json
```

### Expected Result

* `ai stop` stops the Microsandbox workspace microVM but keeps everything — the
  overlay, host source, and project definition remain intact
* no `--yes` needed — stop is non-destructive (CLI §20); pausing a workspace is
  `ai stop`, not `ai delete`/`ai destroy` (which are the same destructive removal)
* `ai start test-project` recovers the workspace (re-mounts the same overlay)

**Unverified:** no test in `test/acceptance/` or `test/integration/` drives a
plain `ai stop` + `ai start` round trip end-to-end. The closest automated
coverage is `TestOverlayPersistsAcrossRecreation` (§13.1, `ai destroy` +
`ai start` — a *stronger* teardown than a mere stop) and the mocked unit test
`TestRestartStopsThenStartsExistingMicroVM`
(`internal/workspace/workspace_test.go`), which exercises `Restart` (stop then
start) against a fake sandbox, not the real CLI round trip.

---

## 3.4 Project Delete `[S1]`

### Test

```bash id="t7b"
ai create del-test --os debian-trixie --json

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
* **(c)** default delete: the microVM is torn down, the overlay removed, and the
  project's **whole `<project>/.ai-platform/` directory** removed (config /
  profile / project.yaml / Dockerfile / `run/` all gone);
  `config/projects.yaml` no longer lists `del-test`; the user's **OTHER** files in
  the directory are **kept**
* `--purge` (e.g. `ai delete <p> --purge --yes` on a separately created
  disposable project) additionally removes the project directory entirely

**Automated coverage:** cases (a) and (c) are `TestProjectLifecycle`
(`test/acceptance/acceptance_test.go`). Case (b) — unknown project + `--yes` →
exit 2 — is not exercised by `test/acceptance/`; it is verified at the CLI-unit
level by `TestNewDeleteCmdUnknownProject`
(`internal/cli/project_helpers_test.go`) and black-box by
`TestWorkspaceCreateTeardown/delete_unknown_errors`
(`test/integration/workspace_lifecycle_test.go`, needs only installed
templates, not a running stack). `--purge` removing the whole project
directory is also covered by
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

# 6. Environment (Dockerfile) Tests

(There is no snapshot system — no versioning, upgrade, or rollback. A project's
environment is its `.ai-platform/Dockerfile`, architecture §25.)

## 6.1 Project Dockerfile Created `[S1]`

### Test

```bash id="t12"
ai create env-test --os debian-trixie --json
ai exec env-test --json -- cat /etc/os-release
```

### Expected Result

* `<project>/.ai-platform/Dockerfile` exists, seeded from the `debian-trixie`
  template (asserted on-disk by the file-inspection probe; runnable without a
  microVM)
* the workspace image is built from that Dockerfile; `data.stdout` of
  `os-release` shows Debian trixie (the in-VM half is hardware-gated)

(Caveman is installed at workspace start by its own upstream installer, not seeded at
create; not asserted here — see §8.)

**Automated coverage:** `TestProjectDockerfileReflectsOS`
(`test/acceptance/s1_remaining_test.go`) asserts the on-disk `FROM` line for all
four OS templates — this is the file-inspection half described above. No test
runs the in-VM `cat /etc/os-release` half (it needs a real microVM build and is
not present in `test/acceptance/` or `test/integration/`).

---

## 6.2 Environment Change Via Dockerfile `[S1]`

### Test

```bash
# edit the project's Dockerfile to add a package, then rebuild (debian-trixie → apt)
printf '\nRUN apt-get update && apt-get install -y jq && rm -rf /var/lib/apt/lists/*\n' >> <project>/.ai-platform/Dockerfile
ai start   env-test --json   # start rebuilds the OCI image (`msb create --replace`)
ai exec    env-test --json -- command -v jq
```

### Expected Result

* the rebuilt image contains `jq` (final exec `data.exit_code == 0`)
* the change came from editing the Dockerfile — no snapshot/upgrade command exists

**Unverified:** no test in `test/acceptance/` or `test/integration/` edits a
project's Dockerfile and rebuilds — this scenario needs a real image build and
is not automated today.

---

## 6.3 Agent CLI Selection `[S1]`

### Test

```bash
# select a non-default agent CLI in addition to the default
ai create cli-test --os debian-trixie --agents opencode,codex --json
ai exec cli-test --json -- sh -c 'command -v opencode && command -v codex'
ai exec cli-test --json -- sh -c 'command -v gemini'   # NOT selected
```

### Expected Result

* the create envelope reports `data.tools == [opencode, codex]`
* `config.yaml` records the selected agent CLIs, and each appears as an
  `# agent CLI: <name>` snippet in `.ai-platform/Dockerfile`; an unselected CLI
  (`gemini`) does **not** appear (asserted on-disk; runnable without a microVM)
* in the workspace (hardware-gated): the `opencode` + `codex` probe succeeds
  (`data.exit_code == 0`), and the unselected `gemini` probe fails
  (`data.exit_code != 0`)
* default selection (`ai create x --os <key>`, no `--agents`) yields
  `data.tools == [opencode]` (§3.1)

**Automated coverage:** `TestAgentCLISelectionReflected` (selection +
Dockerfile snippet presence/absence) and `TestAgentCLIDefaultSelection`
(default `[opencode]`), both in `test/acceptance/s1_remaining_test.go` — the
on-disk half only; the in-VM `command -v` probes are hardware-gated and not
present as an automated test.

---

## 6.4 Software Stack Selection `[S1]`

### Test

```bash
# select software stacks
ai create stack-test --os debian-trixie --stacks go,rust --json
ai exec stack-test --json -- sh -c 'command -v go && command -v cargo'
ai exec stack-test --json -- sh -c 'command -v deno'      # NOT selected
```

### Expected Result

* the create envelope reports `data.stacks == [go, rust]`
* `profile.yaml` records `stacks: [go, rust]` and the matching `# stack: <name>`
  snippets appear in `.ai-platform/Dockerfile`; an unselected stack (`deno`)
  does **not** (§25, repo-layout §1.5; asserted on-disk, runnable without a
  microVM)
* in the workspace (hardware-gated): the `go` + `cargo` probe succeeds
  (`data.exit_code == 0`), and the unselected `deno` probe fails
  (`data.exit_code != 0`)
* Node.js and Python 3 are **baked into every base** (not stacks), so
  `command -v node` / `command -v python3` succeed regardless of `--stacks`
* default selection (`ai create x --os <key>`, no `--stacks`) installs **no**
  stacks beyond the base image (`profile.yaml` `stacks: []`)

**Automated coverage:** `TestSoftwareStackSelectionReflected` and
`TestSoftwareStackDefaultEmpty`, both in
`test/acceptance/s1_remaining_test.go` — the on-disk half only; the in-VM
`command -v` probes are hardware-gated and not present as an automated test.

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

**Gap:** the group-7 source file on disk is still
`test/integration/vllm_runtime_test.go` and was not updated for the vLLM→omlx
refactor — it still names the service `vllm` and still drives the now-removed
`ai models pull`/`vllm/<alias>` surface, so it does **not** actually exercise
the omlx behavior described below. This section documents the intended
criterion; the test itself still needs rewriting.

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

# 9. Secrets Management Tests

## 9.1 Credentialed Outbound Request `[S1]`

Validates the keys-in-LiteLLM model (architecture §17): the agent never holds the
real provider secret, yet the upstream request is delivered with the real key
that LiteLLM holds in its own store.

### Test

```bash id="t18"
ai create test-project --os debian-trixie --json
# inside the workspace, make a model call through LiteLLM to the mock provider:
ai models test gpt-5 --project test-project --json
```

### Expected Result

* the agent's workspace env contains only the scoped LiteLLM **virtual key**,
  never the sentinel (the real provider key)
* the **mock provider's recorded request contains `$AIP_TEST_SENTINEL`**
  (proves LiteLLM attached the real provider key — held in its own store — on the
  upstream call). Since the mock provider serves **HTTPS** (§1.6), this asserts
  the key end-to-end over TLS while the workspace never saw it
* no `.env` file exists anywhere under the workspace or project
* command exits `0`

**Automated coverage:** `TestCredentialedRequestOnHardware`
(`test/acceptance/s1_hardware_test.go`), hardware-gated — it asserts the mock
provider's recorded `Authorization` header carries the sentinel and that the
workspace's own `env` output does not.

---

## 9.2 Secret Isolation `[S1]`

### Test

The sentinel is expanded by the **host** shell (double quotes, no `sh -c '…'`),
then searched verbatim — it is never relied upon inside the workspace, where it
is unset.

```bash
# 1) workspace env must not contain the sentinel — capture stdout on the host, grep host-side
ai exec test-project --json -- env | jq -r .data.stdout > ws-env.txt
grep -qF "$AIP_TEST_SENTINEL" ws-env.txt        # expect: no match (exit 1)

# 2) workspace filesystem must not contain the sentinel — inner grep, host-expanded arg
ai exec test-project --json -- grep -rIF "$AIP_TEST_SENTINEL" / 2>/dev/null \
  | jq -e '.data.exit_code != 0'                # expect: inner grep found nothing
```

### Expected Result

* the host-side grep of the captured env finds **no** sentinel; the env instead
  contains only the scoped LiteLLM virtual key
* the filesystem grep's `data.exit_code` is non-zero (sentinel absent)
* the real provider key is never reachable from the workspace — it lives only in
  the LiteLLM gateway (keys-in-LiteLLM, arch §17), and any non-allow-listed egress
  from the workspace is denied by the Microsandbox NetworkPolicy (§16.3)

**Unverified:** the first check (workspace env has no sentinel) is covered by
`TestCredentialedRequestOnHardware` (§9.1). The second check — a filesystem-wide
`grep -rIF` for the sentinel across the whole workspace — is not exercised by
any test in `test/acceptance/` or `test/integration/` today.

---

# 10. MCP — Not a Platform Concern

The platform does not manage MCP, so there are no MCP acceptance tests. MCP
servers are configured and run by the in-workspace agent (architecture §12), and
any credentials those servers need are the agent's own concern — the platform
manages only the model-path provider keys (in the LiteLLM gateway, §9) and
confines all other workspace egress with the Microsandbox NetworkPolicy (§16.3).

---

# 11. Runtime Tests (Container + microVM)

## 11.1 Rootless Service Tier + microVM Workspace Test `[S1]`

### Test

```bash
ai doctor test-project --json
```

### Expected Result

`ai doctor` is consolidated: given a project name it adds a WORKSPACE-runtime
section to the report. It always runs to completion and exits `0` (per-check
status conveys health, §14.1) — a runtime/virtualization shortfall is folded into
the checks, not a non-zero exit.

* the envelope is `command: doctor`, `ok == true`, exit `0`
* on a provisioned host the workspace posture checks pass: the `workspace
  rootless` and `workspace virtualization` checks each report status `ok` (the
  platform never runs privileged; workspaces are Microsandbox microVMs)
* off-hardware those same checks appear in the report but report a non-`ok`
  status — the command still exits `0`

**Automated coverage:** `TestWorkspaceDoctorOnHardware`
(`test/acceptance/acceptance_test.go`), hardware-gated for the `ok` posture
assertion. Off-hardware, `TestDoctorRunsAndReports`
(`test/acceptance/acceptance_test.go`) confirms `ai doctor` still exits `0`
with a non-empty report (it does not assert the workspace-runtime checks'
non-`ok` status specifically).

---

## 11.2 Runtime Abstraction Test `[S6]`

### Test

* run the identical project-creation + workspace-start flow once under Docker
  and once under Podman (force via `config/runtime.yaml`)

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
* Microsandbox microVM runtime works (Apple Hypervisor)

**Automated coverage:** this is the same flow as §2.1 —
`TestSetupBringsServiceTierUpOnHardware`/`TestSetupSucceedsOnHardware`.
`.github/workflows/ci.yml` runs `make test-acceptance` on a
`workflow_dispatch`-triggered, self-hosted `[macOS, ARM64]` runner (the only
CI host that can run the full stack); the workflow file itself does not set
`AIP_HARDWARE_TESTS=1`, so whether the hardware-gated tests actually execute
there (vs. self-skip) depends on that variable being exported in the runner's
own environment — unverifiable from the repo alone.

---

## 12.2 Linux Test `[S6]`

### Test

* run full installation on Linux (KVM available)

### Expected Result

* Docker or Podman auto-detected (`config/runtime.yaml.detected` set)
* Microsandbox microVMs run via KVM
* no manual configuration required

**Unverified:** there is no Linux/KVM self-hosted runner in
`.github/workflows/ci.yml` yet (it is future work per that file's own comment)
and no test in `test/acceptance/` or `test/integration/` exercises this on
Linux — the suite's hardware-gated tests only ever run on the Apple Silicon
runner.

---

## 12.3 OS Equivalence Test `[S5]`

### Test

* `ai create os-<key> --os <key> --json` for each OS key
  (`alma`, `debian-trixie`, `debian-bookworm`, `ubuntu`), then `ai start
  --project os-<key>`, each with the same agent-CLI selection, and run the same
  tooling-smoke command (`ai exec os-<key> -- <tool> --version`) in each

### Expected Result

* all four workspaces expose an identical **base** tooling surface (§12: Git,
  GitHub CLI) and the **same selected agent CLI(s)** — the OS choice never
  changes which tools are present
* the smoke command produces equivalent results across all four
* no OS is treated as a default

**Automated coverage:** `TestOSEquivalenceOnHardware`
(`test/acceptance/acceptance_test.go`), hardware-gated — it builds a workspace
for each of the four OS keys and probes `git --version`/`gh --version` in
each.

---

# 13. Persistence Tests

(There is no backup/restore feature — architecture §32. The platform's
persistence guarantee is the overlay: installed programs and agent state survive
restart and recreation.)

## 13.1 Writes Persist Across Stop/Start `[S4]`

Deterministic — no package manager, network, or `sudo` (so it doesn't depend on
distro mirrors or whether the image grants sudo).

### Test

```bash id="t21"
# write an executable into the overlay
ai exec test-project --json -- sh -c \
  'mkdir -p ~/.local/bin && printf "#!/bin/sh\necho overlay-ok\n" > ~/.local/bin/overlay-tool && chmod +x ~/.local/bin/overlay-tool'
# stop (non-destructive) and start the workspace again
ai stop  test-project --json
ai start test-project --json
# the file is still there
ai exec test-project --json -- sh -c '~/.local/bin/overlay-tool'
```

### Expected Result

* after restart the tool runs: `data.stdout` is `overlay-ok`,
  `data.exit_code == 0`
* it survived because it lives in the per-workspace overlay (§26); `ai stop`
  kept the overlay and `ai start` re-mounted it (whereas `ai delete`/`ai destroy`
  would have removed it)

**Note on actual coverage:** the real test, `TestOverlayPersistsAcrossRecreation`
(`test/acceptance/acceptance_test.go`, hardware-gated), exercises the
*stronger* `ai destroy` + `ai start` cycle (full teardown + rebuild against
the overlay) rather than the `ai stop` + `ai start` pause/resume shown above —
both are described as [S4] overlay-persistence guarantees, but a plain
stop/start round trip itself is not separately automated (see §3.3).

---

## 13.2 Agent State Persists Across Restart `[S4]`

### Test

```bash id="t22"
ai exec test-project --json -- sh -c 'echo hi > ~/.local/agent-state'
ai stop  test-project --json
ai start test-project --json
ai exec test-project --json -- sh -c 'cat ~/.local/agent-state'
```

### Expected Result

* the file persists; final exec `data.stdout` is `hi`, `data.exit_code == 0`
* agent-written state outside the project mount survives via the overlay

**Note on actual coverage:** as with §13.1, `TestOverlayPersistsAcrossRecreation`
covers this shape of scenario (a marker written outside the project mount
survives) but does so via `ai destroy` + `ai start`, not `ai stop` + `ai
start`.

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
* the platform-dependency checks are always present: `container runtime`,
  `microsandbox runtime`, `host virtualization`
* the SERVICES section lists every managed service — the host-native inference
  backend `omlx`, plus `presidio`, `valkey`, `redisinsight`,
  `litellm`, `headroom`, `proxy`, `dns` (the names appear even when stopped
  off-hardware). `presidio` reads **`disabled`** when the `secret-masking`
  guardrail is off (listed but not probed). The host tier has no optional services
  (Open WebUI is now a per-workspace in-VM app and Odysseus was removed).
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
host-state-wide sentinel grep or the `.env` search; `TestCredentialedRequestOnHardware`
(§9.1) only checks the narrower case of the workspace's own `env` output.

---

## 16.2 Workspace Isolation Test `[S1]`

### Test

The workspace is a Microsandbox microVM (hardware isolation), so the host
filesystem and Docker socket must be unreachable from inside it. Uses a
**host-only sentinel file** placed outside any workspace mount, plus the
docker-socket probe. The `/proc/1/root` probe is not used (it is unreliable and
namespace-dependent).

```bash
# place a marker on the host, outside ~/projects and any mounted path
printf 'HOST_ONLY_%s' "$AIP_TEST_SENTINEL" > "$AIP_TEST_HOME/host-only-marker"

# the workspace must not be able to read the host-only marker by its host path
ai exec test-project --json -- cat "$AIP_TEST_HOME/host-only-marker" \
  | jq -e '.data.exit_code != 0'

# no host Docker socket inside the workspace
ai exec test-project --json -- test -e /var/run/docker.sock \
  | jq -e '.data.exit_code != 0'
```

### Expected Result

* the host-only marker is **not** readable from the workspace
  (`data.exit_code != 0`) — the host filesystem is not reachable
* no host Docker socket is present (`data.exit_code != 0`) (arch §30)
* the `ai` process itself exits `0` for both (the commands ran); isolation is
  asserted via `data.exit_code`, per §4.5

**Automated coverage:** `TestWorkspaceIsolationOnHardware`
(`test/acceptance/s1_hardware_test.go`), hardware-gated.

---

## 16.3 Egress Confinement Test `[S1]`

### Test

The per-project egress default is now `public` (allow-outbound), so to assert
*confinement* the harness sets **`deny` mode** via the egress policy fixture
(§1.6). Under `deny` the **Microsandbox NetworkPolicy** allows the workspace to
reach only the **host gateway** (the always-on allow rule the platform injects —
`nginx aip-proxy → LiteLLM` (which calls Headroom in-process as a `pre_call`
guardrail), the SOLE model path; default
`host.microsandbox.internal:18787`) plus the allow-listed `$MOCK_PROVIDER_URL`
(arch §29.4). Workspaces never reach LiteLLM directly. The policy comes from the
**egress policy fixture** (§1.6) — so this test asserts against a defined policy,
not ambient behavior. `example.com` is denied **because it is not on the
`deny`-mode fixture's allow-list**. There is no egress proxy.

`$GATEWAY_URL` is the resolved gateway address (`runtime.ResolveGateway`, default
`http://host.microsandbox.internal:18787`); `$MOCK_PROVIDER_URL` is the
harness-exported endpoint from §1.6 Setup (the single extra allow-listed
destination). Both are harness-side values, double-quoted so they expand **on the
host** before the command is passed verbatim into the workspace (CLI §4.5).

```bash
# the host gateway (the always-on allow rule, the model path) is reachable
ai exec test-project --json -- \
  sh -c "curl -fsS --max-time 5 '$GATEWAY_URL/health' >/dev/null"

# allow-listed destination (the mock provider) IS reachable under the NetworkPolicy
# (host-expanded URL, single-quoted inside so the guest receives the literal URL)
ai exec test-project --json -- \
  sh -c "curl -fsS --max-time 5 '$MOCK_PROVIDER_URL/health' >/dev/null"

# NON-allow-listed destination is denied by the Microsandbox NetworkPolicy
ai exec test-project --json -- \
  sh -c 'curl -fsS --max-time 5 https://example.com >/dev/null'   # not on the allow-list → denied
```

### Expected Result

* the gateway health probe succeeds (`data.exit_code == 0`) — the always-on
  allow rule (the model path) is reachable
* the allow-listed mock-provider probe succeeds (`data.exit_code == 0`) — proving
  the NetworkPolicy permits exactly what the fixture allows, so the deny below is
  about policy, not broken connectivity
* the `example.com` probe **fails** (`data.exit_code != 0`) because the
  `deny`-mode Microsandbox NetworkPolicy allow-list (fixture, §1.6) excludes it
* the `ai` process itself exits `0` for all (the commands ran); confinement is
  asserted via `data.exit_code`, per §4.5

(Egress confinement is the Microsandbox net-rules rendered at workspace create,
arch §29.5; this test asserts that applied policy on a provisioned host.)

**Automated coverage:** `TestEgressConfinementOnHardware`
(`test/acceptance/s1_hardware_test.go`), hardware-gated — it checks the
trusted host service (LiteLLM) is reachable and that `example.com` is denied;
it does not additionally probe a second allow-listed mock-provider destination
the way the spec's three-probe sequence above does.

---

# 17. Final System Acceptance Criteria

The system is considered fully implemented when:

* all CLI commands pass tests
* all workflows execute without manual setup
* agents operate independently
* environments are reproducible from `.ai-platform/Dockerfile`
* secrets are never exposed
* cross-platform behavior is identical
* AI-assisted workflows function reliably
