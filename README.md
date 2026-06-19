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

Slice 1 (MVP) is under construction. **M0 (bootstrap)** is complete: the binary
builds and the output envelope + exit-code contract are in place.

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

```bash
source ./installers/install.sh   # installs `ai`, updates PATH in this shell; then run `ai setup`
```

The installer places the `ai` binary on disk, adds its install dir to your
shell rc (`~/.zshrc`, `~/.bashrc`/`~/.bash_profile`, or fish `config.fish`) as a
single managed line — updated in place on re-run, never duplicated — and sources
that rc so `ai` is usable immediately.

**Use `source` (or `.`) as shown.** Running it normally instead:

```bash
./installers/install.sh   # also works, but PATH applies on next shell
```

still installs and updates the rc, but a normally-executed script runs in its
own process and cannot change your current shell's `PATH`. In that case open a
new shell or run `source ~/.zshrc` (or your shell's rc) afterwards.

Environment overrides: `AIP_INSTALL_DIR` (install location),
`AIP_NO_MODIFY_PATH=1` (skip rc edits and just print the export line),
`AIP_VERSION`, `AIP_RELEASE_BASE_URL`.
