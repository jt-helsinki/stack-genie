# AI Development Platform

Reproducible, isolated AI-powered development environments: workspaces run as
hardware-isolated [Microsandbox](https://microsandbox.dev) microVMs, with
centralized model access (LiteLLM), context optimization (Headroom + Caveman),
and wire-level secret brokering (ClawPatrol). One Go binary, `ai`, is the single
control plane.

The full design lives in [`spec/`](spec/):

| Doc | Contents |
|-----|----------|
| `spec/01-architecture-spec.md` | End-state architecture |
| `spec/02-implementation-roadmap.md` | Delivery slices (S1–S7) |
| `spec/03-repository-layout.md` | On-disk + repo layout |
| `spec/04-cli-specification.md` | CLI surface, output envelope, exit codes |
| `spec/05-acceptance-tests.md` | Formal acceptance tests |
| `spec/06-implementation-plan.md` | Build sequence (milestones M0–M8) |

## Status

The full control-plane surface (Slices S1–S7) is implemented **host-side**: the
`ai` CLI, project/workspace lifecycle, context optimization, secrets, models,
doctor/logs, and Linux/Windows detection all work and are unit-tested. What
remains is the **live external-tool integration** — launching the LiteLLM
container, the ClawPatrol gateway + CA, the in-workspace Headroom proxy, and
booting Microsandbox microVMs — which can only be wired and verified on a
provisioned Apple Silicon host. Those seams are tracked in
[`docs/HARDWARE-BRINGUP.md`](docs/HARDWARE-BRINGUP.md).

In practice: project creation, configuration, context/secrets/state commands,
and `ai doctor`/`ai setup` preflight run today; commands that need a live
workspace or service report their deferred status rather than pretending.

## Requirements

- Go 1.26+
- macOS on **Apple Silicon** (Microsandbox requires the Apple Hypervisor); Linux
  (KVM) and Windows (WSL2) land in later slices.

## Build

```bash
make build          # -> bin/ai
./bin/ai --help
./bin/ai --version --json
```

## Develop

```bash
make fmt            # format
make vet test       # static checks + unit tests
make lint           # golangci-lint (if installed)
```

## Install

Once a release is published (see [Releasing](#releasing)), install with one
command:

```bash
curl -fsSL https://raw.githubusercontent.com/jt-helsinki/ideal-robot/main/installers/install.sh | bash
```

This downloads the installer, which fetches the `ai-<os>-<arch>` binary from the
latest GitHub release and puts it on your PATH. Use `bash`, not `sh` (the script
uses bash features). A piped install can't change your *current* shell's PATH, so
open a new shell afterward (the installer updates your rc for future shells).

From a clone (dev), prefer sourcing so PATH applies immediately:

```bash
source ./installers/install.sh   # builds from source, updates PATH in this shell; then run `ai setup`
```

The installer places the `ai` binary on disk, adds its install dir to your
shell rc (`~/.zshrc`, `~/.bashrc`/`~/.bash_profile`, or fish `config.fish`) as a
single managed line — updated in place on re-run, never duplicated — and (for
POSIX shells) sources that rc so `ai` is usable immediately.

Running `./installers/install.sh` normally (not sourced) also works, but PATH
applies only in the next shell. Environment overrides: `AIP_INSTALL_DIR`
(install location), `AIP_NO_MODIFY_PATH=1` (skip rc edits and just print the
export line), `AIP_VERSION`, `AIP_RELEASE_BASE_URL`.

After installing, run `ai doctor` to check the external prerequisites
(Microsandbox, container runtime, virtualization, ClawPatrol) — each missing one
prints a copy-pasteable fix — then `ai setup`.

## Usage

`ai` is the single control plane. Every command takes `--json` for a structured
envelope (`{ok, command, data, error, warnings}`) and a stable exit code
(`0` ok · `2` invalid input · `3` missing dependency · `4` runtime failure ·
`5` permission). Other global flags: `--verbose`, `--dry-run`, `--project`,
`--yes`.

**The project name is optional for project-scoped commands** — `project delete`,
`workspace start|stop|destroy|exec|doctor`, `context status|strategy|caveman`,
and `logs`. They default to the project of your current directory (found by
walking up from any subfolder). Pass a name, or `--project <name>`, to target a
different project; an explicit name always overrides the default. If you give no
name and aren't inside a project, the command exits `2`.

### 1. Install the prerequisites

The platform orchestrates external tools rather than bundling them. You need, on
Apple Silicon:

| Tool | Role | Install |
|------|------|---------|
| [Microsandbox](https://microsandbox.dev) (`msb`) | microVM workspaces | `curl -fsSL https://install.microsandbox.dev \| sh` |
| Docker or Podman (rootless) | service tier | `brew install --cask docker` (or `brew install podman`) |
| [ClawPatrol](https://clawpatrol.dev) | egress firewall + secret broker | `curl -fsSL https://clawpatrol.dev/install.sh \| sh` |

`ai doctor` reports which are missing, each with its install command; `ai setup`
lists every unmet prerequisite at once and refuses to proceed until the blocking
ones are present. (LiteLLM and optional Ollama are *not* installed by you —
`ai setup` runs them as containers. Headroom and Caveman are per-project context
optimization that live **inside the workspace**, not host services.)

### 2. Provision the host

```bash
ai setup            # idempotent: preflight, lay down ~/.ai-platform, start services
ai doctor           # dependency + health report with repair suggestions
```

### 3. Create a project

`ai project create` is an interactive wizard (OS, agent CLIs, software stacks):

```bash
ai project create my-app        # pick options in the menus; or --dry-run to preview
ai project list
ai project delete --yes         # deletes the current directory's project (--yes confirms)
ai project delete my-app --yes  # or name one explicitly; removes the project + its overlay
```

A project owns a git-tracked `.ai-platform/Dockerfile` defining its environment,
plus config and the Caveman skill.

### 4. Work in the workspace

One hardware-isolated microVM per project; installed programs and agent state
persist across restarts via the overlay.

Project-scoped commands **default to the project of your current directory**
(found by walking up from any subfolder), so from inside the project you can
just run:

```bash
cd ~/projects/my-app
ai workspace start                                 # the current directory's project
ai workspace exec -- bash                          # run a command inside
ai workspace doctor                                # runtime + virtualization posture
ai workspace stop
ai workspace destroy                               # non-destructive: keeps the overlay
```

To target a different project from anywhere, pass its name (or `--project`),
which overrides the default:

```bash
ai workspace start my-app
ai workspace start --project my-app
```

### 5. Tune, secure, observe

Context commands also default to the current directory's project:

```bash
ai context status                                  # Headroom strategy + Caveman level
ai context strategy aggressive                     # conservative | balanced | aggressive
ai context caveman  ultra                          # lite | full | ultra | wenyan
ai context strategy my-app aggressive              # or name a project explicitly

ai secrets set  GITHUB_TOKEN                       # value goes to ClawPatrol, never platform disk
ai secrets map  GITHUB_TOKEN --env GITHUB_TOKEN
ai secrets list                                    # names + metadata only

ai models status                                   # LiteLLM gateway health
ai models test  claude-opus-4-8

ai services status                                 # host service tier
ai services console                                # list services with an admin console
ai services console litellm                        # open the LiteLLM UI in the browser
ai services console clawpatrol                     # open the ClawPatrol firewall dashboard
ai logs --tail                                     # current project + platform logs
ai state show                                      # global + project state
ai state repair                                    # reconstruct run-state handles
```

See [`spec/04-cli-specification.md`](spec/04-cli-specification.md) for the full
command reference.

## Releasing

The one-command `curl … | bash` install needs release assets named
`ai-<os>-<arch>`, which `installers/install.sh` downloads from the latest GitHub
release. Pushing a `v*` tag builds and publishes them via
`.github/workflows/release.yml`:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

The workflow runs `make release` (cross-compiles `dist/ai-darwin-arm64`,
`ai-linux-amd64`, `ai-linux-arm64` with the tag injected as the version) and
attaches those binaries plus `install.sh` to the release. `make release` works
locally too, for testing the artifacts before tagging.
