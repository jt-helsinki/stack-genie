# Microsandbox Go SDK migration — Phase 0 findings

Status: **Phase 0 (de-risking spike) complete.** Static gating risks settled + full
docs/source/examples read (three subagents over the upstream clone).
Date: 2026-06-30. SDK source read at `main` (pins msb `0.6.1`); build/API verified
against `v0.5.10` (matches the installed `msb` CLI). Module:
**`github.com/superradcompany/microsandbox/sdk/go`** (package `microsandbox`; the
GitHub repo is `microsandbox/microsandbox` but the module declares the
`superradcompany` path — `microsandbox/...` does NOT resolve).

## Why this exists

`msb` sandboxes wedge after ~5 min idle: the agent relay hits "max clients reached"
and the published-port listener fails to re-bind (EADDRINUSE), while the VM still
reports Running, with a no-backoff CPU spin. Our control plane shells out to the
`msb` CLI for every op (`exec.Command("msb", …)`), so the `ai ui` Sessions/Apps tabs
open a **fresh relay client every ~3s** while polling — walking the relay to
exhaustion on exactly the observed timescale. The SDK lets a long-lived process hold
**one** sandbox handle and reuse it (logs go through a relay-free side channel), and
the relay ceiling was already raised upstream — see below.

## ⭐ Most important practical finding — the relay ceiling (cheap immediate win)

Upstream changelog **2026-05-22**: *"Agent relay supports up to 128 concurrent
clients. Raised from 16."* The "max clients reached" symptom is this ceiling. So
**before any migration**, the cheapest experiment:

1. **Upgrade `msb` to the latest release (`0.6.1`)** — gets the 128-client ceiling
   (+ configurable port bind, IP-pool fixes, relay handshake-timeout fixes). Re-run
   the repro. This alone may substantially relieve the relay side. (Pin it in
   `internal/versions`.)
2. If it STILL exhausts at 128, that implies clients are genuinely **leaked** (not
   released on idle disconnect) — a real upstream bug to file, with the version.
3. The **port-publisher EADDRINUSE leak is NOT documented as fixed** anywhere in the
   changelog — treat that half as an open upstream bug regardless of version.

(Changelog files are dated, not versioned, so whether `0.5.10` already includes the
128 bump couldn't be confirmed from the clone — just upgrade and measure.)

## GATING verdict — CGO is required (THE decision)

The SDK is **CGO over a runtime-loaded FFI bundle** — confirmed three ways: empirical
build, source (`internal/ffi/ffi.go` `//go:build cgo`, `import "C"`, `#cgo linux
LDFLAGS: -ldl`), and docs (*"uses CGO over a runtime-loaded FFI bundle, so `go get`
works without a Rust toolchain"*).

| Build | Result |
|---|---|
| `CGO_ENABLED=0` (today's invariant) | **fails** — `build constraints exclude all Go files in …/internal/ffi` |
| `CGO_ENABLED=1` | **clean build** — system `cc`, **no Rust toolchain, no libkrun at build time** |

Mitigating detail (the *mild* form of CGO): the native Rust lib is **dlopen'd at
runtime**, its bytes **`go:embed`-ed** in the SDK and extracted to `~/.microsandbox/
lib/` on first use; `libkrunfw` is a separate runtime download (`EnsureInstalled`).
So `go build` needs only a C compiler; the self-contained-binary property survives.

**Cost of yes:** drop the `CGO_ENABLED=0` pure-static invariant (AGENTS.md/CI);
cross-compile needs target C toolchains; musl-static needs care. **This is the one
decision that gates the migration.**

⚠️ **CI/vendoring caveat:** source-tree checkouts ship a **0-byte FFI bundle
sentinel** — building from a vendored source tree fails at first call unless you use
a **tagged release** (`go get …@vX.Y.Z`, the bytes are in the release) or build with
`-tags microsandbox_ffi_path` pointing at `$MICROSANDBOX_FFI_PATH`. Plan module/CI
accordingly (don't vendor the source tree raw).

## The core bet — CONFIRMED in source

A `*Sandbox` handle is **one** long-lived Rust-side object (opaque `uint64`);
`Exec`/`FS`/`Metrics`/`LogStream`/`Attach` all reuse that same handle — **none opens
a fresh relay client per call** (`ffi.go:1005-1008`, `2066-2098`). `*Sandbox` is
goroutine-safe. So "hold one handle, poll via `sb.Exec`" collapses N short-lived
relay clients into 1 long-lived one.

**Usage rules that matter (get these wrong and you reproduce the bug):**
- `GetSandbox().Connect()` and `ConnectAgentSandbox()` each **mint a NEW relay-backed
  handle**. Call them ONCE per workspace and reuse; never per poll-tick. Leaking
  un-`Close`d Connect handles is itself a relay-exhaustion path.
- **`Close()` is mode-dependent**: for a DETACHED VM, `Close()` *stops* it. To keep a
  VM running across `ai` invocations you MUST `CreateSandbox(…, WithDetached())` and
  release with **`Detach()`**, never `Close()`. (`OwnsLifecycle()` tells you which.)

## API surface — what migrates, what stays CLI (REVISED from initial Phase 0)

Verified against real source + examples. Two gaps I flagged earlier are now CLOSED.

### Covered — migrate
- **Lifecycle**: `CreateSandbox` + `WithImage/WithImageDisk/WithBindRootfs/
  WithSnapshot`, `WithCPUs/WithMemory/WithIdleTimeout/WithMaxDuration`, `WithMounts`,
  `WithPorts`, `WithNetwork`, `WithReplace`, `WithDetached`, `WithWorkdir/WithUser`.
  Reconnect: `GetSandbox(name)` → `.Connect(ctx)`. Stop/Kill/Drain/WaitUntilStopped;
  `RemoveSandbox`. State: `SandboxHandle.Status()`, `Metrics`, `AllSandboxMetrics`.
- **Exec**: `Exec`/`Shell` (+ `WithExecUser` root vs workspace, `WithExecTimeout`,
  `WithExecCwd`); non-zero exit is data not error (matches `ExecResult`).
  `ExecStream`/`ShellStream` → `Recv` loop / `Signal` / `Close` for streaming.
  ⚠️ default per-call output buffer is **1 MiB** (`FsRead` >~750 KiB →
  `ErrBufferTooSmall`); use the read-stream / `ExecStream` for large output.
- **Interactive PTY**: SSH `sb.SSH().OpenClient()` → `client.Attach(ctx,…)` (raw
  mode, resize, detach keys) OR direct `sb.Attach`/`sb.AttachShell`. **Relay/FFI
  native — NO guest sshd needed.** Keep the real-terminal `tea.ExecProcess` model
  (don't render into an embedded pane). Each SSHClient is a relay session → `Close`
  it; don't open one per poll.
- **Filesystem**: `sb.FS().Write/WriteString/Read/Mkdir/CopyFromHost/...` — replaces
  the `msb exec … cat > file` `WriteFile` shell-out (drops shell-quoting).
- **Networking (GAP CLOSED)**: typed `WithNetwork(*NetworkConfig{Rules:[]PolicyRule})`.
  `PolicyRule.Destination` accepts **domains, `.suffix` wildcards, CIDRs, IPs, and
  groups (`host`/`private`/`public`/`metadata`)**; plus `DenyDomains`/
  `DenyDomainSuffixes`; first-match-wins; `DefaultEgress` deny / `DefaultIngress`
  allow. **DNS**: `DNSConfig.Nameservers []string` (e.g. `"1.1.1.1:53"`) → maps our
  `aip-dns --dns-nameserver`. Ports: `WithPorts(map[uint16]uint16)` **host→guest**
  (note: our CLI used host==guest), `WithPortBindings` for non-loopback binds.
  Replaces the net-rule string DSL in `egress.MsbNetworkArgs` (an upgrade). Under
  default-deny you must allow DNS explicitly (`Rule.allowDns()`), as today.
- **Logs (GAP CLOSED — relay-free)**: `Sandbox.Logs` (tail) / `LogStream{Follow}` →
  `Recv`/`Close`. Backed by an **on-disk `exec.log`**, works on running AND stopped
  sandboxes, **needs no guest-agent/relay traffic** (`logs.go:135,145`), readable
  **by name with no handle**. Cursor-resumable. So moving log retrieval off the
  `msb logs` shell-out is "free" relay relief.
- **Network inspect**: no live policy getter, but read back the *configured* policy
  via `SandboxHandle.ConfigJSON()`/`.Config()` — replaces `msb inspect` parsing for
  `ai network` verification.

### NOT covered — stay CLI / design decision
- **Image load from a locally-built OCI tar**: no `Image.Load/Import(tar)` in the
  SDK. `WithImage` takes a registry ref OR a **local rootfs directory**;
  `WithBindRootfs(dir)` boots a host dir as rootfs (no pull/overlay — but
  incompatible with `WithPatches`); `WithImageDisk` a disk image; `WithSnapshot` +
  `Snapshot.Import(archive)` a local snapshot. Our flow (container runtime builds OCI
  → export tar → `msb load -t`) has **no drop-in SDK equivalent**. Options:
  **(a)** keep `msb load` as the CLI remainder [simplest]; **(b)** push to a local
  insecure registry + `WithImage(ref)` + `WithPullPolicy(Never)` (insecure-registry
  is config.json-only in Go: `registries.hosts.<h>.insecure`); **(c)** rework to
  bind-rootfs/snapshot. → **design decision for Phase 1.**

## Confirmed gone (earlier worries that don't apply)
- "No SDK logs" — WRONG; `Logs`/`LogStream` exist and are relay-free.
- "Domain/`*.suffix` egress + DNS not supported" — WRONG; all in the types.
- "SSH needs a guest sshd" — WRONG; SSH is host-side/relay-native.
- "`IsRunning` has no SDK path" — WRONG; `Status()`/`Metrics`/`AllSandboxMetrics`.

## Runtime-only checks — require Apple Silicon + msb (HARDWARE BRING-UP)

1. **Confirm the relay-reuse win live**: hold one handle, loop `Exec` ~200× while
   watching relay client count / "max clients" — confirm it stays at 1 client. (Code
   says it must; verify on metal.)
2. **Upgrade-and-repro**: bump `msb` to `0.6.1`, re-run the wedge repro — does the
   128 ceiling + reuse eliminate it? Does the port-publisher EADDRINUSE persist?
3. **Log streaming lifecycle**: `LogStream` + a `ShellStream` of `nerdctl logs -f`
   into `LogView`; confirm clean `Close()` on tab blur (one held session per stream).
4. **SSH `Attach` parity**: tmux truecolor + CSI-u extended keys + resize vs the
   current `msb exec -t`.
5. **Local workspace image**: pick option (a)/(b)/(c) above and prove a
   project-Dockerfile build boots.
6. **Detached cross-process reconnect**: `CreateSandbox(WithDetached())`+`Detach` in
   one process, `GetSandbox`+`Connect`+`Exec` from a later `ai` process.

## Recommendation

**Conditional GO, in this order:**

1. **NOW (cheap, no migration):** upgrade `msb` → `0.6.1` (128-client ceiling) and
   re-run the repro; in parallel, **throttle the Sessions/Apps polls** and **move log
   retrieval to the relay-free path**. This may resolve most of the pain immediately.
2. **DECISION:** is `CGO_ENABLED=1` acceptable for the build/CI? This is the only true
   gate. If NO → stop at step 1 (it recovers most of the relief at near-zero risk).
3. **If yes → migrate (the durable fix):** the core bet is confirmed, the two gaps I
   feared are closed, several methods are upgrades (typed net rules incl. domains/DNS,
   relay-free logs, FS writes without shell-quoting). Only `msb load` + the
   insecure-registry-in-config detail remain CLI/config.

### Phases
- **P1**: `workspace_sdk.go` implementing the `Sandbox` interface via the SDK;
  `workspace_real.go` (CLI) kept + selectable; `msb load` stays CLI; decide the
  local-image path. Use a **tagged-release** SDK dep (FFI-bundle caveat).
- **P2**: TUI holds one handle; Sessions/Apps polls + in-VM container logs via the
  handle/streams (the bug payoff). Logs via the relay-free path.
- **P3**: interactive sessions → SSH `Attach` (real terminal).
- **P4**: cleanup; reconcile AGENTS.md (`workspace.go:201` "Go SDK / msb" finally
  true) + `spec/`; CGO build/CI changes; fakes/tests; `make check`.
