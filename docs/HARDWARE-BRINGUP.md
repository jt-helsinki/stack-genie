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
- [ ] `git`, `gh` (already used by project create).

## 2. Resolve the open unknowns first (don't guess the tool CLIs)

Per the "don't invent external-tool invocations" rule, confirm these against the
real docs before wiring:

- [ ] **Microsandbox Go SDK** (`github.com/microsandbox/microsandbox/sdk/go`):
      exact API for create/start/stop/exec, image source, bind mounts, **named
      volumes**, **default-deny network policy** (pin the symbol — confirmed to
      exist in the Python SDK; verify the Go equivalent), and env injection.
- [ ] **LiteLLM credential + virtual-key API** for: storing provider keys in the
      gateway (env passthrough at launch / its Postgres-backed store);
      `ai secrets` set/map/remove/list against that store; and **minting a scoped
      virtual key** for the workspace agent. See the LiteLLM proxy docs.
- [ ] **Agent-CLI install commands** in the template snippets
      (`internal/templates/files/agentclis/*/Dockerfile.snippet`) — the npm
      package names (`opencode-ai`, `@earendil-works/pi-coding-agent` (pi.dev),
      `@anthropic-ai/claude-code`, `@openai/codex`, `@google/gemini-cli`) are
      marked "verify on hardware". `opencode` + `pi` are installed by default.
- [ ] **Agent-CLI → LiteLLM wiring at workspace start.** Each installed agent CLI
      must talk to models through LiteLLM (via the in-workspace Headroom proxy →
      `AI_PLATFORM_HOST:18787`). For opencode/codex/gemini this is the
      OpenAI-compatible base-URL env (`OPENAI_BASE_URL`/key) injected at start;
      for **pi** it is a custom provider registered via a pi extension or
      `models.json` (`registerProvider(..., { baseUrl, apiKey, api:
      "openai-completions" })`, pi.dev/docs custom-provider) — verify pi's exact
      config format on hardware. In all cases the `apiKey` an agent CLI holds is a
      **scoped LiteLLM virtual key**, never a real provider secret: the real
      provider credentials live in the LiteLLM gateway (keys-in-LiteLLM, §17), so
      provider API keys never reach the workspace or any agent CLI config.
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
      `ai setup` persist it to `config/runtime.yaml` as `host_gateway`. `Info.HostAddress()`
      already prefers the `AI_PLATFORM_HOST` env override, else this value.
- [ ] **Prove the four properties** (these make "it works" + "egress is confined"
      falsifiable):
  1. a workspace reaches an **allow-listed** host service (start a host Postgres,
     allow-list `gateway:5432`, connect from inside the microVM);
  2. a **non-allow-listed** host/internet destination is **denied** (default-deny
     NetworkPolicy);
  3. a **published** guest port (`network.publish_ports`) is reachable from the host;
  4. the LiteLLM gateway port remains reachable so the agent's model path works,
     while everything not allow-listed stays denied.
- [ ] Confirm the SDK calls for the allow-list + port maps, then implement the
      `realSandbox.Create` network application below.

## 3. Implement the seams

Each is a thin real impl that currently returns `ErrPending` / a stub.

- [ ] **`internal/workspace/workspace_real.go`**
  - `realBuilder.Build` → run `<rt> build -t <imageRef> -f <root>/.ai-platform/Dockerfile <root>` where `<rt>` is the detected runtime (`docker`|`podman`, Slice 6). The argv is already produced by `runtime.ContainerRuntime.BuildArgs`; just exec it.
  - `realSandbox.Create/Start/Stop/Destroy/Exec` → Microsandbox Go SDK: boot the
    OCI image as a microVM, bind-mount the project at `~/workspace` (the
    `projectMount` arg is the host project path directly — supported hosts are
    macOS and Linux, so no path translation), attach the
    overlay named volume backed by the host `overlayPath` Create now receives
    (ensured by `internal/overlay`, §26), apply the **default-deny network policy**, and inject
    `AI_PLATFORM_HOST` (= `runtime.Info.HostAddress()`) so the agent reaches the
    host Headroom→LiteLLM gateway with its scoped virtual key (arch §17, §29).
    `Exec` returns a real `ExecResult`.
  - **Enforce the `ai network` egress declarations** — apply the project's `network`
    block (arch §29.6, `config.NetworkConfig`, set via `ai network` and stored in
    `config.yaml`: egress mode `deny`[default]/`public`/`unrestricted`, allowed host
    services, published ports) as the Microsandbox **NetworkPolicy**. Add
    `network.allow_host_services` (resolved via
    `NetworkConfig.ResolveHostServices(hostGateway)`) to the policy allow-list as
    **plain-TCP** endpoints, and map
    `network.publish_ports` (host → guest) so the host can reach a dev server in the
    workspace. The NetworkPolicy default-denies and permits only the allow-listed
    host services + published ports; there is no egress proxy. This live
    NetworkPolicy enforcement is the deferred bring-up end-state for `ai network`.
- [ ] **`internal/setup/setup_real.go`**
  - `realServices.Reconcile` → pull pinned images by digest (`config/versions.yaml`)
    and bring up the host service tier on the shared `aip-net` network in order
    **network → DNS → Ollama → Presidio → LLM Guard → LiteLLM(+DB) → Headroom → nginx proxy → Open WebUI**, via the detected
    runtime (`runtime.ContainerRuntime.RunArgs`, docker|podman — not hardcoded);
    health-poll each:
    - `aip-ollama` (`ollama/ollama:latest`, :11434, models bind-mounted from
      `~/.ai-platform/models` → `/models`, `OLLAMA_MODELS=/models`) —
      required local models, replaces any **native** Ollama (stop a native :11434
      first); LiteLLM reaches it by name at `api_base=http://aip-ollama:11434`.
    - `aip-presidio-analyzer` + `aip-presidio-anonymizer`
      (`mcr.microsoft.com/presidio-analyzer:latest` / `-anonymizer:latest`,
      internal :3000) — back LiteLLM's always-on PII guardrail.
    - `aip-llm-guard` (`laiyer/llm-guard-api:latest`, internal :8000, the rendered
      security-only `scanners.yml` bind-mounted at
      `/home/user/app/config/scanners.yml`) — backs LiteLLM's security-scoped legacy
      callback; **heavy** (pulls a HuggingFace model for PromptInjection on first run).
    - `aip-litellm` (`ghcr.io/berriai/litellm:main-latest`, :4000) with the rendered
      config (`callbacks: ["llmguard_moderations"]`, `LLM_GUARD_API_BASE=http://aip-llm-guard:8000`),
      plus the `aip-litellm-db` Postgres (`postgres:18.4-alpine3.24`, host
      `127.0.0.1:5442`) for the DB-backed admin UI/virtual keys.
    - `aip-headroom` (`ghcr.io/chopratejas/headroom:slim`, :8787,
      `OPENAI_TARGET_API_URL=http://aip-litellm:4000`) — input-compression proxy in
      front of LiteLLM; **INTERNAL-ONLY** (no host publish); **pulled image, never built**.
    - `aip-proxy` (`nginx:1.27-alpine`, host :18787 → `aip-headroom:8787`, the
      rendered `nginx.conf` bind-mounted at `/etc/nginx/nginx.conf:ro`) — the gateway
      ENTRY in front of Headroom; **pulled image, never built**.
    Provider keys are injected into the LiteLLM gateway (env passthrough at launch /
    its Postgres-backed store) — there is no native gateway to start.
  - **Verify the live model path + guardrail** — agent → `aip-proxy:18787` (nginx) →
    `aip-headroom:8787` → `aip-litellm:4000` (Presidio pre → Ollama or cloud →
    Presidio post). Confirm the rendered config carries both default-on guardrails
    (`presidio-pii-input` pre_call, `presidio-pii-output` post_call) and that a
    **cloud** route still gets PII masking (default-on guardrails run on every
    request, so cloud cannot bypass).
  - **hardware bring-up: LLM Guard security round-trip** — with `aip-llm-guard` up
    and LiteLLM's `llmguard_moderations` callback wired (`LLM_GUARD_API_BASE`),
    confirm a prompt-injection attempt is blocked and a `Bearer …`/secret in the
    prompt is redacted, while an ordinary coding prompt passes untouched (the
    security-only scanner scope: PromptInjection + Secrets + bearer-token Regex, NOT
    PII/Anonymize/Toxicity). `serviceHealthy("llm-guard")` is currently
    container-running only; add the live HTTP readiness probe here.
  - **hardware bring-up: nginx → Headroom → LiteLLM gateway path** — confirm the
    `aip-proxy` nginx entry on host :18787 forwards to the now-internal-only Headroom
    (`aip-headroom:8787`) and on to LiteLLM end-to-end, including **streamed (SSE)**
    responses flushing through (buffering off, long timeouts). In server mode verify
    the 0.0.0.0:18787 bind reaches a remote client. `serviceHealthy("proxy")` is
    currently container-running only; add the live readiness probe here.
  - `realServices.Status` → real probes (`<rt> ps`, gateway `/health`, …).
  - **Provider-key injection** → load the real provider keys into the LiteLLM
    gateway (env passthrough at launch / its Postgres-backed store; verified API
    from step 2). Keep keys out of platform disk and out of the workspace.
- [ ] **Headroom strategy wiring** (arch §8–10) — Headroom is now the shared host
  container `aip-headroom` (see the Reconcile item above), **not** installed inside
  the workspace image. It has no named-strategy header, so the per-project
  `context.strategy` (conservative/balanced/aggressive) maps to Headroom's two real
  per-request body knobs `keep_turns`/`output_buffer_tokens` —
  (8,12000)/(5,8000)/(2,4000), `internal/contextopt.HeadroomParams` — which are
  baked into the agent CLI's request `extra_body` at workspace start. Verify the
  agent points at `aip-headroom:8787` (via `AI_PLATFORM_HOST`) and that the knobs
  take effect live. Pin the Headroom image in `config/versions.yaml`.
- [ ] **`internal/secrets/secrets_real.go`**
  - Credentials are **keys-in-LiteLLM**: real provider keys live in the LiteLLM
    gateway, never on platform disk or in the workspace (arch §17).
    `realBroker.Set/Map/Remove/List` → LiteLLM credential-store API; the workspace
    agent is given only a scoped virtual key. Keep values out of platform disk;
    `List` returns names/metadata only.
- [ ] **`internal/setup/setup.go` startup ordering** — confirm container runtime +
      Microsandbox verified → service tier (Ollama → Presidio → LiteLLM+DB →
      Headroom) with provider keys loaded into LiteLLM (arch §5).

## 4. Turn on the acceptance tests

In `test/acceptance/` (harness, PTY driver, and the runnable `[S1]` subset already
pass):

- [x] **HTTPS mock-provider** fixture — `test/acceptance/mockprovider_test.go`
      (OpenAI-compatible `/health` + `/v1/chat/completions`, records the credential,
      serves TLS via `httptest`, exposes its cert via `writeCertPEM`). Has its own
      non-hardware unit test.
- [x] Core service `[S1]` tests written (gated by `hardwareAvailable()`, ready to
      run on a provisioned host) — `test/acceptance/s1_hardware_test.go`:
  - §9.1 credentialed request (the real provider key lives in LiteLLM and reaches
    the mock provider, **never** the workspace; only a scoped virtual key is in the
    workspace env);
  - §16.2 workspace isolation (host fs + Docker socket unreachable);
  - §16.3 egress confinement (LiteLLM gateway reachable; non-allow-listed
    destinations denied by the default-deny NetworkPolicy).
  - On hardware these will exercise the real `ErrPending` seams — fill those in
    until the tests pass.
- [ ] Remaining `[S1]` tests still to add: §2.1–2.3 setup/idempotency, §7.1/§7.2
      model status/test, §6.1/§6.3/§6.4 Dockerfile/agent-CLI/stack probes.
- [ ] Point `make test-acceptance` at the suite (currently a stub) and enable the
      self-hosted Apple Silicon job in `.github/workflows/ci.yml` (`acceptance-s1`).

## 5. Smoke sequence (manual, on the host)

1. `ai doctor` → all checks green.
2. `ai setup` → exit 0; service tier (Ollama, Presidio, LiteLLM + DB, Headroom)
   up; templates installed.
3. `ai project create demo` (wizard) → project scaffolded **and** its workspace
   microVM starts.
4. `ai workspace exec demo -- uname -a` → runs inside the microVM.
5. `ai secrets set openai --stdin` + `ai secrets map openai --env OPENAI_API_KEY`;
   `ai models test gpt-5` → works (real key lives in LiteLLM, only a scoped
   virtual key in the workspace).
6. From the workspace, a non-allowlisted destination is **denied** (default-deny
   Microsandbox NetworkPolicy).
7. `make test-acceptance` → the `[S1]` suite is green.

## 6. Deferred beyond Slice 1

- Podman / Linux (S6), Headroom + Caveman context optimization (S2), overlay
  persistence hardening (S4), extended OS templates (S5).
