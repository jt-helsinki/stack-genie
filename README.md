# AI Development Platform

Reproducible, isolated, AI-powered development environments. Each workspace is a
hardware-isolated [Microsandbox](https://microsandbox.dev) microVM; the model
path and guardrails run as a shared host container tier. One Go binary, `ai`, is
the entire control plane.

**Model path:** agent → nginx gateway → Headroom (input compression) → LiteLLM
(always-on secret-masking guardrails) → containerized Ollama or a cloud
provider. A single nginx reverse proxy (`aip-proxy`) is the only host entry to
the service tier — everything else runs internal-only on the `aip-net` network.
Real provider API keys live only in the LiteLLM gateway (keys-in-LiteLLM) — never
on platform disk or in the workspace; the agent holds a scoped virtual key.
Workspace egress is a default-deny Microsandbox NetworkPolicy you configure with
`ai network`. Caveman is a per-project output-compression skill inside the
workspace.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/jt-helsinki/ideal-robot/main/installers/install.sh | bash
```

Fetches the installer, which downloads the `ai-<os>-<arch>` binary from the
latest GitHub release and adds it to your PATH. Use `bash`, not `sh`. A piped
install can't change your *current* shell's PATH — open a new shell, then run
`ai setup`. Overrides: `AIP_INSTALL_DIR`, `AIP_VERSION`, `AIP_RELEASE_BASE_URL`,
`AIP_NO_MODIFY_PATH=1`.

From a clone, build and install from source (PATH applies in this shell):

```bash
source ./installers/install.sh
```

## Prerequisites

`ai` orchestrates external tools rather than bundling them — install these
yourself:

| Tool | Role | Install |
|------|------|---------|
| [Microsandbox](https://microsandbox.dev) (`msb`) | microVM workspaces | `curl -fsSL https://install.microsandbox.dev \| sh` |
| Docker or Podman (rootless) | service tier | `brew install --cask docker` (or `brew install podman`) |

Supported host: **macOS on Apple Silicon** (Microsandbox needs the Apple
Hypervisor) or **Linux with KVM**.

`ai setup` **never installs software** — it detects what's missing and prints how
to install it (command + web address); `ai doctor` reports the same anytime. The
service tier (the nginx gateway, Ollama, Presidio, LiteLLM + Postgres, Headroom,
and the DNS egress-audit resolver) is launched by
`ai setup` as host containers on the `aip-net` network — you don't install those.
Only the nginx gateway (`aip-proxy`) publishes a host port (`:18787`); every other
service is internal-only and reached through it. The single host UI is served as a
Host-based subdomain off a platform base domain (default `aip.local`, set with
`ai domain`): `litellm.<domain>` (the LiteLLM admin UI) — on
`:18787`. (Open WebUI is now a per-workspace in-VM app; Odysseus has been removed.)
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
password`) and Open WebUI enables login. Platform secrets (`UI_PASSWORD`,
`LITELLM_MASTER_KEY`) can be persisted opt-in to `~/.ai-platform.env` (mode 0600,
auto-loaded by `ai`) so they survive restarts without editing your shell rc.

## Use

Every command supports `--json` (envelope `{ok, command, data, error,
warnings}`) and stable exit codes: `0` ok · `2` invalid input · `3` missing
dependency · `4` runtime failure · `5` permission. Global flags: `--verbose`,
`--dry-run`, `--project`, `--yes`.

Project-scoped commands default to the project of your current directory (walking
up to a `.ai-platform/` root); pass a name or `--project` to target another. With
no name and outside a project, the command exits `2`.

**Create a project** (interactive wizard — OS, agent CLIs, software stacks):

```bash
ai create my-app --os debian-trixie   # --dry-run to preview
ai list
ai delete --yes              # the current project, plus its overlay
```

**Work in the workspace** (one microVM per project; installed programs and agent
state persist across restarts via the overlay):

```bash
cd my-app
ai start                     # the cwd's project; also: ai stop, ai restart
ai shell                     # an interactive shell inside (reattachable tmux session)
ai agent opencode            # launch an agent CLI in its own session
ai exec -- bash              # run a one-off command inside
ai sessions                  # list sessions; ai attach reattaches one
ai destroy                   # non-destructive: keeps the overlay
```

**Tune, secure, observe:**

```bash
ai context strategy aggressive       # Headroom: conservative | balanced | aggressive
ai context caveman  ultra            # Caveman:  lite | full | ultra | wenyan

ai network show                      # egress policy (declared + live in-force)
ai network egress deny               # deny | public | unrestricted
ai network allow api.github.com      # allow a host/domain (port defaults to 443; *.suffix ok)
ai network log                       # attempted-egress audit (domains the workspace resolved)

ai gateway show                      # the model gateway every workspace on this machine routes through
ai gateway set my-server:18787       # client mode: route all workspaces through a remote gateway
ai gateway clear                     # back to the local standalone gateway
ai domain                            # show the platform base domain the UI subdomains hang off
ai domain aip.example.com            # set it (default aip.local; server operators override)

ai secrets set OPENAI_API_KEY        # stored in the LiteLLM gateway, never platform disk
ai secrets list                      # names + metadata only (never values)

ai models status                     # LiteLLM gateway health + Ollama connectivity
ai models test  gemma4               # round-trip a model (gemma4 = local default)
ai models list                       # installed local (Ollama) models
ai models popular                    # the bundled installable model catalogue
ai models pull  llama3.2 qwen2.5:7b  # download models into the local Ollama store (rm/show too)

ai services status                   # host service tier
ai services console litellm          # open the LiteLLM admin UI
ai services update                   # re-pull the latest service images and recreate containers
ai litellm password                  # set/rotate the LiteLLM admin UI password (secures the gateway)
ai logs --tail                       # project + platform logs
ai state show                        # global + project state
```

## Uninstall

A native, offline teardown built into the binary; it prompts for confirmation
first and **never touches `~/projects`** (your source):

```bash
ai uninstall                 # binary, PATH/completion entries, aip-* containers
ai uninstall --purge         # also ~/.ai-platform
ai uninstall --remove-deps   # also uninstall msb
ai uninstall --yes           # skip the prompt (automation); --dry-run to preview
```

It streams progress, asks per external dependency (`msb`), and logs to
`~/ai-uninstall.log`.

## Develop

```bash
make build                    # -> bin/ai
make fmt-check vet test build  # the pre-commit gate
make lint                     # golangci-lint
```

## Releasing

Continuous delivery: every push/merge to `main` builds, verifies (`make vet
test`), and — only on success — publishes a GitHub Release with the
`ai-<os>-<arch>` binaries, `SHA256SUMS`, and `install.sh`, then tags the merged
commit `v0.0.<run_number>` and marks it the latest release (so `install.sh`'s
`releases/latest` resolves to it). See `.github/workflows/release.yml`.
`make release` cross-compiles the same artifacts locally for testing.

## Status

The entire control-plane surface is implemented host-side and unit-tested: the
`ai` CLI, project/workspace lifecycle, context/secrets/models/network/state
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
