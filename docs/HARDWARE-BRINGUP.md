# Hardware bring-up checklist

Slice 1 is implemented host-side **and** verified end-to-end on a provisioned
Apple Silicon host: `ai setup` launches the full service tier; LiteLLM mints
scoped agent virtual keys and holds provider keys; workspace OCI images build and
boot as Microsandbox microVMs; `ai workspace exec` runs inside them; the
per-project `ai network` egress policy is applied as `msb` net-rules at workspace
create; and the DNS egress audit (`ai network log`) works.

This file is the **remaining** punch list — only the seams that still depend on
further bring-up. Everything not listed here (workspace build/boot/exec, the
service-tier launch, virtual-key minting, egress net-rules, the DNS audit) is
done. The few genuinely-unwired spots are grep-able as `hardware bring-up` in the
code.

## What is already done (no longer pending)

- **Workspace build + microVM lifecycle** — `internal/workspace/workspace_real.go`:
  `realBuilder.Build` runs `<rt> build` then `msb load`; `realSandbox`
  Create/Start/Stop/Destroy/Exec/WriteFile drive `msb`; the project mounts at
  `/workspace`, the overlay (§26) at `/persist`, and the microVM boots with
  `--dns-nameserver` at the `aip-dns` audit resolver.
- **Service-tier launch** — `internal/setup/setup_real.go`: `realServices.Reconcile`
  brings up the container tier on `aip-net` in order **network → DNS → Ollama →
  Presidio → LLM Guard → LiteLLM (+ DB) → Headroom → nginx proxy → Open WebUI**,
  via the detected runtime (docker|podman).
- **Egress net-rules** — the project `network` block is rendered by
  `egress.MsbNetworkArgs` and applied at `realSandbox.Create` (default-deny +
  allow-listed host services + published ports).
- **DNS egress audit** — `aip-dns` (CoreDNS) logs every queried name;
  `ai network log` surfaces it.
- **Virtual-key minting + provider-key storage** — `litellm.KeyManager` mints the
  scoped agent virtual key; `secrets.Broker` stores provider keys in LiteLLM's
  credential store (keys-in-LiteLLM, §17).
- **Live Ollama health probe** — `internal/ollama` `RealProbe` does a real
  `GET /api/version`; wired into `ai doctor` and `realServices.serviceHealthy`.

## 1. Provision the host (prerequisites)

- [ ] Apple Silicon Mac (Intel is unsupported — the Microsandbox runtime needs
      the Apple Hypervisor). `ai doctor` must report `host virtualization: ok (hvf)`.
- [ ] Go 1.26+.
- [ ] **Docker**, rootless. `ai doctor` → `container runtime: ok (docker)`;
      `ai setup` must verify rootless (`runtime.Verify`, exit 4 if not).
- [ ] **Microsandbox** (`msb`) on `PATH`, code-signed with the
      `com.apple.security.hypervisor` entitlement under Developer ID + notarization.
- [ ] `git`, `gh` (already used by project create).

## 2. Remaining seams to wire/verify

### 2.1 Host-gateway pinning + networking reachability spike (arch §29.2, §29.5)

The host↔workspace networking is the last "will it actually work" unknown.
`runtime.HostGateway(goos)` currently returns `("", false)`; egress net-rules are
already applied at create, but allow-listed host services that use the `gateway`
token cannot resolve until this is pinned.

- [ ] **Pin the host gateway.** Find the gateway address of Microsandbox's
      host-side userspace stack (the address the guest reaches the host at) from
      the SDK; implement `runtime.HostGateway` to return it per `GOOS`, and have
      `ai setup` persist it to `config/runtime.yaml` as `host_gateway`.
      `Info.HostAddress()` already prefers the `AI_PLATFORM_HOST` env override,
      else this value.
- [ ] **Prove the four properties** (these make "it works" + "egress is confined"
      falsifiable) on a live host:
  1. a workspace reaches an **allow-listed** host service (start a host Postgres,
     allow-list `gateway:5432`, connect from inside the microVM);
  2. a **non-allow-listed** host/internet destination is **denied** (default-deny);
  3. a **published** guest port (`network.publish_ports`) is reachable from the host;
  4. the model-gateway path (nginx → Headroom → LiteLLM) remains reachable while
     everything not allow-listed stays denied.

### 2.2 Live service/microVM log capture (`ai logs`)

- [ ] `ai logs` currently shows logs already on disk. The live capture that
      produces service/microVM logs is the deferred seam (see
      `internal/cli/logs.go`, grep `hardware bring-up`).

### 2.3 Live HTTP readiness probes for LLM Guard and the nginx proxy

`realServices.serviceHealthy` treats `llm-guard` and `proxy` as healthy once the
container is running (parity with Presidio). The live HTTP round-trips are the
remaining probe work:

- [ ] **LLM Guard security round-trip** — with `aip-llm-guard` up and LiteLLM's
      `llmguard_moderations` callback wired (`LLM_GUARD_API_BASE`), confirm a
      prompt-injection attempt is blocked and a `Bearer …`/secret in the prompt is
      redacted, while an ordinary coding prompt passes untouched (the security-only
      scanner scope: PromptInjection + Secrets + bearer-token Regex, NOT
      PII/Anonymize/Toxicity). Add the live HTTP readiness probe to
      `serviceHealthy("llm-guard")`.
- [ ] **nginx → Headroom → LiteLLM gateway path** — confirm the `aip-proxy` nginx
      entry on host :18787 forwards to the internal-only Headroom
      (`aip-headroom:8787`) and on to LiteLLM end-to-end, including **streamed
      (SSE)** responses flushing through (buffering off, long timeouts). In server
      mode verify the 0.0.0.0:18787 bind reaches a remote client. Add the live
      readiness probe to `serviceHealthy("proxy")`.

### 2.4 Headroom strategy verification (arch §8–10)

- [ ] Headroom runs as the shared host container `aip-headroom`. The per-project
      `context.strategy` (conservative/balanced/aggressive) maps to Headroom's two
      real per-request body knobs `keep_turns`/`output_buffer_tokens` —
      (8,12000)/(5,8000)/(2,4000), `internal/contextopt.HeadroomParams` — baked
      into the agent CLI's request `extra_body` at workspace start. Verify the
      knobs take effect live against the running proxy.

### 2.5 Secret/credential masking round-trip (arch §15, §17)

- [ ] Verify the live guardrails on every route (cloud included): the always-on
      Presidio **secret** guardrails (`presidio-secrets-input` pre_call /
      `presidio-secrets-output` post_call, scoped to CREDIT_CARD, US_SSN,
      US_BANK_NUMBER, IBAN_CODE, CRYPTO) plus `hide-secrets` mask
      secrets/credentials, while ordinary coding prompts pass untouched (general
      PII masking is deliberately not done). Because the guardrails are
      `default_on: true` and every route traverses the proxy, a cloud route cannot
      bypass them.

## 3. Turn on the remaining acceptance tests

In `test/acceptance/` (harness, PTY driver, and the runnable `[S1]` subset already
pass):

- [x] **HTTPS mock-provider** fixture — `test/acceptance/mockprovider_test.go`
      (OpenAI-compatible `/health` + `/v1/chat/completions`, records the credential,
      serves TLS via `httptest`, exposes its cert via `writeCertPEM`).
- [x] Core service `[S1]` tests written (gated by `hardwareAvailable()`) —
      `test/acceptance/s1_hardware_test.go`: §9.1 credentialed request, §16.2
      workspace isolation, §16.3 egress confinement.
- [ ] Remaining `[S1]` tests still to add: §2.1–2.3 setup/idempotency, §7.1/§7.2
      model status/test, §6.1/§6.3/§6.4 Dockerfile/agent-CLI/stack probes.
- [ ] Point `make test-acceptance` at the suite (currently a stub) and enable the
      self-hosted Apple Silicon job in `.github/workflows/ci.yml` (`acceptance-s1`).

## 4. Smoke sequence (manual, on the host)

1. `ai doctor` → all checks green.
2. `ai setup` → exit 0; service tier (DNS, Ollama, Presidio, LLM Guard, LiteLLM +
   DB, Headroom, nginx proxy, Open WebUI) up; templates installed.
3. `ai project create demo` (wizard) → project scaffolded **and** its workspace
   microVM builds and starts.
4. `ai workspace exec demo -- uname -a` → runs inside the microVM.
5. `ai secrets set openai --stdin` + `ai secrets map openai --env OPENAI_API_KEY`;
   `ai models test gpt-5` → works (real key lives in LiteLLM, only a scoped
   virtual key in the workspace).
6. From the workspace, a non-allowlisted destination is **denied** (default-deny
   Microsandbox net-rules); `ai network log` shows the attempted name.
7. `make test-acceptance` → the `[S1]` suite is green.

## 5. Deferred beyond Slice 1

- Podman / Linux hardening (S6), overlay persistence hardening (S4), extended OS
  templates (S5).
