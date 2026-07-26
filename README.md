# Stack Genie - the local AI Development Platform

Reproducible, isolated, AI-powered development environments. Each workspace is a
hardware-isolated [Microsandbox](https://microsandbox.dev) microVM; the model
path and guardrails run as a shared host container tier. One Go binary, `ai`, is
the entire control plane.

**Model path:** agent → nginx gateway → LiteLLM → containerized Ollama or a cloud
provider. LiteLLM applies a **user-selectable guardrail set** chosen at `ai setup`:
the **Headroom** input-compression guardrail (LiteLLM POSTs the request to
`aip-headroom:8787/v1/compress` in-process) is on by default, while Presidio
secret-masking, detect-secrets, and a destructive-command tool-firewall are
opt-in — Headroom is a compression service LiteLLM calls, not an nginx hop. A
single nginx reverse proxy (`aip-proxy`) is the only host entry to
the service tier — everything else runs internal-only on the `aip-net` network.
Real provider API keys live only in the LiteLLM gateway (keys-in-LiteLLM),
encrypted in its DB and added with `ai keys` — never on platform disk, in
`config.yaml`, or in the workspace; the agent holds a scoped virtual key. Models
are DB-backed and catalog-driven: adding a provider key registers that provider's
[models.dev](https://models.dev) catalog models into the gateway (removing it
unregisters them), and `ai models pull` registers local Ollama models — there is
no built-in default model.
Workspace egress is a Microsandbox NetworkPolicy you configure with `ai network`;
it defaults to **public** (DNS-audited outbound, private ranges blocked) so the
in-VM container runtime can pull images and is re-lockable to default-deny per
project. Caveman is a per-project output-compression skill inside the workspace.

## Install

```bash
curl -fsSL https://github.com/jt-helsinki/stack-genie/releases/latest/download/install.sh | bash
```

Fetches the installer (published with each release), which downloads the
`ai-<os>-<arch>` binary from the latest GitHub release, verifies it against the
release `SHA256SUMS`, and adds it to your PATH. Use `bash`, not `sh`. A piped
install can't change your *current* shell's PATH — open a new shell, then run
`ai setup`. Overrides: `AIP_INSTALL_DIR`, `AIP_VERSION`, `AIP_RELEASE_BASE_URL`,
`AIP_NO_MODIFY_PATH=1`, `AIP_NO_VERIFY=1`.

## Prerequisites

`ai` orchestrates external tools rather than bundling them — install these
yourself:

| Tool | Role | Install |
|------|------|---------|
| Docker or Podman (rootless) | service tier | `brew install --cask docker` (or `brew install podman`) |
| [Ghostty](https://ghostty.org) | terminal emulator (recommended) | `brew install --cask ghostty` |

You do **not** install Microsandbox (`msb`) yourself: the platform pins the `msb`
CLI to the exact build of the embedded Microsandbox SDK and downloads it
(sha256-verified) into `~/.ai-platform/bin/msb` on first workspace build. This keeps
the CLI and the SDK's in-process runtime in lockstep — installing `msb` separately
(e.g. from a different source) can desync their shared `~/.microsandbox` DB schema
and break workspace loads.

Supported host: **macOS on Apple Silicon** (Microsandbox needs the Apple
Hypervisor) or **Linux with KVM**.

> **Use [Ghostty](https://ghostty.org).** Workspace microVMs are headless — the
> agent CLIs (OpenCode, etc.) run inside the VM and their terminal output is
> rendered by *your* terminal emulator. A modern, truecolor- and CSI-u-capable
> emulator is required for these TUIs to display correctly; **Ghostty is the
> recommended choice** (WezTerm or a recent iTerm2 also work). The stock macOS
> Terminal.app will not render them correctly.

`ai setup` **never installs software** — it detects what's missing and prints how
to install it (command + web address); `ai doctor` reports the same anytime. The
service tier (the nginx gateway, Ollama, Presidio, LiteLLM + Postgres, Headroom,
Valkey + its RedisInsight GUI, and the DNS egress-audit resolver) is launched by
`ai setup` as host containers on the `aip-net` network — you don't install those.
Only the nginx gateway (`aip-proxy`) publishes a host port (`:18787`); every other
service is internal-only and reached through it. The host UIs are served as
Host-based subdomains off a platform base domain (default `aip.local`, set with
`ai domain`), both on `:18787`: `litellm.<domain>` (the LiteLLM admin UI) and
`valkey.<domain>` (the RedisInsight GUI for the LiteLLM response cache). (Open WebUI
is now a per-workspace in-VM app, run with `ai apps`; Odysseus has been removed.)
In standalone mode `ai setup` offers to add the matching `/etc/hosts`
entries; in server mode it prints the DNS + TLS contract for an operator.

## Set up

```bash
ai setup     # idempotent: preflight, create ~/.ai-platform, start the service tier (streams progress)
ai doctor    # dependency + health report, each gap with a fix
```

On a TTY, `ai setup` asks for a **deployment role** (or pass `--mode`): `standalone`
(default — full service tier + workspaces here, services bound to loopback),
`server` (only the shared service tier, bound to `0.0.0.0` for other machines, no
workspaces), or `client` (only workspaces here, routing to a remote `--server`
address — no local service tier). The role is persisted in `runtime.yaml`.

Role drives UI auth: standalone/client run **open** for a smooth single-user
local experience (no login wall); a `server` is network-exposed, so the LiteLLM
admin UI requires a password (`ai setup` prompts, or set it later with `ai litellm
password`). Platform secrets (`UI_PASSWORD`,
`LITELLM_MASTER_KEY`) can be persisted opt-in to `~/.ai-platform/.ai-platform.env` (mode 0600,
auto-loaded by `ai`) so they survive restarts without editing your shell rc.

## Use

Every command supports `--json` (envelope `{ok, command, data, error,
warnings}`) and stable exit codes: `0` ok · `2` invalid input · `3` missing
dependency · `4` runtime failure · `5` permission. Global flags: `--verbose`,
`--dry-run`, `--project`, `--yes`.

Project-scoped commands default to the project of your current directory (walking
up to a `.ai-platform/` root); pass a name or `--project` to target another. With
no name and outside a project, the command exits `2`.

**Create a project** (interactive wizard — OS, agent CLIs, software stacks, AI tools, in-VM apps):

```bash
ai create my-app --os debian-trixie   # --dry-run to preview; --apps openwebui to seed an in-VM app
ai create my-app --os ubuntu --tools caveman,graphify  # pick the AI tools non-interactively
ai list
ai delete --yes              # the current project, plus its overlay
```

Each selected in-VM app (and the hermes `hermes dashboard`, if hermes is chosen) is
published on a per-(workspace,app) host port picked at create — the wizard prompts
for one, or set it non-interactively with `--app-port <app>=<port>` (e.g.
`--app-port openwebui=8080`, `--app-port hermes=9119`). Ports are persisted in the
project `config.yaml`.

Every OS base bakes in a common tooling layer — Git, the GitHub CLI, **Node.js**
(pinned 24 LTS), the latest **Python 3**, **uv** (Astral's Python package/tool
manager), and **rtk** (Rust Token Killer). Node.js and Python are baked in, so
neither is a `--stacks` option — the selectable stacks are `go,rust,java,maven,deno`.

Four opt-in per-project **AI tools** are chosen from one multi-select — the
`--tools` flag or the wizard's AI-tools step (default: `caveman,graphify,code-review-graph`
on, `codebase-memory-mcp` off):

- **caveman** — an output-compression toolkit, installed at workspace start.
- **graphify** (PyPI `graphifyy`, CLI `graphify`) — a knowledge-graph skill for AI
  coding assistants, added as a conditional Dockerfile snippet (`uv tool install`
  with all extras except the region/DB/niche-specific
  `chinese,azure,bedrock,falkordb,neo4j,leiden,dm,pascal`) only when selected; each
  selected agent CLI then registers it with itself (`graphify install [--platform <cli>]`).
- **code-review-graph** (`code-review-graph.com`) — a conditional snippet, registered
  as an MCP server with every installed agent CLI at workspace start; also writes a
  D3 graph visualization.
- **codebase-memory-mcp** (`github.com/DeusData/codebase-memory-mcp`) — a conditional
  snippet, registered as an MCP server; ships an optional on-demand 3D graph UI at `:9749`.

The three code-graph tools are appended to the project image **only when chosen** —
conditional Dockerfile snippets, not baked into every base. All are local and need
no API key.

**Work in the workspace** (one microVM per project; installed programs and agent
state persist across restarts via the overlay):

```bash
cd my-app
ai start                     # the cwd's project; also: ai stop, ai restart
ai resize --disk 32 --memory 12  # change disk/memory/vCPUs (any of --disk/--memory/--cpus), then restart to apply
ai shell                     # pick a session to attach, or create a new one (tmux)
ai agent opencode            # launch an agent CLI in its own session
ai exec -- bash              # run a one-off command inside
ai sessions                  # list sessions; ai attach picks one to reattach
ai sessions kill build       # kill a named tmux session
```

`ai delete` (alias `ai destroy`) is the single removal verb: it tears down the
microVM, drops the project's `.ai-platform/` dir + overlay, and de-registers it,
keeping your other files; `ai delete --purge` removes the whole directory.

**Run an in-VM app** (Open WebUI runs as a nerdctl container inside the
microVM, on the rootful in-VM container runtime, routed through the same gateway and
published on a unique host port):

```bash
ai apps list                 # the app catalogue + per-workspace status
ai apps add openwebui        # install; published on restart at a per-(workspace,app) port
ai apps update openwebui     # re-pull the latest image and recreate
ai apps remove openwebui     # also: ai apps start | stop | restart <app>
```

**Tune, secure, observe:**

```bash
ai context strategy aggressive       # Headroom: conservative | balanced | aggressive
ai context caveman  ultra            # Caveman:  lite | full | ultra | wenyan

ai network show                      # egress policy (declared + live in-force)
ai network egress deny               # deny | public (default) | unrestricted
ai network allow api.github.com      # allow a host/domain (port defaults to 443; *.suffix ok)
ai network disallow api.github.com   # remove an allow-listed host/domain
ai network publish 8080:8080         # publish a guest port to the host (unpublish removes it)
ai network log                       # attempted-egress audit (domains the workspace resolved)

ai gateway show                      # the model gateway every workspace on this machine routes through
ai gateway set my-server:18787       # client mode: route all workspaces through a remote gateway
ai gateway clear                     # back to the local standalone gateway
ai domain                            # show the platform base domain the UI subdomains hang off
ai domain aip.example.com            # set it (default aip.local; server operators override)

ai keys add openai                   # store a provider API key (encrypted in the LiteLLM DB; registers its catalog models)
ai keys list                         # providers + whether a key is set (never the key value)
ai keys remove openai                # remove the key and unregister that provider's models

ai models status                     # gateway's live served models (added keys + pulled Ollama)
ai models test  llama3.2             # round-trip one of the served models
ai models list                       # installed local (Ollama) models
ai models popular                    # the live ollama.com installable model library
ai models pull  llama3.2 qwen2.5:7b  # pull Ollama models, registering them as served (rm too)

ai services status                   # host service tier
ai services console litellm          # open the LiteLLM admin UI
ai services update                   # re-pull the latest service images and recreate containers
ai litellm password                  # set/rotate the LiteLLM admin UI password (secures the gateway)
ai logs --tail                       # project + platform logs
ai state show                        # global + project state
ai theme                             # show/select the CLI + TUI colour theme
```

**Manage it all from a TUI:**

```bash
ai ui                                # K9s-style dashboard: Services · Workspaces ·
                                     #   Local/Cloud Models · API Keys · Settings
```

`ai ui` navigates with Tab/←→/number keys (quit `q`) and captures the mouse by
default — the tab bars are clickable and the wheel scrolls the focused list.
Toggle capture off in the **Settings** tab (`m`) to restore modifier-free native
text selection (with capture on, selection needs the terminal's modifier —
Shift, or Option on macOS). The per-workspace view has Workspace ·
**Logs** · **Metrics** · Network · Context · **Shell** (session manager —
attach/new/kill) · Apps sub-tabs; the **Workspace** tab shows the summary plus a
scrollable live **Sandbox Configuration** block, the **Logs** tab is the live,
read-only workspace log (streamed on the SDK backend; `f`/`enter` follows it live
in your real terminal), and the **Metrics** tab streams CPU/memory/disk/net.
Workspace start/stop/restart run in the background (the microVM build/boot
survives closing `ai ui` — a spinner tracks progress), and interactive shells run
in your real terminal.

## Uninstall

A native, offline teardown built into the binary; it prompts for confirmation
first and **never touches `~/projects`** (your source):

```bash
ai uninstall                 # binary, PATH/completion entries, aip-* containers, ~/.ai-platform state (KEEPS downloaded models)
ai uninstall --purge         # also the downloaded model store — removes ~/.ai-platform in full
ai uninstall --remove-deps   # also uninstall msb
ai uninstall --yes           # skip the prompt (automation); --dry-run to preview
```

It streams progress, asks per external dependency (`msb`), and logs to
`~/ai-uninstall.log`.

## Develop

Requires **Go 1.26+** and `CGO_ENABLED=1` (the Makefile exports it): the binary
links the Microsandbox Go SDK, so the old pure-static build is gone. Build hosts
are macOS (Apple Silicon) and Linux.

```bash
make build             # -> bin/ai (injects the version via -ldflags)
make check             # the pre-commit gate: fmt-check vet lint test build
make fmt               # gofmt -w .
make lint              # golangci-lint (also run standalone)
make test              # unit tests (go test ./...)
make test-acceptance   # acceptance suite (AIP_HARDWARE_TESTS=1 adds the full-stack [S1] tests)
make test-integration  # LIVE suite vs a running Docker + Microsandbox stack (self-skips if absent)
make test-all          # all suites (unit + acceptance + integration)
```

## Releasing

Continuous delivery: every push/merge to `main` runs the full gate (`make check`)
and, only on success, computes the next **semver** from the Conventional Commits
since the last tag (`feat` → minor, `fix`/other → patch, `BREAKING CHANGE`/`!` →
major; default patch), builds the `ai-<os>-<arch>` binaries (darwin/arm64,
linux/amd64, linux/arm64), and publishes a GitHub Release — the binaries,
`SHA256SUMS`, and `installers/install.sh` — creating the tag `vX.Y.Z` at the merged
commit and marking it the latest release (so `install.sh`'s `releases/latest`
resolves to it). See `.github/workflows/release.yml`. `make release` builds the
host-arch binary locally for testing.

## Status

The entire control-plane surface is implemented host-side and unit-tested: the
`ai` CLI, project/workspace lifecycle, context/keys/models/network/state
commands, `doctor`/`logs`, host detection, and the service-tier + guardrail +
egress wiring. On a provisioned Apple Silicon host the service tier comes up live
via `ai setup`, microVMs build and boot, LiteLLM virtual-key minting works, and
the DNS egress audit captures attempted destinations — verified end-to-end. The
remaining live-integration details are tracked in
[`docs/HARDWARE-BRINGUP.md`](docs/HARDWARE-BRINGUP.md); commands that need a piece
not yet wired report their deferred status rather than pretending.

## Design docs

The full design lives in [`spec/`](spec/) — the source of truth:

| Doc | Contents |
|-----|----------|
| `spec/01-architecture-spec.md` | End-state architecture |
| `spec/02-implementation-roadmap.md` | Delivery slices (S1–S6) |
| `spec/03-repository-layout.md` | On-disk + repo layout |
| `spec/04-cli-specification.md` | CLI surface, output envelope, exit codes |
| `spec/05-acceptance-tests.md` | Acceptance tests |
| `spec/06-implementation-plan.md` | Build milestones (M0–M8) |

## License

MIT

## Acknowledgments

This software was generated primarily by AI from a human-authored design spec. The author reviewed and tested the
generated code. See the MIT license for warranty and liability terms.

