# Hardware bring-up checklist

Slice 1 is implemented host-side **and** verified end-to-end on a provisioned
Apple Silicon host: `ai setup` launches the full service tier; LiteLLM mints
scoped agent virtual keys and holds provider keys; workspace OCI images build and
boot as Microsandbox microVMs; commands run inside them; the
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
  Presidio → LiteLLM (+ DB) → Headroom → Open WebUI/Odysseus (if enabled) →
  nginx proxy (LAST)**, via the detected runtime (docker|podman). nginx
  (`aip-proxy`) is the sole host entry — all other containers are internal-only.
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
- [ ] `git`, `gh` — workspace agent tooling and the acceptance suite's
      `requireGit` gate; the platform itself runs no git (VCS is out of scope, so
      `ai create` does **not** use git).

## 2. Remaining seams to wire/verify

### 2.1 Host-gateway pinning + networking reachability spike (arch §29.2, §29.5)

The host↔workspace networking is the last "will it actually work" unknown. The
host-gateway address is now pinned; egress net-rules are applied at create, and
allow-listed host services that use the `gateway` token resolve to it in every mode.

- [x] **Host gateway pinned.** The guest→host address is the fixed,
      backend-independent Microsandbox DNS name `host.microsandbox.internal` (the
      same under HVF on macOS and KVM on Linux, verified end-to-end during bring-up).
      `runtime.HostGateway(_)` returns `(DefaultGatewayHost, true)`, Detect persists
      it to `config/runtime.yaml` as `host_gateway`, and `egress`'s host-gateway
      target derives from the same `runtime.DefaultGatewayHost`. `Info.HostAddress()`
      prefers the `AI_PLATFORM_HOST` env override, else this value.
- [ ] **Prove the four properties** (these make "it works" + "egress is confined"
      falsifiable) on a live host:
  1. a workspace reaches an **allow-listed** host service (start a host Postgres,
     allow-list `gateway:5432`, connect from inside the microVM);
  2. a **non-allow-listed** host/internet destination is **denied** (default-deny);
  3. a **published** guest port (`network.publish_ports`) is reachable from the host;
  4. the model-gateway path (nginx → Headroom → LiteLLM) remains reachable while
     everything not allow-listed stays denied.

### 2.2 Live service/microVM log capture (`ai logs`)

- [x] **Live service-log viewing is done.** `ai logs --service <svc> --follow`
      streams `<runtime> logs -f` to stdout, and the `ai ui` Services view shows a
      service's logs full-pane, auto-refreshing (the `l` key). The on-disk
      snapshot (`ai setup` / `ai services status` → `~/.ai-platform/logs`) remains
      for the non-follow path.
- [ ] **microVM (`msb`) log follow** — `msb logs -f <name>` streaming for
      `ai logs --workspace <p> --follow` is the remaining piece (needs a running
      microVM to verify).

### 2.3 Tool-firewall verification + nginx proxy readiness probe

`realServices.serviceHealthy` treats `proxy` as healthy once the container is
running. The remaining verification work:

- [ ] **Tool-firewall (`tool_permission`) live verification** — the firewall denies
      destructive command tool-calls (`rm -rf`, `git push --force`, `git reset
      --hard`, `terraform destroy`, `kubectl delete`, `dd`, `mkfs`) at the gateway.
      The config was corrected against LiteLLM's verified semantics
      (`tool_permission` matches `allowed_param_patterns` with **re.fullmatch**, so
      `destructiveCommandRegex` is `.*( … ).*`-wrapped; `default_action: allow` +
      `decision: deny` rules block on match). `shellToolNameRegex` and the command
      param paths (`command`, `command[]`, `cmd`) were verified against each agent's
      tool schema (opencode/pi `bash`→`command`, claude-code `Bash`→`command`, codex
      `shell`→`command[]` array / `exec_command`→`cmd` / `shell_command`→`command`,
      gemini `run_shell_command`→`command`). Remaining LIVE checks:
  - confirm on a real host that `git push --force` is **blocked** (HTTP error) while
    `git status` passes, for each installed agent CLI;
  - **codex `shell` array semantics** — LiteLLM matches the `command[]` path
    **per element**; verify whether a destructive element in `["bash","-lc","rm -rf
    /"]` triggers the deny (any-element) or is missed (all-element) and adjust if
    needed.
  KNOWN LIMIT (not a bring-up item — inherent): the firewall only sees the model's
  structured tool-call args, so chained/compound commands inside one call, scripts
  the agent writes then runs, free-text-parsed actions, and obfuscation
  (`rm -r -f`, `$(echo rm) -rf`) are invisible. The microVM isolation + default-deny
  egress remain the hard boundary; the firewall is thin defence-in-depth.
- [ ] **In-process prompt-injection** — confirm the `detect_prompt_injection`
      callback flags an injection attempt without corrupting ordinary coding prompts
      (watch for false positives, the reason general PII masking was dropped).
- [ ] **nginx → Headroom → LiteLLM gateway path** — confirm the `aip-proxy` nginx
      entry on host :18787 forwards to the internal-only Headroom
      (`aip-headroom:8787`) and on to LiteLLM end-to-end, including **streamed
      (SSE)** responses flushing through (buffering off, long timeouts). In server
      mode verify the 0.0.0.0:18787 bind reaches a remote client. Add the live
      readiness probe to `serviceHealthy("proxy")`.
- [ ] **nginx as the SOLE host entry — the new routes** — every service container
      is now INTERNAL-ONLY on `aip-net` (LiteLLM, Ollama, Open WebUI, Odysseus +
      companions no longer host-publish); only nginx publishes. Verify on a live
      host that the new nginx routes work end-to-end:
  - `localhost:18787/llm/*` reaches the LiteLLM admin surface (e.g.
    `GET /llm/model/info`, `/llm/v1/models`, `/llm/health/liveliness`, the
    `/llm/key/*` + `/llm/credentials` paths) — the prefix is stripped to
    `aip-litellm:4000/*`. This is what the host CLI uses (`litellm.AdminBaseURL`,
    `secrets` broker, `litellm.KeyManager`);
  - `localhost:18787/ollama/api/*` reaches the Ollama HTTP API (`ollama.DefaultBaseURL`
    → `/ollama/api/tags|pull|delete|show|version`), prefix stripped to
    `aip-ollama:11434/*`;
  - the chat test (`ai models test`) on `localhost:18787/v1/chat/completions` still
    goes through Headroom → LiteLLM (the real model path), NOT `/llm`;
  - the agent microVM `/v1` path (`host.microsandbox.internal:18787/v1` → Headroom)
    is unchanged — confirm a workspace agent still routes correctly.

- [ ] **UI Host-based vhosts on the single :18787 (the new topology)** — the web
      UIs are now subdomains (NOT separate host ports :18090/:7000): nginx matches
      them by `server_name` on the same :18787. Verify on a live host:
  - `http://litellm.<domain>:18787/` serves the LiteLLM admin UI (redirects `/` →
    `/ui`; proxies to `aip-litellm:4000`, bypassing Headroom);
  - `http://chat.<domain>:18787/` serves Open WebUI (proxies `aip-open-webui:8080`,
    WebSocket upgrade working) when open-webui is enabled;
  - `http://odysseus.<domain>:18787/` serves Odysseus (proxies `aip-odysseus:7000`,
    WebSocket upgrade) when odysseus is enabled;
  - `<domain>` is the resolved platform base domain (default `aip.local`; `ai domain`).
- [ ] **Standalone `/etc/hosts` write (the sudo seam)** — `ai setup` in standalone
      mode prompts for consent and writes the managed block via
      `uihosts.sudoWriteHosts` (temp file → `sudo cp <tmp> /etc/hosts`). Confirm on a
      live host the sudo prompt appears, the block is written (all UI subdomains →
      127.0.0.1, incl. not-yet-enabled ones), the names then resolve, and the
      no-TTY/`--json`/declined paths print the manual block instead (never fail
      setup). `ai uninstall` removes the block the same way (standalone only).
- [ ] **Open WebUI / Odysseus reverse-proxy envs** — confirm the apps behave
      correctly behind their subdomain vhost: Open WebUI's `WEBUI_URL` +
      `CORS_ALLOW_ORIGIN` (set to `http://chat.<domain>:18787`) let links + the
      websocket handshake work; Odysseus's `APP_BIND=0.0.0.0` / `APP_PUBLIC_URL` /
      `SECURE_COOKIES=false` seeds are best-effort — VERIFY the exact env names
      against the live app (Odysseus configures providers in-app at `/setup`; the
      env names were not confirmable from its docs) and that `X-Forwarded-Proto` is
      forwarded once TLS terminates at nginx.
- [ ] **Server-mode DNS/TLS contract** — in server mode `ai setup`/`ai doctor` print
      the operator contract (create `*.<domain>` or per-host DNS → this server, and
      a TLS cert terminated at nginx). Confirm a remote client can reach the UI
      subdomains once real DNS + cert are in place (the platform does NOT edit
      `/etc/hosts` on a server). The server hostname is collected at `ai setup`
      (default `localhost`, which only resolves on the server itself) and persisted
      as runtime.yaml's `domain`; a real DNS name is set there or via `ai domain`.
- [ ] **http→https redirect (TLS-time requirement)** — TLS terminates PER-VHOST at
      nginx. Today nginx serves plain http on the single `:18787` entry; TLS is
      deferred. When HTTPS is configured (see the `proxyNginxConf` server-block
      comment): add a `listen 443 ssl;` + `ssl_certificate`/`ssl_certificate_key`
      (a wildcard for `*.<domain>` or per-vhost) to each vhost, and make the `:80`
      http listener redirect-only — `return 301 https://$host$request_uri;` — so
      http MUST 301-redirect to https on the single entry. Do NOT emit that redirect
      before an https listener exists (a 301 with no :443 breaks every plain-http
      caller). Verify the redirect + per-vhost TLS once a cert is in place.
- [ ] **Role-based UI auth policy** (`runtime.RequireUIAuth`, server-only). Verify
      the auth posture per role against the live UIs:
  - standalone/client (loopback) are OPEN: Open WebUI launches `WEBUI_AUTH=false`
    (no login wall) and `ai setup` does NOT prompt for a LiteLLM password.
  - server (`0.0.0.0`) requires auth: Open WebUI launches `WEBUI_AUTH=true` (the
    first signup becomes admin — confirm `ENABLE_SIGNUP` default lets that first
    account register, then consider disabling further signups), and `ai setup`
    forces a non-empty LiteLLM admin password (generated if not entered).
  - **Odysseus auth is NOT platform-settable** — it is configured in-app at
    `/setup`. On a server, confirm the in-app login is actually enabled before the
    box is network-exposed; `SECURE_COOKIES` stays `false` until TLS terminates at
    nginx, so FLIP IT ON (to `true`) once HTTPS is live (and confirm the exact env
    name against the live app — it is a best-effort seed today).
  - **`ai litellm password`** relaunches the live LiteLLM with the new password —
    verify the relaunch + login work end-to-end on a provisioned host (the
    relaunch is mocked in unit tests).
- [ ] **`~/.ai-platform.env` auto-load** — confirm that a password saved via the
      `ai setup` / `ai litellm password` persist offer survives a restart: the
      0600 file is auto-loaded at `ai` startup (existing shell env still wins) and
      the LiteLLM container relaunch picks `UI_PASSWORD`/`LITELLM_MASTER_KEY` up via
      env passthrough without the user editing their shell rc.

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
- [x] Remaining `[S1]` tests written — `test/acceptance/s1_remaining_test.go`:
      §6.1/§6.3/§6.4 Dockerfile/agent-CLI/stack probes (run host-side), §2.1–2.3
      setup/idempotency and §7.1/§7.2 model status/test (gated by
      `hardwareAvailable()`). The hardware-gated ones still need live-host
      verification (next item).
- [ ] Run/verify the acceptance suite on hardware — `make test-acceptance` already
      runs `go test -count=1 ./test/acceptance/` (the package passes `go test ./...`);
      confirm the `hardwareAvailable()`-gated `[S1]` tests go green on a live host and
      enable the self-hosted Apple Silicon job in `.github/workflows/ci.yml`
      (`acceptance-s1`).

## 4. Smoke sequence (manual, on the host)

1. `ai doctor` → all checks green.
2. `ai setup` → exit 0; service tier (DNS, Ollama, Presidio, LiteLLM +
   DB, Headroom, nginx proxy, Open WebUI) up; templates installed.
3. `ai create --name demo --os debian-trixie` → **scaffold-only**: writes the
   project's `.ai-platform/` files in the cwd and registers it; it does **not**
   build the image or boot a microVM.
4. `ai start` (or `ai start demo`) → builds the workspace OCI image and boots its
   microVM.
5. `ai exec demo -- uname -a` → runs inside the microVM (one-shot).
   `ai shell demo` (or `ai shell` from the project dir) → a real
   interactive login PTY in the microVM via `msb exec -t -u workspace` (verify line
   editing, Ctrl-C, and a clean exit back to the host).
5a. **Workspace sessions (wired; needs a live microVM to verify).** The session
   model — persistent, reattachable per-name **tmux** sessions inside the microVM,
   with a managed `~/.tmux.conf` written at start — is fully wired host-side. On a
   live microVM verify: `ai agent opencode` starts/attaches a per-CLI session;
   `ai sessions` lists it (NAME/ATTACHED/IDLE; a no-server workspace lists zero,
   not an error); detaching (`Ctrl-b d`) leaves it running and `ai attach opencode`
   reattaches; `ai shell` ↔ a second agent run concurrently; the TUI **Sessions**
   view attaches/kills via `ai attach`. (Needs `tmux` in the image — now
   in every OS Dockerfile — and a booted microVM, so it is verified during
   bring-up alongside the shell.)
6. `ai secrets set openai --stdin` + `ai secrets map openai --env OPENAI_API_KEY`;
   `ai models test gpt-5` → works (real key lives in LiteLLM, only a scoped
   virtual key in the workspace).
7. From the workspace, a non-allowlisted destination is **denied** (default-deny
   Microsandbox net-rules); `ai network log` shows the attempted name.
8. `make test-acceptance` → the `[S1]` suite is green.

## 5. Deferred beyond Slice 1

- Podman / Linux hardening (S6), overlay persistence hardening (S4), extended OS
  templates (S5).
