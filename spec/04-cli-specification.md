# 04-cli-specification.md

# AI Development Platform

## CLI Specification

Version: 1.0

---

# 0. Purpose

This document defines the complete CLI interface for the AI Development Platform.

All system functionality must be accessible via CLI.

The CLI is the primary control plane for:

* installation
* project creation
* workspace lifecycle
* model routing status
* diagnostics
* context optimization

---

# 1. Global CLI Design Principles

## 1.1 Deterministic Behavior

All commands must be:

* deterministic
* idempotent where possible
* state-aware: global index in `~/.ai-platform/config/projects.json`,
  per-project state in `<project>/.ai-platform/`

---

## 1.2 No Hidden State

All state lives in documented locations:

```text id="c1"
~/.ai-platform/config/        # global settings + projects index
<project>/.ai-platform/       # per-project state (run/ is gitignored)
```

CLI must never rely on undocumented runtime state.

---

## 1.3 Machine-Friendly Output

All commands must support:

* human-readable output (default)
* JSON output (`--json` flag)

---

## 1.4 Cross-Platform Consistency

CLI must behave identically on:

* macOS (Apple Silicon)
* Linux (KVM)
* Windows (via WSL2 with nested virtualization — at-risk)

---

## 1.5 Implementation & Distribution

* The `ai` CLI and all platform tooling — installers, state management,
  Microsandbox orchestration, runtime abstraction, and diagnostics — are
  implemented in **Go** and shipped as a **single static binary** per host.
* Host bootstrap uses **thin launchers only** (bash / zsh / PowerShell) whose
  sole job is to download/locate and exec the compiled Go binary. No platform
  logic lives in shell scripts.
* Project templates and agent configuration are **declarative data**
  (YAML / JSON). They are never executable application logic.
* External components (Microsandbox, LiteLLM, Headroom, ClawPatrol, git, docker /
  podman, gh) are invoked via their Go SDK or as subprocesses — never
  reimplemented.

---

## 1.6 Binary & Command Naming

There is **one binary**, `ai`, installed on `PATH`. There are **no hyphenated
command names and no command aliases or symlinks**. Every command follows a
single, consistent pattern:

```text
ai <noun> [<verb>] [args] [flags]
```

Examples: `ai setup`, `ai project create`, `ai project delete`,
`ai workspace start`, `ai workspace exec`, `ai context status`.

Rules:

* one verb grammar everywhere — no `setup-ai-platform` / `create-ai-project`
  style names
* every command and subcommand supports `--help` / `-h` (§17.0)
* `ai <unknown>` exits `2`

## 1.7 Shell Completion

`ai completion <bash|zsh|fish|powershell>` prints a completion script for the
shell. Completion is **not active until that script is loaded** by the shell
(it is not a runtime flag) — e.g. `source <(ai completion zsh)` for the session,
or install it into the shell's completion directory to persist.

Beyond command and flag-name completion, the CLI provides **dynamic value**
completion:

* `--project` and the optional `[project]` positional → known project names
  (from the global index)
* `ai context strategy` → `conservative|balanced|aggressive`
* `ai context caveman` → `lite|full|ultra|wenyan`
* `ai services console` → services that have an admin console
* `ai logs --service` → the host services; `ai logs --workspace` → project names

## 2.1 Installation

### ai setup

```bash id="c2"
ai setup [--provider-config <file>]
```

`--provider-config <file>` points LiteLLM at a provider/endpoint config (model
aliases → provider URLs). Used both for real provider setup and by the
acceptance harness to target the mock provider.

Purpose:

* installs platform dependencies
* initializes state directory
* configures runtime
* installs, configures, and starts the host services as the single control
  plane — see architecture §5, "Host Services Control Plane". The host service
  set is LiteLLM + Ollama (required) + ClawPatrol, plus the Microsandbox
  workspace runtime. (Headroom is **not** a host service — it is installed per
  project in the workspace image, §8–10.)
* renders each service config from the platform config, verifies the Microsandbox
  runtime + host virtualization, and registers the native service (ClawPatrol)
  with the OS service manager (no docker compose)
* ensures the **ClawPatrol TLS-interception CA** exists (generates it on first
  run); the CA root is installed into each workspace's trust store at workspace
  start, not here — without it HTTPS credential injection cannot work
  (architecture §17, "TLS Interception and the Trust Anchor")
* seeds the **ClawPatrol gateway config** at `~/.clawpatrol/gateway.hcl` from the
  upstream example on first run (download-if-absent; never overwrites local
  edits), applying sensible local defaults (`state_dir` → `~/.clawpatrol`). On a
  TTY (not `--json`) it may prompt for the **dashboard password** and set it via
  `clawpatrol gateway --set-dashboard-password` (it is not an HCL field); the
  password is never logged or written to platform disk

Missing **provider credentials are not a setup hard-fail**: `setup` may prompt
interactively but otherwise proceeds and warns; `ai doctor` flags any absent
credential, and a model call fails (exit `5`) only when that credential is
actually needed (architecture §17, plan §7). Missing **dependencies** (runtime,
virtualization, git, ClawPatrol privileges) do fail fast with exit `3`.

Idempotent:

* safe to re-run (reconciles desired vs. actual state)
* `--upgrade` bumps pinned versions in `config/versions.json` and re-reconciles

---

### ai setup --upgrade

```bash id="c3"
ai setup --upgrade
```

Purpose:

* upgrades platform components
* preserves state and projects

---

# 3. Project Commands

## 3.1 Create Project

```bash id="c4"
ai project create [<name>]
```

`ai project create` sets up a new environment through an **interactive wizard**;
it requires a terminal. Apart from the optional project name (and `--clone`,
which takes a repo URL — a value, not a menu choice), **there are no flags for
the choices** — every selectable option is made in the wizard. In particular
there is **no `--os` flag**: the OS is always picked in the wizard.

Options:

```bash id="c5"
--clone <repo>   # optional: seed the project from an existing git repo
```

### Interactive setup wizard

The wizard walks an ordered set of steps. **Every step** presents a recommended
**default** — selection steps pre-highlight/pre-check it, and **text-input steps
always pre-fill a sensible default** the user can accept or edit. Every step
offers **Back** (return to the previous step, keeping prior answers) and
**Abort** (cancel and exit, making no changes). A step never
advances without a valid choice and never exits or crashes on empty input — it
re-prompts. **Multi-select steps use checkboxes; single-select steps use a
highlighted list.**

**Navigation (all selection steps):**

* **Up / Down arrows** — move the highlight between options
* **Right / Left arrows** — select / deselect the highlighted option
* **Space bar** — toggle (select / deselect) the highlighted option
* **Enter** — confirm the step and advance
* **Back** and **Abort** are always available as selectable controls

Steps, in order:

1. **Name** — only if `<name>` was not given as an argument; text input
   pre-filled with a sensible default (the current directory's basename,
   sanitized to the naming rules in §10) that the user accepts or edits.
2. **OS** — single-select from the supported keys; default `debian-trixie`
   (Slice 1 ships only `debian-trixie`; Slice 5 adds `alma`, `debian-bookworm`,
   `ubuntu`). Selects the template that seeds `.ai-platform/Dockerfile`.
3. **Agent CLIs** — **multi-select checkboxes**; `OpenCode` pre-checked; choose
   any subset of `OpenCode`, `Claude Code`, `Codex`, `Gemini CLI` (at least one).
4. **Default agent CLI** — single-select from the CLIs chosen in step 3; default
   `OpenCode` (recorded as `agent.default_tool`).
5. **Software stacks** — **multi-select checkboxes**; choose the language/tool
   stacks to install into the environment (e.g. `Java`, `Maven`, `Node`, `Deno`,
   `Go`, `Python`, `Rust` — the list is extensible, §25). None pre-checked (a
   project may need nothing beyond the base image); when a repo is cloned
   (`--clone`), the wizard pre-checks stacks it detects. Selected stacks are
   installed into the generated `.ai-platform/Dockerfile` and recorded in
   `profile.yaml`.
6. **Confirm** — shows a summary; choose **Create**, **Back**, or **Abort**.

Prompts render on the terminal (stderr); with `--json` the **final result** is
still the single envelope on stdout (§19). **Abort** exits `0` and makes no
changes (`data.cancelled = true`). A **non-interactive context (no TTY) exits
`2`** with guidance — automation drives the wizard through a pseudo-terminal
(PTY), see acceptance-tests §1.6.

Behavior (after **Confirm**):

* creates the project directory and initializes git (or clones `--clone <repo>`)
* **writes `<project>/.ai-platform/Dockerfile`** by seeding it from the selected
  OS template and adding the selected software stacks (step 5) and agent CLIs
  (step 3) (architecture §25, §12); from then on the project owns that Dockerfile
* **(Slice 2+)** installs the Caveman skill into
  `<project>/.ai-platform/skills/caveman/` (§9); in Slice 1 no context-optimization
  skill is seeded
* writes `project.json`, `config.yaml` (including `agent.tools` and
  `agent.default_tool`), `profile.yaml`, and a `.gitignore` that ignores `run/`
* builds the workspace OCI image from `.ai-platform/Dockerfile` and creates the
  Microsandbox workspace microVM
* records the project in the global index (`config/projects.json`)

---

## 3.3 Project List

```bash id="c7"
ai project list
```

Output:

* project name
* os
* active agents
* workspace status

---

## 3.4 Delete Project

```bash id="c7a"
ai project delete <project>
```

Options:

```bash
--purge          also delete host source at ~/projects/<project>
--yes            skip the interactive confirmation
```

Behavior:

* destroys the project workspace (Microsandbox `rm`)
* removes agent worktrees and **all per-workspace overlays** for the project
* removes the project's entry from the global `config/projects.json` index
* **without `--purge`** (default): preserves host source, including the tracked
  `.ai-platform/` files (`Dockerfile`, `config.yaml`, `profile.yaml`,
  `project.json`); **clears the gitignored `.ai-platform/run/`** (stale
  workspace/agent runtime handles) so no orphaned state remains
* **with `--purge`**: additionally removes `~/projects/<project>` entirely
* destructive: requires interactive confirmation, or `--yes`; refuses and exits
  `2` if neither is present in a non-interactive context
* `--dry-run` lists exactly what would be removed and changes nothing
* secrets are untouched (owned by ClawPatrol)

---

# 4. Workspace Commands

## 4.1 List Workspaces

```bash id="c8"
ai workspace list
```

---

## 4.2 Start Workspace

```bash id="c9"
ai workspace start <project>
```

Behavior:

* starts the Microsandbox workspace microVM
* mounts host project
* injects `AI_PLATFORM_HOST` + service ports, and the S1 egress proxy
  (`HTTPS_PROXY`/`HTTP_PROXY` → ClawPatrol) into the workspace environment
* installs the **ClawPatrol CA root** into the workspace trust store (system
  store + the common per-tool stores, e.g. `NODE_EXTRA_CA_CERTS`,
  `SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`) so HTTPS credential
  injection works (architecture §17)
* initializes tooling

---

## 4.3 Stop Workspace

```bash id="c10"
ai workspace stop <project>
```

Behavior:

* stops the microVM
* preserves state
* does not delete project data

---

## 4.4 Destroy Workspace

```bash id="c11"
ai workspace destroy <project>
```

Behavior:

* deletes the Microsandbox microVM / runtime handle only
* **keeps the persistent overlay** (architecture §26) and the host project
* fully recoverable: `ai workspace start` rebuilds the workspace and re-mounts
  the same overlay
* **non-destructive** — no confirmation needed (nothing the user can't
  reconstruct is lost); see §20

---

## 4.5 Exec In Workspace

```bash id="c11a"
ai workspace exec <project> -- <command> [args...]
```

Behavior:

* runs `<command>` inside the project workspace via Microsandbox and
  streams stdout/stderr
* everything after `--` is passed verbatim to the workspace (no shell expansion
  on the host)
* used by acceptance tests to probe the workspace filesystem/environment

Exit semantics — **platform failure is distinct from inner-command failure**:

* **platform/exec failure** (workspace not running, Microsandbox error, command not
  found in workspace): the `ai` process exits non-zero per §18 (e.g. `4` for a
  runtime failure) and, with `--json`, `ok=false` with the error.
* **inner-command ran**: the `ai` process exits `0` and reports the inner
  command's exit status separately:
  * with `--json`: `ok=true`, and `data.exit_code` / `data.stdout` /
    `data.stderr` carry the inner result (so a non-zero inner exit does **not**
    make `ai` exit non-zero)
  * without `--json`: `ai` propagates the inner command's exit code as its own
    (conventional shell behavior)
* tests that assert on the inner result MUST read `data.exit_code` (with
  `--json`), not the `ai` process exit code

---

# 5. Agent Commands — Removed (not a platform concern)

There are **no `ai agent` commands**. The platform provides one workspace per
project (§4); running multiple AI agents on a project, and any git they need
(branches, worktrees, commits, merges, rebases), is the **in-workspace agent
CLI's** job — not the platform's (architecture §20–22).

---

# 6. Git Commands — Removed

The platform's only git action is `git init`/`git clone` at project creation
(§3.1). It exposes no git commands; all branching, merging, and conflict
resolution happen inside the workspace, driven by the agent (architecture §21).

---

# 7. Environment (Dockerfile)

There are **no snapshot commands** — no `ai snapshot`, no `ai project upgrade`,
no `ai project rollback`. A project's environment is its
`<project>/.ai-platform/Dockerfile` (architecture §25):

* to change the environment, edit `.ai-platform/Dockerfile` and recreate the
  workspace (`ai workspace destroy` then `ai workspace start`, which rebuilds)
* the Dockerfile is git-tracked, so its history *is* the version record
* ad-hoc installs in a running workspace persist via the overlay without
  editing the Dockerfile

---

# 8. Model Commands

## 8.1 Model Status

```bash id="c22"
ai models status
```

Returns:

* LiteLLM health
* provider status
* routing configuration
* Ollama connectivity

---

## 8.2 Model Test

```bash id="c23"
ai models test <model>
```

---

# 9. Context Optimization Commands (Headroom + Caveman)

## 9.1 Context Status

```bash id="c24"
ai context status <project>
```

Returns:

* Headroom input-compression metrics (tokens saved, strategy)
* Caveman output-compression metrics (level, tokens saved)

---

## 9.2 Set Headroom Strategy

```bash id="c25"
ai context strategy <project> <conservative|balanced|aggressive>
```

---

## 9.3 Set Caveman Level

```bash id="c26"
ai context caveman <project> <lite|full|ultra|wenyan>
```

---

# 10. Diagnostics

## 10.1 System Doctor

```bash id="c27"
ai doctor
```

Checks:

* Microsandbox runtime + host virtualization (Apple Silicon / KVM / WSL2 nested-virt)
* Docker/Podman
* LiteLLM
* ClawPatrol (gateway health + the workspace trusts the current ClawPatrol CA — flags drift after a CA rotation; architecture §17)
* Caveman *(from Slice 2)*
* Headroom *(from Slice 2)*

---

## Output

* warnings
* errors
* repair suggestions

---

## 10.2 Service Management

The `ai` CLI is the single control plane for all host services (LiteLLM,
ClawPatrol, Ollama). The user never invokes
`docker compose`, `launchctl`, or `systemctl` directly. The same verbs apply
whether a service runs as a container or a native process (see architecture
§5, "Host Services Control Plane"). (The Microsandbox workspace runtime is not
a long-running service — it is driven by the `ai workspace`
commands, not `ai services`.)

```bash id="c27a"
ai services status                 # health + version of every service
ai services start   [<service>]    # start one or all
ai services stop    [<service>]    # stop one or all
ai services restart [<service>]    # restart one or all
```

`<service>`: `litellm` | `clawpatrol` | `ollama`.

Behavior:

* `status` reports each service's run mode (container | native), health, and
  pinned version; `--json` returns the §19 envelope with a `data.services` array
* lifecycle verbs dispatch to the runtime (container-tier) or the OS service
  manager (native-tier) transparently
* docker compose is not used; container-tier services are managed through the
  runtime abstraction (§6)
* service install/upgrade is handled by `ai setup` /
  `ai setup --upgrade`, not by these verbs
* **Slice phasing:** only `ai services status` is in the Slice 1 surface;
  `start` / `stop` / `restart` land with Slice 6 (full runtime/service
  lifecycle), and are tested from then

---

# 11. Backup — Removed

There is no backup/restore command. It isn't needed (architecture §32): project
source is the user's git repo, platform state is rebuildable via
`ai state repair`, secrets live in ClawPatrol, and installed programs + agent
state persist in the per-workspace overlay (§26 / `ai workspace` lifecycle).

---

# 12. Workspace Diagnostics

## 12.1 Workspace Health

```bash id="c30"
ai workspace doctor <project>
```

---

# 13. Logging Commands

## 13.1 View Logs

```bash id="c31"
ai logs
```

Options:

```bash id="c32"
--workspace <project>
--service <microsandbox|litellm|clawpatrol|ollama>
--tail
```

---

# 14. State Inspection

## 14.1 Show State

```bash id="c33"
ai state show
```

---

## 14.2 Repair State

```bash id="c34"
ai state repair
```

Behavior:

* rebuilds state from filesystem
* reconciles missing entries
* fixes inconsistencies

---

# 15. AI Behavior Rules

## 15.1 No Silent Failure

All commands must:

* return error codes
* log failures

---

## 15.2 AI-Assisted Commands

The platform never makes coding, merge, or agent-reasoning model calls — that is
the in-workspace agent's job. The platform's own model interaction is limited to:

* **context optimization** (Headroom input compression, Caveman output steering,
  including Headroom summarization of context)
* **explicit diagnostic calls** — `ai models test <model>` pings a model to
  verify connectivity/latency

Never for:

* conflict resolution or code merging (delegated to the agent)
* state mutation without confirmation

---

# 16. Security Rules

* no secrets exposed in CLI output
* ClawPatrol is only secret provider
* no environment variable leakage
* no `.env` generation

---

## 16.1 Credential Commands (`ai secrets`)

Credentials are imported into ClawPatrol's store; the platform never writes them
to disk itself (architecture §17). These commands are the only credential entry
points.

```bash id="c37"
ai secrets set <name> [--value <v> | --stdin]   # store a credential in ClawPatrol
ai secrets list                                 # names + metadata only, never values
ai secrets rm <name>                            # remove a credential
ai secrets map <name> --env <ENV_VAR>           # bind a credential to a placeholder env var
```

Behavior:

* `--value` is discouraged (shell history); `--stdin` is preferred and is what
  the acceptance harness uses
* `list` output and all `--json` envelopes contain **names and metadata only** —
  never secret values (exit `5` is returned if a value would otherwise leak)
* `map` records which workspace placeholder env var (e.g. `OPENAI_API_KEY`) the
  gateway swaps for which stored credential
* values are written only to ClawPatrol's SQLite store, never to platform disk

---

# 17. Configuration Flags

Global flags (accepted by every command and subcommand):

```bash id="c35"
--help, -h             show usage for this command/subcommand and exit 0
--json                 machine-readable output (see §19)
--verbose              extra human-readable detail (ignored with --json)
--dry-run              compute and print the planned actions; mutate nothing
--project <name>       scope the command to a project (overrides the default)
--yes                  assume "yes" for destructive confirmation prompts
```

### Project resolution

Project-scoped commands (`workspace *`, `context *`, `project delete`,
`logs`) resolve their target project with this precedence:

1. an explicit project name given as a positional argument;
2. the `--project <name>` flag;
3. otherwise the project that owns the **current working directory**, found by
   walking up until a directory containing `.ai-platform/project.json` is reached
   (so it works from any subfolder).

When none resolve — no name, no flag, and the CWD is not inside a project — the
command exits `2` (invalid input). The positional `<project>` is therefore
optional and written `[project]`.

## 17.0 Help

**Every command and every subcommand must support `--help` / `-h`** — no
exceptions. This includes `ai` itself, every group (`ai project`, `ai workspace`,
`ai context`, `ai services`, `ai secrets`, …), and every leaf
command (`ai project create`, `ai workspace exec`, …).

* `--help` / `-h` prints usage, all flags, arguments, and a one-line
  description, then exits `0`
* `ai` or `ai <group>` invoked with no subcommand prints that level's help and
  exits `0`
* with `--json`, help is emitted as a structured `data.help` object (name,
  summary, args, flags) so tooling can introspect the full command surface
* help text is generated from the command definitions, so it can never drift
  from the actual flags
* a missing `--help` on any command is a spec violation and a release blocker

## 17.1 --dry-run Semantics

`--dry-run` must:

* perform no state mutation, no Microsandbox/runtime calls that change state, no git
  writes, no secret access
* print the ordered list of actions that *would* run
* with `--json`, emit the planned actions in the `data.plan` array
* exit `0` if the plan is valid, `2` if inputs are invalid

---

# 18. Exit Codes

Standardized:

```text id="c36"
0   success
1   general error
2   invalid input
3   missing dependency
4   runtime failure
5   permission denied
```

## 18.1 Per-Command Mapping

| Condition | Code |
|---|---|
| missing Docker/Podman/Microsandbox/LiteLLM/ClawPatrol (`doctor`, `setup`) | 3 |
| unknown project / agent / OS key | 2 |
| Microsandbox or runtime operation failed | 4 |
| rootless required but unavailable | 4 |
| destructive command (`project delete` / `agent remove`) without `--yes`, non-interactive | 2 |
| ClawPatrol denies an action or lacks a credential | 5 |

Every non-zero exit must also emit a structured error (see §19) and log the
failure (§15.1).

---

# 19. JSON Output Schema

With `--json`, every command emits a single JSON object with this envelope:

```json
{
  "ok": true,
  "command": "agent.create",
  "data": { },
  "error": null,
  "warnings": []
}
```

On failure:

```json
{
  "ok": false,
  "command": "agent.create",
  "data": null,
  "error": { "code": 4, "kind": "runtime_failure", "message": "..." },
  "warnings": []
}
```

Rules:

* `error.code` equals the process exit code (§18)
* `data` shape is command-specific and stable per `schema_version`
* secrets are never present in any field (§16)
* `--json` output is the only thing written to stdout; logs/diagnostics go to
  stderr

---

# 20. Destructive-Action Confirmation

The platform has no AI-approval flow (AI merge/conflict resolution was removed —
architecture §22). The only interactive gate is confirmation of **destructive**
actions — those that remove something the user cannot trivially reconstruct:

* `ai project delete` (removes the project record and overlay, and — with
  `--purge` — host source)

`ai workspace destroy` is **not** destructive — it preserves source and overlay
and is fully recoverable (§4.4), so it needs no confirmation.

* interactive runs prompt for confirmation
* `--yes` confirms non-interactively (used by the acceptance harness)
* in a non-interactive context without `--yes`, a destructive command exits `2`
  and changes nothing

---

# 21. Success Criteria

CLI is considered complete when:

* all platform functions are accessible via CLI
* state is fully controllable via commands
* agents can be created and cleaned up
* the project environment is defined by `.ai-platform/Dockerfile`
* system is recoverable via `ai state repair`
* no manual filesystem intervention is required
