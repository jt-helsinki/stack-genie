# Hardware bring-up checklist

Slice 1 is implemented host-side (M0–M8). The remaining work is the real
external-tool integration, which can only be built and verified on a provisioned
**Apple Silicon** host. Everything below is seamed behind interfaces in the code;
search for `ErrPending` and `hardware bring-up` to find the exact spots.

This is the punch list to take Slice 1 from "host-side complete" to "runs for
real."

## 1. Provision the host

- [ ] Apple Silicon Mac (Intel is unsupported — the Microsandbox runtime needs
      the Apple Hypervisor). `ai doctor` must report `host virtualization: ok (hvf)`.
- [ ] Go 1.26+.
- [ ] **Docker**, rootless. `ai doctor` → `container runtime: ok (docker)`;
      `ai setup` must verify rootless (`runtime.Verify`, exit 4 if not).
- [ ] **Microsandbox** (`msb`) on `PATH`, code-signed with the
      `com.apple.security.hypervisor` entitlement under Developer ID + notarization.
- [ ] **ClawPatrol** gateway binary on `PATH`.
- [ ] `git`, `gh` (already used by project create).

## 2. Resolve the open unknowns first (don't guess the tool CLIs)

Per the "don't invent external-tool invocations" rule, confirm these against the
real docs before wiring:

- [ ] **Microsandbox Go SDK** (`github.com/microsandbox/microsandbox/sdk/go`):
      exact API for create/start/stop/exec, image source, bind mounts, **named
      volumes**, **default-deny network policy** (pin the symbol — confirmed to
      exist in the Python SDK; verify the Go equivalent), and env injection.
- [ ] **ClawPatrol CLI/API** for: generating the TLS-interception **CA**;
      **credential** set/map/remove/list; and running the gateway as a **forward
      proxy** (S1 egress model). See `denoland/clawpatrol` docs.
- [ ] **Agent-CLI install commands** in the template snippets
      (`internal/templates/files/agentclis/*/Dockerfile.snippet`) — the npm
      package names (`opencode-ai`, `@anthropic-ai/claude-code`, `@openai/codex`,
      `@google/gemini-cli`) are marked "verify on hardware".
- [ ] **Hypervisor entitlement + notarization** chain for distributing the signed
      `ai` (and/or `msb`) binary (Developer ID, not App Store). The M3 spike
      (plan §8.2) covers this plus a no-admin/no-kext networking check.

### Networking reachability spike (do this first — arch §29.2, §29.5, §29.6)

The host↔workspace networking is the biggest "will it actually work" unknown.
Pin it before wiring the rest, and fill in `runtime.HostGateway(goos)` (currently
returns `("", false)`):

- [ ] **Pin the host gateway.** Find the gateway address of Microsandbox's
      host-side userspace stack (the address the guest reaches the host at) from
      the SDK; implement `runtime.HostGateway` to return it per `GOOS`, and have
      `ai setup` persist it to `config/runtime.json` as `host_gateway`. `Info.HostAddress()`
      already prefers the `AI_PLATFORM_HOST` env override, else this value.
- [ ] **Prove the four properties** (these make "it works" + "egress is confined"
      falsifiable):
  1. a workspace reaches an **allow-listed** host service (start a host Postgres,
     allow-list `gateway:5432`, connect from inside the microVM);
  2. a **non-allow-listed** host/internet destination is **denied**;
  3. a **published** guest port (`network.publish_ports`) is reachable from the host;
  4. internet egress is still forced through **ClawPatrol** (the firewall).
- [ ] Confirm the SDK calls for the allow-list + port maps, then implement the
      `realSandbox.Create` network application below.

## 3. Implement the seams

Each is a thin real impl that currently returns `ErrPending` / a stub.

- [ ] **`internal/workspace/workspace_real.go`**
  - `realBuilder.Build` → run `<rt> build -t <imageRef> -f <root>/.ai-platform/Dockerfile <root>` where `<rt>` is the detected runtime (`docker`|`podman`, Slice 6). The argv is already produced by `runtime.ContainerRuntime.BuildArgs`; just exec it.
  - `realSandbox.Create/Start/Stop/Destroy/Exec` → Microsandbox Go SDK: boot the
    OCI image as a microVM, bind-mount the project at `~/workspace` (the
    `projectMount` arg arrives already normalized — WSL2 `/mnt/<drive>` form on a
    Windows host, via `internal/hostpath`, Slice 7), attach the
    overlay named volume backed by the host `overlayPath` Create now receives
    (ensured by `internal/overlay`, §26), apply the **default-deny network policy**, inject
    `AI_PLATFORM_HOST` (= `runtime.Info.HostAddress()`) + `HTTPS_PROXY`, and install
    the ClawPatrol **CA root** into the workspace trust store (arch §17, §29).
    `Exec` returns a real `ExecResult`.
  - Apply the project's `network` block (arch §29.6, `config.NetworkConfig`):
    add `network.allow_host_services` (resolved via `NetworkConfig.ResolveHostServices(hostGateway)`)
    to the Microsandbox policy allow-list as **plain-TCP** endpoints (no ClawPatrol
    TLS interception), and map `network.publish_ports` (host → guest) so the host
    can reach a dev server in the workspace. ClawPatrol remains the sole internet-egress
    firewall; the network policy only default-denies and forces egress to it.
- [ ] **`internal/setup/setup_real.go`**
  - `realServices.Reconcile` → pull pinned images by digest (`config/versions.json`),
    run LiteLLM with the rendered config via the detected runtime
    (`runtime.ContainerRuntime.RunArgs`, docker|podman — not hardcoded), start the
    Ollama (required), and the ClawPatrol gateway (native, register with launchd); health-poll.
    (Headroom is NOT a host service — see the workspace item below.)
  - `realServices.Status` → real probes (`<rt> ps`, gateway `/health`, …).
  - `realCA.Ensure` → generate the ClawPatrol CA (verified CLI from step 2).
- [ ] **In-workspace Headroom wiring** (arch §8–10) — Headroom is installed in the
  sandbox image (NOT run as a container). Verify the
  `internal/templates/files/contextopt/headroom/Dockerfile.snippet` install
  (`pip install headroom-ai[proxy]`) on each OS, then launch the agent CLI
  **wrapped by Headroom** — `headroom wrap <cli>` (e.g. `headroom wrap opencode`),
  or the drop-in proxy `headroom proxy` with `OPENAI_BASE_URL=http://localhost:8787`
  — upstreaming to LiteLLM at `AI_PLATFORM_HOST` and applying the project's
  `context.strategy`. Pin the Headroom version in `config/versions.json`.
- [ ] **`internal/secrets/secrets_real.go`**
  - ClawPatrol is the full agent firewall (intercepts all traffic incl. local
    Ollama + cloud, evaluates HCL rules, audits everything), not just a secret
    store (arch §17). `realBroker.Set/Map/Remove/List` → ClawPatrol credential CLI. Keep values out
    of platform disk; `List` returns names/metadata only.
- [ ] **`internal/setup/setup.go` startup ordering** — confirm container runtime +
      Microsandbox verified → ClawPatrol (creds loaded) → LiteLLM → [Ollama] (arch §5).

## 4. Turn on the acceptance tests

In `test/acceptance/` (harness, PTY driver, and the runnable `[S1]` subset already
pass):

- [ ] Build the **HTTPS mock-provider** fixture (OpenAI-compatible `/health` +
      `/v1/chat/completions` that records the credential; serve TLS and configure
      ClawPatrol to trust its cert — acceptance-tests §1.6).
- [ ] Add the service `[S1]` tests from `spec/05-acceptance-tests.md`, all gated by
      `hardwareAvailable()`:
  - §2.1–2.3 setup + idempotency; §12.1 macOS install.
  - §7.1/§7.2 model status/test; §9.1 credentialed request (sentinel appears at the
    mock provider, **never** in the workspace).
  - §16.2 workspace isolation (host fs unreachable, no Docker socket); §16.3 egress
    confinement (allowlisted reachable, non-allowlisted denied).
  - §6.1 Dockerfile-built workspace; §6.3 agent-CLI selection; §6.4 stack selection
    (via `ai workspace exec` probes).
- [ ] Point `make test-acceptance` at the suite (currently a stub) and enable the
      self-hosted Apple Silicon job in `.github/workflows/ci.yml` (`acceptance-s1`).

## 5. Smoke sequence (manual, on the host)

1. `ai doctor` → all checks green.
2. `ai setup` → exit 0; LiteLLM + ClawPatrol up; CA created; templates installed.
3. `ai project create demo` (wizard) → project scaffolded **and** its workspace
   microVM starts.
4. `ai workspace exec demo -- uname -a` → runs inside the microVM.
5. `ai secrets set openai --stdin` + `ai secrets map openai --env OPENAI_API_KEY`;
   `ai models test gpt-5` → injection works (real key at provider, placeholder in
   workspace).
6. From the workspace, a non-allowlisted destination is **denied** (egress confined
   to ClawPatrol).
7. `make test-acceptance` → the `[S1]` suite is green.

## 6. Deferred beyond Slice 1

- **WireGuard L3 egress** (arch §29 end-state) — replaces the S1 forward proxy for
  transparent capture of all tools. Gated by the userspace-WG-with-ClawPatrol
  feasibility spike (plan §8.2). Not needed for S1.
- Podman / Linux (S6), Windows/WSL2 (S7), Headroom + Caveman context optimization
  (S2), overlay persistence hardening (S4), extended OS templates (S5).
