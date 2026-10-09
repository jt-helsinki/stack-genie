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

* macOS
* Linux

Workspaces are sandboxed by **nono** (https://nono.sh — Landlock LSM on Linux,
Seatbelt on macOS), which has no virtualization gate and no architecture
restriction: any host the `nono` binary runs on can run workspaces. (The
optional host-native **omlx** local-inference backend is separately
macOS-Apple-Silicon-only — that restriction is about local model inference, not
workspace sandboxing; see §10.2.)

---

## 1.5 Implementation & Distribution

* The `ai` CLI and all platform tooling — installers, state management, the
  `nono` process-sandbox wrapper, and diagnostics — are implemented in **Go** and
  shipped as a **single self-contained binary** per host.
* Host bootstrap uses **thin launchers only** (bash / zsh) whose
  sole job is to download/locate and exec the compiled Go binary. No platform
  logic lives in shell scripts.
* The standard install is **`curl … | bash`** of `installers/install.sh` (a thin
  launcher, no platform logic — it installs only). Uninstall lives in the binary:
  **`ai uninstall`** (§2.2) is a **native, offline** teardown (no network, no
  external script) that **prompts for confirmation**. It **never touches
  `~/projects`**.
* Project configuration is **declarative data** (YAML / JSON). It is never
  executable application logic.
* External components (`nono`, LiteLLM, Headroom, Presidio, omlx, docker /
  podman) are invoked as subprocesses or over their HTTP APIs — never
  reimplemented. Version control is largely out of scope: the CLI exposes no git
  commands and never runs `gh`; the only git it runs is an internal `git init` at
  workspace start (to seat the Graphify hook — §6), never clone/remote.

---

## 1.6 Binary & Command Naming

There is **one binary**, `ai`, installed on `PATH`. There are **no hyphenated
command names and no symlinks**, and essentially no command aliases — the only
one is the documented `destroy` alias of `ai delete` (§4.4). Every command
follows a single, consistent pattern:

```text
ai <noun> [<verb>] [args] [flags]
```

Examples: `ai setup`, `ai create`, `ai delete`, `ai start`, `ai agent`,
`ai context status`. Workspace verbs are **top-level** (`ai create`,
`ai start`, `ai agent`, …); grouped commands like `ai context status` /
`ai services status` keep the `ai <noun> <verb>` form.

Rules:

* one verb grammar everywhere — no `setup-ai-platform` / `create-ai-project`
  style names
* every command and subcommand supports `--help` / `-h` (§17.0)
* `ai <unknown>` exits `2`

## 1.7 Shell Completion

`ai completion <bash|zsh>` **installs** the completion script into
the shell's standard location and wires it up:

* **bash** → `$XDG_DATA_HOME/bash-completion/completions/ai` (auto-loaded by
  bash-completion)
* **zsh** → `~/.zsh/completions/_ai`, and a managed block is appended to
  `~/.zshrc` (idempotent) to put that dir on `fpath` and run `compinit`

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
* `ai theme` → the available theme names

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
  (`ai keys add --value/--stdin`) it is used directly even on a TTY (only a
  *missing* value is prompted, hidden); and `ai services` start/stop/restart act
  directly on an explicit name/`all` and only show their multi-select checkbox
  when no service is named (see below).
* **Known option sets are presented, not typed**: single-choice values use a
  select menu (`ai context strategy|caveman`, `ai models test`, `ai completion`,
  the `ai setup` deployment role, `ai network egress`, `ai theme`), and "one or more" values
  use a **checkbox** list. `ai services start|stop|restart` with no argument shows
  every service and its current state as checkboxes and acts on the selection
  (an explicit name or `all` skips the prompt; non-interactive use still targets
  all services).
* **Back-navigation**: when a command collects more than one value (e.g. the
  `ai setup` role then a client's server address), all prompts share one form so
  the user can step **back** to a previous
  prompt before submitting.
* Credentials are entered through a **hidden** prompt and never echoed.

## 2.1 Installation

### ai setup

```bash id="c2"
ai setup [--provider-config <file>] [--mode standalone|server|client] [--server <addr>] [--optional <csv>] [--guardrails <csv>] [--host-agents <csv>]
```

`--provider-config <file>` points LiteLLM at a provider/endpoint config (model
aliases → provider URLs). Used both for real provider setup and by the
acceptance harness to target the mock provider.

**Interactive form.** On a TTY, `ai setup` ALWAYS prompts for this host's
configuration in a single back-navigable form — the deployment role and the remote
server address (client only) — with every
field **pre-seeded** from the `--mode` / `--server` flags (which set
defaults but are **never required**, §21). Known choices are a select / checkbox,
not free text; the free-text server address is validated. Under `--json` / no TTY
the flags drive setup directly with no prompt (automation stays scriptable).

**Deployment role.** `ai setup` supports three roles, chosen on a TTY via the
form's select prompt (pre-seeded by `--mode`), and persisted in
`config/runtime.yaml` (`role:`) so they survive across runs:

* **standalone** (default) — run the full service tier **and** workspaces on this
  host. The shared services bind to **127.0.0.1** (loopback); workspace agent CLIs
  run directly on this same host, so they reach the services over plain loopback
  — there is no guest-network translation.
* **server** — run **only** the shared service tier here, bound to **0.0.0.0** so
  other machines connect; this host runs no workspaces. Preflight requires only
  the container runtime + a rootless service tier (no workspace-sandbox runtime
  needed). Setup warns that 0.0.0.0 exposes the services — put TLS in front and
  rely on LiteLLM virtual-key auth on untrusted networks.
* **client** — run **only** workspaces here (no local Docker tier); route to a
  remote server, whose address is prompted (or `--server <addr>`: host, host:port,
  or URL) and stored in `runtime.yaml` (`ai_platform_host`). Preflight requires
  only the `nono` sandboxing CLI (no docker / rootless). The local service-tier
  reconcile is **skipped**; setup warns to verify the server with
  `ai gateway show`.

The chosen `--mode` (else the persisted role, else standalone) drives both the
preflight blocking set and which tier is reconciled.

**Optional host tools.** The optional host-service set is currently **empty** —
there are no opt-in host services. The `--optional <csv>` flag is retained for
forward compatibility but accepts only the sentinel `none` (a safe no-op); any
other value is unknown → exit `2`. Omitting `--optional` keeps the empty set.

**Workspace sandboxing runtime (`nono`) and host agent CLIs.** For the
**standalone** and **client** roles (both run workspaces; `server` does not),
`ai setup` best-effort installs the **`nono`** CLI (https://nono.sh —
Landlock/Seatbelt kernel-capability sandboxing; `setup.EnsureNonoInstalled`) when
it is not already on `PATH` (`brew install nono`, else nono's own
`curl -fsSL https://nono.sh/install.sh | sh`), Detect-gated so a present install
is left untouched and a failed/offline install never fails `ai setup` (a manual
re-run retries it). **`--host-agents <csv>`** additionally installs any of
`claude-code`/`codex`/`opencode` directly onto this host via each CLI's own
official installer (`https://claude.ai/install.sh`, `https://chatgpt.com/codex/install.sh`,
`https://opencode.ai/install`) — these are run **directly on the host** (there is
no VM/image to bake them into); already-present binaries are left untouched.
On a TTY both of these are offered as part of the same setup form (a multi-select
for the host agent CLIs, shown for standalone/client, hidden for server); under
`--json`/no-TTY, `--host-agents` drives it directly (a comma-separated subset, or
`none`/omitted for no host-agent install — an unknown name exits `2`). Every
agent CLI still routes through the LiteLLM gateway at `ai agent`/`ai start` time
regardless of whether it was installed here or already present on `PATH`.

**Guardrails.** The LiteLLM gateway guardrails are **user-selectable** at setup.
On a TTY (standalone/server) the form presents a multi-select checkbox; under
`--json` / no TTY the `--guardrails <csv>` flag drives it. The four options are:

* `headroom` — "Input compression (Headroom)" — **default on** (the only
  default-on guardrail out of the box)
* `secret-masking` — "Secret masking (Presidio)" — opt-in; expands to the Presidio
  input (`pre_call`) + output (`post_call`) pair and gates the Presidio containers
* `hide-secrets` — "API-key / token detection (detect-secrets)" — opt-in
* `tool-firewall` — "Destructive-command tool firewall" — opt-in

`--guardrails` takes a comma-separated subset of those keys, or the literal `none`
to disable all. On a TTY the flag **seeds** the checkbox; the default is Headroom
only; an unknown name exits `2`. The selection is persisted machine-wide in
`runtime.yaml` (`guardrails:`); an absent field means the default (Headroom only),
a present set — including an explicit empty one — is honoured verbatim. Only the
enabled guardrails are rendered into LiteLLM's config; an unselected guardrail is
omitted entirely (never referencing a backend that isn't running). Each rendered
guardrail is `default_on` (active on every request, and since every route —
including cloud — traverses the proxy, a client cannot opt out of an **enabled**
guardrail); the security guardrails (secret-masking, hide-secrets, tool-firewall)
are opt-in (architecture §15/§17).

Purpose:

* installs platform dependencies
* initializes state directory
* configures runtime
* installs, configures, and starts the host services as the single control
  plane — see architecture §5, "Host Services Control Plane". The container service
  set is the `aip-dns` CoreDNS egress-audit resolver +
  Presidio (analyzer + anonymizer, reconciled only when the `secret-masking`
  guardrail is selected) + Valkey (+ RedisInsight) + Headroom + LiteLLM
  (+ Postgres) + the `aip-proxy` nginx gateway (the SOLE host entry, reconciled
  LAST) — all containers on the `aip-net` network, reconciled in order network →
  DNS → Presidio → Valkey(+RedisInsight) → Headroom → LiteLLM(+DB) →
  proxy (Headroom PRECEDES LiteLLM because LiteLLM's `headroom` compression
  guardrail calls it in-process). The **local-model inference tier is HOST-NATIVE,
  not a container**: **omlx** is the sole local-inference runtime — ONE shared
  `omlx serve --model-dir <dir> --port 8100` process (a fixed loopback port,
  `services.OmlxPort`) serving every locally-served model it finds by scanning its
  own model directory (`~/.ai-platform/volumes/models/omlx`), probed at
  `127.0.0.1:8100/v1`; the local inference tier never runs as an `aip-*` container,
  and there is no per-model process/port (unlike the retired vLLM design). Setup
  also best-effort installs **omlx** (`ensureOmlxInstalled`) into the platform
  venv, starts the server, syncs LiteLLM's `omlx/*` registrations against the
  server's own live `GET /v1/models` (`SyncOmlxModels` — automatic, no user-facing
  sync command needed at setup time; see §8.3), and (standalone/client only)
  best-effort installs the **`nono`** workspace-sandboxing CLI (see above).
  *(hardware bring-up: the real `omlx serve` fork and the live LiteLLM sync
  round-trip are exercised only on a provisioned host.)* Models themselves are
  downloaded/added/removed/tuned entirely through **omlx's own admin panel**
  (`ai services console omlx`, §10.2) — this platform has no
  `ai models pull`/`rm`/`configure` equivalent.
* renders each service config from the platform config (no docker compose; the
  container service tier runs as containers while the local-model tier is
  host-native, so there is no native OS service to register)
* provisions a small **Postgres** (`aip-litellm-db`, `postgres:18.4-alpine3.23`)
  that backs LiteLLM's DB-only features (admin UI login, virtual keys, spend
  tracking — PostgreSQL is the only engine LiteLLM supports for these). It runs
  on a private docker network (`aip-net`) with trust auth (so `DATABASE_URL`
  carries no secret) and is INTERNAL-ONLY — no host port; LiteLLM reaches it by
  name at `aip-litellm-db:5432`. This is the one **stateful** service-tier
  container (a named volume); LiteLLM reaches it over the private network
* secures the **LiteLLM admin UI**: the container is launched with `UI_USERNAME`
  (`admin`), `UI_PASSWORD`, and `LITELLM_MASTER_KEY` passed as **env passthrough**
  (values are read from the environment, never inlined in argv, the config, or
  platform disk). The prompt follows the **role-based UI-auth policy**
  (`runtime.RequireUIAuth`, architecture §17): **standalone/client** are open and
  `setup` does NOT prompt (a user opts into a password later with
  `ai litellm password`); **server** REQUIRES one — `setup` loops on a TTY until a
  non-empty password is entered and generates a strong random one
  non-interactively, so a network-exposed gateway is never left open. The existing
  short-circuits still apply (already secured / `UI_PASSWORD`+`LITELLM_MASTER_KEY`
  already in the env). The generated master key is shown once. To persist the
  secrets across restarts, `setup` OFFERS (on a TTY) to save them to
  **`~/.ai-platform/.ai-platform.env`** (a 0600 file the `ai` CLI auto-loads at startup —
  existing env wins); on decline / non-TTY the manual `export UI_PASSWORD … /
  LITELLM_MASTER_KEY …` block is printed instead (keys-in-LiteLLM, architecture
  §17). The real **provider** API keys are
  likewise held by the gateway — passed as env passthrough at launch and/or in
  LiteLLM's Postgres-backed store — never written to platform disk

**Platform base domain & name resolution.** The host UIs are served on nginx
subdomains of the platform base domain (`litellm.<domain>` → LiteLLM admin UI and
`valkey.<domain>` → RedisInsight, both on the single gateway port `:18787`;
`ai domain`, §10.6). In
**server** mode the form additionally prompts for the **server hostname / domain**
(the name clients and browsers reach this host at, default **`localhost`**),
persisted as the `domain`. Once the domain is known, `setup` wires name
resolution best-effort:

* **standalone** — offers (on a TTY, with consent) to write a managed block to
  `/etc/hosts` mapping the UI subdomain (`litellm.<domain>`) to `127.0.0.1`. The write needs root, so
  it shells out through a **labeled sudo prompt** (`[ai] enter your login password
  to update /etc/hosts:`); on decline / non-TTY it prints the manual block instead.
* **server** — does **not** edit `/etc/hosts`; it prints the **DNS/TLS operator
  contract** (create `*.<domain>` or per-host `litellm.<domain>`
  records → this server's IP, terminate TLS at nginx).
* **client** — nothing (no local UIs).

Missing **provider credentials are not a setup hard-fail**: `setup` does **not**
prompt for cloud-provider API keys — add those anytime from `ai ui` (the API Keys
tab) or `ai keys add <provider>`. Setup only runs the initial catalog → gateway
model sync (reflecting any already-keyed providers + the models omlx is currently
serving);
`ai doctor` flags any absent credential, and a model call fails (exit `5`) only
when that credential is actually needed (architecture §17, plan §7). Missing
**dependencies** fail fast: a missing container runtime or missing `nono` exits
`3` (`internal/setup/setup.go` — `installablePrograms` = container runtime +
`nono` runtime), while an unmet host capability (rootless posture for the
service tier) exits `4`. git is **not** a prerequisite — the only git the
platform runs is an internal `git init` at workspace start.

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
ai uninstall [--purge] [--remove-deps] [--keep-runtimes]
```

The inverse of install + setup, implemented **natively in the binary** — it runs
entirely offline, with no network call and no external script (§1.5).

Behavior:

* **streams uninstall status/progress** live as each step runs — stopping any
  running workspace microVMs (`msb stop -f` each `aip-*` sandbox, **preserving all
  workspace DATA** — project source and `/persist` overlays are host bind mounts,
  never deleted; the VM instance is only halted, never `msb delete`'d), stopping and
  removing the platform containers (`aip-*`), **removing all platform container
  images** (the pinned service-tier images plus every `aip-*` image, so nothing is
  left on the host — done on EVERY uninstall, not only `--purge`), removing the `ai` binary,
  removing the completion scripts, and stripping the managed PATH/completion lines
  from the shell rc files (leaving the user's own lines intact)
* **also removes the host-native omlx runtime by default** (best-effort: stops the
  one shared `omlx serve` process and uninstalls the runtime itself) — on a
  terminal this is a confirm prompt ("Remove the host-native omlx runtime?",
  default **yes**, seeded by `--keep-runtimes`); under `--json`/no-TTY it removes
  the runtime unless `--keep-runtimes` is passed. The **downloaded models are
  preserved either way** — only `--purge` deletes them (see below)
* **removes the platform state under `~/.ai-platform` but KEEPS the downloaded
  model store (`volumes/models`)** — the one expensive-to-refetch piece a user
  usually wants to keep across a reinstall; the local omlx model store
  (`volumes/models/omlx`) is preserved on a non-purge uninstall. On a terminal
  whether to ALSO remove the downloaded models is its own confirm prompt (seeded by
  `--purge`); under `--json`/no-TTY, `--purge` removes `~/.ai-platform` in full,
  including the downloaded models
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
* `--keep-runtimes` keeps the host-native omlx runtime installed instead of
  removing it (the default is to remove it); the downloaded models are kept
  either way unless `--purge` is also given
* `--dry-run` prints the planned steps (including which external dependencies
  it would ask about) and changes nothing
* exit `4` if the teardown fails

Idempotent: safe to re-run after a partial or completed uninstall.

---

# 3. Workspace Commands (create / list / delete)

> **Flat surface.** The commands are the **top-level verbs** below
> (`ai create` / `ai list` / `ai delete`). There are **no** `ai project …`
> command groups or aliases. The envelope `command` keys are unchanged
> (`project.create` / `project.list` / `project.delete`). The user-facing noun is
> **workspace** everywhere.

## 3.1 Create Workspace

```bash id="c4"
ai create [<name>] [--name <name>] [--agents <list>] [--auth-mode <cli=mode>]
          [--shell <bash|zsh>] [--graphify-model <ref>] [--tools <list>] [--location <dir>]
```

`ai create` registers a new workspace at the chosen **location** (default: the
current working directory). A workspace is a plain **host directory** — there is
no VM, no OS image, and nothing to boot: `ai create` just scaffolds
`<location>/.ai-platform/{config.yaml,project.yaml,.gitignore}` and records the
project in the global index, which finishes in milliseconds. Agent CLIs run
**directly on the host**, sandboxed per-invocation by **nono**
(https://nono.sh — Landlock on Linux, Seatbelt on macOS) the first time
`ai agent`/`ai shell`/`ai start` launches a session in it — there is nothing to
download or build at create time.

On a terminal with no create flags it runs an **interactive wizard**; for
non-interactive use — and for external programs via `--json` — **every input
also has a flag**, so the whole workspace is specifiable in one command:

* `--name <name>` (or the `[<name>]` positional) — defaults to the location
  directory's basename
* `--agents <list>` — comma-separated agent CLIs
  (`opencode,omp,claude-code,codex,gemini,hermes`); defaults to
  `opencode`, and the first listed becomes the default agent CLI. `opencode`/
  `omp`/`hermes` are always gateway/api-key; `claude-code`/`codex`/`gemini`
  are gateway by default but OAuth-selectable (see `--auth-mode`). Each selected
  CLI must be installed on the host (its own binary on `PATH` — see
  `ai setup --host-agents`, §2.1) before a session using it can launch; an
  unrecognized name exits `2`
* `--auth-mode <cli=mode>` — per-agent auth mode, comma-separated, for
  the OAuth-capable CLIs `claude-code`/`codex`/`gemini` (`api-key`|`oauth`;
  default `api-key`). `api-key` routes through the gateway with the scoped virtual
  key (firewall + masking apply); `oauth` uses the CLI's own subscription login,
  talking directly to the provider and bypassing the gateway. opencode/omp/hermes
  are always gateway/api-key and never accept it
* `--shell <shell>` — the workspace's default interactive shell for `ai shell`,
  `bash|zsh` (default `bash`), written to `workspace.shell`
* `--graphify-model <ref>` — the **name of a model omlx is already serving**, for
  Graphify's headless LLM backend; optional (blank/`"(none — leave Graphify
  unconfigured)"` = none, the default). There is no download at create
  time and no curated Hugging-Face-repo-id list (retired with the old vLLM design);
  model management lives entirely in omlx's own admin panel (`ai services console
  omlx`, §10.2). On a terminal the wizard shows a **SELECT** (not a free-text field)
  built from omlx's live `GET /v1/models` — a "none" option always first, shown alone
  when omlx has nothing loaded/isn't running (never blocks the wizard) — and a
  pre-seeded `--graphify-model` value omlx doesn't currently report is appended to the
  picker's options rather than dropped; the chosen name is stored as
  `agent.graphify_model` and routed through the gateway as `omlx/<name>` the next
  time an agent session launches (architecture §17). The `--graphify-model` flag's
  own `--help` text matches this picker behavior.
* `--tools <list>` — the per-project **AI tools** to install, a comma-separated
  multi-select (like `--agents`) chosen from `caveman`, `graphify`,
  `code-review-graph`, `codebase-memory-mcp`, `openviking`, `tokensave`,
  `agentmemory`, `ponytail`, `agent-skills` (`create.SupportedAITools()`).
  Default (flag omitted): **none** — every AI tool is opt-in and must be named
  explicitly; `--tools=""` behaves the same as omitting the flag. An unknown value
  exits `2`. Each maps to a `context.<tool>_enabled` bool, persisted at create and
  changeable later with `ai tools enable|disable` (§4.5e); each tool installs
  directly on the host (plain `os/exec`, no Dockerfile/image) the next time
  `ai agent`/`ai shell`/`ai start` launches a session, not at `ai create` time.
  One-line descriptions (`ai tools list`, §4.5e):
  * `caveman` — compresses agent output into a terser style to cut tokens.
  * `graphify` — builds a knowledge graph of the codebase for faster, cheaper
    code navigation and queries.
  * `code-review-graph` — code-review knowledge-graph MCP server; generates a
    visual D3 dependency graph.
  * `codebase-memory-mcp` — persistent codebase-memory MCP server, with an
    optional 3D graph UI.
  * `openviking` — local context database server for agents (openviking.ai).
  * `tokensave` — code-graph MCP server for 34 languages — semantic queries
    instead of grepping/reading files, cutting token use.
  * `agentmemory` — persistent cross-session memory — captures and recalls
    prior work so agents stop re-learning the codebase.
  * `ponytail` — nags the agent to check YAGNI/stdlib/existing code before
    writing anything new (an anti-over-engineering skill).
  * `agent-skills` — curated senior-engineer skills (spec, TDD, review,
    security, shipping, …) from github.com/addyosmani/agent-skills.
* `--location <dir>` — the workspace directory (default: cwd). **Created if it does
  not exist.** It must **not** be — or be nested inside — an existing workspace
  (a directory with a `.ai-platform/project.yaml` at it or any ancestor); otherwise
  create exits `2`. In the wizard the location field offers **path autocompletion**;
  the flag offers shell directory completion.

On a terminal (with `--json` off) the wizard **always** runs, **pre-seeded** with
any flags you passed — flags set the defaults rather than bypassing the UI. Under
`--json` or no terminal, the spec is built straight from flags with **no prompt**;
there is no required flag (`--agents` defaults to `opencode`), so a bare
`ai create --json` is valid. Unknown flag values, an unrecognized agent/tool/CLI
name, or a location nested in an existing workspace all exit `2`. `--dry-run`
prints the plan in either mode.

The workspace lives at the location you choose — there is no fixed projects
directory. The chosen path is recorded in the global index
(`config/projects.yaml`), and every later command resolves the workspace **by
name** through that index. With no name given, the name defaults to the location
directory's basename.

**Git.** The `ai create` command itself does **not** init or clone a git repo — it
only writes the `.ai-platform/` state and leaves existing files untouched.
However, at every **agent/shell session launch** the platform runs `git init` in
the project dir when it is not already a git repo (right before Graphify
registration, so the Graphify git hook always has a repo to attach to); this
writes `.git` straight into the project directory on the host (there is no guest
filesystem to translate through). Cloning remains out of scope — bring your own
remote.

**Attach if one already exists.** If the current directory (or any parent) is
already a workspace, `create` does **not** scaffold a new one — it **attaches** to
that workspace instead: on a terminal it opens an interactive sandboxed shell in
it (equivalent to `ai shell`); under `--json`/no-TTY it reports
`{"project": <name>, "attached": false}` without launching anything. This makes
`ai create` idempotent per directory.

### Interactive setup wizard

The wizard walks an ordered set of back-navigable groups, sharing **one** `huh`
form so the user can step back between them (§1.8). **Every step** presents a
recommended **default** — select steps pre-highlight it, text-input steps
pre-fill it — that the user can accept or edit; a step never advances on an
invalid value.

Groups, in order:

1. **Name, location, and default shell** — one group with three fields:
   **Workspace name** (text input, validated against the naming rules, §10),
   **Location** (text input with path autocompletion — `SuggestionsFunc` —
   created if missing, must not be/nest inside an existing workspace), and
   **Default interactive shell** (single-select `bash`|`zsh`, default `bash`,
   recorded as `workspace.shell`). If the name is left at its auto-derived
   default while the user changes location, the name re-derives from the new
   directory's basename.
2. **Agent CLIs** — multi-select checkbox (space to toggle), at least one
   required; `opencode` pre-checked. Installed directly on this host, wired to
   the LiteLLM gateway.
3. **Default agent CLI** — single-select from the CLIs chosen in step 2
   (recorded as `agent.default_tool`).
4. **Per-agent auth mode** *(one single-select group per OAuth-capable CLI
   selected in step 2 — `claude-code`/`codex`/`gemini`, in that order — each
   hidden unless that CLI was actually chosen; skipped entirely if none of the
   three was selected)* — `api-key` (default — routes through the gateway with
   the scoped virtual key, so the tool firewall + secret masking apply) or
   `oauth` (the CLI's own subscription login, talking directly to the provider
   and bypassing the gateway's guardrails). `opencode`/`omp`/`hermes` are
   always gateway/api-key and never shown this step.
5. **AI tools** — multi-select checkboxes (like the agent-CLI list, not a
   screen each): `caveman`, `graphify`, `code-review-graph`,
   `codebase-memory-mcp`, `openviking`, `tokensave`, `agentmemory`, `ponytail`,
   `agent-skills` — each labeled with its one-line description. **None
   pre-checked** — every AI tool is opt-in; pre-seeded from `--tools`. Each is
   recorded as a `context.<tool>_enabled` bool.
6. **Graphify model** *(shown only when `graphify` is checked in step 5)* — an
   optional **SELECT** built from omlx's live `GET /v1/models`: a
   `"(none — leave Graphify unconfigured)"` option always first (shown alone
   when omlx has nothing loaded/isn't running — never blocks the wizard), then
   every model id omlx currently reports; a pre-seeded `--graphify-model` value
   omlx doesn't currently report is appended rather than dropped. Nothing is
   downloaded at create time regardless of what is picked — model management
   lives entirely in omlx's own admin panel (`ai services console omlx`). The
   value is stored as `agent.graphify_model` and routed through the gateway as
   `omlx/<name>`.

There is no separate confirm step and no resources/ports/OS/software-stack
steps — completing the last applicable group (Enter) creates the workspace;
**esc** at any point cancels (`huh.ErrUserAborted`).

Prompts render on the terminal; with `--json` the **final result** is still the
single envelope on stdout (§19). Cancelling exits `0` and makes no changes
(`data.cancelled = true`). A **non-interactive context (no TTY) builds the spec
straight from flags with no prompt** (matching `--json`) — automation can instead
drive the wizard through a pseudo-terminal (PTY), see acceptance-tests §1.6.

Behavior (on completion, `internal/create.Execute` — shared verbatim with the
`ai ui` in-TUI wizard so both paths behave identically):

* validates the spec (name, location-not-nested) and creates the location
  directory if it does not exist
* writes `config.yaml` (`agent.tools`, `agent.default_tool`,
  `agent.auth_modes`, `agent.graphify_model`, `context.<tool>_enabled` for each
  chosen AI tool, `workspace.shell`), `project.yaml` (name, the **host** OS this
  workspace was created on, creation timestamp), and a `.gitignore` that ignores
  `run/`
* seeds the context-optimization defaults (Headroom strategy `balanced`,
  Caveman level `full`, §9)
* records the project in the global index (`config/projects.yaml`)
* emits a warning for each OAuth-mode agent: it bypasses the gateway, so the
  tool firewall, secret masking, and content-level egress audit do not apply to
  it

`create` is **scaffold-only**: it writes the `.ai-platform/` state and registers
the project — there is no image to build and nothing to pull. The per-CLI agent
configs (`.opencode/opencode.json`, etc.), the nono sandbox profile, and each
selected AI tool's host install all happen lazily, the first time
`ai agent`/`ai shell`/`ai start` launches a session in the workspace (§4.5b).

---

## 3.3 List Workspaces

```bash id="c7"
ai list
```

Human output renders one row per workspace with `NAME` / `AGENTS` / `STATUS`
(em-dash for an empty cell). `--json` carries the full `project.Entry` per
workspace: `name`, `path`, `os` (the **host** OS the workspace was created on —
`darwin`/`linux`; there is no OS image to select, §3.1), `agents`, `status`
(the recorded workspace lifecycle state, or `"none"` if never started), `id`,
`created`, and `last_started`.

---

## 3.4 Delete Workspace

```bash id="c7a"
ai delete [<name>]            # `ai destroy` is an ALIAS of this
ai delete [<name>] --purge
```

`ai delete` (with `ai destroy` as its cobra alias) is the single removal verb. With
no `[<name>]` it targets the workspace that owns the current directory (or
`--project`). The **full, consolidated behavior — including the `--purge` and
`--keep-agent-config` prompts — is specified in §4.4**; the summary is: a plain
delete stops any live `nono` sessions for the workspace, revokes its scoped
gateway credential from the OS keystore, removes the project's `.ai-platform`
tree, and de-registers it, keeping the user's OTHER files; `--purge` removes the
whole project directory. Provider API keys themselves are untouched (held in the
LiteLLM gateway, §16.1) — only this workspace's scoped virtual key is revoked.

---

# 4. Workspace Session Commands (start / stop / restart / delete / shell / agent / attach / sessions / …)

> **Flat surface.** The commands are the **top-level verbs** below
> (`ai start` / `ai stop` / `ai restart` / `ai delete` / `ai shell` / `ai agent` /
> `ai attach` / `ai sessions` / `ai tools` / `ai doctor`). Each takes an
> **optional `[name]`** positional that defaults to the workspace owning the
> current directory (then `--project`). There are **no** `ai workspace …` command
> groups or aliases. The envelope `command` keys are unchanged (`workspace.*`).
> There is no separate per-workspace doctor: the single top-level `ai doctor`
> reports platform health and **additionally** the workspace-runtime section when
> run inside a workspace or given a `[name]` (§10.1, §12.1). A workspace is a
> plain host directory sandboxed per-invocation by `nono` — there is no VM to
> boot/stop, no `ai exec`, no `ai resize`, and no `ai mounts`/`ai apps` (removed
> entirely with the Microsandbox-VM architecture; §0 of the migration notes).

## 4.1 List Workspaces

```bash id="c8"
ai list
```

> There is a **single** `ai list` (§3.3) — it is the workspace inventory and also
> reflects each workspace's live `nono` session status. There is no separate
> session-only listing command (see `ai sessions`, §4.5b, for the live session
> table of one workspace).

---

## 4.2 Start Workspace

```bash id="c9"
ai start [<name>]
```

`ai start` is **sugar over `ai agent <default-cli>`** — there is no persistent
VM to boot, so "starting" a workspace means launching (or reattaching to) its
**default agent CLI's** sandboxed session. It is **interactive-only** (it
suspends into the session, exactly like `ai agent`), so under `--json` or a
non-TTY invocation it exits `2`.

Behavior:

* resolves the workspace's default agent CLI from `config.yaml`
  (`agent.default_tool`, falling back to the first entry in `agent.tools`)
* if a session named after that CLI is already **running** for the workspace,
  **attaches** to it (`manager.Attach`) — exactly `ai attach <cli-session>`
* otherwise mints a fresh scoped LiteLLM virtual key (stored only in the OS
  keystore), (re)writes that CLI's host-side config and `nono` sandbox profile
  (applying the project's current egress policy — `network.egress`/
  `allow_domain`/`deny_domain`, §10.4), and launches a new detached `nono`
  session (`nono run --detached --profile … --env-credential-map … -- <cli>`),
  then attaches to it
* either way the session's PTY takes over the terminal — a clean exit from the
  agent CLI (or detaching, `Ctrl-b d` style per nono's own session semantics)
  returns control to the shell

---

## 4.3 Stop Workspace

```bash id="c10"
ai stop [<name>]
```

Stops **every** live `nono` session belonging to the workspace (not just the
default agent's) — `Manager.StopAll`. Unlike `start`/`restart` this is a
**normal, non-interactive** command (it runs in-process, no terminal
suspension needed): it works identically under `--json`. A workspace with no
running sessions is a no-op success, not an error. Workspace data (the project
directory, `.ai-platform/` state) is untouched — only the live sandboxed
processes are stopped.

---

## 4.3a Restart Workspace

```bash id="c10a"
ai restart [<name>]
```

Stops every live session for the workspace (`Manager.StopAll`, tolerating none
running), then launches the default agent CLI fresh — the same
attach-or-launch flow as `ai start` (§4.2). Like `ai start`, it is
**interactive-only** and exits `2` under `--json`/no-TTY. There is no
"must have been started before" precondition — restarting a workspace with no
prior session simply starts one.

---

## 4.4 Destroy / Delete Workspace (consolidated)

```bash id="c11"
ai delete [<name>]            # `ai destroy` is an ALIAS of this
ai delete [<name>] --purge
```

`destroy` and `delete` are **one command** (`destroy` is a cobra alias of
`delete`): there is no separate "stop the sandbox but keep the project"
operation — use `ai stop` to end a workspace's live sessions and
`ai start`/`ai agent`/`ai restart` to launch a fresh one. Flags:
`--purge` (also delete the whole project directory, not just platform state)
and `--keep-agent-config` (keep the per-CLI agent config folders; their
symlinked content is materialized first). Behavior of delete:

* **stops every live `nono` session** for the workspace (`Manager.StopAll`) —
  best-effort: a failure is reported as a **warning**
  ("stopping live sessions failed (…) — check `nono ps` manually"), never
  aborting the delete
* **revokes and removes the workspace's scoped gateway credential** from the
  OS keystore (`Manager.DeleteCredentials` — best-effort revoke at LiteLLM by
  alias, then remove the keychain entry; a failure is reported as a warning).
  The real provider API keys themselves are untouched (held in LiteLLM, §16.1)
* removes the project's **`.ai-platform` directory** (config + run state + the
  shared `{agents,skills,prompts,projects}` resource pool — the platform's
  footprint)
* de-registers it from `config/projects.yaml`
* **keeps the user's OTHER files** in the directory; `--purge` additionally
  removes the WHOLE project directory
* the **per-CLI agent config folders** (`.opencode`/`.claude`/`.codex`/`.omp`/
  `.gemini`/`.hermes`) and the `.venv-msb` virtualenv — which the platform
  writes into the project dir (outside `.ai-platform`) at session launch — are
  removed too on a plain delete UNLESS kept with **`--keep-agent-config`** (or
  by answering the interactive prompt). When they are KEPT, their symlinks into
  the shared `.ai-platform` pool are **materialized into real files** before
  `.ai-platform` is removed, so nothing dangles. (`--purge` removes everything
  regardless.)
* **destructive** — on a TTY it interactively prompts for `--purge` (delete the
  whole project dir) and, when not purging, for removing those agent config
  folders; under `--json`/no-TTY it requires `--yes` (§20) and is driven by
  `--purge` / `--keep-agent-config`. `--dry-run` prints the side-effect-free plan.

---

## 4.5a Interactive Shell (`ai shell`)

```bash
ai shell [<name>]   # defaults to the current directory's workspace
```

Opens a **fresh, anonymous interactive shell** sandboxed by `nono` to the
workspace's project directory — a real PTY (`nono shell --profile … --allow-cwd`),
running the workspace's default shell (`workspace.shell`, `bash`|`zsh`) with the
caller's stdin/stdout/stderr wired straight through. It is **interactive-only**:
it owns the terminal and emits no JSON envelope, so under `--json` or a non-TTY
it is exit `2`.

Every invocation starts a **new** session — `ai shell` does **not** list existing
sessions or offer a picker (that asymmetry is deliberate: `nono shell` sessions
are anonymous, so there is nothing distinguishing one from another to pick
between; use `ai sessions`/`ai attach` to reattach to one that is still running).
A platform failure (unknown workspace, `nono` missing) maps per §18 (2/3); the
shell exiting normally is a clean end, not a failure. The same path backs the
TUI Workspace view's `e` key (via `tea.ExecProcess`) and the `ai create`
attach-to-existing flow (§3.1).

---

## 4.5b Workspace Sessions (`ai agent` / `ai attach` / `ai sessions`)

```bash
ai agent <cli> [<name>]
ai attach [session] [<name>]
ai sessions [<name>]
ai sessions kill <session> [<name>] [--force]
# each [<name>] defaults to the current directory's workspace
```

There is no VM and no tmux layer: each interactive shell or agent CLI invocation
is its own **`nono` session** — a single host process sandboxed per-invocation by
nono (Landlock on Linux, Seatbelt on macOS). A session exists only while its
wrapped command is running or detached-but-alive (`nono run --detached` / `nono
attach` / `nono stop` / `nono ps --json` / `nono inspect --json`, wrapped by
`internal/nono.Runner`). Agent CLI processes run **directly on the host** — there
is no guest kernel, no tmux config, and no terminfo workaround needed: the host
terminal renders the agent's TUI exactly as it would any other host process.

* **`ai agent <cli> [name]`** starts or reattaches a **per-CLI** session named
  after the CLI — one of `opencode`, `omp`, `claude-code`, `codex`, `gemini`,
  `hermes` (`workspace.AgentCLINames()`) — so each agent CLI has at most one
  "the" session and several can run side by side. If a session named `<cli>` is
  already **running** for the workspace it **attaches** to it; otherwise it mints
  a fresh scoped LiteLLM virtual key (stored only in the OS keystore via
  `--env-credential-map`, never on disk), (re)writes that CLI's host-side config
  and `nono` sandbox profile, launches a new detached session, and attaches to
  it. An **unknown `<cli>`** exits `2` with the valid set listed; a `<cli>` whose
  binary is not installed on this host exits `4` with guidance to install it
  (`ai setup --host-agents`, §2.1).
* **`ai attach [session] [name]`** attaches to an **existing, running** session
  (it never creates one — that is `ai shell`'s/`ai agent`'s job). With an
  explicit `[session]` (an id or name `nono` can resolve) it attaches directly;
  with none it **lists the workspace's running sessions and prompts** for which
  to attach. When the workspace has **no running sessions** it reports a hint
  pointing at `ai agent <cli>` to create one (exit `0`, nothing to attach to —
  not an error).

`ai agent` and `ai attach` are **interactive-only** (they own the terminal and
emit no JSON envelope), so under `--json` or a non-TTY they are exit `2`, exactly
like `ai shell`. `ai sessions [name]` is a **normal command**: it lists the
workspace's live `nono` sessions (filtered to those whose working directory is
this project) as a table — ID (short) / NAME / STATUS / ATTACHED / STARTED — by
default and the full JSON envelope (one object per `internal/nono.Session`,
nono's own `ps --json` shape) under `--json`. A workspace with no sessions lists
**zero** rows, not an error. **`ai sessions kill <session> [name] [--force]`**
stops a session by id or name (`Manager.Stop`; `--force` SIGKILLs instead of a
graceful stop), emitting a confirmation result. Platform/session failures
(unknown workspace, unknown agent CLI, no active session → exit `2`; `nono` not
installed → exit `3`; any other runtime failure → exit `4`) are mapped
uniformly across all of `ai agent`/`ai attach`/`ai sessions`/`ai sessions kill`
by `mapWorkspaceErr`. The TUI **Shell** tab (§14.4) is the session manager and
drives the same paths (attach via `ai attach`, new via `ai shell`, stop via the
kill path).

---

## 4.5c In-VM Apps — Removed

There is no VM, so there is nothing to run an "in-VM app" inside: `ai apps` and
the entire `internal/apps` package (Open WebUI, agent-CLI dashboards, published
app ports) were removed with the Microsandbox-VM architecture. In-VM apps are
simply out of scope now — not replaced by a host-side equivalent.

### In-workspace agent CLI provider config — keyless per-CLI HOST configs, key in the OS keychain only

All **six** supported agent CLIs route through the LiteLLM gateway **by default**.
At every session launch (`ai agent`/`ai shell`/`ai start`) the platform writes each
CLI's provider config at **that CLI's own default location**, directly on the
**host** (there is no guest filesystem to translate through — `<project>/…` IS
the real path the CLI reads). Every on-disk config is **KEYLESS** (the scoped
virtual key is injected only into the sandboxed process's environment by `nono`'s
`--env-credential-map`, never written to a file) and any pre-existing file is
**deep-merged** so the managed block wins while the user's other keys survive:

* **opencode** → `<project>/.opencode/opencode.json` (`apiKey: "{env:AIP_GATEWAY_KEY}"`).
  It carries the per-request Headroom knobs on every model.
* **claude-code** → `<project>/.claude/settings.json` — an `env` block setting only
  `ANTHROPIC_BASE_URL` (the gateway root — LiteLLM's Anthropic-compatible surface, it
  appends `/v1/messages`); the bearer token is injected as `ANTHROPIC_AUTH_TOKEN`
  directly into the sandboxed claude-code process's environment by nono (settings.json
  has no `${VAR}` interpolation), so the file itself is keyless.
* **codex** → `<project>/.codex/config.toml` — a keyless `[model_providers.aip-gateway]`
  block (`base_url = "<gateway>/v1"`, `wire_api = "responses"` — codex requires the OpenAI
  Responses API, which LiteLLM exposes — `env_key = "AIP_GATEWAY_KEY"`) PLUS a **GLOBAL**
  `~/.codex/config.toml` trust entry (`[projects."<project-root>"] trust_level = "trusted"`)
  so codex loads the project config. Unlike the old per-VM-isolated design, this global
  file is now **shared across every project on the host**: the platform idempotently
  **appends** one `[projects."<root>"]` block per project rather than rewriting the
  file wholesale, so other projects' trust entries are never destroyed.
* **gemini** → **env-only** (no settings key for a base URL exists): `GOOGLE_GEMINI_BASE_URL`
  (the gateway root, honoured by the `@google/genai` SDK) is set as a profile environment
  variable (non-secret, so it is written into the `nono` profile JSON directly); the
  `GEMINI_API_KEY` credential is injected the same way as every other CLI's key.
* **omp** → `~/.omp/agent/models.yml` (**GLOBAL**, shared across projects) +
  `<project>/.omp/config.yml`.
* **hermes** → `~/.hermes/config.yaml` (**GLOBAL**, shared across projects).

**The scoped virtual key (and any real key) is NEVER written to host disk.** It
is minted fresh per project (`Manager.ensureScopedKey` — revoking any prior key
by alias first) and stored ONLY in the **OS keychain** (macOS Keychain via
`security`, Linux via `secret-tool` — `internal/nono.StoreCredential`, keyed
`aip-gateway-<project>`); at session launch `nono run`/`nono shell`'s
`--env-credential-map <keychain-ref> <ENV_VAR>` resolves it straight into the
sandboxed process's environment (`AIP_GATEWAY_KEY` for
opencode/omp/hermes/codex, `ANTHROPIC_AUTH_TOKEN` for claude-code,
`GEMINI_API_KEY` for gemini) — the secret never touches a config file, the
`nono` profile JSON, or argv. `ai delete` revokes and removes this credential
(§4.4). An OAuth-mode agent (claude-code/codex/gemini set to `oauth`, §3.1)
skips key-minting entirely and logs in natively, bypassing the gateway. Because
each session regenerates its CLI's config fresh at launch from the gateway's
**live** served-model list, there is no separate "refresh models without
restarting" command — the old in-VM `refresh-models` script no longer exists;
simply launch (or relaunch) `ai agent <cli>` to pick up a model added/removed
with `ai keys`/through omlx's admin panel.

---

## 4.5d Mounts — Removed

There is no VM and no guest/host filesystem boundary to manage: `ai mounts` and
the isolated-dir/shared-mount concepts (`workspace.isolated_dirs`/
`workspace.shared_mounts`) were removed with the Microsandbox-VM architecture.
An agent CLI sandboxed by `nono` reads and writes the project directory
directly on the host; there is nothing to mount.

---

## 4.5e AI Tools (`ai tools`)

```bash
ai tools list                        [<name>]
ai tools enable  <tool>              [<name>]
ai tools disable <tool>              [<name>]
```

`ai tools` inspects and toggles a project's per-project **AI tools** (§3.1's
`--tools`) after `ai create` time, without re-running the wizard. `<tool>` is one
of `caveman`, `graphify`, `code-review-graph`, `codebase-memory-mcp`, `openviking`,
`tokensave`, `agentmemory`, `ponytail`, `agent-skills` (`create.SupportedAITools()`);
an unknown tool exits `2`. The optional trailing `[<name>]` resolves the workspace
exactly like every other project-scoped verb (explicit name → `--project` → cwd).

* **`list`** — prints a TOOL / ENABLED / DESCRIPTION table: every supported tool,
  its current `context.<tool>_enabled` state for this project, and its one-line
  description (the same text shown next to each tool in the create wizard, §3.1).
  Envelope command `tools.list`; `--json` carries `{project, tools: [{name,
  enabled, description}]}`.
* **`enable <tool>`** / **`disable <tool>`** — flips that tool's
  `context.<tool>_enabled` bool in `config.yaml`. There is no image/Dockerfile to
  regenerate and no restart dance: each AI tool installs directly on the host
  (plain `os/exec`, best-effort) at the next `ai agent`/`ai shell`/`ai start`, so
  the change simply **applies on the next session launch** — the human output
  says so explicitly. Envelope commands `tools.enable` / `tools.disable`;
  `--json` carries `{project, tool, action}`.

Exit codes (§18): unknown action / unknown tool / invalid project → `2`; config
read/write failure → `4`.

---

## 4.6 Name Resolution For The Lifecycle Verbs

```bash id="c11b"
ai start [<name>]
ai stop [<name>]
ai restart [<name>]
```

These are the canonical lifecycle verbs (§4.2/§4.3/§4.3a). Each takes an
**optional `[<name>]`** and resolves the target workspace the same way every
workspace verb does:

* an explicit `[<name>]` positional, then the `--project` flag, then the
  workspace that owns the **current directory** — found by walking **up** parent
  directories until a **workspace root** is found, defined as a directory that
  contains a `.ai-platform/project.yaml` marker file (matching §17)
* the emitted envelope `command` is `workspace.start` / `workspace.stop` /
  `workspace.restart` (the envelope keys are unchanged by the flat surface)
* when **no name resolves** — none given, no `--project`, and the cwd is not
  inside any workspace — fail with exit `2` (invalid input, §18) and an
  actionable message directing the user to `ai create` or to `cd` into a workspace
* all other exit semantics are unchanged (missing dependency — `nono` not
  installed — → `3`; runtime failure → `4`); unlike the old VM model, a
  `restart` with no prior session is not an error (§4.3a)

---

# 5. Multi-Agent Orchestration — Removed (not a platform concern)

The platform does **not** manage agent identities, branches, worktrees, commits,
merges, or rebases — running multiple AI agents on a project and any git they need
is the **in-workspace agent CLI's** job, not the platform's (architecture §20–22).
The `[S3]` orchestration slice is retired. Note this is NOT the `ai agent` /
`ai attach` / `ai sessions` commands (§4.5b): those DO exist and are thin
`nono`-session launchers/reattachers for an in-workspace agent-CLI process — a
session surface, not an agent-orchestration system.

---

# 6. Git Commands — Removed

The platform exposes **no git commands** in its CLI surface; branching, merging,
committing, and conflict resolution are the user's and the in-workspace agent's job.
The one exception is internal, not a command: at every **agent/shell session
launch** the platform runs `git init` on a non-git project (right before Graphify
registration, so the Graphify git hook has a repo — §3.1, architecture §12/§21).
It never clones, adds a remote, or manages worktrees.

---

# 7. Environment — Removed (no image, no Dockerfile)

There is **no environment to define or snapshot** — no `ai snapshot`, no
environment upgrade/rollback verbs, and no `.ai-platform/Dockerfile`. A
workspace is a plain host directory sandboxed per-invocation by `nono`: an
agent CLI runs directly on the host, using whatever toolchains/software are
already installed there. There is no OS image selection, no per-workspace
software-stack provisioning, and nothing to rebuild — the platform does not
and cannot install language runtimes or system packages into an isolated
environment on the user's behalf. Installing a toolchain the agent needs is
the same operation as installing it for any other host tool (the user's own
package manager); `ai setup --host-agents` (§2.1) and each selected AI tool's
host install (§3.1/§4.5e) are the only software the platform itself installs,
and both install straight onto the host.

---

# 8. Model Commands

## 8.1 Model Status

```bash id="c22"
ai models status
```

Returns a labeled, actionable summary (not a raw field dump):

* LiteLLM gateway reachability — including the endpoint URL and, when it is down,
  how to bring it up (`ai services start` → `ai doctor`)
* the default model — there is **no** built-in default in the catalog-driven model
  system (`DefaultRouting()` is the zero `Routing`), so the default line is omitted
  unless platform config ever sets one; the gateway's model-list endpoints do not
  mark a default
* the **LIVE** list of models the gateway currently serves, sourced from LiteLLM's
  own endpoints (`/model/info`, falling back to `/v1/models`) — **not** a hardcoded
  list. The list is the gateway's **DB-backed** served models: a keyed provider's
  registered models.dev catalog models plus the registered omlx models
  (`omlx/<id>`, `id` exactly as omlx's own live model list reports it), each shown
  with its provider and (when `/model/info` exposes it) its mode. The **providers**
  line is DERIVED from this live list (the distinct provider prefixes), not from
  any hardcoded routing.
* the local-model (omlx — no key needed) vs cloud-provider (each needs a key
  via `ai keys add <provider>`) split
* a `ai models test <model>` next-step hint

The model-list call authenticates with the gateway master key (read from the
running container, same as `ai models test`). If the gateway is reachable but the
model-list call fails (e.g. unauthorized), the command still reports health and
shows a note rather than erroring out.

The `--json` envelope carries the underlying fields (`healthy`, `providers`
[live-derived], `default`, `local` [true when the gateway serves at least one
local omlx `omlx/<id>` model], `models` [the live served list of
`{name, provider, mode}`], `models_note` [why the list is empty when otherwise
reachable], `base_url`).

---

## 8.2 Model Test

```bash id="c23"
ai models test [model]
```

The `[model]` is **optional**: on a terminal you are prompted to pick one of the
named model handles (pre-selected from any model passed); under `--json` / no TTY
the model argument is **required** (missing → exit 2). A transport failure
(gateway unreachable) is exit `4`; an auth/credential failure is exit `5`; other
gateway errors are exit `4`.

---

## 8.3 Local Model Sync (`ai models refresh`)

`ai models status` / `ai models test` describe and probe **LiteLLM routing**.
There is **no local-model store CLI surface anymore** — the entire former
`ai models pull|configure|enable|disable|rm|list|popular|install-vllm|show`
surface (the vLLM + Hugging Face CLI design) has been **removed**. **omlx** is
now the sole local-inference runtime: it is ONE shared host process
(`omlx serve --model-dir <dir> --port 8100`) that downloads, serves, evicts, and
tunes every locally-served model **entirely through its own admin panel**, opened
with:

```bash
ai services console omlx     # opens http://127.0.0.1:8100/admin
```

There is no `ai models pull`/`rm`/`configure`/`enable`/`disable`/`list`/`popular`/
`show`/`install-vllm` equivalent — model download/add/remove/tune is omlx's job,
not this CLI's. A model omlx is serving registers in the gateway under the public
handle `omlx/<id>` (`id` exactly as omlx's own `GET /v1/models` reports it — no
alias derivation), routed internally to `openai/<id>`, with every omlx model
sharing the **same** `api_base` (omlx runs one shared endpoint, unlike the old
per-model vLLM design where each model had its own port).

```bash id="c23r"
ai models refresh
```

`ai models refresh` reconciles LiteLLM's `omlx/*` registrations against omlx's own
live `GET /v1/models` **on demand**, using the **same pure add/delete diff** as the
cloud-key sync: a model already registered under an unchanged name is left ALONE
(so hand-edited settings on it in LiteLLM's admin UI survive), only a newly-reported
model is added, and only one that disappeared from omlx's list is deleted. Use it
after adding/removing/renaming a model through omlx's admin panel, without needing
a full `ai services restart omlx`.
This is the **same** sync that already runs automatically at `ai setup` and at
`ai services start|restart omlx` (§10.2); this command just triggers it
standalone, so there is no separate step needed after a full setup/restart. The TUI
mirror is the `m` key in the omlx Service Detail pane (shown/offered only for that
service). It
takes no arguments. The `--json` envelope carries `data.added` / `data.deleted`
(the model names newly registered / no-longer-reported by this diff — an unchanged
model appears in **neither** list); when omlx
currently serves no models the human output reports "omlx currently serves no
models — nothing to rebuild" rather than an empty diff. A gateway/omlx failure exits
`4`.

---

# 9. Context Optimization Commands (Headroom + Caveman)

## 9.1 Context Status

```bash id="c24"
ai context status [project]
```

The `[project]` is **optional** and resolves like every project-scoped command
(positional → `--project` → the workspace owning the current directory; §17).
Returns:

* Headroom input-compression metrics (tokens saved, strategy)
* Caveman output-compression metrics (level, tokens saved)

---

## 9.2 Set Headroom Strategy

```bash id="c25"
ai context strategy [project] [conservative|balanced|aggressive]
```

Both positionals are **optional**: the project resolves like every project-scoped
command (§17), and on a terminal omitting the value **presents a select menu**
(pre-seeded with the current/given strategy); under `--json` / no TTY the value
must be passed. On a terminal, after the change is applied: against a **running**
workspace you are offered (default no) to restart it now so the baked-in
Headroom knobs pick up the new strategy immediately — declining leaves the change
to apply only on a future `ai restart` (**not** a plain `ai start`, which takes
the cheap non-rebuilding path and never picks it up); against a **stopped**
workspace there is no prompt — it is rebuilt immediately (then left stopped),
since deferring would silently never apply the change either. Under
`--json`/no-TTY the command does nothing beyond writing the config; the change
is picked up only by a subsequent `ai restart`. The strategy is kept per project (default `balanced`). Headroom runs
as a shared **host** container (`aip-headroom`, internal-only on `:8787`) that
LiteLLM invokes in-process as a `pre_call` compression guardrail (it is no longer
an nginx proxy); the strategy maps to the per-request compression knobs
(`keep_turns` / `output_buffer_tokens`) baked into the agent's request body, which
LiteLLM's `headroom` guardrail forwards to Headroom's `/v1/compress`.

---

## 9.3 Set Caveman Level

```bash id="c26"
ai context caveman [project] [lite|full|ultra|wenyan]
```

Both positionals are **optional** (project resolution per §17; on a terminal the
level is a select menu when omitted, pre-seeded with the current/given level;
`--json` / no TTY requires the value).

---

# 10. Diagnostics

## 10.1 System Doctor

```bash id="c27"
ai doctor [<name>]
```

`ai doctor` is the **consolidated** health command. It **always** reports:

* **Platform dependencies** — a top-level **`nono runtime`** check (is the `nono`
  CLI installed, mirroring the shape of the container-runtime check — `nono.Detect`),
  plus Docker/Podman (container runtime) and the rootless service-tier posture.
  Unlike the old Microsandbox runtime this replaced, there is no separate
  virtualization-capability check: `nono` has no VM/HVF/KVM gate, so "installed"
  is the whole check.
* **The local-model inference tier** — reported as one **HOST-NATIVE** service
  (Mode `host`, no container): **omlx** — the sole local-inference runtime, ONE
  shared server process for every locally-served model — with state from its own
  health probe at `127.0.0.1:8100/v1`; always **available** as a serving option,
  though it may read `stopped` when the server is down. Model management is not a
  `doctor` concern — it lives entirely in omlx's own admin panel
  (`ai services console omlx`).
* **The remaining service-tier services** (containers) — Presidio (analyzer +
  anonymizer back LiteLLM's opt-in `secret-masking` guardrail; reconciled only when
  it is enabled; architecture §15), LiteLLM (health + that the configured provider
  keys are present in the gateway — a missing key is warned, not fatal; architecture
  §17), Headroom (the input-compression service LiteLLM calls as a `pre_call`
  guardrail), the nginx proxy, and DNS (there are no optional host services).
  *(hardware bring-up: the real `omlx serve` fork and the live LiteLLM sync
  round-trip are exercised only on a provisioned host.)*

**Additionally**, when run inside a workspace directory (cwd or an ancestor) or
given a `[<name>]`, it reports exactly two more checks (§12.1) — there is no
separate workspace doctor command and, unlike the old microVM runtime this
replaced, nothing to probe for "rootless"/"virtualization" per workspace:

* **`workspace runtime`** — whether `nono` is installed (the same fact as the
  top-level `nono runtime` check, restated per-workspace for symmetry with the
  old report shape).
* **`workspace sessions`** — a live probe of the named workspace's current
  `nono` sessions: **ok** with a count when at least one is running
  (`"N running (M recorded)"`), **warn** with the same detail and a suggestion
  to run `ai agent <cli> <name>`/`ai shell <name>` when none is, or **error**
  if listing sessions itself fails.

`ai doctor` **always exits 0** with a report — per-check status (ok / warn /
fail) conveys health, rather than the process exit code.

---

## Output

* warnings
* errors
* repair suggestions

---

## 10.2 Service Management

The `ai` CLI is the single control plane for all host services — the platform
containers `dns`, `presidio`, `valkey`, `redisinsight`, `litellm`, `headroom`, and
`proxy`, plus the **HOST-NATIVE** local-model inference tier: `omlx` (the sole
local-inference runtime) (there are no optional host services).
The user never invokes `docker compose`, `launchctl`, or `systemctl` directly. The
container tier runs as containers on `aip-net`; **omlx is host-native, NOT a
container** — reported with Mode `host` (see
architecture §5, "Host Services Control Plane"). (The `nono` workspace-sandboxing
runtime is not a long-running service — it is driven by the top-level workspace
verbs, not `ai services`.)

```bash id="c27a"
ai services status                 # health + version of every service
ai services start   [<service>]    # start one or all (enabled services only)
ai services stop    [<service>]    # stop one or all
ai services restart [<service>]    # restart one or all
ai services update  [<service>]    # re-pull the latest image(s) and recreate one or all
ai services enable  <service>      # enable an optional service (and bring it up)
ai services disable <service>      # disable an optional service (and bring it down)
ai services console [<service>]    # list/open a service's admin console (--print for the URL)
ai services compose                # write a DEBUG-ONLY ~/.ai-platform/docker-compose.yaml mirroring the service-tier topology (NOT the launcher)
```

`<service>`: `omlx` | `presidio` | `litellm` | `headroom` | `proxy` | `dns` |
`valkey` | `redisinsight` | `all` (no arg = all). All eight are **core** (always on);
`omlx` is the **host-native** local-model service rather than a container. The
optional host-service set is currently **empty**, so `enable`/`disable` have
nothing to act on (retained for forward compatibility). (`presidio` reports
"disabled" unless the `secret-masking` guardrail is selected at `ai setup`.)

Behavior:

* `status` reports each service's health and pinned version; `--json` returns the
  §19 envelope with a `data.services` array. (The optional set is empty, so every
  listed service is a core service.) A **display-only `postgres` line** appears
  immediately after `litellm` — it surfaces the LiteLLM Postgres (`aip-litellm-db`,
  reconciled with LiteLLM by `ensureLiteLLMDB`): State `running`/`stopped`, Mode
  `container`, no host endpoint (INTERNAL-ONLY, no host port). It has **no** independent
  lifecycle verb (`ai services <action> litellm-db` → "unknown service"); it is
  managed with `litellm`. When the `secret-masking` guardrail is off, `presidio`
  is still listed but reads **`disabled`** (surfaced, not probed). Health probing
  shells out to the container runtime, so a **wedged/hung docker daemon fails fast**
  rather than hanging: each probe is bounded by a 15s timeout and a short-lived
  circuit breaker trips the remaining probes after the first timeout (self-healing
  after a few seconds), so `ai services status` returns instead of blocking on a
  dead daemon.
* the **local-model inference tier** is reported as **host-native** (Mode `host`,
  no container): the **`omlx`** line — the sole local-inference runtime, ONE shared
  server process for every locally-served model — has state from its own health
  probe at `127.0.0.1:8100/v1`. omlx is **always available** as a serving option and
  may read `stopped` when its server is down. *(hardware bring-up: the real
  `omlx serve` fork and the live end-to-end omlx serving path are not yet wired
  live.)*
* lifecycle verbs (`start`/`stop`/`restart`) act on the **platform-owned container
  set** via the runtime abstraction (§6). With no service, or `all`, they act on
  every **enabled** container in **dependency order** (a disabled optional service
  is skipped). On a terminal with no service they show a **checkbox** of every
  controllable service and its current state; an explicit name or `all` skips the
  prompt; non-interactive use targets all. Acting on a *named* disabled optional
  service exits `2` with a hint to `ai services enable <service>` first.
* `update` **re-pulls** each target service's pinned image — refreshing a moved tag
  like `latest` — and **recreates** the affected container(s). It accepts the same
  targets as the lifecycle verbs (a name, `all`, the no-arg checkbox on a terminal,
  or every service non-interactively). The native pull progress streams to stderr
  (no spinner, like the `ai setup` pre-pull); `--json` returns the `data.services`
  status array. (Image **version pinning** is changed by `ai setup --upgrade`;
  `update` re-pulls the *current* pins.)
* `enable`/`disable` apply **only to optional services** — they update the
  persisted optional-service set (`runtime.yaml`) and then start / stop the
  service's container(s). Because the optional set is currently empty, enabling a
  core service or naming an unknown service exits `2`; with no `runtime.yaml`
  (setup not run) they exit `3`.
* docker compose is not the launcher; container-tier services are managed through
  the runtime abstraction (§6). `ai services compose` writes a **debug-only**
  `~/.ai-platform/docker-compose.yaml` mirroring the topology (generated from the
  same consts/helpers so it stays in sync) for a developer to bring the same stack
  up under compose's tooling — it never owns startup
* service install/upgrade is handled by `ai setup` / `ai setup --upgrade`, not by
  these verbs
* an unknown service exits `2`. `ai services console [<service>]` lists/opens a
  service's admin dashboard: with no argument it lists the services that have a
  console; with a name it **opens** that console in the browser, or — with
  `--print` (and always under `--json`) — prints the URL instead. Most host consoles
  are **nginx subdomain UIs** served on the single gateway port `:18787`:
  `litellm.<domain>:18787/ui/login` (the LiteLLM admin UI) and `valkey.<domain>:18787`
  (RedisInsight, the Valkey cache GUI) — where `<domain>` is the platform base domain
  (`ai domain`, default `aip.local`). `ai services console omlx` is the ONE
  exception: it opens the omlx server's **own** admin panel **directly on its
  host-native loopback port** — `http://127.0.0.1:8100/admin` — not an nginx
  subdomain (omlx has no UI vhost); this is the **only** place models are
  downloaded/added/removed/tuned — there is no CLI equivalent. (Open
  WebUI is now a per-workspace in-VM app; Odysseus was removed.)

## 10.3 LiteLLM gateway (`ai litellm`)

```bash id="c27a2"
ai litellm password                # set/rotate the LiteLLM admin UI password
```

`ai litellm password` sets or rotates the LiteLLM admin-UI password later (after
`setup`) — primarily so a **standalone** user who skipped a password at setup
(open access) can opt into one. It works in ANY role with a local LiteLLM
(standalone/server; a `client` host has none → exit 3).

Behavior:

* prompts (hidden, `promptSecret`) for the new password — a credential is never a
  flag/arg and can't be pre-seeded, so under `--json` / no-TTY there is nothing to
  supply and it exits `2`. An empty entry exits `2`.
* reuses the running gateway's `LITELLM_MASTER_KEY` when it has one (the same
  source `RelaunchLiteLLMWithAuth` reads), else mints a new one.
* relaunches LiteLLM with both set via env passthrough (`RelaunchLiteLLMWithAuth`)
  — the secrets never touch argv or platform disk.
* then OFFERS to save them to `~/.ai-platform/.ai-platform.env` (the same opt-in 0600
  auto-loaded persistence as `ai setup`).
* **Exit codes**: `3` when the platform/LiteLLM isn't set up (no `runtime.yaml`,
  client role, or the gateway container isn't running); `2` on bad/empty input or
  a non-TTY invocation; `4` on a relaunch / master-key failure.

---

# 10.4 Network (workspace egress policy)

```bash id="c27b"
ai network show      [project]          # show the egress policy
ai network egress    [deny|public] [project]  # set the default posture
ai network allow     [domain] [project] # allow the workspace to reach a domain
ai network disallow  [domain] [project] # clear a domain from the allow/deny lists
ai network log       [project] [--tail N]     # attempted-egress-by-name audit (host-wide)
```

Each verb takes an optional trailing `[project]` which, like every
project-scoped command, resolves the target project (positional → `--project`
→ the workspace owning the current directory; §17).

Project-scoped (default the current directory's project, like `ai context`).
These edit the project's `network` block in `config.yaml` — `network.egress`
(`deny`|`public`, empty resolves to **`public`** — nono's own default,
unrestricted outbound) plus `network.allow_domain`/`network.deny_domain`. There
is **no port** and **no `gateway` token** anymore — `nono` sandboxes a single
host process directly, so there is no guest network to publish a port out of;
a value is a bare **domain** (a hostname/IP, or a `*.suffix` wildcard such as
`*.npmjs.org`). This command manages the **declaration** only; it is compiled
into the project's `nono` sandbox profile (`internal/nono.Profile`'s `network`
section) the next time `ai agent`/`ai shell`/`ai start` launches a session —
**there is no restart step and no "offer to restart" flow**: the very next
launch picks up the change automatically, because every launch regenerates the
profile fresh. On a terminal, omitting the value **presents the options**:
`egress` (no mode) shows a posture select menu; `allow`/`disallow` (no arg)
prompt for a domain (validated the same way either way — `egress.ValidateHost`:
non-empty, no whitespace, no `://`/`/`, and at most a single leading `*.`
wildcard). With `--json` or no TTY, the value must be passed as an argument.

* `show` lists the project's **declared** egress policy: `egress`, `allow`
  (the allow-list, or a note that only the gateway is reachable in `deny`
  mode when empty), and `deny` (the deny-list, shown only if non-empty).
  There is no "live/in-force" view — `nono` has no equivalent of the old
  `msb inspect` applied-policy introspection; `show` is always the declared
  config.
* `egress [deny|public]` sets the default outbound posture: **public**
  (default — open internet, private ranges still blocked) or **deny** (only
  the gateway + explicitly allowed domains — most locked-down). The old third
  mode, **`unrestricted`**, no longer exists. A project whose `network.egress`
  is unset resolves to **public**.
* `allow <domain>` adds a domain the workspace may reach, removing it from the
  deny list if present there. **`disallow <domain>`** removes the domain from
  **both** the allow and deny lists (restoring the default posture for that
  domain) — there is no separate `ai network deny` verb exposed at the CLI.
* `log [project] [--tail N]` prints the **attempted-egress-by-name audit**: the
  DNS names workspaces tried to resolve, read from the platform's **`aip-dns`**
  resolver (unchanged service-tier infrastructure, architecture §29.7), which
  reads `docker logs aip-dns` and prints one line per query (the name + record
  type), most-recent last, capped by `--tail` (default 50). The header makes
  the **v1 caveats** explicit: it is **host-wide across all workspaces, not
  attributed per project** (the `[project]` argument is accepted for forward
  compatibility but does **not** filter); it is **names only — not connection
  verdicts and not direct-IP egress** (literal-IP traffic never touches DNS);
  and it is **not enforcement** — a name shown here was *resolved*, not
  necessarily *reached*. Enforcement is the project's `nono` network policy
  shown in `ai network show`, and under a `deny` posture `nono` may filter a
  denied name before it reaches the resolver (so denied names can be absent).
  If `aip-dns` is not running (or no container runtime is present), `log`
  prints a clear note and exits `0`; a genuine runtime failure reading the log
  exits `4`.
* invalid mode / domain → exit `2`.

Human-readable output by default; `--json` emits the standard §19 envelope.

---

# 10.5 Model Gateway (`ai gateway`)

Configure, machine-wide, which model gateway every workspace on this host routes
through. The address is persisted in `config/runtime.yaml` as `ai_platform_host`
(the same field `ai setup --mode client --server` sets) and is read at each
agent/shell session launch to derive both the agent CLI's `base_url` and (for a
remote gateway) the egress allow-list entry.

```bash
ai gateway show               # the configured gateway + the URL workspaces will use
ai gateway set [host[:port]]  # route every workspace here through a remote gateway (client mode)
ai gateway clear              # back to the local standalone gateway
```

The address is a **bare host or `host:port`** (NOT a URL). The default port is
`18787` (the nginx gateway port). Resolution:

* empty → `127.0.0.1:18787` (standalone/local — agent CLIs run directly on this
  same host, so the gateway is reached over plain loopback; there is no VM, so
  there is no guest-network host alias to resolve);
* `host` → that host on port `18787`;
* `host:port` → that host and port.

Workspaces reach the gateway at `http://<host>:<port>/v1` (the `/v1` suffix
opencode requires).

* `show`/`clear` take no arguments. `set` takes an **optional** address: on a
  terminal it always prompts (pre-seeded with any address passed); under `--json` /
  no TTY the address is **required** (missing → exit 2).
* a missing `runtime.yaml` (no `ai setup` yet) → exit `3` with a "run `ai setup`
  first" note; an invalid address (empty, whitespace, a scheme/slash, or a
  non-numeric port) → exit `2`; a failed persist/read → exit `4`.

Command names in the envelope are `gateway.show` / `gateway.set` / `gateway.clear`.
Human-readable output by default; `--json` emits the standard §19 envelope.

---

# 10.6 Platform Base Domain (`ai domain`)

Configure, machine-wide, the platform **base domain** the nginx UI subdomains hang
off: `litellm.<domain>` (LiteLLM admin UI) and `valkey.<domain>` (RedisInsight). The value is
persisted in `config/runtime.yaml` as `domain`. The default is **`aip.local`** for
local/standalone; operators **override it in server mode** so the UI is served on
a routable hostname.

```bash
ai domain                 # show the resolved base domain + the UI subdomain
ai domain <name>          # set the base domain (e.g. aip.example.com)
```

The name is a **syntactically valid DNS hostname** (NOT a URL): one or more
lowercase, dot-separated labels of `[a-z0-9-]` (each 1–63 chars, not starting or
ending with a hyphen), no scheme/path/whitespace, no leading/trailing dot. A dotted
name like `aip.local` or `aip.example.com` is fine, as is a bare label.

* with no argument → **show** the resolved domain (falling back to `aip.local` when
  unset), change nothing;
* with a name → set it. On a terminal the prompt always shows, pre-seeded with any
  name passed (the user confirms/edits); under `--json` / no TTY a name is applied
  directly.
* a missing `runtime.yaml` (no `ai setup` yet) → exit `3` with a "run `ai setup`
  first" note; an invalid name → exit `2`; a failed persist/read → exit `4`.

The envelope command name is `domain`. Human-readable output by default; `--json`
emits the standard §19 envelope.

---

# 11. Backup — Removed

There is no backup/restore command. It isn't needed (architecture §32): project
source is the user's git repo, platform state is rebuildable via
`ai state repair`, provider keys live in the LiteLLM gateway (§16.1), and
installed programs + agent state persist in the per-workspace overlay
(§26 / the workspace lifecycle verbs).

---

# 12. Workspace Diagnostics

## 12.1 Workspace Health

```bash id="c30"
ai doctor [<name>]
```

There is no separate workspace doctor command. The per-workspace section
(`workspace runtime` + `workspace sessions`) is part of the **consolidated
`ai doctor`** (§10.1): it appears **only** when `ai doctor` is run inside a
workspace directory (cwd or an ancestor) or given a `[<name>]`. When run outside
any workspace and with no `[<name>]`, `ai doctor` reports platform +
service-tier health only. `[<name>]` defaults to the current directory's
workspace. Unlike the old Microsandbox-VM runtime this replaced, there is no
rootless/virtualization posture to probe per workspace — `nono` sandboxes the
host process directly, so the section is just "is `nono` installed" plus the
live session count.

---

# 13. Logging Commands

## 13.1 View Logs

```bash id="c31"
ai logs
```

Options:

```bash id="c32"
--workspace <project>
--service <microsandbox|omlx|presidio|valkey|redisinsight|litellm|headroom|proxy|dns>
--tail
--follow
```

Log scopes are per-CONTAINER (finer-grained than `ai services`, which acts on whole
logical services).

Without `--follow`, `ai logs` shows the latest on-disk snapshot (refreshed by
`ai setup` / `ai services status`). **`--follow`** streams a `--service`'s logs
live (`<runtime> logs -f`) to stdout until Ctrl-C — human-only (it streams
continuously, so it is rejected under `--json`, and it requires a `--service`).
The `ai ui` Services view embeds the same live container log (`<runtime> logs
--tail`) beneath the per-service detail summary, auto-refreshing (§14.4).

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

## 14.3 UI Theme

```bash
ai theme [name]
```

Selects the CLI colour theme applied to the interactive prompts, forms, the
setup stepper, and headings. The choice is persisted **per host** in
`~/.ai-platform/config/ui.yaml` (`theme:`) and applied at startup, so every
command matches. `default` is the platform's own bright-pink-and-blue palette
(`blue-pink` selects the same palette by its explicit name), not huh's built-in
charm theme. The other selectable themes wrap huh's remaining built-ins
(`dracula`, `catppuccin`, `base16`, `monochrome`) plus custom palettes (`orange`,
`synthwave`, `cyberpunk`, `tokyo-night`, `vaporwave`, `tron`, `nord`, `gruvbox`,
`onedark`) — run `ai theme` to list them all.

Behavior (follows §1.8):

* On a TTY it ALWAYS shows a select menu, pre-seeded with `[name]` (else the
  active theme); the choice is applied and persisted.
* Under `--json` / no TTY, a provided `name` is applied directly (unknown name →
  exit `2`, listing the valid set); with **no** name it reports the active theme
  and the available set, changing nothing.

The semantic status colours (success/warn/failure = green/amber/red) stay fixed
across themes; a theme changes the form styling and the accent/heading colour.

---

## 14.4 Management UI

```bash
ai ui
```

A full-screen, K9s-style management TUI — a thin interactive layer over the same
package APIs the other commands use (no duplicated logic). It is **interactive
only**: it requires a terminal and has **no JSON envelope**, so `--json` or a
non-TTY invocation is exit `2`.

**Scope on launch.** `ai ui` is a global project switcher. It **always opens on the
home screen (Services)** with no project selected — the active view is not persisted
across restarts (the cwd is still kept for the create wizard's default location).
The switcher reaches any project in the index, and a new project may be created from
it (see below).

**Top-level tabs** (Services · Workspaces · Cloud Models ·
API Keys · Settings; cycled by `tab`/`⇧tab`/`←→` or jumped to directly with the
number keys `1`-`9`. There is **no `:` command palette** — `q`/`ctrl+c` quits and
`?` toggles the key-binding help):

* **Services** — a **two-level drill-down** mirroring the Workspaces hub. It opens
  on the *list* (live service-tier + container status: SERVICE / MODE / STATE /
  HEALTH / ADDRESS, auto-polled every 2s so STATE stays live — there is no manual
  refresh key); `e` enables/disables an **optional** service from the list
  (toggling on its current state; core services are always on), `r` **restarts the
  highlighted service in place** (the list's own auto-poll picks up the result, so
  no separate refresh is needed), `a` **restarts ALL services**, and
  `enter`/`d` **drills into a per-service DETAIL** (`esc` backs out to the list).
  The detail shows a fixed **summary** (name / mode / state / health / address /
  console) on top with the **live container log embedded beneath it** — a
  scrollable, READ-ONLY, selectable, auto-refreshing `LogView` tailing the
  container via `setup.ServiceLogTail` (`<runtime> logs --tail <n> <container>`;
  a multi-container service like Presidio concatenates per-container sections
  under a `── <container> ──` heading). This is the only per-workspace-style log
  view left in `ai ui` — a workspace itself has no persistent log to tail (§4,
  there is no VM). The log shows only while the service is RUNNING (a stopped
  service shows a "not running" hint, not stale output), follows the tail unless
  scrolled up, passes through the terminal-output normalizer, and PAUSES while
  the Services tab is not the visible top-level tab or the user has backed out
  to the list. In the detail, `s`/`x`/`r` start/stop/restart THIS service **in
  place** (an animated braille spinner on the state line while in flight, then a
  refresh — they do **not** pop back to the list), `p` (update) re-pulls +
  recreates the service via the **live embedded terminal overlay**
  (`ai services update <svc>`, whose CLI pull spinner shows the image-pull
  progress) and stays in the detail, `o` opens its admin console, and the
  remaining keys drive the embedded log: `↑/↓`/`PgUp`/`PgDn` scroll, `f`/`enter`
  follows the container log live in the REAL terminal
  (`ai logs --service <svc> --follow` → `<runtime> logs -f`). There is no separate
  Logs tab; the file-backed `internal/logs` reader still backs `ai logs`.
* **Workspaces** — a **two-level hub**. It opens on the *switcher*: every
  workspace (NAME / OS / STATUS / AGENTS — `OS` is the **host** OS it was
  created on, there being no image to select, §3.1); `enter` opens one, `n`
  starts a **fully in-TUI, multi-step create wizard** (no subprocess — it runs
  the shared `create.Execute` IN-PROCESS, so it behaves identically to
  `ai create`), `d` describes the selected one, `r` refreshes. The wizard walks
  native `ai ui` widgets in this order: a **location field** starting in the
  current directory (a text input above a live **dropdown of the folders**
  under the entered path — type to filter it and `tab` to complete the
  highlighted folder, or `↓`/`↑` to select a folder row; folders that are
  ALREADY a workspace are **greyed out and not selectable**; a leading `~`
  expands to home; the entered path is validated by the create rules and a
  non-existent path is created with intermediate folders); **Name** (text
  input, defaulting to — and re-deriving from — the location's basename until
  the user edits it); **Default interactive shell** (`bash`/`zsh`); **Agent
  CLIs** (multi-select, at least one, `opencode` pre-checked — there is no
  combined apps step anymore, since in-VM apps no longer exist, §4.5c);
  **Default agent CLI** (single-select from the chosen CLIs); a per-OAuth-CLI
  **auth mode** select (one screen per `claude-code`/`codex`/`gemini` actually
  selected, each `api-key`|`oauth`); the unified **AI tools** multi-select
  (caveman/graphify/code-review-graph/codebase-memory-mcp/openviking/tokensave/
  agentmemory/ponytail/agent-skills — none pre-checked, all opt-in); and,
  only when `graphify` is checked, a **Graphify model** SELECT populated from
  omlx's live model list (a "none" option always first). There are no
  OS/software-stack/resources/ports/isolated-dirs/idle-timeout/app-port steps —
  none of that exists anymore (§3.1). `listWindow` backs the single/multi-selects;
  plain text inputs cover the rest. On confirm it runs `create.Execute` off the
  event loop (this finishes in milliseconds — there is nothing to build or pull)
  and refreshes the hub, landing on the new workspace.
  Opening a workspace drops INTO it, revealing a **sub-tab bar**: **Workspace ·
  Network · Context · Shell** (`tab`/`←→` cycle them; the focused sub-view's
  pane is acted on directly; `esc` backs UP to the switcher):
  * **Workspace** — a fixed summary (name / agents / workspace status / path)
    and a **Configuration** diagnostics block read straight from the declared
    `config.yaml` — `os` (the host OS), `shell`, `egress`, and, when non-empty,
    `allowed domains` / `denied domains` — plus a **Gateway Endpoints** block:
    the host nginx entry to LiteLLM that external apps use to reach the served
    models (`models (OpenAI /v1)`, the LiteLLM admin API + consoles, and an auth
    hint). The endpoints block is workspace-independent (one gateway per host,
    resolved from `runtime.yaml`), so it shows regardless of workspace state.
    The pane is **scrollable** (`↑/↓`/`PgUp`/`PgDn`) so the summary + config stay
    reachable when they overflow. Hints: `s start · x stop · r restart · d
    delete · e shell · ↑/↓ scroll` — there is no resize (`z`) key anymore
    (§4.3b was removed). `s`/`r` (start/restart) launch or reattach the
    workspace's **default agent CLI session**, suspending the whole TUI into
    the user's REAL terminal via `tea.ExecProcess` (running `ai start <name>` /
    `ai restart <name>`, restoring the TUI when the session ends) — exactly
    like the Shell tab's attach flow, since "starting" a workspace now IS
    launching an interactive session, not booting a VM. `x` (stop) runs
    **IN-PROCESS** (`Manager.StopAll` — no sandbox to suspend into, so there is
    no spinner/detached-process dance). `d` (delete) runs in the confirm
    **terminal overlay** (see below). `e` opens a fresh shell the same way as
    `s`/`r` (`ai shell <name>`).
  * **Network** — the project's egress policy, edited inline: a summary line
    (`internet: on|off`, `mode:`, `allow:`, `deny:`) with hints `i internet
    on/off · a allow domain · d disallow domain`. `i` is a quick toggle between
    `public` (on) and `deny` (off); `a`/`d` open an inline text prompt for a
    **domain** (not a `host:port` — there is no port/gateway-token concept
    anymore, §10.4) — `enter` applies it, `esc` cancels. Every change applies
    the next time a session launches; there is no restart prompt (profiles are
    regenerated fresh on every launch). CLI mirror: `ai network
    egress`/`allow`/`disallow`.
  * **Context** — Headroom strategy + Caveman level; `s`/`c` cycle them
    (unchanged, §9).
  * **Shell** — the per-workspace **session manager** over the live `nono`
    sessions (ID / NAME / STATUS / ATTACHED / STARTED), auto-refreshing every
    ~3s while visible. Hints: `enter/a attach · n new shell · d stop · r
    refresh`. `enter`/`a` attach the highlighted session; `n` **always starts a
    brand-new anonymous session** (`nono` sessions have no name to reuse, so
    there is no "create a named session" prompt anymore — it is exactly
    `ai shell <name>`); `d` stops the highlighted session (`Manager.Stop`); `r`
    refreshes. Attaching/creating suspends the TUI into the user's **REAL
    terminal** via `tea.ExecProcess` (running `ai attach <session> <name>` /
    `ai shell <name>`, restoring on exit) — NOT an embedded emulator — so keys,
    native text selection, and in-place output all work. CLI mirror: `ai shell`
    / `ai attach` / `ai agent` / `ai sessions` (+ `ai sessions kill`).

  There is no Logs, Metrics, Mounts, or Apps sub-tab anymore — a workspace has
  no persistent process to tail metrics/logs from between sessions, no
  guest/host mount boundary to manage, and no in-VM app runtime (§4.5c/§4.5d
  were removed entirely).

  `ai ui` captures the mouse so the top-level tab bar and the sub-tab bars
  (Workspaces hub, Services detail) are clickable; native text selection then
  needs the terminal's selection modifier (Shift on most terminals, Option on
  macOS). Mouse capture is a persisted user setting (default ON, stored in
  `~/.ai-platform/config/ui.yaml` alongside the theme): the Settings tab's `m`
  key toggles it live and persists it across restarts, and with it OFF native
  text selection needs no modifier. When mouse capture is ON the wheel scrolls
  the focused list/table/viewport (one row per notch); keyboard scroll
  (PgUp/PgDn/arrows) works either way.
* **Cloud Models** — the **models.dev catalog** ⨯ the gateway's live registered set,
  in a MODEL · PROVIDER · STATUS · CONTEXT table (registered-first). It shows ONLY
  cloud providers — local `omlx/<id>` models are EXCLUDED. There is no local-model
  tab: local-model management is gone from this CLI/TUI entirely — omlx (the sole
  local-inference runtime) manages its own models through its own admin panel
  (`ai services console omlx`, §10.2), and `ai models refresh` (§8.3) resyncs the
  gateway's `omlx/*` registrations on demand (a pure add/delete diff against
  omlx's live model list — an unchanged model is left alone). `enter` opens the catalog metadata in a describe pane; `t` tests a
  **registered** model
  (round-trips it through the gateway; the catalog-driven system has no default
  model); `r` re-fetches the catalog + resyncs the gateway. Provider keys are added
  in the **API Keys** tab, not here. When models.dev is unreachable the cached copy
  is used and a source-availability message is flashed.
* **API Keys** — the LiteLLM-routable catalog providers (PROVIDER · NAME · KEY? ·
  MODELS); `a` adds a key for the selected provider, `e` edits (overwrites) it, `d`
  removes it (each runs `ai keys add|remove <provider>` in the live embedded terminal
  — the hidden key prompt shows there, so the value never enters the view), `r`
  refreshes; `esc` backs out of the add/edit terminal overlay.
* **Settings** — a live **theme** picker (every `ai theme` theme; `enter` applies
  the selected one to the whole UI immediately and persists it, `↑/↓` select) above
  the **mouse tab-clicking toggle** (`m` flips it, applied live and persisted in
  `~/.ai-platform/config/ui.yaml`; OFF frees native text selection from the
  terminal's selection modifier) and a read-only platform info block (deployment
  role + model gateway, changed via `ai gateway` / `ai setup`).

**Live embedded terminal.** A **live terminal overlay** that fills the body — the
platform runs the corresponding command on a pseudo-terminal (`creack/pty`) and
renders the program's screen with a vt10x emulator, forwarding keystrokes to it —
backs the flows that need an in-pane TTY *without* suspending the TUI: workspace
**delete** (its confirm prompt), a service `p` **update**
(`ai services update <svc>`), and **API Keys** add/remove (its hidden key prompt
shows there). While it is open, keys go to the program; **ctrl+q** force-detaches,
and once the program exits **any key** closes the overlay (the affected views
refresh on close). There is no `ai apps`/image-pull flow using this overlay
anymore — in-VM apps were removed entirely (§4.5c).

Workspace **start/stop/restart** do NOT use this overlay. `start`/`restart` launch
or reattach the default agent CLI's sandboxed session, which is an interactive
program — so, exactly like the Shell tab's attach flow, the whole TUI **suspends**
and runs it in the user's REAL terminal via `tea.ExecProcess` (`ai start <name>` /
`ai restart <name>`), restoring the TUI when the session ends. `stop` runs
**IN-PROCESS** (`Manager.StopAll` — there is no sandbox to suspend into). The
interactive **sessions** (shell/agent/attach) likewise bypass the embedded
emulator and run in the user's **REAL terminal** via `tea.ExecProcess`, so keys,
native text selection, and in-place output all work exactly as they would
outside the TUI.

Keyboard navigation always works, regardless of the mouse-capture setting above:
`tab`/`⇧tab`/`←→` cycle the top-level tabs and
the number keys `1`-`9` jump straight to one (there is no `:` command palette).
On the model/Workspaces list views `d`/`enter` opens a scrollable describe pane; on
the **Services** list `enter`/`d` drills into the per-service detail (its embedded
container log). **`esc` backs out one level** everywhere
it makes sense — it closes a describe pane, backs an open workspace / open service
detail out to its list, closes the menu/help overlay, or cancels the create flow,
returning to the level above; at the top level it is a no-op (`q` quits). `?` shows
the key bindings. The UI honours the theme set by `ai theme` (§14.3). The embedded
service **container logs** auto-refresh from the live `<runtime> logs` tail.

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

## 16.1 Provider API Keys (`ai keys`)

Provider API keys live **in the LiteLLM gateway** (keys-in-LiteLLM): they are
stored **encrypted at rest** in LiteLLM's Postgres-backed credential store
(encrypted by `LITELLM_SALT_KEY`); the platform never writes the values to its own
disk (architecture §17). `ai keys` is keyed **per provider** off the model catalog
(models.dev, `internal/catalog`): a key is stored against a LiteLLM-routable
catalog provider, and adding/removing a key **syncs** that provider's catalog
models into/out of the gateway so the agent can route to them.

```bash id="c37"
ai keys list                                    # routable providers: id, name, KEY?, catalog model count
ai keys add <provider> [--value <v> | --stdin]  # store the provider key + register its catalog models
ai keys remove <provider>                        # remove the provider key + drop its models
```

Behavior:

* `<provider>` must be a **LiteLLM-routable catalog provider** (the catalog
  providers that map to a LiteLLM prefix — e.g. `openai`, `anthropic`, `google`
  routes as `gemini`); an unknown provider exits `2` and lists the valid ones
* the key value is read from `--value` (discouraged — shell history) or `--stdin`
  (preferred, what the acceptance harness uses), else prompted **hidden** on a TTY;
  under `--json`/no-TTY a missing value exits `2`. The value **never** appears in
  argv, logs, or any `--json` envelope
* `add` stores the key via the LiteLLM credential API (encrypted at rest) then
  registers that provider's catalog models (alongside every currently-keyed
  provider + the models omlx is currently serving); `remove` deletes the key then
  re-syncs (its models drop out)
* the gateway/master key must be reachable (`ai setup`); when it is not, the
  command exits `3`. The workspace agent receives only a scoped LiteLLM **virtual
  key**, never a provider secret; that key is aliased to the project and `ai start`
  **rotates** it so a re-start never collides with LiteLLM's unique-alias
  requirement
* `ai keys` **replaces** the former `ai secrets` command

---

# 17. Configuration Flags

Global flags (accepted by every command and subcommand):

```bash id="c35"
--help, -h             show usage for this command/subcommand and exit 0
--json                 machine-readable output (see §19)
--verbose              extra human-readable detail (ignored with --json)
--plain                plain, non-interactive human output (no TUI: no spinners/colour), keeps human-readable text (§1.3)
--dry-run              compute and print the planned actions; mutate nothing
--project <name>       scope the command to a project (overrides the default)
--yes                  assume "yes" for destructive confirmation prompts
```

`ai` itself (root command only, not a persistent flag) additionally accepts
`--version`, which prints the CLI version and exits `0` (`{"name":"ai","version":
…}` under `--json`).

### Project resolution

Project-scoped commands (the workspace verbs `start`/`stop`/`restart`/
`shell`/`agent`/`attach`/`sessions`/`tools`/`delete` (`destroy` alias),
`context *`, `network *`, `doctor`, `logs --workspace`) resolve their target project
with this precedence:

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
exceptions. This includes `ai` itself, the remaining groups (`ai context`,
`ai services`, `ai keys`, …), and every top-level verb (`ai create`,
`ai agent`, `ai start`, …).

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

* perform no state mutation, no `nono`/runtime calls that change state, no git
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
| missing Docker/Podman/`nono` (`doctor`, `setup`) | 3 |
| unknown project / agent CLI | 2 |
| `nono` or runtime operation failed | 4 |
| rootless required but unavailable (service tier) | 4 |
| destructive command (`ai delete`) without `--yes`, non-interactive | 2 |
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
  "command": "project.create",
  "data": { },
  "error": null,
  "warnings": []
}
```

On failure:

```json
{
  "ok": false,
  "command": "project.create",
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

* `ai delete` (stops any live `nono` sessions, revokes the workspace's scoped
  gateway credential, and removes the project's `.ai-platform` state + index
  entry — keeping the user's other files — and, with `--purge`, the whole
  project directory). `ai destroy` is a **cobra alias** of `ai delete` (the same
  destructive command, §4.4), so it gates identically.

* interactive runs prompt for confirmation
* `--yes` confirms non-interactively (used by the acceptance harness)
* in a non-interactive context without `--yes`, a destructive command exits `2`
  and changes nothing

---

# 21. Success Criteria

CLI is considered complete when:

* all platform functions are accessible via CLI
* state is fully controllable via commands
* agent CLI sessions can be created and cleaned up (`ai agent`/`ai shell`, `ai
  sessions kill`, `ai stop`)
* a workspace is a plain, self-describing host directory (`.ai-platform/
  {config.yaml,project.yaml}`) with no image or environment definition to
  build or version — there is nothing to snapshot (§7)
* system is recoverable via `ai state repair`
* no manual filesystem intervention is required
