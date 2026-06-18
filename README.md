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
./installers/install.sh   # places `ai` on disk; then run `ai setup`
```
