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
  implemented in **Go** and shipped as a **single self-contained binary** per host.
  (It links the Microsandbox **Go SDK**, a cgo binding that `go:embed`s an FFI
  library extracted at first run — so `CGO_ENABLED=1` is required; it is one binary
  but no longer a pure-static `CGO_ENABLED=0` build. See docs/MSB-SDK-MIGRATION.md.)
* Host bootstrap uses **thin launchers only** (bash / zsh) whose
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
  HTTP APIs — never reimplemented. Version control is largely out of scope: the CLI
  exposes no git commands and never runs `gh`; the only git it runs is an internal
  `git init` at workspace start (to seat the Graphify hook — §6), never clone/remote.

---

## 1.6 Binary & Command Naming

There is **one binary**, `ai`, installed on `PATH`. There are **no hyphenated
command names and no symlinks**, and essentially no command aliases — the only
ones are the documented `destroy` alias of `ai delete` (§4.4) and the
`remove`/`delete` aliases of `ai models rm`. Every command follows a
single, consistent pattern:

```text
ai <noun> [<verb>] [args] [flags]
```

Examples: `ai setup`, `ai create`, `ai delete`, `ai start`, `ai exec`,
`ai context status`. Workspace lifecycle verbs are **top-level** (`ai create`,
`ai start`, `ai exec`, …); grouped commands like `ai context status` /
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
* `ai apps <verb>` → the verb set, then the app keys (`openwebui|anythingllm`)
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
ai setup [--provider-config <file>] [--mode standalone|server|client] [--server <addr>] [--optional <csv>] [--guardrails <csv>]
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

**Optional host tools.** The optional host-service set is currently **empty** —
there are no opt-in host services. (Open WebUI is now a per-workspace in-VM app,
launched at `ai create` and managed via `ai apps`; Odysseus has been removed from
the platform entirely.) The `--optional <csv>` flag is retained for forward
compatibility but accepts only the sentinel `none` (a safe no-op); any other value
is unknown → exit `2`. Omitting `--optional` keeps the empty set.

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
  plane — see architecture §5, "Host Services Control Plane". The host service
  set is the `aip-dns` CoreDNS egress-audit resolver + Ollama (required) +
  Presidio (analyzer + anonymizer, reconciled only when the `secret-masking`
  guardrail is selected) + Valkey (+ RedisInsight) + Headroom + LiteLLM
  (+ Postgres) + the `aip-proxy` nginx gateway (the SOLE host entry, reconciled
  LAST) — all containers on the `aip-net` network, reconciled in order network →
  DNS → Ollama → Presidio → Valkey(+RedisInsight) → Headroom → LiteLLM(+DB) →
  proxy (Headroom PRECEDES LiteLLM because LiteLLM's `headroom` compression
  guardrail calls it in-process) — plus verification of the Microsandbox
  workspace runtime.
* renders each service config from the platform config and verifies the
  Microsandbox runtime + host virtualization (no docker compose; the whole
  service tier runs as containers, so there is no native OS service to register)
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
`ai domain`, §10.5). In
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
model sync (reflecting any already-keyed providers + installed Ollama models);
`ai doctor` flags any absent credential, and a model call fails (exit `5`) only
when that credential is actually needed (architecture §17, plan §7). Missing
**dependencies** fail fast: a missing container or Microsandbox runtime exits `3`,
while an unmet host capability (virtualization, rootless posture) exits `4`
(internal/setup/setup.go — `installablePrograms` = container/microsandbox runtime
only). git is **not** a prerequisite — the only git the platform runs is an
internal `git init` at workspace start.

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

* **streams uninstall status/progress** live as each step runs — stopping any
  running workspace microVMs (`msb stop -f` each `aip-*` sandbox, **preserving all
  workspace DATA** — project source and `/persist` overlays are host bind mounts,
  never deleted; the VM instance is only halted, never `msb delete`'d), stopping and
  removing the platform containers (`aip-*`), **removing all platform container
  images** (the pinned service-tier images plus every `aip-*` image, so nothing is
  left on the host — done on EVERY uninstall, not only `--purge`), removing the `ai`
  binary, removing the completion scripts, and stripping the managed PATH/completion
  lines from the shell rc files (leaving the user's own lines intact)
* **removes the platform state under `~/.ai-platform` but KEEPS the downloaded
  model store (`volumes/models`)** — the one expensive-to-refetch piece a user
  usually wants to keep across a reinstall; `--purge` removes `~/.ai-platform`
  in full, including the downloaded models
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

# 3. Workspace Commands (create / list / delete)

> **Flat surface.** The commands are the **top-level verbs** below
> (`ai create` / `ai list` / `ai delete`). There are **no** `ai project …`
> command groups or aliases. The envelope `command` keys are unchanged
> (`project.create` / `project.list` / `project.delete`). The user-facing noun is
> **workspace** everywhere.

## 3.1 Create Workspace

```bash id="c4"
ai create [<name>] [--name <name>] [--os <os>] [--shell <bash|zsh>] [--agents <list>]
          [--auth-mode <cli=mode>] [--stacks <list>] [--apps <list>] [--app-port <app=port>] [--graphify-model <ref>]
          [--cpus <n>] [--memory <size>] [--ports <list>] [--location <dir>]
          [--idle-timeout <dur>] [--tools <list>]
```

`ai create` sets up a new environment at the chosen **location** (default: the
current working directory). On a terminal with no create flags it runs an
**interactive wizard**; for non-interactive use — and for external programs via
`--json` — **every input also has a flag**, so the whole workspace is specifiable in
one command:

* `--name <name>` (or the `[<name>]` positional) — defaults to the location
  directory's basename
* `--os <os>` — one of `debian-trixie|debian-bookworm|ubuntu|alma`; **required**
  when running non-interactively
* `--shell <shell>` — the workspace's default interactive shell, `bash|zsh`
  (default `bash`), written to `workspace.shell` and applied at every start by
  `applyShellChoice`
* `--agents <list>` — comma-separated agent CLIs
  (`opencode,omp,claude-code,codex,gemini,copilot,hermes`); defaults to
  `opencode`, and the first listed becomes the default agent CLI. `opencode`/
  `omp`/`hermes` are always gateway/api-key; `claude-code`/`codex`/`gemini`
  are gateway by default but OAuth-selectable (see `--auth-mode`); `copilot` is
  forced-OAuth / gateway-incapable
* `--auth-mode <cli=mode>` — per-agent auth mode, repeatable/comma-separated, for
  the OAuth-capable CLIs `claude-code`/`codex`/`gemini` (`api-key`|`oauth`;
  default `api-key`). `api-key` routes through the gateway with the scoped virtual
  key (firewall + masking apply); `oauth` uses the CLI's own subscription login,
  bypassing the gateway. `copilot=api-key` is rejected (exit 2 — copilot is
  forced-OAuth); opencode/omp/hermes are always gateway/api-key and never accept it
* `--stacks <list>` — comma-separated EXTRA software stacks
  (`go,rust,java,maven,deno`); optional. **Neither Python nor Node is a stack
  option** — Node.js and the latest **Python 3** + **uv** (Astral's Python
  package/tool manager) are baked into every OS base image by default.
  **Graphify** (`graphifyy`, the knowledge-graph CLI skill) is NO LONGER baked into the
  base — it is a selectable **AI tool** (`--tools graphify`, default on) installed via a
  CONDITIONAL `tools/graphify/` Dockerfile snippet (`uv tool install "graphifyy[…extras]"`
  with all optional extras except the region/DB/niche-specific
  chinese/azure/bedrock/falkordb/neo4j/leiden/dm/pascal) only when selected; when selected,
  each agent CLI registers Graphify with itself at workspace START, once per project
  (`graphify install --project [--platform <cli>]` in `~/project` — not at image build,
  since it writes project-scoped files). Each workspace gets a per-project **`.venv-msb`**
  virtualenv created at start regardless (see §7/§25)
* `--graphify-model <ref>` — the **Ollama model Graphify uses** for its headless LLM
  backend, e.g. `qwen2.5-coder:7b`; optional (blank = none). On a terminal the wizard
  offers an optional model+tag select from the cached Ollama library; the chosen model
  is stored as `agent.graphify_model`, pulled into the local Ollama store and
  registered in the gateway if absent (best-effort — a pull failure is a create
  warning, not a failure), and routed through the gateway as `ollama/<model>` at
  workspace start (architecture §17)
* `--apps <list>` — comma-separated in-VM AI apps to install
  (`openwebui,anythingllm`); **opt-in, default none**. Like `--stacks` it
  pre-seeds the wizard's apps multi-select on a terminal and drives the selection
  directly under `--json`/no-TTY. Each selected app is exposed on a host port
  at create time (see §4.5c and `--app-port`)
* `--app-port <app>=<port>` — the HOST port to expose a selected app's web UI on
  (repeatable, e.g. `--app-port openwebui=8080 --app-port anythingllm=3001`). On a
  terminal the wizard **prompts** for each selected app's port (seeded with the app's
  familiar container port — Open WebUI 8080, AnythingLLM 3001 — when free, else an
  auto-allocated one); this flag pre-seeds that prompt and sets it non-interactively.
  A blank/omitted port is **auto-assigned**. The port is validated unique + host-free
  at create; an unknown app or out-of-range port exits `2`, an unavailable port exits `2`.
  A dashboard-capable **agent CLI** may also be a target: when **hermes** is selected the
  wizard prompts for (and `--app-port hermes=<port>` sets) the host port for its web
  dashboard (`hermes dashboard`, default 9119), persisted under `config.yaml`
  `agent_dashboards:` and published from the microVM the same way as an app.
* `--cpus <n>` — workspace vCPUs, written to `workspace.cpu_limit`. Defaults to the
  global default (4) and is **capped at the host's logical CPU count** — a larger
  request exits `2`.
* `--memory <size>` — workspace memory in **GB, a plain number** (e.g. `8`, `16`;
  a `512M`/`4G` unit suffix is still accepted for back-compat), written to
  `workspace.memory_limit`. Defaults to the global default (`8` GB) and is **capped
  BELOW the host's total RAM** — the platform reserves headroom for the host OS,
  service tier, and hypervisor (`config.UsableHostMemoryMiB` reserves the larger of
  2 GiB or 25% of host RAM), because a microVM given all host RAM cannot boot. A
  request above the usable ceiling exits `2`; an unset value resolves to the default
  clamped at that ceiling.
* `--ports <list>` — comma-separated host↔guest ports to open into the workspace,
  each `PORT` (host == guest) or `HOST:GUEST` (Docker-style host-first), written to
  `network.publish_ports`. Malformed/out-of-range ports exit `2`.
* `--location <dir>` — the workspace directory (default: cwd). **Created if it does
  not exist.** It must **not** be — or be nested inside — an existing workspace
  (a directory with a `.ai-platform/project.yaml` at it or any ancestor); otherwise
  create exits `2`. In the wizard the location field offers **path autocompletion**;
  the flag offers shell directory completion.
* `--idle-timeout <dur>` — the microVM idle timeout (e.g. `24h`), written to
  `microsandbox.idle_timeout`.
* `--tools <list>` — the per-project **AI tools** to install, a comma-separated
  multi-select (like `--agents`) chosen from `caveman`, `graphify`, `code-review-graph`,
  `codebase-memory-mcp`. Default (flag omitted): `caveman,graphify,code-review-graph`;
  `--tools=""` selects none. An unknown value exits `2`. Each maps to a
  `context.<tool>_enabled` bool. The tools:
  * `caveman` — the Caveman output-compression toolkit, installed at workspace start.
  * `graphify` — the Graphify knowledge-graph toolkit; baked into the image via a
    CONDITIONAL `tools/graphify/` Dockerfile snippet (no longer in the OS base) and
    registered per agent CLI at start (`graphify install --platform <cli>`).
  * `code-review-graph` — install code-review-graph (`code-review-graph.com`, PyPI
    `code-review-graph`) via a conditional snippet and register it as an MCP server. For the
    CLIs whose config the platform does not own it uses the tool's native
    `code-review-graph install --platform <cli>` (opencode/claude-code/gemini/copilot —
    `codeReviewGraphPlatformFlag`; codex was removed because its config is
    platform-rewritten); codex/omp/hermes instead get its MCP server
    (`code-review-graph serve`) injected into their platform-managed configs. Then `build`
    the graph and write its D3 visualization HTML.
  * `codebase-memory-mcp` — install codebase-memory-mcp
    (`github.com/DeusData/codebase-memory-mcp`) via a conditional snippet and register it
    as an MCP server: the tool's own `codebase-memory-mcp install` auto-detects
    claude-code/opencode/codex/gemini/copilot/hermes (not omp), but because
    codex/hermes configs are platform-rewritten and omp isn't detected, those three
    get its MCP server injected into their managed configs instead. Ships an optional
    on-demand 3D graph UI (`codebase-memory-mcp --ui=true --port=9749`).

On a terminal (with `--json` off) the wizard **always** runs, **pre-seeded** with
any flags you passed — flags set the defaults rather than bypassing the UI. Under
`--json` or no terminal, the spec is built straight from flags with **no prompt**.
Unknown flag values, an over-host `--cpus`/`--memory`, a malformed `--ports`, a
location nested in an existing workspace, or a missing `--os` when non-interactive
all exit `2`. `--dry-run` prints the plan in either mode.

The workspace lives at the location you choose — there is no fixed projects
directory. The chosen path is recorded in the global index
(`config/projects.yaml`), and every later command resolves the workspace **by
name** through that index. With no name given, the name defaults to the location
directory's basename. The microVM itself is created through the **Microsandbox Go
SDK** (`internal/workspace` SDK backend), with the resolved CPU/memory/ports applied
to the sandbox at start.

**Git.** The `ai create` command itself does **not** init or clone a git repo — it
only writes the `.ai-platform/` environment definition and leaves existing files
untouched. However, at every **workspace start** the platform runs `git init` in the
project dir when it is not already a git repo (right before Graphify registration, so
the Graphify git hook always has a repo to attach to); this writes `.git` into the
bind-mounted project on the host. Cloning remains out of scope — bring your own remote.

**Attach if one already exists.** If the current directory (or any parent) is
already a workspace, `create` does **not** scaffold a new one — it **attaches** to
that workspace instead: it starts the microVM (a no-op if already running) and
opens an interactive login shell inside it. This makes `ai create` idempotent per
directory.

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
   `ubuntu`). Selects the template that seeds `.ai-platform/Dockerfile`. The same
   group also carries a **Default interactive shell** single-select (`--shell`,
   `bash`|`zsh`, default `bash`, recorded as `workspace.shell`) — applied at
   every workspace start (`applyShellChoice`): the managed agent-env/alias block
   is appended to both `~/.bashrc` and `~/.zshrc`, and choosing `zsh` `chsh`es
   the workspace user's login shell to zsh.
3. **Agent CLIs** — **multi-select checkboxes**; `OpenCode` pre-checked
   (installed by default); choose any subset of `OpenCode`, `Omp`,
   `Claude Code`, `Codex`, `Gemini`, `Copilot`, `Hermes` (at least one).
   All connect to models through LiteLLM **except** `Copilot` (GitHub Copilot CLI),
   which is forced-OAuth / gateway-incapable and talks directly to GitHub.
4. **Default agent CLI** — single-select from the CLIs chosen in step 3; default
   `OpenCode` (recorded as `agent.default_tool`).
5. **Software stacks** — **multi-select checkboxes**; choose the language/tool
   stacks to install into the environment (`Go`, `Rust`, `Java`, `Maven`,
   `Deno` — the list is extensible, §25). **Neither Python nor Node is a stack** —
   Node.js, Python 3 and uv are baked into every base by default (Graphify is now a
   selectable AI tool, step 9).
   None pre-checked (a project
   may need nothing beyond the base image). Selected stacks are installed into the
   generated `.ai-platform/Dockerfile` and recorded in `profile.yaml`.
6. **AI apps** — **multi-select checkboxes**; choose the opt-in in-VM AI
   applications to install into the workspace (`Open WebUI`, `AnythingLLM`).
   **None pre-checked** (apps are opt-in). Pre-seeded from `--apps`. For EACH
   selected app the wizard then **prompts for the HOST port** to expose its web UI
   on (a text input per app, seeded with a suggested free port — the app's familiar
   container port when free, else auto-allocated; `--app-port <app>=<port>`
   pre-seeds it, blank auto-assigns). Each selected app is recorded in
   `config.yaml`'s `apps:` block with its port; the containers run inside the
   microVM and are managed later via `ai apps` (§4.5c).
7. **Resources & ports** — text inputs for **vCPUs** (`--cpus`, default 4,
   host-capped), **memory in GB** (`--memory`, a plain number, default 8,
   host-capped), and **ports to open** (`--ports`, comma-separated `PORT` or
   `HOST:GUEST`). Blank accepts the default; over-host or malformed values are
   rejected in place.
8. **Idle timeout** — text input for the Microsandbox idle timeout (`--idle-timeout`,
   default 24h).
9. **AI tools** — **multi-select checkboxes** (like the agent-CLI list, not a screen
   each): `caveman`, `graphify`, `code-review-graph`, `codebase-memory-mcp`. Pre-checked
   with the default set (`caveman,graphify,code-review-graph`); pre-seeded from `--tools`.
   Each is recorded as a `context.<tool>_enabled` bool.
10. **Graphify model** *(shown only when `graphify` is selected in step 9 AND the Ollama
   library cache is available)* — an **optional** single-select of an Ollama model + a tag
   select (blank = none), mirroring the Models page. The choice is stored as
   `agent.graphify_model`, pulled into the local Ollama store + registered in the gateway if
   absent, and routed through the gateway as `ollama/<model>`. Also settable
   non-interactively via `--graphify-model`.

There is no separate confirm step — completing the last group (Enter) creates
the project; **Abort** at any point cancels.

Prompts render on the terminal (stderr); with `--json` the **final result** is
still the single envelope on stdout (§19). **Abort** exits `0` and makes no
changes (`data.cancelled = true`). A **non-interactive context (no TTY) builds
the spec straight from flags with no prompt** (matching `--json`), and exits `2`
only when a required flag (`--os`) is absent or a flag value is unknown —
automation can instead drive the wizard through a pseudo-terminal (PTY), see
acceptance-tests §1.6.

Behavior (on completion):

* uses the current directory as the project root (no git is run)
* **writes `<project>/.ai-platform/Dockerfile`** by seeding it from the selected
  OS template and adding the selected software stacks (step 5) and agent CLIs
  (step 3) (architecture §25, §12); from then on the project owns that Dockerfile.
  Before composing, create **refreshes the on-disk templates**
  (`~/.ai-platform/templates`) from the running binary's embedded copies — composition
  reads the on-disk copies, so this keeps a new project in sync with the installed
  binary even when those copies were last written by an older `ai` (a binary upgrade
  alone suffices; no separate `ai setup` re-run is needed)
* **(Slice 2+)** installs the Caveman skill into
  `<project>/.ai-platform/skills/caveman/` (§9); in Slice 1 no context-optimization
  skill is seeded
* writes `project.yaml`, `config.yaml` (including `agent.tools`,
  `agent.default_tool`, and an `apps:` block for any in-VM apps chosen in step 6),
  `profile.yaml`, and a `.gitignore` that ignores `run/`
* records the project in the global index (`config/projects.yaml`)

`create` is **scaffold-only**: it writes the `.ai-platform/` definition and
registers the project — it does **not** build the workspace OCI image or
create/start the workspace microVM on first creation. The image is built and the
microVM is created on demand by `ai start` (§4.2), and `create` itself
starts/execs a workspace only when run in a directory that is **already** a
project (the attach path above).

---

## 3.3 List Workspaces

```bash id="c7"
ai list
```

Output (one row per workspace), columns:

* `NAME` — workspace name
* `OS` — operating system
* `AGENTS` — active agent CLIs
* `STATUS` — workspace status
* `ID` — microVM / runtime handle id
* `CREATED` — creation timestamp
* `LAST-STARTED` — last-started timestamp

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
delete tears down the microVM, removes the project's `.ai-platform` tree + overlay,
and de-registers it, keeping the user's OTHER files; `--purge` removes the whole
project directory. Provider credentials are untouched (held in the LiteLLM
gateway, §16.1).

---

# 4. Workspace microVM Commands (start / stop / restart / delete / exec / …)

> **Flat surface.** The commands are the **top-level verbs** below
> (`ai start` / `ai stop` / `ai restart` / `ai delete` / `ai exec` / `ai shell`
> / `ai agent` / `ai attach` / `ai sessions` / `ai doctor`). Each takes an
> **optional `[name]`** positional that defaults to the workspace owning the
> current directory (then `--project`). There are **no** `ai workspace …` command
> groups or aliases. The envelope `command` keys are unchanged (`workspace.*`).
> There is no separate per-workspace doctor: the single top-level `ai doctor`
> reports platform health and **additionally** the workspace-runtime section when
> run inside a workspace or given a `[name]` (§10.1, §12.1).

## 4.1 List Workspaces

```bash id="c8"
ai list
```

> There is a **single** `ai list` (§3.3) — it is the workspace inventory and also
> reflects each workspace's microVM runtime status. There is no separate
> microVM-only listing command.

---

## 4.2 Start Workspace

```bash id="c9"
ai start [<name>]
```

Behavior:

* starts the Microsandbox workspace microVM
* mounts the host workspace source
* injects `AI_PLATFORM_HOST` + service ports and the scoped **LiteLLM virtual
  key** into the workspace environment (the workspace holds no provider secret;
  keys-in-LiteLLM, architecture §17)
* applies the project's egress policy (the default posture — **public** by
  default; deny/unrestricted are opt-in — plus allow-listed host services +
  published ports, from the `network` block; architecture §29.4) as Microsandbox
  net-rules rendered at workspace create (`egress.MsbNetworkArgs` → `msb create`)
* initializes tooling

---

## 4.3 Stop Workspace

```bash id="c10"
ai stop [<name>]
```

Behavior:

* stops the microVM
* preserves state
* does not delete workspace data

---

## 4.3a Restart Workspace

```bash id="c10a"
ai restart [<name>]
```

Behavior:

* restarts the **existing** workspace — it stops the microVM (tolerating an
  already-stopped microVM) then runs the **full start path** again
  (`Manager.Restart` → `Manager.Start`), which **rebuilds the OCI image and
  recreates the microVM** (`Sandbox.Create`). This is deliberate: recreating
  re-derives the network / published-port set, so a restart picks up config
  changes — notably a newly added/removed in-VM app's host port, which `msb` only
  applies at create (a bare stop+start with no recreate would leave it
  unpublished). Provider/containerd/app setup is re-run.
* the persistent overlay and host source are **untouched** across the rebuild —
  installs and agent state survive (architecture §26)
* preserves state and refreshes the handle to `started` with a new
  `last_started`
* requires a workspace that was previously started; if none exists it fails with
  `ErrNotStarted` (exit `2`) and directs the user to `ai start` first

---

## 4.4 Destroy / Delete Workspace (consolidated)

```bash id="c11"
ai delete [<name>]            # `ai destroy` is an ALIAS of this
ai delete [<name>] --purge
```

`destroy` and `delete` are **one command** (`destroy` is a cobra alias of
`delete`): there is no separate "tear down the microVM but keep the project"
operation — use `ai stop` to pause a workspace and `ai start`/`ai restart` to
(re)build it. Behavior of delete:

* tears down the Microsandbox microVM (idempotent — a no-op if not running). If
  the teardown FAILS (an msb error or an odd VM state — but not a missing msb),
  the platform state is removed **anyway** (the user asked to delete) and a
  **warning** carries the manual cleanup (`msb remove -f aip-<name>`), rather than
  aborting and leaking `.ai-platform`.
* removes the project's **`.ai-platform` directory** (config + run state + the
  shared `{agents,skills,prompts,projects}` resource pool — the platform's
  footprint) and its persistent overlay (architecture §26)
* de-registers it from `config/projects.yaml`
* **keeps the user's OTHER files** in the directory; `--purge` additionally
  removes the WHOLE project directory
* the **per-CLI agent config folders** (`.opencode`/`.claude`/`.codex`/
  `.gemini`) and the `.venv-msb` virtualenv — which the platform wrote into the
  project dir at workspace start, outside `.ai-platform` — are removed too on a
  plain delete UNLESS kept with **`--keep-agent-config`** (or by answering the
  interactive prompt). When they are KEPT, their symlinks into the shared
  `.ai-platform` pool are **materialized into real files** before `.ai-platform`
  is removed, so nothing dangles. (`--purge` removes everything regardless.)
* **destructive** — on a TTY it interactively prompts for `--purge` (delete the
  whole project dir) and, when not purging, for removing those agent config
  folders; under `--json`/no-TTY it requires `--yes` (§20) and is driven by
  `--purge` / `--keep-agent-config`. `--dry-run` prints the side-effect-free plan.

---

## 4.5 Exec In Workspace

```bash id="c11a"
ai exec [<name>] -- <command> [args...]
```

Behavior:

* runs `<command>` inside the workspace via Microsandbox and
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

## 4.5a Interactive Shell (`ai shell`)

```bash
ai shell [<name>]               # defaults to the current directory's workspace
ai shell --session <s> [<name>] # attach to / create session <s> directly (no picker)
```

Opens an **interactive shell inside the running workspace microVM** — a real PTY
via `msb exec -t`, with the caller's stdin/stdout/stderr wired straight through
(distinct from `ai exec`, which is one-shot and buffered). It is
**interactive-only**: it owns the terminal and emits no JSON envelope, so under
`--json` or a non-TTY it is exit `2`.

On a TTY (no `--session`) it first **lists the workspace's existing sessions** and
prompts: **attach to one of them, or create a NEW session** (typing its name —
validated to letters/digits/`-`/`_`; the new-session name is pre-filled with the
default **`shell`**, so first use is a single Enter). The chosen session is opened
with one atomic **`tmux new-session -A`** (create-or-attach) in the interactive
exec — the tmux server daemonizes, so the session persists after a DETACH and is
listed by `ai sessions`. A new name is created; an existing one is reattached.
`--session
<s>` skips the picker and attaches/creates `<s>` directly (scriptable). Note that a
session persists when you **detach** (or close the terminal); typing `exit` ends
the session's shell and so removes the session. On a clean exit (the shell ends)
there is no stdout envelope, like `ai ui`. The same create-or-attach path backs the
TUI Workspace view's `e` key (via `tea.ExecProcess`) and the `ai create`
attach-to-existing flow. A platform failure (workspace not running, msb missing)
maps per §18 (3/4); the inner shell exiting is a clean end, not a failure.

---

## 4.5b Workspace Sessions (`ai agent` / `ai attach` / `ai sessions`)

```bash
ai agent <cli> [<name>]
ai attach [session] [<name>]
ai sessions [<name>]
ai sessions kill <session> [<name>]
# each [<name>] defaults to the current directory's workspace
```

The workspace runs a **tmux-transparent** session model: each interactive shell
and agent CLI lives in a **persistent, reattachable tmux session** inside the
microVM (all running as the `workspace` user, via `msb exec -t -u workspace`), so
work survives detaching and multiple agents run concurrently — but the user never
types a tmux command. A **managed `~/.tmux.conf`** is written at workspace start
(mouse-scroll on, status bar hidden, vi copy-keys, generous scrollback), so tmux
is invisible to a casual user.

**Terminal rendering for modern TUI agent CLIs (OpenCode, …).** The microVM is
**headless** — there is no terminal emulator inside it. Each agent runs inside
tmux over a PTY (`msb exec -t`), and its byte stream is rendered by the **user's
HOST terminal emulator** (WezTerm/Ghostty/iTerm/…); a modern emulator is
recommended for OpenCode. Correct rendering is therefore a matter of terminfo +
tmux config, not a VM-side emulator: every workspace image ships **`ncurses-term`**
(so the `tmux-256color` and other modern terminfo entries exist), and the managed
`~/.tmux.conf` sets `default-terminal "tmux-256color"`, `terminal-features
",*:RGB"` (24-bit truecolor passthrough), and `terminal-features ",*:extkeys"` +
`extended-keys on` (CSI-u / kitty extended keys, so OpenCode's shift+enter and
ctrl-combos survive the tmux layer) plus a short `escape-time`. These require
tmux ≥ 3.2 (`terminal-features`/`extended-keys`), which all four bases satisfy.
**Known limitation:** tmux does **not** pass GPU/graphics protocols (kitty
graphics, sixel) through, so a TUI's image-rendering features will not work inside
the tmux session.

* **`ai shell`** (§4.5a) **creates or attaches** a session in `~/project` — on a
  TTY it offers a picker (attach an existing session, or create a new one by name;
  default new name **`shell`**), or `--session <s>` to go straight to `<s>`. The
  session is opened with one atomic **`tmux new-session -A`** (create-or-attach;
  the daemonized server makes it persist after a detach + listed).
* **`ai agent <cli>`** starts (or reattaches to) a **per-CLI** session named after
  the CLI — `opencode`, `omp`, `claude-code` (runs `claude`), `codex`, `gemini`,
  `copilot`, `hermes` —
  so each agent has one durable session and several can run side by side. An
  **unknown `<cli>`** is exit `2` with the valid set listed.
* **`ai attach [session]`** attaches to an **existing** session (it never creates —
  that is `ai shell`'s job). With an explicit `[session]` it attaches directly; with
  none it **lists the workspace's sessions and prompts** for which to attach. When
  the workspace has **no sessions** it prints a hint pointing at **`ai shell`** to
  create one (exit `0`, nothing to attach to).

`ai agent` and `ai attach` are **interactive-only** (they own the terminal and
emit no JSON envelope), so under `--json` or a non-TTY they are exit `2`, exactly
like `ai shell`. `ai sessions` is a **normal command**: it lists the workspace's
sessions as a table (NAME / ATTACHED / IDLE) by default and the JSON envelope
under `--json`. A no-running-tmux-server workspace lists **zero** sessions, not an
error. **`ai sessions kill <session> [<name>]`** is a normal command that kills a
named session (over `Manager.KillSession`), emitting a confirmation result.
Platform failures (workspace not running, msb missing) map per §18 (3/4).
The TUI **Shell** tab (§14.4) is the session manager and drives the same paths
(attach/create via `ai attach`, delete via the kill path).
The interactive entry points (shell / agent / attach), the session list, **and the
in-VM apps listing** first verify the workspace is **running** — they read the
platform's lifecycle handle (set by start/stop), so a **stopped or never-started**
workspace fails fast with `ErrNotStarted` (exit 2, "workspace is not running — run
`ai start` first") rather than invoking `msb exec`, which HANGS on a stopped
microVM (and `msb exec -t` on a missing one can leave the terminal in raw mode).
The handle alone is not enough, though: it can say **"started"** while the real
microVM is gone (the VM was reaped, never finished booting, or msb lost it), in
which case every in-VM `msb exec` (tmux list-sessions, `nerdctl ps`) hangs until a
timeout. So the in-VM probes are **time-bounded** (`inVMProbeTimeout`), and on a
timeout/failure the manager runs a short, metadata-only **VM-liveness probe**
(`msb inspect`, `livenessProbeTimeout`) — NOT on the happy path, so a healthy
workspace pays no extra latency — to CLASSIFY the failure into a precise, actionable
error: a microVM that is **not actually present** → `ErrWorkspaceStale` (exit 4,
"workspace is marked started but its microVM isn't running (stale state) — run
`ai restart`"); a microVM that **is present but didn't answer in time** →
`ErrWorkspaceUnresponsive` (exit 4, "workspace is running but not responding — it
may be overloaded; try `ai restart`"). msb reporting **"sandbox not found"** (a
stale handle whose microVM was removed) is likewise classified as
`ErrWorkspaceStale`, not surfaced as a raw `tmux`/`nerdctl` exit. The TUI Shell/Apps
tabs surface these exact messages (no longer a blanket "may be busy — e.g. pulling
an image"), and the TUI's own fetch backstop is sized larger than the manager's
worst-case classification so the precise message always wins.

**Sleep recovery.** When the host sleeps, the hypervisor pauses the microVM; on wake
the first `msb exec` typically hangs while the vsock re-establishes, and the guest
clock has drifted. So each bounded in-VM probe is **retried once** on a timeout
(`inVMProbeAttempts`) — a fresh `msb exec` after the kill usually succeeds, so the
Apps/Shell views self-heal after a sleep instead of forcing a manual restart — and
**`ai start`/restart sync the guest clock** to host UTC (`Sandbox.SyncClock`, `date
-u -s` as root) so the post-sleep skew can't break in-VM TLS (e.g. an image pull's
cert validation). A microVM that stays wedged past the retries still reports
`ErrWorkspaceUnresponsive` (run `ai restart`). The
TUI Workspace view additionally guards its `e` shell key with an inline "workspace
not running — press s to start" hint, so it never suspends into a doomed subprocess.
Workspace creation also passes an explicit **Microsandbox idle timeout** sourced from
the project config (`<project>/.ai-platform/config.yaml`
`microsandbox.idle_timeout`, default `24h`, set at create time by
`ai create --idle-timeout <duration>` and editable later in the config). The value is
read on every `ai start`/`ai restart` and rendered as `msb create --idle-timeout
<value>`, so a development VM is not reaped after a short no-traffic window while the
host remains awake. This is a create-time runtime policy, not a heartbeat loop or host
power assertion: it performs no periodic work, does not tax CPU/battery, and does not
prevent the computer from sleeping. If the host sleeps, the VM sleeps with it and the
retry/clock-sync behaviour above handles wake-up.
The session launchers also verify **tmux is present in the workspace image** (a
buffered probe before the PTY); a tmux-less image — e.g. a project created with an
older `ai` whose template predates tmux — fails with `ErrTmuxMissing` (exit 3) and
remediation guidance (add `tmux` to the workspace's `.ai-platform/Dockerfile` and
restart, or recreate with an up-to-date `ai`), instead of msb's raw "failed to exec tmux"
leak (which also misreports a successful exit).

## 4.5c In-VM Apps (`ai apps`)

```bash
ai apps list [<name>]
ai apps <add|remove|update|start|stop|restart> <app> [<name>]
```

`ai apps` manages the opt-in AI applications that run as `nerdctl` containers
**inside** the workspace microVM (Open WebUI, AnythingLLM). The optional trailing
`[<name>]` resolves the workspace exactly like the other verbs (explicit name →
`--project` → cwd); `<app>` is validated against the manifest set
(`openwebui|anythingllm`).

* **`list`** — table (APP / STATUS / URL) by default, JSON envelope under `--json`.
  STATUS is `not installed` / `installed (stopped)` / `running`; URL is the app's
  host URL (`http://localhost:<port>`). `list` works against a **stopped**
  workspace (running status simply reports false).
* **`add`** — records the app, allocates its **unique host port**, and (when the
  microVM is running) starts the container. Because adding an app changes the
  microVM's published-port set — which `msb` only applies at create — `add` reports
  **restart required** (`ai restart`) to publish the host port.
* **`remove`** — stops/removes the container, unrecords the app, and frees its
  port (also restart-required to stop publishing).
* **`update`** — `nerdctl pull` the latest image and recreate the container.
* **`start` / `stop` / `restart`** — `nerdctl start|stop|restart` the container.

Each installed app is allocated a unique host port (recorded in the project
`config.yaml`'s `apps:` block) so two running microVMs never collide. The port
chain is `host:<port> → (msb -p <port>:<port>) → VM:<port> → (nerdctl -p
<port>:<containerPort>) → container`; the host and guest side use the same number,
reusing the `ai network publish` plumbing. App data persists on the workspace
overlay (`/persist/apps/<key>`). Each app is configured to route through the same
model gateway the agent CLIs use, with the workspace's scoped virtual key.

Exit codes (§18): invalid app/args → `2`; a lifecycle verb (`update`/`start`/
`stop`/`restart`, and `add` on a started workspace) that needs a **running**
workspace when none is running → `3`; runtime failures → `4`. The TUI **Apps**
view (§14) drives the same path via `ai apps`.

### In-workspace agent CLI provider config — keyless per-CLI project configs, key in-VM only

All **eight** gateway-capable agent CLIs (every CLI except forced-OAuth Copilot) route through the LiteLLM gateway **by default**. At every
workspace start/restart `workspace.registerAgentProviders` (over `internal/agentcfg`)
writes each CLI's provider config at **that CLI's own default per-project location**
inside the bind-mounted project dir (`<project>/…` on host = `/home/workspace/project/…`
in-VM). Every on-disk config is **KEYLESS** (the scoped virtual key is env-supplied, never
on disk) and any pre-existing file is **deep-merged** so the managed block wins while the
user's other keys survive:

* **opencode** → `<project>/.opencode/opencode.json` (`apiKey: "{env:AIP_GATEWAY_KEY}"`);
  the in-VM agent env file exports `OPENCODE_CONFIG` pointing opencode at this file. It
  carries the per-request Headroom knobs on every model.
* **claude-code** → `<project>/.claude/settings.json` — an `env` block setting only
  `ANTHROPIC_BASE_URL` (the gateway root — LiteLLM's Anthropic-compatible surface, it
  appends `/v1/messages`); the bearer token stays in the exported `ANTHROPIC_AUTH_TOKEN`
  env var (settings.json has no `${VAR}` interpolation), so the file is keyless.
* **codex** → `<project>/.codex/config.toml` — a keyless `[model_providers.aip-gateway]`
  block (`base_url = "<gateway>/v1"`, `wire_api = "responses"` — codex requires the OpenAI
  Responses API, which LiteLLM exposes — `env_key = "AIP_GATEWAY_KEY"`) PLUS a global in-VM
  `~/.codex/config.toml` trust entry (`[projects."/home/workspace/project"]
  trust_level = "trusted"`, off host disk) so codex loads the project config.
* **gemini** → **env-only** (no settings key for a base URL exists): `GOOGLE_GEMINI_BASE_URL`
  (the gateway root, honoured by the `@google/genai` SDK) + `GEMINI_API_KEY`, via the in-VM
  agent env file.

**The scoped virtual key (and any real key) is NEVER written to host disk.** It lives
ONLY in the in-VM agent env file `~/.config/aip/agent-env.sh` (`Sandbox.WriteFile`, sourced
by every shell + agent session), which exports `AIP_GATEWAY_KEY` (opencode/codex),
`ANTHROPIC_AUTH_TOKEN` (claude-code), and `GEMINI_API_KEY` (gemini). The on-disk configs
reference the key by env interpolation (`{env:}`/`$VAR`), codex via `env_key`, claude via
the exported token — all keyless. A user's edits to the on-disk configs survive restart
(present files are deep-merged, never clobbered). The `<project>/.ai-platform/{agents,
skills,prompts,projects}` shared resource pool is symlinked into each installed CLI's
per-project dir at start (repo-layout §12.1c). *(hardware bring-up: opencode honouring
`.opencode/opencode.json` via `OPENCODE_CONFIG`; codex loading the trusted project config;
the symlinks resolving in-VM — all not yet verified live.)*

### In-workspace `refresh-models` — re-pull the model picker without restarting

At workspace start the platform installs a self-contained **`refresh-models`**
command on `PATH` inside the microVM (`/usr/local/bin/refresh-models`, generated
per-workspace by `agentcfg.RefreshScript` and installed via `sudo install -m 0755`).
Run it **inside the workspace** after changing the served models on the host (add a
provider key with `ai keys add …`, or pull/remove an Ollama model with
`ai models pull`/`rm`) to re-pull the in-VM agent model picker **without restarting
the microVM**:

```bash
refresh-models      # run from any workspace session (ai shell / ai agent)
```

It re-fetches the models the gateway currently **serves** (its DB-backed models)
from the gateway's `/v1/models` endpoint — authenticated with the workspace's scoped
virtual key — dedups + sorts them **exactly** as a fresh workspace start does, and
rewrites the **opencode** PROJECT config
(`/home/workspace/project/.opencode/opencode.json`) **in place, byte-identical** to the canonical
config a fresh start produces — and **KEYLESS** (the `{env:}`/`$VAR` key refs are
preserved; the fetched key authenticates the `/v1/models` call only, never entering the
rewritten files). Restart the agent CLI afterwards to pick up the new list. (It rewrites
the canonical opencode config only; any user deep-merge edits and the env-routed CLIs
— claude-code/codex/gemini, whose served model set is discovered at request time, not
baked — are re-applied on the next workspace start.) It **degrades**: if the gateway is unreachable it leaves the
existing configs **untouched** (it never wipes them to an empty list) and exits
non-zero with a warning; a missing `curl` (image without it) errors clearly. The
host-side generation and the script's own logic are unit-tested (the generated
script is executed against a fake `curl`); **live in-VM execution is a
`hardware bring-up` verification item.**

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
* all other exit semantics (missing dependency → `3`, runtime failure → `4`,
  restart-without-existing-workspace → `2` per §4.3a) are unchanged

---

# 5. Multi-Agent Orchestration — Removed (not a platform concern)

The platform does **not** manage agent identities, branches, worktrees, commits,
merges, or rebases — running multiple AI agents on a project and any git they need
is the **in-workspace agent CLI's** job, not the platform's (architecture §20–22).
The `[S3]` orchestration slice is retired. Note this is NOT the `ai agent` /
`ai attach` / `ai sessions` commands (§4.5b): those DO exist and are thin
tmux-transparent launchers/reattachers for an in-workspace agent-CLI session — a
session surface, not an agent-orchestration system.

---

# 6. Git Commands — Removed

The platform exposes **no git commands** in its CLI surface; branching, merging,
committing, and conflict resolution are the user's and the in-workspace agent's job.
The one exception is internal, not a command: at every **workspace start** the platform
runs `git init` on a non-git project (right before Graphify registration, so the
Graphify git hook has a repo — §3.1, architecture §12/§21). It never clones, adds a
remote, or manages worktrees.

---

# 7. Environment (Dockerfile)

There are **no snapshot commands** — no `ai snapshot`, no environment
upgrade/rollback verbs. A project's environment is its
`<project>/.ai-platform/Dockerfile` (architecture §25):

* to change the environment, edit `.ai-platform/Dockerfile` and recreate the
  workspace (`ai restart`, which rebuilds)
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
* the default model — there is **no** built-in default in the catalog-driven model
  system (`DefaultRouting()` is the zero `Routing`), so the default line is omitted
  unless platform config ever sets one; the gateway's model-list endpoints do not
  mark a default
* the **LIVE** list of models the gateway currently serves, sourced from LiteLLM's
  own endpoints (`/model/info`, falling back to `/v1/models`) — **not** a hardcoded
  list. The list is the gateway's **DB-backed** served models: a keyed provider's
  registered models.dev catalog models plus the registered Ollama models
  (`ollama/<name>`), each shown with its provider and (when `/model/info` exposes it)
  its mode. The **providers** line is DERIVED from this live list (the distinct
  provider prefixes), not from any hardcoded routing.
* the local-model (Ollama — no key needed) vs cloud-provider (each needs a key
  via `ai keys add <provider>`) split
* a `ai models test <model>` next-step hint

The model-list call authenticates with the gateway master key (read from the
running container, same as `ai models test`). If the gateway is reachable but the
model-list call fails (e.g. unauthorized), the command still reports health and
shows a note rather than erroring out.

The `--json` envelope carries the underlying fields (`healthy`, `providers`
[live-derived], `default`, `ollama`, `models` [the live served list of
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

## 8.3 Local Model Store (Ollama)

`ai models status` / `ai models test` describe and probe **LiteLLM routing**. The
commands below instead manage the **local Ollama store** directly over its HTTP API
— the local-model backend LiteLLM routes `ollama/<name>` models to. (The rendered
LiteLLM config carries **no** `model_list` and **no** per-provider wildcards — the
served model set is **DB-backed**, §14.) Installing a model is a two-part act:
`ai models pull` downloads it into the Ollama store **and** registers it as a
DB-backed model in the gateway (public `model_name` `ollama/<name>`, via
`litellm.RegisterOllamaModel`), so it becomes routable immediately; `ai models rm`
removes it from the store **and** unregisters it (`UnregisterOllamaModel`). Merely
registering a model in the gateway is **not** the same as having it installed
locally — `pull` is what downloads the weights. The pure store commands (`list` /
`popular` / `show`) do not touch the gateway registration.

If Ollama is unreachable, these exit **3** (missing dependency) with a hint to run
`ai services start ollama` (or `ai setup`); bad input exits **2**; other failures
exit **4**.

### 8.3.1 List

```bash id="c23a"
ai models list
```

Lists the **installed** models in the local Ollama store (`GET /api/tags`). There
is **no hardcoded catalog** — installable suggestions live behind `ai models
popular` (§8.3.2). The human output is a NAME / SIZE / PARAMS table; `--json`
returns the installed list (each entry carries `installed: true`).

### 8.3.2 Popular (installable, live library)

```bash id="c23p"
ai models popular
```

Lists **installable** models **scraped from the live ollama.com library**
(`internal/ollama/library.go`, `ollama.Library()`): a GET of the
`https://ollama.com/library` index enumerates every model (name + description),
then each model's `https://ollama.com/library/<model>/tags` table is fetched
(bounded concurrency) for the per-variant **size / context / input**. The result is
**cached as YAML** at `~/.ai-platform/cache/ollama-models.yaml`; when ollama.com is
unreachable the **cached copy is used**, and with no cache the command errors only
when nothing is available. There is **no bundled `models.yaml` and no offline
fallback set**. (The old third-party `ollama-models.zwz.workers.dev` JSON endpoint
was stale and has been removed.)

Each library model carries its pullable **tags** (variants, e.g. `7b`, `72b`), each
with a scraped `size` / `context` / `input` — pull a specific variant with
`ai models pull <name>:<tag>`. For each entry it reports:

* **name** — the base model name (e.g. `qwen2.5`)
* **size / context / input** — the default (`latest`, else first) tag's values from
  the model's ollama.com /tags table; a `—` marks a column the table omits
* the **repo link** — **derived** as `https://ollama.com/library/<name>` (the index
  carries no `repo_url`)

The human output is a NAME / SIZE / CONTEXT / INPUT / REPO table; `--json` returns
the structured list (each tag carries `{name,size,context,input}`). `ai models pull`
always also accepts a free-text reference, so an out-of-date or unreachable library
never blocks pulling anything.

### 8.3.3 Pull (install / update)

```bash id="c23b"
ai models pull [name...]
```

`pull` is **variadic** — it installs **one or more** models in a single run.

* With one or more `name` arguments, or under `--json` / no TTY: each given
  reference is pulled in turn (the **custom-reference** path — e.g.
  `llama3.2:3b qwen2.5:7b`, or a custom ref like `hf.co/user/model`). Under
  `--json` at least one name is **required** (none → exit 2). Pull stays
  **free-form** — any model reference can be pulled, listed or not. Names are
  de-duplicated; the run **continues past a failure** and reports a per-model
  summary, exiting non-zero (mapped from the last failure) if any failed.
* On a terminal with **no** arguments: the user gets a **checkbox multi-select** of
  the live library's pullable `name:tag` references (§8.3.2 — one option per tag,
  e.g. `qwen2.5:7b`), plus a final **"✎ enter custom model(s)…"** checkbox that, when
  ticked, prompts for free-text references (space- or comma-separated). If the
  library is unavailable (and uncached) it falls back to just the custom-entry
  prompt — the picker never blocks pulling.

The pull **streams** Ollama's NDJSON progress while a spinner shows ongoing work.
There is **no separate update verb** — re-pulling an installed model updates it.

The `--json` envelope carries `data.pulled`, one `{model, ok, error}` outcome per
requested model.

### 8.3.4 Remove

```bash id="c23c"
ai models rm [name]
```

Removes a model from the local store (`DELETE /api/delete`, body `{"model":…}`). On
a terminal with no argument the user picks from the **installed** models; with an
argument (or under `--json`) that name is removed. On a terminal the user is asked
to **confirm** before deleting. A model not in the store → a clear not-found error
(exit 2).

### 8.3.5 Show

```bash id="c23d"
ai models show <name>
```

Shows metadata for a local model (`POST /api/show`): parameter size, quantization,
family, format, and capabilities.

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
must be passed. The strategy is kept per project (default `balanced`). Headroom runs
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

* **Platform dependencies** — Microsandbox runtime + host virtualization (Apple
  Silicon / KVM), Docker/Podman
* **All service-tier services** — Ollama, Presidio (analyzer + anonymizer back
  LiteLLM's opt-in `secret-masking` guardrail; reconciled only when it is enabled;
  architecture §15), LiteLLM (health + that the configured provider keys are
  present in the gateway — a missing key is warned, not fatal; architecture §17),
  Headroom (the input-compression service LiteLLM calls as a `pre_call` guardrail),
  the nginx proxy, and DNS (there are no optional host services)

**Additionally**, it reports the **workspace-runtime** section (per-workspace
runtime / virtualization check, §12.1) **only** when run inside a workspace
directory (cwd or an ancestor) or given a `[<name>]`. There is no separate
workspace doctor command.

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
containers `dns`, `ollama`, `presidio`, `valkey`, `redisinsight`, `litellm`,
`headroom`, and `proxy` (there are no optional host services). The user never
invokes `docker compose`, `launchctl`, or `systemctl` directly. The whole service
tier runs as containers (see architecture §5, "Host Services Control Plane").
(The Microsandbox workspace runtime is not a long-running service — it is driven
by the top-level workspace verbs, not `ai services`.)

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

`<service>`: `ollama` | `presidio` | `litellm` | `headroom` | `proxy` | `dns` |
`valkey` | `redisinsight` | `all` (no arg = all). All eight are **core** (always on).
The optional host-service set is currently **empty**, so `enable`/`disable` have
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
  is still listed but reads **`disabled`** (surfaced, not probed).
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
  `--print` (and always under `--json`) — prints the URL instead. Host consoles are
  **nginx subdomain UIs** served on the single gateway port `:18787`:
  `litellm.<domain>:18787/ui/login` (the LiteLLM admin UI) and `valkey.<domain>:18787`
  (RedisInsight, the Valkey cache GUI) — where `<domain>` is the platform base domain
  (`ai domain`, default `aip.local`). These are **not** direct container ports. (Open
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

# 10a. Network (workspace egress policy)

```bash id="c27b"
ai network show      [project]                              # show the egress policy
ai network egress    [deny|public|unrestricted] [project]  # set the default posture
ai network allow     [host[:port]] [project]               # allow an external destination
ai network disallow  [host[:port]] [project]               # revoke an allowed destination
ai network publish   [guest:host] [project]                # publish a workspace port
ai network unpublish [guest:host] [project]                # unpublish a workspace port
ai network log       [project] [--tail N]                  # attempted-egress-by-name audit (host-wide)
```

Each verb takes an optional trailing `[project]` (after its own value) which, like
every project-scoped command, resolves the target project (positional → `--project`
→ the workspace owning the current directory; §17).

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
all tcp/443) plus a custom `host[:port]` entry; `disallow` / `publish` /
`unpublish` (no arg) likewise prompt for the value. With `--json` or no TTY, the
value must be passed as an argument.

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
* `egress` sets the default outbound posture: **public** (default — open
  internet, private ranges still blocked, every name DNS-audited via `ai network
  log` and re-lockable per project), **deny** (only the model gateway + allowed
  services — most locked-down), **unrestricted** (allow everything). A project
  whose `network.egress` is unset resolves to **public**.
* `allow <host[:port]>` adds an allowed host service the workspace may reach — a
  database, Kafka broker, or a specific API/domain. `host` may be a
  hostname/IP/domain, a `*.suffix` wildcard (e.g. `*.npmjs.org`), or the
  `gateway` token for a service on the host machine. The port is **optional**
  and **defaults to 443 (HTTPS)**, so a bare domain like `api.github.com` allows
  it on 443. **`disallow <host[:port]>`** revokes it.
* `publish <guest:host>` publishes a workspace (guest) port to a host port — the
  guest port inside the workspace followed by the host port it is reachable at
  (e.g. `3000:3000`); **`unpublish <guest:host>`** undoes it.
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
  Enforcement is the msb net-rules in `ai network show`, and under a `deny`
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
ai gateway set [host[:port]]  # route every workspace here through a remote gateway (client mode)
ai gateway clear              # back to the local standalone gateway
```

The address is a **bare host or `host:port`** (NOT a URL). The default port is
`18787` (the nginx gateway port). Resolution:

* empty → `host.microsandbox.internal:18787` (standalone/local — the in-VM name for
  the host machine);
* `host` → that host on port `18787`;
* `host:port` → that host and port.

microVMs reach the gateway at `http://<host>:<port>/v1` (the `/v1` suffix opencode
requires), and the workspace egress policy always allows `<host>:tcp:<port>`.

* `show`/`clear` take no arguments. `set` takes an **optional** address: on a
  terminal it always prompts (pre-seeded with any address passed); under `--json` /
  no TTY the address is **required** (missing → exit 2).
* a missing `runtime.yaml` (no `ai setup` yet) → exit `3` with a "run `ai setup`
  first" note; an invalid address (empty, whitespace, a scheme/slash, or a
  non-numeric port) → exit `2`; a failed persist/read → exit `4`.

Command names in the envelope are `gateway.show` / `gateway.set` / `gateway.clear`.
Human-readable output by default; `--json` emits the standard §19 envelope.

---

# 10.5 Platform Base Domain (`ai domain`)

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

There is no separate workspace doctor command. The per-workspace
runtime/virtualization check is a **section of the consolidated `ai doctor`**
(§10.1): it appears **only** when `ai doctor` is run inside a workspace directory
(cwd or an ancestor) or given a `[<name>]`. When run outside any workspace and
with no `[<name>]`, `ai doctor` reports platform + service-tier health only.
`[<name>]` defaults to the current directory's workspace.

---

# 13. Logging Commands

## 13.1 View Logs

```bash id="c31"
ai logs
```

Options:

```bash id="c32"
--workspace <project>
--service <microsandbox|ollama|presidio|valkey|redisinsight|litellm|headroom|proxy|dns>
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
command matches. Available themes wrap huh's built-ins (`default` (charm),
`dracula`, `catppuccin`, `base16`, `monochrome`) plus custom palettes (`orange`,
`orange-blue`, `synthwave`, `cyberpunk`, `tokyo-night`, `vaporwave`, `tron`,
`nord`, `gruvbox`, `onedark`) — run `ai theme` to list them all.

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

**Top-level tabs** (Services · Projects (Workspaces) · Local Models · Cloud Models ·
API Keys · Settings; cycled by `tab`/`⇧tab`/`←→` or jumped to directly with the
number keys `1`-`9`. There is **no `:` command palette** — `q`/`ctrl+c` quits and
`?` toggles the key-binding help):

* **Services** — a **two-level drill-down** mirroring the Workspaces hub. It opens
  on the *list* (live service-tier + container status: SERVICE / MODE / STATE /
  HEALTH / ADDRESS); `e` enables/disables an **optional** service from the list
  (toggling on its current state; core services are always on), `r` refreshes, and
  `enter`/`d` **drills into a per-service DETAIL** (`esc` backs out to the list).
  The detail shows a fixed **summary** (name / mode / state / health / address /
  console) on top with the **live container log embedded beneath it** — the SAME
  scrollable, READ-ONLY, selectable, auto-refreshing component the Workspace tab
  embeds (the generic `LogView`), here tailing the container via
  `setup.ServiceLogTail` (`<runtime> logs --tail <n> <container>` — the service-tier
  analogue of the workspace log's `msb logs --tail`; a multi-container service like
  Presidio concatenates per-container sections under a `── <container> ──` heading).
  The log shows only while the service is RUNNING (a stopped service shows a "not
  running" hint, not stale output), follows the tail unless scrolled up, passes
  through the terminal-output normalizer, and PAUSES while the Services tab is not
  the visible top-level tab or the user has backed out to the list. In the detail,
  `s`/`x`/`r` start/stop/restart THIS service **in place** (an animated braille
  spinner on the state line while in flight, then a refresh — they do **not** pop
  back to the list), `p` (update) re-pulls + recreates the service via the **live
  embedded terminal overlay** (`ai services update <svc>`, whose CLI pull spinner
  shows the image-pull progress) and stays in the detail, `o` opens its admin
  console, and the remaining keys drive the embedded log: `↑/↓`/`PgUp`/`PgDn` scroll,
  `f`/`enter` follows the container log live in the REAL terminal
  (`ai logs --service <svc> --follow` → `<runtime> logs -f`). There is no separate
  Logs tab; the file-backed `internal/logs` reader still backs `ai logs`.
* **Projects** — a **two-level hub**. It opens on the *switcher*: every project
  (name / OS / workspace status / agents); `enter` opens one, `n` starts a
  **fully in-TUI, multi-step create wizard** (no subprocess — it runs the shared
  `create.Execute` IN-PROCESS, so it behaves identically to `ai create`), `d`
  describes the selected one. The wizard walks native `ai ui` widgets: a
  **location field** starting in the current directory (a text input above a
  live **dropdown of the folders** under the entered path — type to filter it and
  `tab` to complete the highlighted folder, or `↓`/`↑` to select a folder row;
  folders that are ALREADY a workspace are **greyed out and not selectable** with a
  "workspace already exists" note; a leading `~` expands to home; the entered path
  is validated by the create rules — a path that IS or is NESTED INSIDE an existing
  workspace is rejected — and a non-existent path is created with intermediate
  folders), then **text inputs** (name / cpus / memory / ports / idle timeout),
  **`listWindow` single/multi-selects** (OS / agent CLIs / default agent CLI /
  stacks / apps), and a **Graphify model picker** mirroring the Local Models pane.
  On confirm it runs `create.Execute` off the event loop and refreshes the hub.
  Opening a project drops
  INTO it, revealing a **sub-tab bar** for that project (the project name + the
  sub-tabs below). `tab`/`←→` cycle the sub-tabs; the focused sub-view's pane is
  acted on directly; `esc` backs UP to the switcher. The per-project sub-tabs are:
  * **Workspace** — a fixed summary block (name / OS / agents / workspace status /
    path) and a live **Sandbox Configuration** diagnostics block (the SDK
    `SandboxConfig`: image, memory, vcpus, workdir, user, idle timeout, detached,
    published ports, egress default + rule count, dns — `Manager.WorkspaceConfig`).
    The pane is **scrollable** (`↑/↓`/`PgUp`/`PgDn`) so the summary + configuration
    stay reachable when they overflow. Workspace lifecycle is `s`/`x`/`r`/`d`
    (start/stop/restart/delete) and `e` (an interactive shell). `s`/`x`/`r` run
    **DETACHED** from the TUI: the app spawns
    `ai <action> <name>` in its own session (`setsid` + `Process.Release`) with its
    stdout+stderr **tee'd to `<project>/.ai-platform/run/<action>.log`** (the build
    log; falling back to `/dev/null` only if that file can't be opened) so the microVM
    build/boot keeps running even if `ai ui` is closed, then shows an **animated
    spinner** on the workspace status line and polls the state handle until the target
    status (or a ~6m timeout). The TUI stays navigable.
    `d` (delete) runs in the confirm **terminal overlay** (see below). `e` opens the
    shell in the REAL terminal (see Shell, below).
  * **Logs** — the workspace log as its own sub-tab: a READ-ONLY, scrollable,
    selectable view of the microVM's captured output. On the SDK backend it STREAMS
    (`Manager.OpenWorkspaceLogStream` → one relay-free `LogStream{Follow:true}`:
    history first, then new entries pushed — no polling); on the CLI backend it falls
    back to the `Manager.WorkspaceLogTail` ~2s poll. Before the microVM exists (a
    detached start/restart still building), it falls back to the tee'd **build log**
    at `.ai-platform/run/<action>.log` so the build output is visible. `Manager.Start`
    emits `▸` step-progress lines (image build → boot → agent providers → containerd →
    venv → Graphify → Caveman) into that log, and `readLatestLifecycleLog` appends the
    detached `run/caveman-install.log`, so the full setup sequence + background Caveman
    install are visible in the Logs view. The high-volume
    agent-relay connect/disconnect lines are **hidden by default**; `d` toggles them
    on/off (the hint shows `d debug (on|off)`). `f`/`enter` follows live in the real
    terminal.
  * **Metrics** — live sandbox metrics streamed from `sb.MetricsStream` into a table
    (CPU / memory / disk / net / uptime), updated ~2s, over the single reused relay
    handle.
  * **Network** — egress mode + allow-list + published ports, managed inline:
    `m` cycles the mode, `a` allow · `d` disallow a destination, `p` publish · `u`
    unpublish a port (the value is typed at an inline prompt). CLI mirror:
    `ai network egress`/`allow`/`disallow`/`publish`/`unpublish`.
  * **Context** — Headroom strategy + Caveman level; `s`/`c` cycle them.
  * **Shell** — the per-workspace **session manager** over the tmux sessions
    (NAME / ATTACHED / IDLE): `enter`/`a` attach the selected session, `n` opens an
    inline prompt to create a new named session, `d`/`k` kill the selected one, `r`
    refreshes. Attaching/creating runs the interactive shell in the user's **REAL
    terminal** via `tea.ExecProcess` (the TUI suspends and runs `ai attach <session>
    <name>` / `ai shell <name>`, restoring on exit) — NOT an embedded emulator — so
    keys, native text selection, and in-place output all work. CLI mirror: `ai shell`
    / `ai attach` / `ai agent` / `ai sessions` (+ `ai sessions kill`).
  * **Apps** — the in-VM AI apps (install/start/stop/restart/remove).

  The **workspace log** lives in the **Logs** sub-tab (above): a READ-ONLY
  (no command input), scrollable, selectable view of the microVM's captured output,
  following the tail unless scrolled up and freezing while scrolled up. On the SDK
  backend it STREAMS — one `LogStream` delivers recent history then pushes new entries
  (re-armed `Recv` commands, generation-guarded + cancelable-ctx teardown, over the
  relay-free host log channel so it costs no agent-relay client); on the CLI backend
  it falls back to the `Manager.WorkspaceLogTail` ~2s poll. The text is passed through
  a terminal-output normalizer (interpreting `\r`/cursor-moves/erase-line) so progress
  redraws (image pulls) collapse IN PLACE while the full scrollback + plain selectable
  text are kept; the normalize pass runs off the bubbletea event loop. The view only
  shows the log while the workspace is RUNNING (a STOPPED workspace shows a "not
  running" hint, not stale output) and the stream is closed when the sub-tab is not
  visible / on workspace switch. `f`/`enter` opens a live follow in the REAL terminal.
  `ai ui` captures the mouse so the top-level tab bar and the sub-tab bars
  (Workspaces hub, Services detail) are clickable; native text selection then needs
  the terminal's selection modifier (Shift on most terminals, Option on macOS).
  Mouse capture is a persisted user setting (default ON, stored in
  `~/.ai-platform/config/ui.yaml` alongside the theme): the Settings tab's `m` key
  toggles it live and persists it across restarts, and with it OFF native text
  selection needs no modifier. When mouse capture is ON the wheel scrolls the
  focused list/table/viewport (one row per notch); keyboard scroll (PgUp/PgDn/
  arrows) works either way.
* **Local Models** — the local Ollama store ⨯ the **live ollama.com installable
  library**, in one **NAME · DESCRIPTION** list (there is **no TAGS column** — tags
  appear only in the per-model drill-down) split into an **Installed** section
  (library models with ≥1 pulled tag, plus installed customs) and an **Installable**
  section (the rest of the library). It is rendered as a **custom viewport-windowed
  list (NOT a bubbles table)** so each section header can be bold + accent-coloured
  with blank-line padding. `enter` opens a per-model **tag drill-down**: `space`
  ticks 1+ NOT-installed tags, `enter`/`p` pulls the ticked tags, `d` removes the
  installed tag under the cursor, `t` tests it (and `enter` on an installed tag opens
  its `/api/show` detail); `esc` backs out to the list. List keys: `enter` manage
  tags · `t` test (first installed tag) · `d` remove (first installed tag) · `r`
  refresh. When the library endpoint is unreachable the cached copy is used and a
  source-availability message is flashed; with no cache the Installable section is
  empty with that message.
* **Cloud Models** — the **models.dev catalog** ⨯ the gateway's live registered set,
  in a MODEL · PROVIDER · STATUS · CONTEXT table (registered-first). It shows ONLY
  cloud providers — local `ollama/<name>` models are EXCLUDED (they live on the Local
  Models tab). `enter` opens the catalog metadata in a describe pane; `t` tests a
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
**delete** (its confirm prompt), the **Apps** lifecycle actions, **models**
pull/rm, and **API Keys** add/remove (its hidden key prompt shows there). While it
is open, keys go to the program; **ctrl+q** force-detaches, and once the program
exits **any key** closes the overlay (the affected views refresh on close).

Workspace **start/stop/restart** do NOT use this overlay — they run **DETACHED**
(`setsid` + `Process.Release`, stdout+stderr tee'd to
`.ai-platform/run/<action>.log`) with a spinner on the Workspace
sub-tab's status line (above), so there is no log/terminal pane for them and the
build/boot survives closing `ai ui`. The interactive **sessions** (shell/agent/
attach) likewise bypass the embedded emulator: they run in the user's **REAL
terminal** via `tea.ExecProcess` (the TUI suspends, then restores on exit) so keys,
native text selection, and in-place output all work. The in-VM behavior of
`msb exec -t` over the session PTY is verified during hardware bring-up.

Navigation is **keyboard-only**: `tab`/`⇧tab`/`←→` cycle the top-level tabs and
the number keys `1`-`9` jump straight to one (there is no `:` command palette).
On the model/Projects list views `d`/`enter` opens a scrollable describe pane; on
the **Services** list `enter`/`d` drills into the per-service detail (its embedded
container log). **`esc` backs out one level** everywhere
it makes sense — it closes a describe pane, backs an open project / open service
detail out to its list, closes the menu/help overlay, or cancels the create flow,
returning to the level above; at the top level it is a no-op (`q` quits). `?` shows
the key bindings. The UI honours the theme set by `ai theme`
(§14.3). The embedded workspace/container **logs** auto-refresh from the live
`msb logs` / `<runtime> logs` tail; the live execution of those tails against a
running runtime is exercised during hardware bring-up.

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
  provider + the installed Ollama models); `remove` deletes the key then re-syncs
  (its models drop out)
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

### Project resolution

Project-scoped commands (the workspace lifecycle verbs `start`/`stop`/`restart`/
`exec`/`shell`/`agent`/`attach`/`sessions`/`delete` (`destroy` alias), `context *`,
`network *`, `doctor`, `logs --workspace`) resolve their target project with this
precedence:

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
`ai exec`, `ai start`, …).

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
| missing Docker/Podman/Microsandbox (`doctor`, `setup`) | 3 |
| unknown project / agent / OS key | 2 |
| Microsandbox or runtime operation failed | 4 |
| rootless required but unavailable | 4 |
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

* `ai delete` (tears down the microVM and removes the project's `.ai-platform`
  state + overlay + index entry — keeping the user's other files — and, with
  `--purge`, the whole project directory). `ai destroy` is a **cobra alias** of
  `ai delete` (the same destructive command, §4.4), so it gates identically.

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
