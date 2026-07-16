# Live integration suite (`test/integration`)

A **black-box** suite that exercises the real `ai` binary against a **running
Docker + Microsandbox stack** on this machine — the live `hardware bring-up`
path the unit tests deliberately fake. It is **separate from the unit tests**:
every file is tagged `//go:build integration`, so it is **excluded** from
`go build ./...` and `go test ./...` and never affects normal CI.

```bash
make test-integration
# = go test -tags integration ./test/integration/... -v -timeout 30m
```

## Self-skip

`TestMain` builds `./cmd/ai` once and probes the stack:

- `docker` and `msb` must be on `PATH`,
- `docker info` must succeed (daemon up),
- `ai doctor --json` must return an ok envelope (platform set up + healthy).

If any precondition is missing, **every test self-skips** with a clear reason —
so the suite is a clean no-op on a machine without the stack.

The tests run against the **real `~/.ai-platform` state** (not an ephemeral
HOME) — they must, since the point is to talk to the live services. They clean
up everything they create (the suite workspace + any dummy keys) via
`t.Cleanup`, even on failure.

## What it covers

| Group | Test | Exercises |
|-------|------|-----------|
| 1 | `TestGroup01SetupHealth` | `ai setup --mode standalone` (idempotent, non-interactive); `ai doctor` every check ok; catalog file on disk; gateway healthy |
| 2 | `TestGroup02Services` | `ai services status` all running; restart `presidio` → back to running; `ai logs --service litellm` returns output |
| 3 | `TestGroup03ModelsKeys` | local `ollama/smollm:135m` served; **dummy** key add → models registered + `keys list` keyed; key remove → models drop |
| 4 | `TestGroup04WorkspaceLifecycle` | `create` → `start` → `exec echo` → in-VM `nerdctl` (containerd) → `refresh-models` → `apps add openwebui` reachable on its host port → egress `deny` blocks / `public` allows (restart between) → `stop` + `delete --purge` |
| 5 | `TestGroup05GatewayInference` | `ai models test ollama/smollm:135m` → real local chat completion through nginx → Headroom → LiteLLM → Ollama |
| 6 | `TestGroup06Uninstall` | `ai uninstall --dry-run` plan (always); destructive `--purge --yes` only when gated (see below) |
| — | `TestWorkspaceCreateTeardown` | fast, scenario-rich `ai create`/`delete`/`destroy` coverage — scaffold + registration, delete keeps user files, `--purge` removes the dir, missing/unknown `--os` → exit 2, flags persist to `config.yaml`, nested/duplicate locations rejected, `--dry-run` no side effects, unknown-delete error, `destroy` alias. Needs only installed templates (`requireSetup`), **not** a running stack, so it runs in seconds without building a microVM |

Each step in group 4 asserts independently and logs the exact `ai` output on
failure — those failures are real bring-up findings, which is the point.
`TestWorkspaceCreateTeardown` (in `workspace_lifecycle_test.go`) is the
lightweight counterpart to group 4: it exercises only the non-interactive
create/teardown surface (no `start`, no microVM), one scenario per subtest, each
self-cleaning via `t.Cleanup`.

## Env gates

| Env | Effect |
|-----|--------|
| `AIP_INTEGRATION_UNINSTALL=1` | run the **destructive** `ai uninstall --purge --yes` (removes the stack + binary). Off by default. |
| `AIP_INTEGRATION_OPENAI_KEY` | register that real key and run one cloud `ai models test` (group 5). Also `_ANTHROPIC_KEY`, `_GEMINI_KEY`, `_GROQ_KEY`. Off by default — cloud is registration-only with dummy keys otherwise. |

## What it can NOT test

- **The TUI** (`ai ui`, `ai create`'s huh wizard, prompts) — these need a real
  PTY; automation of the interactive flows is out of scope here (the acceptance
  harness covers PTY-driven `create`). The integration suite always uses
  `--json` / non-interactive paths.
- **Real cloud inference** without a provider key — only registration is
  exercised by default; live cloud calls require `AIP_INTEGRATION_<PROVIDER>_KEY`.
- **`ai models rm`** — `smollm:135m` is the only pulled model and the user keeps
  it, so removal is not exercised (no throwaway model is pulled).
- **`sudo`-requiring host mutations without NOPASSWD** — e.g. the `/etc/hosts`
  UI-subdomain block. `ai doctor` reports it; the suite does not mutate it.
- **server / client deployment roles** — only `standalone` is exercised (the
  brought-up role on this host).
- **Cross-platform paths** — runs on the host it is invoked on (macOS Apple
  Silicon / Linux); there is no cross-OS path-translation layer to test.
