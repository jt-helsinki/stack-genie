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
* secrets integration
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

## 1.4 Interactive Project Creation (PTY)

`ai project create` is an interactive wizard with no per-choice flags (CLI §3.1),
so the harness drives it through a **pseudo-terminal (PTY)**. Tests use a helper:

```text
create_project <name> [os=<key>] [clis=<csv>] [default_tool=<key>] \
                      [stacks=<csv>] [abort=1]
```

* spawns `ai project create <name> --json` attached to a PTY
* navigates each wizard step **accepting the presented default** unless an
  override is given; with no overrides the defaults are `os=debian-trixie`,
  `clis=opencode`, `default_tool=opencode`, `stacks=` (none)
* `abort=1` drives the wizard to **Abort** instead of **Confirm**
* returns the final `--json` envelope captured from stdout (wizard prompts are on
  the PTY/stderr and are not part of the envelope)

Because every `ai project create` in these tests goes through `create_project`,
the Slice 1 default OS (`debian-trixie`) and default CLI (`opencode`) are used
unless a test overrides them. A `create` with **no TTY** is itself a test case
that must exit `2` (§3.1).

---

## 1.5 Slice Tagging

Each test is tagged with the slice from which it must pass (see
`02-implementation-roadmap.md`). A test does not apply before its slice.

```text
[S1] Slice 1   [S2] Slice 2   [S3] Slice 3
[S4] Slice 4   [S5] Slice 5   [S6] Slice 6
```

---

## 1.6 Test Harness

All tests run unattended against a clean, ephemeral environment. Commands are
non-interactive except `ai project create`, which is driven through a PTY via the
`create_project` helper (§1.4).

### Fixtures

* **HOME**: a throwaway `$AIP_TEST_HOME`; `~/.ai-platform` resolves under it.
  Removed on teardown.
* **sample dir**: `fixtures/sample-app` — a small multi-language directory used
  as a working directory for `ai project create` (the project is created in
  place). The platform does no git, so there is no clone/repo fixture. (There
  are also no merge/conflict tests — git is the in-workspace agent's job, not
  the platform's; architecture §21–§22.)
* **large repo**: `fixtures/large-repo` (synthetic, >1M LOC) for performance
  tests, generated deterministically by `fixtures/gen-large-repo` from a fixed
  seed.
* **mock provider**: `fixtures/mock-provider` — a local **HTTPS** server that
  emulates an OpenAI-compatible model endpoint with deterministic responses and
  records the provider credential on each inbound request (used to assert that
  LiteLLM attached the real key). It serves TLS with a test cert the harness
  trusts (arch §17). LiteLLM holds the real provider key (keys-in-LiteLLM) and
  attaches it on the upstream call, so the injection tests (§9.1) assert the key
  is present in the request the mock provider records — end-to-end over HTTPS.
* **credential sentinel**: the test credential stored in the LiteLLM gateway has
  a known, unique value `AIP_TEST_SENTINEL_<uuid>` (env: `AIP_TEST_SENTINEL`).
  Secret assertions are exact: the sentinel must appear in the mock provider's
  recorded request, and must appear **nowhere** in the workspace env, the
  workspace filesystem, `~/.ai-platform`, or `~/projects`. The workspace agent
  holds only a scoped LiteLLM **virtual key**, never the provider secret.
* **egress policy fixture** (`fixtures/egress-policy`): the harness configures
  the egress controls explicitly so tests assert against a *known* policy, not
  ambient host behavior — a **Microsandbox default-deny NetworkPolicy** (declared
  via `ai network`, applied per-workspace via the Microsandbox **Go SDK**, plan
  §3.4) that permits the workspace to reach only the trusted host service ports
  (`AI_PLATFORM_HOST`: LiteLLM, Headroom) plus the allow-listed
  `$MOCK_PROVIDER_URL` (§ Setup). It is rendered from this fixture (parameterized
  by `$MOCK_PROVIDER_URL`) so the source of "what is allowed" is the fixture, not
  a test's expectation. There is **no egress proxy** — confinement is the
  NetworkPolicy; live enforcement at workspace start is a deferred
  hardware-bring-up seam (arch §29.4–29.5).

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
  export MOCK_PROVIDER_URL=$(start fixtures/mock-provider)   # starts the local HTTPS OpenAI-compatible endpoint; prints its https:// base URL (reachable from the workspace) — also the one allow-listed destination (egress policy fixture). The harness trusts its test cert.
  ai setup --json --provider-config fixtures/mock-provider/litellm.yaml
  printf '%s' "$AIP_TEST_SENTINEL" | ai secrets set openai --stdin --json   # stored in the LiteLLM gateway (keys-in-LiteLLM)

teardown:
  ai project delete <each> --purge --yes        # best effort
  stop fixtures/mock-provider
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

Destructive commands (`ai project delete`) are confirmed
non-interactively with `--yes` (CLI §20). `ai workspace destroy` is **not**
destructive — it keeps the overlay and host source and needs no `--yes` (§4.4) —
so it is excluded. There is no AI-approval flow.

### Thresholds

| Metric | Threshold |
|---|---|
| `ai models test` round-trip (mock provider) | < 2000 ms |
| Headroom input reduction on large-repo context | ≥ 50% tokens, stays ≤ `context.max_tokens` |
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
* the service tier started as containers: Ollama, Presidio (analyzer +
  anonymizer), LiteLLM (+ Postgres), Headroom — all on `aip-net`
* command exits `0`

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

---

# 3. Project Lifecycle Tests

## 3.1 Project Creation `[S1]`

### Test

```bash id="t5"
create_project test-project              # PTY-driven wizard; accepts defaults (§1.4)
```

### Expected Result

* project created under `~/projects/`
* Microsandbox workspace microVM started
* debian-trixie microVM running (the wizard's default OS)
* `agent.tools` is `[opencode]` and `agent.default_tool` is `opencode` (wizard defaults)
* LiteLLM accessible
* the workspace holds a scoped LiteLLM virtual key (keys-in-LiteLLM, arch §17); provider keys live only in the gateway
* state updated under `~/projects/test-project/.ai-platform/` + indexed in `config/projects.yaml`

### Negative case

* `ai project create test-project` with **no TTY** (not attached to a PTY) exits
  `2` — the wizard cannot prompt non-interactively
* aborting the wizard (`create_project test-project abort=1`) exits `0`, makes no
  changes, and reports `data.cancelled == true`

---

## 3.2 Create In Existing Directory `[S1]`

### Test

```bash id="t6"
# A directory with pre-existing files (the platform runs no git).
mkdir -p "$AIP_TEST_HOME/work/app" && echo hi > "$AIP_TEST_HOME/work/app/README.md"
cd "$AIP_TEST_HOME/work/app" && create_project app
```

### Expected Result

* the project is created in the current directory (`root` == that directory)
* pre-existing files are left untouched (`README.md` still present)
* no `.git` is created by the platform
* re-running `ai project create` in the same directory **attaches** to the
  existing workspace instead of erroring

---

## 3.3 Workspace Destroy Safety `[S1]`

### Test

```bash id="t7"
ai workspace destroy test-project --json
```

### Expected Result

* the Microsandbox workspace microVM is destroyed; the overlay and host source remain intact
* `ai workspace start test-project` recovers the workspace (re-mounts the overlay)
* no `--yes` needed — destroy is non-destructive (CLI §20)

---

## 3.4 Project Delete `[S1]`

### Test

```bash id="t7b"
create_project del-test

# (a) KNOWN project, missing confirmation → exit 2 (isolates the missing-`--yes` cause)
ai project delete del-test --json

# (b) UNKNOWN project, WITH --yes → exit 2 (isolates the unknown-project cause)
ai project delete no-such-project --yes --json

# (c) successful default delete (keeps host source)
ai project delete del-test --yes --json
```

### Expected Result

The two exit-`2` cases are deliberately separated so the test never conflates an
unknown project with a missing confirmation:

* **(a)** exits `2` **because confirmation is missing** — the project provably
  exists, and `--yes` is the only thing absent; nothing is removed and `del-test`
  still appears in `config/projects.yaml`
* **(b)** exits `2` **because the project is unknown** — `--yes` is present, so a
  missing confirmation cannot be the cause; nothing is removed
* **(c)** default delete: workspaces + overlays removed; `config/projects.yaml`
  no longer lists `del-test`; tracked `~/projects/del-test/.ai-platform/` files
  (Dockerfile / config / profile / project.yaml / `skills/caveman/`) **remain**;
  `.ai-platform/run/` is **cleared**
* `--purge` (e.g. `ai project delete <p> --purge --yes` on a separately created
  disposable project) additionally removes `~/projects/<p>` entirely

---

# 4. Agent System Tests — Removed

There is no platform "agent" abstraction and no `ai agent` commands (architecture
§20, CLI §5). Running multiple AI agents on a project is the in-workspace agent
CLI's concern, so there are no platform-level agent tests. The `[S3]` tag is
retired.

---

# 5. Git Workflow Tests — Removed

The platform performs no git at all (architecture §21) — not even at project
creation — so there are no platform-level git-workflow tests. Init/clone,
branching, merging, and rebasing are the user's and the in-workspace agent's
job.

---

# 6. Environment (Dockerfile) Tests

(There is no snapshot system — no versioning, upgrade, or rollback. A project's
environment is its `.ai-platform/Dockerfile`, architecture §25.)

## 6.1 Project Dockerfile Created `[S1]`

### Test

```bash id="t12"
create_project env-test
ai workspace exec env-test --json -- cat /etc/os-release
```

### Expected Result

* `~/projects/env-test/.ai-platform/Dockerfile` exists, seeded from the
  `debian-trixie` template
* the workspace image is built from that Dockerfile; `data.stdout` of
  `os-release` shows Debian trixie

(Caveman is seeded from Slice 2, not asserted here; see §8.)

---

## 6.2 Environment Change Via Dockerfile `[S1]`

### Test

```bash
# edit the project's Dockerfile to add a package, then recreate (debian-trixie → apt)
printf '\nRUN apt-get update && apt-get install -y jq && rm -rf /var/lib/apt/lists/*\n' >> ~/projects/env-test/.ai-platform/Dockerfile
ai workspace destroy env-test --json
ai workspace start   env-test --json
ai workspace exec    env-test --json -- command -v jq
```

### Expected Result

* the rebuilt image contains `jq` (final exec `data.exit_code == 0`)
* the change came from editing the Dockerfile — no snapshot/upgrade command exists

---

## 6.3 Agent CLI Selection `[S1]`

### Test

```bash
# select a non-default agent CLI in addition to the default
create_project cli-test clis=opencode,codex default_tool=codex
ai workspace exec cli-test --json -- sh -c 'command -v opencode && command -v codex'
ai workspace exec cli-test --json -- sh -c 'command -v gemini'   # NOT selected
```

### Expected Result

* the selected CLIs are installed: the `opencode` + `codex` probe succeeds
  (`data.exit_code == 0`)
* an **unselected** CLI is absent: the `gemini` probe fails (`data.exit_code != 0`)
* `config.yaml` records `agent.tools: [opencode, codex]` and
  `agent.default_tool: codex`; both appear in `.ai-platform/Dockerfile`
* default selection (`create_project x`) yields `agent.tools: [opencode]` (§3.1)

---

## 6.4 Software Stack Selection `[S1]`

### Test

```bash
# select software stacks in the wizard (step 5)
create_project stack-test stacks=go,node
ai workspace exec stack-test --json -- sh -c 'command -v go && command -v node'
ai workspace exec stack-test --json -- sh -c 'command -v python3'   # NOT selected
```

### Expected Result

* the selected stacks are installed: the `go` + `node` probe succeeds
  (`data.exit_code == 0`)
* an **unselected** stack is absent: the `python3` probe fails
  (`data.exit_code != 0`)
* `profile.yaml` records `stacks: [go, node]` and the matching stack snippets
  appear in `.ai-platform/Dockerfile` (§25, repo-layout §1.5)
* default selection (`create_project x`) installs **no** stacks beyond the base
  image (`profile.yaml` `stacks: []`)

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

---

## 7.2 Model Invocation Test `[S1]`

### Test

```bash id="t16"
ai models test gpt-5 --json
```

### Expected Result

* response received
* latency within acceptable bounds

---

# 8. Context Optimization Tests

## 8.1 Caveman Output Compression `[S2]`

### Test

```bash id="t17"
ai context caveman test-project full --json
```

### Expected Result

* the Caveman skill is present at `<project>/.ai-platform/skills/caveman/`
  (seeded at create from Slice 2)
* Caveman level applied
* agent output tokens reduced
* technical accuracy preserved (code, URLs, facts unchanged)

---

## 8.2 Headroom Compression `[S2]`

### Test

* with `fixtures/large-repo` open, issue a model call whose context exceeds
  `context.max_tokens`

### Expected Result

* Headroom reduces input tokens by ≥ 50% (harness threshold, §1.6)
* the request sent to the provider stays ≤ `context.max_tokens`
* `ai context status --json` reports the reduction
* command exits `0`

---

# 9. Secrets Management Tests

## 9.1 Credentialed Outbound Request `[S1]`

Validates the keys-in-LiteLLM model (architecture §17): the agent never holds the
real provider secret, yet the upstream request is delivered with the real key
that LiteLLM holds in its own store.

### Test

```bash id="t18"
create_project test-project
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

---

## 9.2 Secret Isolation `[S1]`

### Test

The sentinel is expanded by the **host** shell (double quotes, no `sh -c '…'`),
then searched verbatim — it is never relied upon inside the workspace, where it
is unset.

```bash
# 1) workspace env must not contain the sentinel — capture stdout on the host, grep host-side
ai workspace exec test-project --json -- env | jq -r .data.stdout > ws-env.txt
grep -qF "$AIP_TEST_SENTINEL" ws-env.txt        # expect: no match (exit 1)

# 2) workspace filesystem must not contain the sentinel — inner grep, host-expanded arg
ai workspace exec test-project --json -- grep -rIF "$AIP_TEST_SENTINEL" / 2>/dev/null \
  | jq -e '.data.exit_code != 0'                # expect: inner grep found nothing
```

### Expected Result

* the host-side grep of the captured env finds **no** sentinel; the env instead
  contains only the scoped LiteLLM virtual key
* the filesystem grep's `data.exit_code` is non-zero (sentinel absent)
* the real provider key is never reachable from the workspace — it lives only in
  the LiteLLM gateway (keys-in-LiteLLM, arch §17), and any non-allow-listed egress
  from the workspace is denied by the Microsandbox NetworkPolicy (§16.3)

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
ai workspace doctor test-project --json
```

### Expected Result

* service tier: `data.runtime.rootless == true`; no privileged containers
  (`data.runtime.privileged == false`); if rootless is unavailable, the command
  exits `4` (no rooted fallback, §6.1)
* workspace: runs as a Microsandbox microVM (`data.workspace.kind == "microvm"`);
  if host virtualization is unavailable, the command exits `4` (no non-microVM
  fallback, §6.2)

---

## 11.2 Runtime Abstraction Test `[S6]`

### Test

* run the identical project-creation + workspace-start flow once under Docker
  and once under Podman (force via `config/runtime.yaml`)

### Expected Result

* identical `--json` `data` for both runtimes (modulo `runtime` field and ids)
* no workflow changes required

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

---

## 12.2 Linux Test `[S6]`

### Test

* run full installation on Linux (KVM available)

### Expected Result

* Docker or Podman auto-detected (`config/runtime.yaml.detected` set)
* Microsandbox microVMs run via KVM
* no manual configuration required

---

## 12.3 OS Equivalence Test `[S5]`

### Test

* `create_project per-<os> os=<key>` for each OS key
  (`alma`, `debian-trixie`, `debian-bookworm`, `ubuntu`), each with the same
  agent-CLI selection, and run the same tooling-smoke command in each

### Expected Result

* all four workspaces expose an identical **base** tooling surface (§12: Git,
  GitHub CLI) and the **same selected agent CLI(s)** — the OS choice never
  changes which tools are present
* the smoke command produces equivalent results across all four
* no OS is treated as a default

---

# 13. Persistence Tests

(There is no backup/restore feature — architecture §32. The platform's
persistence guarantee is the overlay: installed programs and agent state survive
restart and recreation.)

## 13.1 Writes Persist Across Recreation `[S4]`

Deterministic — no package manager, network, or `sudo` (so it doesn't depend on
distro mirrors or whether the image grants sudo).

### Test

```bash id="t21"
# write an executable into the overlay
ai workspace exec test-project --json -- sh -c \
  'mkdir -p ~/.local/bin && printf "#!/bin/sh\necho overlay-ok\n" > ~/.local/bin/overlay-tool && chmod +x ~/.local/bin/overlay-tool'
# destroy (non-destructive) and recreate the workspace
ai workspace destroy test-project --json
ai workspace start   test-project --json
# the file is still there
ai workspace exec test-project --json -- sh -c '~/.local/bin/overlay-tool'
```

### Expected Result

* after recreation the tool runs: `data.stdout` is `overlay-ok`,
  `data.exit_code == 0`
* it survived because it lives in the per-workspace overlay (§26); `destroy`
  kept the overlay and `start` re-mounted it

---

## 13.2 Agent State Persists Across Restart `[S4]`

### Test

```bash id="t22"
ai workspace exec test-project --json -- sh -c 'echo hi > ~/.local/agent-state'
ai workspace destroy test-project --json
ai workspace start   test-project --json
ai workspace exec test-project --json -- sh -c 'cat ~/.local/agent-state'
```

### Expected Result

* the file persists; final exec `data.stdout` is `hi`, `data.exit_code == 0`
* agent-written state outside the project mount survives via the overlay

---

# 14. Diagnostics Tests

## 14.1 Doctor Command `[S1]`

### Test

```bash id="t23"
ai doctor --json
```

### Expected Result

* system health report generated
* no crashes
* actionable output

---

## 14.2 State Repair `[S1]`

### Test

```bash id="t24"
ai state repair --json
```

### Expected Result

* missing state reconstructed
* inconsistencies resolved

---

# 15. Performance Tests

## 15.1 Large Repository Context `[S2]`

### Test

* with `fixtures/large-repo` (>1M LOC), issue a model call whose raw context
  exceeds `context.max_tokens`

### Expected Result

* Headroom keeps the request ≤ `context.max_tokens` (§1.6 threshold)
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
ai workspace exec test-project --json -- cat "$AIP_TEST_HOME/host-only-marker" \
  | jq -e '.data.exit_code != 0'

# no host Docker socket inside the workspace
ai workspace exec test-project --json -- test -e /var/run/docker.sock \
  | jq -e '.data.exit_code != 0'
```

### Expected Result

* the host-only marker is **not** readable from the workspace
  (`data.exit_code != 0`) — the host filesystem is not reachable
* no host Docker socket is present (`data.exit_code != 0`) (arch §30)
* the `ai` process itself exits `0` for both (the commands ran); isolation is
  asserted via `data.exit_code`, per §4.5

---

## 16.3 Egress Confinement Test `[S1]`

### Test

Egress is governed by the **Microsandbox default-deny NetworkPolicy**, which
allows the workspace to reach only the trusted host service ports plus the
allow-listed `$MOCK_PROVIDER_URL` (arch §29.4). The policy comes from the
**egress policy fixture** (§1.6) — so this test asserts against a defined policy,
not ambient behavior. `example.com` is denied **because it is not on the
fixture's allow-list**. There is no egress proxy.

`$MOCK_PROVIDER_URL` is the harness-exported endpoint from §1.6 Setup (the single
allow-listed destination). Note the quoting convention below: values the
platform injects **into the workspace** (`AI_PLATFORM_HOST`, `LITELLM_PORT`) are
single-quoted so they expand **in the guest**; `$MOCK_PROVIDER_URL` is a
harness-side fixture value, so it is double-quoted to expand **on the host**
before the command is passed verbatim into the workspace (CLI §4.5).

```bash
# trusted host service (LiteLLM) is reachable directly via AI_PLATFORM_HOST (guest-side vars)
ai workspace exec test-project --json -- \
  sh -c 'curl -fsS "http://$AI_PLATFORM_HOST:$LITELLM_PORT/health" >/dev/null'

# allow-listed destination (the mock provider) IS reachable under the NetworkPolicy
# (host-expanded URL, single-quoted inside so the guest receives the literal URL)
ai workspace exec test-project --json -- \
  sh -c "curl -fsS --max-time 5 '$MOCK_PROVIDER_URL/health' >/dev/null"

# NON-allow-listed destination is denied by the Microsandbox NetworkPolicy
ai workspace exec test-project --json -- \
  sh -c 'curl -fsS --max-time 5 https://example.com >/dev/null'   # not on the allow-list → denied
```

### Expected Result

* the LiteLLM health probe via `AI_PLATFORM_HOST` succeeds (`data.exit_code == 0`)
* the allow-listed mock-provider probe succeeds (`data.exit_code == 0`) — proving
  the NetworkPolicy permits exactly what the fixture allows, so the deny below is
  about policy, not broken connectivity
* the `example.com` probe **fails** (`data.exit_code != 0`) because the
  Microsandbox NetworkPolicy default-deny allow-list (fixture, §1.6) excludes it
* the `ai` process itself exits `0` for all (the commands ran); confinement is
  asserted via `data.exit_code`, per §4.5

(Live NetworkPolicy enforcement at workspace start is a deferred
hardware-bring-up seam, arch §29.5; this test runs once that seam is wired.)

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
