# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The `ai` CLI — the single control-plane binary for the **AI Development Platform**: reproducible, isolated AI dev environments where workspaces are hardware-isolated **Microsandbox microVMs**, with local + centralized model access (Ollama → LiteLLM routing), context optimization (Headroom + Caveman, both per-project in the workspace), and a full agent firewall + credential broker over all model traffic (ClawPatrol). Go 1.26, one static binary, module `github.com/jt-helsinki/ideal-robot`.

**`spec/` is the source of truth, not documentation.** The six numbered specs define the end-state architecture (`01`), delivery slices (`02`), on-disk/repo layout (`03`), the exact CLI surface + JSON envelope + exit codes (`04`), acceptance tests (`05`), and the milestone build plan (`06`). Code comments cite spec sections (e.g. `§17`, `CLI §3.1`, `repo-layout §12.5`) — when changing behavior, read the cited section first and keep code and spec in sync. The build proceeds in milestones **M0–M8** (see `spec/06`), with later work organized as slices **S1–S6** (`spec/02`). The entire surface is implemented **host-side**: M0–M8 plus slices S1, S2, S4–S6 (S3 and S7 retired). What remains is the **live external-tool integration** — launching the LiteLLM container, the ClawPatrol gateway + CA, the in-workspace Headroom proxy, and booting Microsandbox microVMs — which can only be wired and verified on a provisioned Apple Silicon (or Linux/KVM) host. Those seams are grep-able (`hardware bring-up`, `ErrPending`) and tracked in `docs/HARDWARE-BRINGUP.md`. Before changing a slice, read its milestone in `spec/06` and its `[Sx]`-tagged tests in `spec/05`.

## Commands

```bash
make build            # -> bin/ai  (injects version via -ldflags)
make test             # unit tests (go test ./...)
make fmt              # gofmt -w .
make fmt-check        # CI gate: fails if not gofmt-clean
make vet              # go vet
make lint             # golangci-lint (no-op if not installed; CI installs it)
make test-acceptance  # [S1] acceptance suite — STUB; needs Apple Silicon + Docker + Microsandbox
make tidy             # go mod tidy

go test ./internal/setup/ -run TestRunIdempotent   # a single test
go run ./cmd/ai --version --json                   # run without building
```

CI (`.github/workflows/ci.yml`) is split: hosted runners do build/vet/lint/unit-tests; a self-hosted **Apple Silicon** runner runs `[S1]` acceptance (the only host that can run the full stack). Before committing, `make fmt-check vet test build` should all pass — `errcheck` (via golangci-lint) is strict, so explicitly discard intentional writes (`_, _ = fmt.Fprintln(...)`).

## Architecture (the parts that span files)

**Output envelope is the universal contract.** Every command returns the `internal/output` envelope (`{ok, command, data, error, warnings}`, CLI §19). `error.code` **equals** the process exit code (§18: 0 ok, 2 invalid input, 3 missing dependency, 4 runtime failure, 5 permission). Pattern: `Execute()` (in `internal/cli/root.go`) owns an `exitCode` int and an `Emitter`; each cobra command's `RunE` does its work and sets `*exit = emitter.Success(...)` or `emitter.Failure(...)`, returning `nil`. Errors that must carry a specific exit code are `*output.Error` (build with `output.Errorf(code, ...)`); the emitter unwraps them. cobra/arg errors are mapped to exit 2 in `Execute`. Output is **human-readable by default**; the JSON envelope is emitted only with `--json` (commands provide a typed result + a Human renderer — see `ai logs`).

**External tools sit behind injectable interfaces; logic is faked in tests.** Docker/Podman, the Microsandbox runtime (`msb`), ClawPatrol, Ollama, and LiteLLM are never called directly from orchestration code. Instead: `runtime.Prober`, `setup.Services`, `setup.CA`, `secrets.Broker`, `litellm.Client`, `ollama.Client`. Orchestrators (`internal/setup`, etc.) take these via a `Deps`-style struct and are unit-tested with fakes; real implementations live in `*_real.go` and are wired by the CLI. **Many live actions are deliberately deferred to "hardware bring-up" on Apple Silicon** (launching the LiteLLM container, starting the ClawPatrol gateway, generating its CA, ClawPatrol secret ops, the in-workspace Headroom proxy, booting microVMs). These are explicit, commented stubs that return `ErrPending` — **do not invent the external tools' CLI/API invocations**; verify against the real tool's docs or leave the seam and mark it deferred. (LiteLLM and Ollama are exceptions: both are HTTP, so `litellm`/`ollama` talk to them for real, including the live `ai doctor` health probes.)

**Newer packages worth knowing.** `internal/ollama` (required local-model service), `internal/contextopt` (Headroom strategy + Caveman level, per-project, surfaced by `ai context`), `internal/overlay` (S4 per-workspace persistence), `internal/envimage` (workspace OCI image build from the project Dockerfile), `internal/console` (admin-dashboard URL registry for `ai services console`), `internal/templates` (embedded OS/stack/agent-CLI snippets), `internal/uninstall` (native `ai uninstall` teardown), `internal/layout`. **Version control is out of scope** — there is no git package and `ai project create` runs no git. **Supported hosts are macOS (Apple Silicon) and Linux** — there is no cross-OS path-translation layer.

**State is split global vs project-local, written atomically.** Global lives under `~/.ai-platform/` (config, `runtime.json`, `versions.json`, `projects.json` index); per-project state lives in `<project>/.ai-platform/` (tracked `Dockerfile`/`config.yaml`/`profile.yaml`/`project.json`/`skills/caveman/`, gitignored `run/`). All paths derive from `os.UserHomeDir()` via `internal/paths`, so **tests redirect with `t.Setenv("HOME", t.TempDir())`**. All JSON writes go through `internal/jsonfile` (temp file + `rename`; reads reject unknown fields). Config merges `project > global` via `internal/config` (deep map-merge).

**Dual runtime model.** Microsandbox microVMs run workspaces; Docker/Podman run only the stateless service tier. Detection (`internal/runtime` + `internal/sandbox`) is parameterized by `GOOS/GOARCH` + a `Prober` so it tests on any host; missing deps → exit 3, missing rootless/virtualization → exit 4. macOS requires **Apple Silicon** (no Intel). S1 egress is ClawPatrol-as-forward-proxy + a default-deny Microsandbox network policy; transparent WireGuard L3 capture is the deferred end-state.

**Model catalogue is global and agent-selected.** LiteLLM (the shared host gateway) exposes models via per-provider **wildcards** (`ollama/*`, `openai/*`, `anthropic/*`, `gemini/*`, `groq/*`) plus a few named handles; the default model is the local Ollama `gemma4` (`litellm.DefaultRouting` in `internal/litellm`). The in-workspace agent names any model per request and LiteLLM routes it (cloud keys injected by ClawPatrol, Ollama needs none) — there is **no per-project model config**. Registering a model ≠ installing it (an Ollama model still needs `ollama pull`; a cloud model still needs its key). `ai services start|stop|restart` control only the platform-owned LiteLLM container; Ollama/ClawPatrol are managed by their own installers. The LiteLLM admin UI is secured with `UI_USERNAME`/`UI_PASSWORD`/`LITELLM_MASTER_KEY` passed by **env passthrough** (values never in argv/config/disk); `ai setup` prompts for the password on a TTY.

**`ai project create` is an interactive PTY wizard** (CLI §3.1), built on `charmbracelet/huh`: no `--os` or per-choice flags — project name, OS (`debian-trixie`/`debian-bookworm`/`ubuntu`/`alma`), agent CLIs (multi-select: `opencode`/`claude-code`/`codex`/`gemini`), and software stacks (`go`/`node`/`python`/`rust`/`java`/`maven`/`deno`) are picked in menus. The project is created **in the current directory** (no fixed `~/projects`); if the cwd is already a project it **attaches** instead. The platform runs **no git** (VCS is out of scope). No TTY → exit 2; `--dry-run` emits the side-effect-free action plan. Automation drives it through a pseudo-terminal (`creack/pty`; acceptance harness in `test/acceptance`, §1.4).

## Conventions

- **No single-character variable names anywhere** — including method receivers and loop vars (`store`, `emitter`, `prober`, `entry`, `value`, …). Self-documenting names over Go's terse-receiver convention. The one kept exception is the test handle `t *testing.T`.
- **Secrets never touch platform disk.** Values flow straight to ClawPatrol via `secrets.Broker`; `secrets.Entry` has no value field by design, so listings can't leak.
- Commit messages must not include AI-attribution trailers (no `Co-Authored-By` for the assistant). Commit only when asked; the work lives on a feature branch off `main`.
