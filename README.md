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
