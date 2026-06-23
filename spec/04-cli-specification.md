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
* state-aware: global index in `~/.ai-platform/config/projects.yaml`,
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

### Programmatic / external invocation

External programs drive the CLI as a subprocess — `ai <command> [flags] --json` —
and rely on this contract (a future web interface will sit on top of the same
contract; there is no server today):

* Exactly **one JSON envelope** is written to **stdout** per invocation
  (`{ok, command, data, error, warnings}`, §19). Diagnostics, progress, and the
  interactive TUI never touch stdout — they go to stderr and are suppressed under
  `--json`.
* `error.code` **equals** the process exit code (§18), so a caller can branch on
  either.
* Under `--json` the CLI is **fully non-interactive**: it never prompts and never
  renders a TUI. **Every input has a flag (or positional)**, so a command is
  completely specifiable in a single invocation; a missing required input is an
  error (exit `2`), never a prompt.
* `--plain` also disables the TUI (spinners/colour) but keeps human-readable
  (non-JSON) output — for dumb terminals or simple log capture.

---

## 1.4 Cross-Platform Consistency

CLI must behave identically on:

* macOS (Apple Silicon)
* Linux (KVM)

---

## 1.5 Implementation & Distribution

* The `ai` CLI and all platform tooling — installers, state management,
  Microsandbox orchestration, runtime abstraction, and diagnostics — are
  implemented in **Go** and shipped as a **single static binary** per host.
* Host bootstrap uses **thin launchers only** (bash / zsh / PowerShell) whose
  sole job is to download/locate and exec the compiled Go binary. No platform
  logic lives in shell scripts.
* The standard install is **`curl … | bash`** of `installers/install.sh` (a thin
  launcher, no platform logic — it installs only). Uninstall lives in the binary:
  **`ai uninstall`** (§2.2) is a **native, offline** teardown (no network, no
  external script) that **prompts for confirmation**. It **never touches
  `~/projects`**.
* Project templates and agent configuration are **declarative data**
  (YAML / JSON). They are never executable application logic.
* External components (Microsandbox via the `msb` CLI, LiteLLM, Headroom,
  Presidio, Ollama, docker / podman) are invoked as subprocesses or over their
  HTTP APIs — never reimplemented. Version control is out of scope: the platform
  runs no `git`/`gh`.

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

`ai completion <bash|zsh|fish|powershell>` **installs** the completion script into
the shell's standard location and wires it up:

* **bash** → `$XDG_DATA_HOME/bash-completion/completions/ai` (auto-loaded by
  bash-completion)
* **zsh** → `~/.zsh/completions/_ai`, and a managed block is appended to
  `~/.zshrc` (idempotent) to put that dir on `fpath` and run `compinit`
* **fish** → `$XDG_CONFIG_HOME/fish/completions/ai.fish` (auto-loaded)
* **powershell** → a script under the PowerShell config dir, dot-sourced from the
  profile (managed block)

Restart the shell (or `exec zsh`) to activate. `--print` writes the raw script to
stdout instead of installing (for piping / manual setup).

Beyond command and flag-name completion, the CLI provides **dynamic value**
completion:

* `--project` and the optional `[project]` positional → known project names
  (from the global index)
* `ai context strategy` → `conservative|balanced|aggressive`
* `ai context caveman` → `lite|full|ultra|wenyan`
* `ai services console` → services that have an admin console
* `ai logs --service` → the host services; `ai logs --workspace` → project names

## 1.8 Interactive Input

To reduce entry errors, commands **prompt for their inputs on a terminal** rather
than requiring everything on the command line. The rules (implemented once in
`internal/cli/prompt.go` and shared across commands):

* On a terminal (stdin is a real TTY and output is not `--json`) a command
  **always shows its prompt**: a value passed as a positional arg or flag
  **pre-seeds** that prompt (its default selection / pre-filled text), so the user
  confirms or edits it — the value never silently bypasses the TUI. Only under
  `--json` or with no TTY (automation/CI) is a provided value **used directly with
  no prompt**; a *missing* required value is then an error (exit 2) — so scripts
  stay fully non-interactive. **Two exceptions** to the seed-and-prompt rule: a
  **hidden credential value** can't display a seed, so when one is provided
  (`ai secrets set --value/--stdin`) it is used directly even on a TTY (only a
  *missing* value is prompted, hidden); and `ai services` start/stop/restart act
  directly on an explicit name/`all` and only show their multi-select checkbox
  when no service is named (see below).
* **Known option sets are presented, not typed**: single-choice values use a
  select menu (`ai context strategy|caveman`, `ai models test`, `ai completion`,
  the `ai setup` deployment role, `ai network egress`), and "one or more" values
  use a **checkbox** list. `ai services start|stop|restart` with no argument shows
  every service and its current state as checkboxes and acts on the selection
  (an explicit name or `all` skips the prompt; non-interactive use still targets
  all services).
* **Back-navigation**: when a command collects more than one value (e.g. the
  `ai setup` role then a client's server address, or `ai secrets set` name then
  value), all prompts share one form so the user can step **back** to a previous
  prompt before submitting.
* Credentials are entered through a **hidden** prompt and never echoed.

## 2.1 Installation

### ai setup

```bash id="c2"
ai setup [--provider-config <file>] [--mode standalone|server|client] [--server <addr>] [--optional <csv>]
```

`--provider-config <file>` points LiteLLM at a provider/endpoint config (model
aliases → provider URLs). Used both for real provider setup and by the
acceptance harness to target the mock provider.

**Interactive form.** On a TTY, `ai setup` ALWAYS prompts for this host's
configuration in a single back-navigable form — the deployment role, the remote
server address (client only), and the enabled optional host tools — with every
field **pre-seeded** from the `--mode` / `--server` / `--optional` flags (which set
defaults but are **never required**, §21). Known choices are a select / checkbox,
not free text; the free-text server address is validated. Under `--json` / no TTY
the flags drive setup directly with no prompt (automation stays scriptable).

**Deployment role.** `ai setup` supports three roles, chosen on a TTY via the
form's select prompt (pre-seeded by `--mode`), and persisted in
`config/runtime.yaml` (`role:`) so they survive across runs:

* **standalone** (default) — run the full service tier **and** workspaces on this
  host. The shared services bind to **127.0.0.1** (loopback); local microVMs still
  reach them through Microsandbox's host netstack.
* **server** — run **only** the shared service tier here, bound to **0.0.0.0** so
  other machines connect; this host runs no workspaces. Preflight requires only
  the container runtime + a rootless service tier (no microVM runtime /
  virtualization). Setup warns that 0.0.0.0 exposes the services — put TLS in
  front and rely on LiteLLM virtual-key auth on untrusted networks.
* **client** — run **only** workspaces here (no local Docker tier); route to a
  remote server, whose address is prompted (or `--server <addr>`: host, host:port,
  or URL) and stored in `runtime.yaml` (`ai_platform_host`). Preflight requires
  only the microVM runtime + host virtualization (no docker / rootless). The
  local service-tier reconcile is **skipped**; setup warns to verify the server
  with `ai gateway show`.

The chosen `--mode` (else the persisted role, else standalone) drives both the
preflight blocking set and which tier is reconciled.

**Optional host tools.** Beyond the always-on service tier, `ai setup` can enable
opt-in host services (currently **open-webui** and **odysseus**), chosen on a TTY
via a checkbox pre-checked from the persisted/default set and carrying a prominent
**security warning**: these run on the host **outside** the workspace microVM
sandbox with elevated privileges, and **odysseus mounts the host Docker socket**
(`/var/run/docker.sock` — full host-Docker control). `--optional <csv>` seeds the
checkbox on a TTY and drives the set directly under `--json` / no TTY: a
comma-separated list of service names (validated; unknown → exit `2`), or the
sentinel `none` to disable them all. Omitting `--optional` keeps the
persisted/default set (open-webui on first run); the choice persists in
`runtime.yaml` (`optional_services:`).

Purpose:

* installs platform dependencies
* initializes state directory
* configures runtime
* installs, configures, and starts the host services as the single control
  plane — see architecture §5, "Host Services Control Plane". The host service
  set is Ollama (required) + Presidio (analyzer + anonymizer) + LiteLLM
  (+ Postgres) + Headroom — all containers on the `aip-net` network — plus the
  Microsandbox workspace runtime.
* renders each service config from the platform config and verifies the
  Microsandbox runtime + host virtualization (no docker compose; the whole
  service tier runs as containers, so there is no native OS service to register)
* provisions a small **Postgres** (`aip-litellm-db`, `postgres:18.4-alpine3.24`)
  that backs LiteLLM's DB-only features (admin UI login, virtual keys, spend
  tracking — PostgreSQL is the only engine LiteLLM supports for these). It runs
  on a private docker network (`aip-net`) with trust auth (so `DATABASE_URL`
  carries no secret) and a loopback host port `127.0.0.1:5442` (non-default, to
  avoid clashing with other Postgres). This is the one **stateful** service-tier
  container (a named volume); LiteLLM reaches it over the private network
* secures the **LiteLLM admin UI**: the container is launched with `UI_USERNAME`
  (`admin`), `UI_PASSWORD`, and `LITELLM_MASTER_KEY` passed as **env passthrough**
  (values are read from the environment, never inlined in argv, the config, or
  platform disk). On a TTY (not `--json`), if those are not already in the
  environment, `setup` prompts for a UI password, generates a master key, and
  relaunches LiteLLM with both set; the generated master key is shown once. To
  persist secrets across restarts without writing them to disk, export
  `UI_PASSWORD` / `LITELLM_MASTER_KEY` before `setup` / `ai services start`
  (keys-in-LiteLLM, architecture §17). The real **provider** API keys are
  likewise held by the gateway — passed as env passthrough at launch and/or in
  LiteLLM's Postgres-backed store — never written to platform disk

Missing **provider credentials are not a setup hard-fail**: `setup` may prompt
interactively but otherwise proceeds and warns; `ai doctor` flags any absent
credential, and a model call fails (exit `5`) only when that credential is
actually needed (architecture §17, plan §7). Missing **dependencies** (runtime,
virtualization, git) do fail fast with exit `3`.

Idempotent:

* safe to re-run (reconciles desired vs. actual state)
* `--upgrade` bumps pinned versions in `config/versions.yaml` and re-reconciles

---

### ai setup --upgrade

```bash id="c3"
ai setup --upgrade
```

Purpose:

* upgrades platform components
* preserves state and projects

---

## 2.2 Uninstall

### ai uninstall

```bash id="c3b"
ai uninstall [--purge] [--remove-deps]
```

The inverse of install + setup, implemented **natively in the binary** — it runs
entirely offline, with no network call and no external script (§1.5).

Behavior:

* **streams uninstall status/progress** live as each step runs — stopping the
  platform containers (`aip-*`), removing the `ai` binary, removing the
  completion scripts, and stripping the managed PATH/completion lines from the
  shell rc files (leaving the user's own lines intact)
* `--purge` additionally removes the platform state under `~/.ai-platform`
* **asks, per external dependency, whether to also uninstall it** — for `msb`
  (Microsandbox) that is detected on the host, it prompts (on a terminal) before
  removing that tool's install artifacts. Microsandbox ships no uninstaller, so
  removal is the on-disk locations published by its installer, not an invented
  subcommand:
  * **msb** → `$MSB_HOME` (default `~/.microsandbox`) and the `~/.local/bin`
    symlinks (`msb`, `microsandbox`)
* **never touches `~/projects`** (the user's source)
* **writes a transcript to `~/ai-uninstall.log`** — every task it performs is
  appended under a timestamped session header. It lives in the home directory
  (not under `~/.ai-platform`), so it survives `--purge` and remains as a record
  after the binary removes itself
* **quits when finished**: the run blocks on the teardown, prints a completion
  summary, then the process exits (the running binary removes itself; its inode
  survives until exit)

Guards / flags:

* **destructive** — **prompts for confirmation** on a terminal before doing
  anything; `--yes` (§20) skips the prompt. Where there is no terminal to prompt
  on (e.g. `--json` / automation) and `--yes` was not given, it exits `2`
* `--remove-deps` removes every detected external dependency **without
  prompting** (for non-interactive / `--json` use); without it, and with no
  terminal to prompt on, the dependencies are left in place and reported
* `--dry-run` prints the planned steps (including which external dependencies
  it would ask about) and changes nothing
* exit `4` if the teardown fails

Idempotent: safe to re-run after a partial or completed uninstall.

---

# 3. Project Commands

## 3.1 Create Project

```bash id="c4"
ai project create [<name>] [--name <name>] [--os <os>] [--agents <list>] [--stacks <list>]
```

`ai project create` sets up a new environment **in the current working
directory**. On a terminal with no create flags it runs an **interactive
wizard**; for non-interactive use — and for external programs via `--json` —
**every input also has a flag**, so the whole project is specifiable in one
command:

* `--name <name>` (or the `[<name>]` positional) — defaults to the current
  directory's basename
* `--os <os>` — one of `debian-trixie|debian-bookworm|ubuntu|alma`; **required**
  when running non-interactively
* `--agents <list>` — comma-separated agent CLIs
  (`opencode,pi,claude-code,codex,gemini`); defaults to `opencode,pi`, and the
  first listed becomes the default agent CLI
* `--stacks <list>` — comma-separated software stacks
  (`go,node,python,rust,java,maven,deno`); optional

On a terminal (with `--json` off) the wizard **always** runs, **pre-seeded** with
any flags you passed — flags set the defaults rather than bypassing the UI. Under
`--json` or no terminal, the spec is built straight from flags with **no prompt**.
Unknown flag values — or a missing `--os` when non-interactive — exit `2`.
`--dry-run` prints the plan in either mode.

The project lives wherever you run the command — there is no fixed projects
directory. The chosen path is recorded in the global index
(`config/projects.yaml`), and every later command resolves the project **by
name** through that index. With no name given, the name defaults to the current
directory's basename.

**Version control is out of scope.** `ai project create` does **not** init or
clone a git repo — it only writes the `.ai-platform/` environment definition into
the directory and leaves any existing files untouched. Bring your own git
(architecture §21).

**Attach if one already exists.** If the current directory (or any parent) is
already a project, `create` does **not** scaffold a new one — it **attaches** to
that project's workspace instead: it starts the microVM (a no-op if already
running) and opens an interactive login shell inside it. This makes `ai project
create` idempotent per directory.

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
3. **Agent CLIs** — **multi-select checkboxes**; `OpenCode` and `Pi` pre-checked
   (both installed by default); choose any subset of `OpenCode`, `Pi`,
   `Claude Code`, `Codex`, `Gemini CLI` (at least one). All connect to models
   through LiteLLM.
4. **Default agent CLI** — single-select from the CLIs chosen in step 3; default
   `OpenCode` (recorded as `agent.default_tool`).
5. **Software stacks** — **multi-select checkboxes**; choose the language/tool
   stacks to install into the environment (e.g. `Java`, `Maven`, `Node`, `Deno`,
   `Go`, `Python`, `Rust` — the list is extensible, §25). None pre-checked (a
   project may need nothing beyond the base image). Selected stacks are installed
   into the generated `.ai-platform/Dockerfile` and recorded in `profile.yaml`.
6. **Confirm** — shows a summary; choose **Create**, **Back**, or **Abort**.

Prompts render on the terminal (stderr); with `--json` the **final result** is
still the single envelope on stdout (§19). **Abort** exits `0` and makes no
changes (`data.cancelled = true`). A **non-interactive context (no TTY) exits
`2`** with guidance — automation drives the wizard through a pseudo-terminal
(PTY), see acceptance-tests §1.6.

Behavior (after **Confirm**):

* uses the current directory as the project root (no git is run)
* **writes `<project>/.ai-platform/Dockerfile`** by seeding it from the selected
  OS template and adding the selected software stacks (step 5) and agent CLIs
  (step 3) (architecture §25, §12); from then on the project owns that Dockerfile
* **(Slice 2+)** installs the Caveman skill into
  `<project>/.ai-platform/skills/caveman/` (§9); in Slice 1 no context-optimization
  skill is seeded
* writes `project.yaml`, `config.yaml` (including `agent.tools` and
  `agent.default_tool`), `profile.yaml`, and a `.gitignore` that ignores `run/`
* records the project in the global index (`config/projects.yaml`)

`create` is **scaffold-only**: it writes the `.ai-platform/` definition and
registers the project — it does **not** build the workspace OCI image or
create/start the workspace microVM on first creation. The image is built and the
microVM is created on demand by `ai workspace start` (§4.2), and `create` itself
starts/execs a workspace only when run in a directory that is **already** a
project (the attach path above).

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
* removes the project's entry from the global `config/projects.yaml` index
* **without `--purge`** (default): preserves host source, including the tracked
  `.ai-platform/` files (`Dockerfile`, `config.yaml`, `profile.yaml`,
  `project.yaml`); **clears the gitignored `.ai-platform/run/`** (stale
  workspace/agent runtime handles) so no orphaned state remains
* **with `--purge`**: additionally removes `~/projects/<project>` entirely
* destructive: requires interactive confirmation, or `--yes`; refuses and exits
  `2` if neither is present in a non-interactive context
* `--dry-run` lists exactly what would be removed and changes nothing
* provider credentials are untouched (held in the LiteLLM gateway, §16.1)

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
* injects `AI_PLATFORM_HOST` + service ports and the scoped **LiteLLM virtual
  key** into the workspace environment (the workspace holds no provider secret;
  keys-in-LiteLLM, architecture §17)
* applies the project's egress policy (default-deny egress + allow-listed host
  services + published ports, from the `network` block; architecture §29.4) as
  Microsandbox net-rules rendered at workspace create (`egress.MsbNetworkArgs` →
  `msb create`)
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

## 4.3a Restart Workspace

```bash id="c10a"
ai workspace restart <project>
```

Behavior:

* restarts the **existing** microVM — stops it (tolerating an already-stopped
  microVM) then starts it again
* does **not** rebuild the OCI image and does **not** recreate the microVM (the
  persistent overlay and host project are untouched); use `start` for a fresh
  build
* preserves state and refreshes the handle to `started` with a new
  `last_started`
* requires a workspace that was previously started; if none exists it fails with
  exit `2` and directs the user to `ai workspace start` first

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

## 4.6 Lifecycle Shortcuts (`ai start` / `ai stop` / `ai restart`)

```bash id="c11b"
ai start
ai stop
ai restart
```

Top-level convenience shortcuts for `ai workspace start|stop|restart` that
operate on the **current directory's** workspace.

Behavior:

* take **no** arguments — unlike `ai workspace start [project]`, these never
  accept a `[project]` positional or honor `--project`; the target is purely
  cwd-derived
* resolve the target workspace by walking **up** parent directories until a
  **workspace root** is found, defined as a directory that contains a
  `.ai-platform` directory (`<project>/.ai-platform`)
* delegate to the identical workspace lifecycle and emit the **same** result
  envelope as the corresponding subcommand — `ai start` is `ai workspace start`
  for the resolved project, and likewise for `stop`/`restart` (so the envelope
  `command` is `workspace.start` / `workspace.stop` / `workspace.restart`)
* when the cwd is **not** inside any workspace (no ancestor has `.ai-platform`),
  fail with exit `2` (invalid input, §18) and an actionable message directing
  the user to `ai project create` or to `cd` into a project
* all other exit semantics (missing dependency → `3`, runtime failure → `4`,
  restart-without-existing-workspace → `2` per §4.3a) match the delegated
  `ai workspace` subcommand

---

# 5. Agent Commands — Removed (not a platform concern)

There are **no `ai agent` commands**. The platform provides one workspace per
project (§4); running multiple AI agents on a project, and any git they need
(branches, worktrees, commits, merges, rebases), is the **in-workspace agent
CLI's** job — not the platform's (architecture §20–22).

---

# 6. Git Commands — Removed

The platform runs no git at all (§3.1, architecture §21). It exposes no git
commands; init/clone, branching, merging, and conflict resolution are the user's
and the in-workspace agent's job.

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

Returns a labeled, actionable summary (not a raw field dump):

* LiteLLM gateway reachability — including the endpoint URL and, when it is down,
  how to bring it up (`ai services start` → `ai doctor`)
* the default model
* the local-model (Ollama — no key needed) vs cloud-provider (each needs a key
  via `ai secrets set <PROVIDER>_API_KEY`) split
* a `ai models test <model>` next-step hint

The `--json` envelope carries the underlying fields (`healthy`, `providers`,
`default`, `ollama`, `base_url`).

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

The strategy is kept per project (default `balanced`). Because Headroom now runs
as a shared **host** container (not in the workspace), the strategy maps to the
per-request compression knobs (`keep_turns` / `output_buffer_tokens`) sent to the
host Headroom proxy.

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

* Microsandbox runtime + host virtualization (Apple Silicon / KVM)
* Docker/Podman
* LiteLLM (health + that the configured provider keys are present in the gateway — a missing key is warned, not fatal; architecture §17)
* Presidio (analyzer + anonymizer back LiteLLM's always-on PII guardrails; architecture §15)
* Ollama
* Caveman *(from Slice 2)*
* Headroom *(from Slice 2)*

---

## Output

* warnings
* errors
* repair suggestions

---

## 10.2 Service Management

The `ai` CLI is the single control plane for all host services — the platform
containers `ollama`, `presidio`, `litellm`, `headroom`, and the optional `open-webui` chat UI. The user never
invokes `docker compose`, `launchctl`, or `systemctl` directly. The whole service
tier runs as containers (see architecture §5, "Host Services Control Plane").
(The Microsandbox workspace runtime is not a long-running service — it is driven
by the `ai workspace` commands, not `ai services`.)

```bash id="c27a"
ai services status                 # health + version of every service
ai services start   [<service>]    # start one or all
ai services stop    [<service>]    # stop one or all
ai services restart [<service>]    # restart one or all
```

`<service>`: `ollama` | `presidio` | `litellm` | `headroom` | `open-webui` |
`all` (no arg = all).

Behavior:

* `status` reports each service's health and pinned version; `--json` returns the
  §19 envelope with a `data.services` array. It covers the platform
  **containers** — `ollama`, `presidio`, `litellm`, `headroom`, `open-webui` — which are the
  whole service tier
* lifecycle verbs act on the **platform-owned container set** — `ollama`,
  `presidio`, `litellm`, `headroom`, `open-webui` — via the runtime abstraction (§6). With no
  service, or `all`, they act on every container in **dependency order**
* docker compose is not used; container-tier services are managed through the
  runtime abstraction (§6)
* service install/upgrade is handled by `ai setup` / `ai setup --upgrade`, not by
  these verbs
* an unknown service exits `2`. `ai services console` lists/opens a service's
  admin dashboard.

---

# 10a. Network (workspace egress policy)

```bash id="c27b"
ai network show                                  # show the egress policy
ai network egress  [deny|public|unrestricted]    # set the default posture
ai network allow   [host[:port]] [--remove]      # allow/revoke an external destination
ai network publish [host:guest] [--remove]       # publish/unpublish a workspace port
ai network log     [project] [--tail N]          # attempted-egress-by-name audit (host-wide)
```

Project-scoped (default the current directory's project, like `ai context`).
These edit the project's `network` block in `config.yaml` — `network.egress`,
`network.allow_host_services`, `network.publish_ports`. This command manages the
**declaration**; enforcement renders that declaration into Microsandbox net-rules
at workspace create (`egress.MsbNetworkArgs` → `msb create`). The whole policy is
managed by the `ai` app — **no manual file editing is required** (though the
`network` block stays hand-editable). On a terminal, omitting the value
**presents the options**: `egress` (no mode) shows a posture select menu; `allow`
(no arg) shows interactive service presets that pre-fill **both host and port** —
host-local/infra services (Postgres, MySQL, Redis, Kafka, MongoDB, RabbitMQ,
Elasticsearch — host defaults to `gateway`) and common developer domains (npm
`registry.npmjs.org`, PyPI `pypi.org` / `files.pythonhosted.org`, GitHub
`github.com` / `api.github.com` / `raw.githubusercontent.com`, GHCR `ghcr.io` —
all tcp/443) plus a custom `host[:port]` entry; `publish` (no arg) prompts for the
ports. With `--json` or no TTY, the value must be passed as an argument.

* `show` lists the project's **declared** egress policy (egress mode, allowed
  host services, published ports — from `config.yaml`). When the project's
  workspace microVM is **running**, `show` ALSO reads the policy actually **in
  force** on it via `msb inspect <sandbox> --format json` and renders it in a
  separate **"in force (live)"** section: the applied `default_egress`, the
  applied net-rules (one readable line each), and the secrets-broker
  `on_violation` posture. This live view is the applied **policy** — *what would
  be blocked* — **not** a record of blocked connections: msb 0.5.7 exposes only
  the policy config, not per-connection denials. The applied policy is parsed
  from `config.network.policy` (`default_egress` + `rules`, each rule's
  `destination` keyed by `domain` / `domain_suffix` / `cidr` / `ip`, with
  `protocols` + `ports`) and `config.network.secrets.on_violation`. If the
  workspace is **not running** (no sandbox) or `msb` is not installed, `show`
  gracefully prints **only** the declared policy with a note
  *"(workspace not running — showing declared policy only)"* — this is **not** an
  error (exit `0`); only a genuine `msb` runtime failure surfaces (exit `4`). With
  `--json`, the live policy appears under an optional `in_force` field (omitted
  when unavailable).
* `egress` sets the default outbound posture: **deny** (default — only the model
  gateway + allowed services), **public** (open internet, private ranges still
  blocked), **unrestricted**.
* `allow <host[:port]>` adds an allowed host service the workspace may reach — a
  database, Kafka broker, or a specific API/domain. `host` may be a
  hostname/IP/domain, a `*.suffix` wildcard (e.g. `*.npmjs.org`), or the
  `gateway` token for a service on the host machine. The port is **optional**
  and **defaults to 443 (HTTPS)**, so a bare domain like `api.github.com` allows
  it on 443. `--remove` revokes it.
* `publish <host:guest>` publishes a workspace (guest) port to a host port;
  `--remove` undoes it.
* `log [project] [--tail N]` prints the **attempted-egress-by-name audit**: the
  DNS names workspaces tried to resolve, read from the platform's **`aip-dns`**
  resolver (architecture §29.7). Every workspace microVM forwards its DNS to that
  resolver, whose `log` plugin records each query; this command reads
  `docker logs aip-dns` and prints one line per query (the name + record type),
  most-recent last, capped by `--tail` (default 50). The header makes the **v1
  caveats** explicit: it is **host-wide across all workspaces, not attributed per
  project** (the `[project]` argument is accepted for forward compatibility but
  does **not** filter in v1); it is **names only — not connection verdicts and not
  direct-IP egress** (literal-IP traffic never touches DNS); and it is **not
  enforcement** — a name shown here was *resolved*, not necessarily *reached*.
  Enforcement is the msb net-rules in `ai network show`, and under a default-deny
  posture msb may filter a denied name before it reaches the resolver (so denied
  names can be absent). If `aip-dns` is not running (or no container runtime is
  present), `log` prints a clear note and exits `0`; a genuine runtime failure
  reading the log exits `4`.
* invalid mode / port → exit `2`.

Human-readable output by default; `--json` emits the standard §19 envelope.

---

# 10.4 Model Gateway (`ai gateway`)

Configure, machine-wide, which model gateway every workspace microVM on this host
routes through. The address is persisted in `config/runtime.yaml` as
`ai_platform_host` (the same field `ai setup --mode client --server` sets) and is
read at workspace start to derive both the in-VM agent `base_url` and the
always-on egress allow rule (architecture §29.2).

```bash
ai gateway show               # the configured gateway + the URL microVMs will use
ai gateway set <host[:port]>  # route every workspace here through a remote gateway (client mode)
ai gateway clear              # back to the local standalone gateway
```

The address is a **bare host or `host:port`** (NOT a URL). The default port is
`18787` (the Headroom port). Resolution:

* empty → `host.microsandbox.internal:18787` (standalone/local — the in-VM name for
  the host machine);
* `host` → that host on port `18787`;
* `host:port` → that host and port.

microVMs reach the gateway at `http://<host>:<port>/v1` (the `/v1` suffix opencode
and pi require), and the workspace egress policy always allows `<host>:tcp:<port>`.

* `show`/`clear` take no arguments; `set` takes exactly one address.
* a missing `runtime.yaml` (no `ai setup` yet) → exit `3` with a "run `ai setup`
  first" note; an invalid address (empty, whitespace, a scheme/slash, or a
  non-numeric port) → exit `2`; a failed persist/read → exit `4`.

Command names in the envelope are `gateway.show` / `gateway.set` / `gateway.clear`.
Human-readable output by default; `--json` emits the standard §19 envelope.

---

# 11. Backup — Removed

There is no backup/restore command. It isn't needed (architecture §32): project
source is the user's git repo, platform state is rebuildable via
`ai state repair`, provider keys live in the LiteLLM gateway (§16.1), and
installed programs + agent state persist in the per-workspace overlay
(§26 / `ai workspace` lifecycle).

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
--service <microsandbox|ollama|presidio|litellm|headroom|proxy|open-webui|dns>
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
* provider keys live only in the LiteLLM gateway (keys-in-LiteLLM, §16.1)
* no environment variable leakage
* no `.env` generation

---

## 16.1 Credential Commands (`ai secrets`)

Provider credentials live **in the LiteLLM gateway** (keys-in-LiteLLM) — supplied
as env passthrough at launch and/or held in LiteLLM's Postgres-backed store; the
platform never writes the values to its own disk (architecture §17). These
commands are the only credential entry points; they manage the LiteLLM-side
credentials.

```bash id="c37"
ai secrets set <name> [--value <v> | --stdin]   # record a provider credential for the LiteLLM gateway
ai secrets list                                 # names + metadata only, never values
ai secrets rm <name>                            # remove a credential
```

Behavior:

* `--value` is discouraged (shell history); `--stdin` is preferred and is what
  the acceptance harness uses
* `list` output and all `--json` envelopes contain **names and metadata only** —
  never secret values (exit `5` is returned if a value would otherwise leak)
* credentials are held by the LiteLLM gateway, never written to platform disk; the
  workspace agent receives only a scoped LiteLLM **virtual key**, never a provider
  secret

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
   walking up until a directory containing `.ai-platform/project.yaml` is reached
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
| missing Docker/Podman/Microsandbox/LiteLLM (`doctor`, `setup`) | 3 |
| unknown project / agent / OS key | 2 |
| Microsandbox or runtime operation failed | 4 |
| rootless required but unavailable | 4 |
| destructive command (`project delete` / `agent remove`) without `--yes`, non-interactive | 2 |
| a required provider credential is missing in the LiteLLM gateway at request time | 5 |

Every non-zero exit must also emit a structured error (see §19) and log the
failure (§15.1).

## 18.2 Usage Errors Are Human-Helpful

A **usage error** (wrong number of arguments, unknown flag, unknown command —
all exit `2`) must never be a bare parser message. For **every** command it:

* states the problem in plain language (e.g. "needs 1 argument(s), received 0",
  not "accepts 1 arg(s), received 0"); and
* shows the failing command's **own help** — its usage line and flags — so the
  user can immediately see how to invoke it correctly.

In human output the friendly message is printed, then the command's full help.
With `--json` the envelope's `error.message` carries the friendly message plus
the one-line usage (`(usage: …)`). This is handled centrally so it holds for all
commands uniformly.

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
