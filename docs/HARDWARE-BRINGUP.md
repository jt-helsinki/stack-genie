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
  Presidio → LiteLLM (+ DB) → Headroom →
  nginx proxy (LAST)**, via the detected runtime (docker|podman). nginx
  (`aip-proxy`) is the sole host entry — all other containers are internal-only.
- **Egress net-rules** — the project `network` block is rendered by
  `egress.MsbNetworkArgs` and applied at `realSandbox.Create` (default-deny +
  allow-listed host services + published ports).
- **In-VM gateway + host-service reachability** — `egress.MsbNetworkArgs` targets
  msb's `host` GROUP token (`allow:egress@host:tcp:<port>`) for the LOCAL gateway
  rule (standalone) and the `gateway`/empty host-service allow-list entries — NOT
  the resolved `host.microsandbox.internal` NAME. A host-NAME target lets the guest
  connect to the msb gateway but does NOT engage msb's host-forwarding, so the
  request never reaches the host service and the guest gets an empty reply (verified
  live). A remote gateway (client mode) and explicit domain/IP host-services stay
  verbatim. Verified live: the in-VM agent reaches the gateway (`/v1/models` → 200
  with the served list) and `refresh-models` succeeds.
- **In-VM DNS under default-deny** — `egress.MsbNetworkArgs` emits an always-on
  DNS allow pair (`allow:egress@host:udp:53` + `allow:egress@host:tcp:53`, msb's
  `host` GROUP token = `Rule::allow_dns()`) right after the gateway allow rule, in
  EVERY mode. Without it, a default-deny policy whose rules are all host-name/IP
  based matches nothing at DNS-decision time and every lookup is denied; the `host`
  group matches the local gateway forwarder the query is delivered to (the only
  target that re-opens DNS). Verified live — name resolution works under
  `egress deny`.
- **DNS egress audit** — `aip-dns` (CoreDNS) logs every queried name;
  `ai network log` surfaces it.
- **In-VM container runtime boots and persists** — `Manager.ensureContainerd`
  (`internal/workspace/workspace.go`) probes `nerdctl info` and, if down, boots
  containerd detached then **polls `nerdctl info` for true readiness** (bounded to
  ~30s, 150 × 0.2s) so the daemon both establishes AND serves requests before the
  boot exec returns (msb tears down the exec's process group on return, which would
  otherwise kill the just-forked daemon; the poll also keeps the exec alive until
  then). It **returns whether the runtime is ready**, and `Manager.Start` starts the
  in-VM apps only when it is — otherwise it skips them with one clear warning rather
  than letting nerdctl fatal on a dead socket. `Sandbox.ExecRoot` runs `msb exec -u
  root` (msb's no-`-u` default is the unprivileged `workspace` uid 1000, NOT root —
  so the earlier rootful boot failed with a silently-swallowed permission error).
  Verified live: a plain `ai start` now leaves containerd running, which also
  unblocks the in-VM apps.
- **Virtual-key minting + provider-key storage** — `litellm.KeyManager` mints the
  scoped agent virtual key and stores provider keys in LiteLLM's credential store
  (keys-in-LiteLLM, §17), fronted by `ai keys`.
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
      falsifiable) on a live host. NOTE the default egress mode is now **"public"**
      (allow-outbound — a deliberate security-posture change so in-VM nerdctl can
      pull images and AI processes reach the internet); the deny properties below
      apply when the project is re-locked with `ai network egress deny`:
  1. a workspace reaches an **allow-listed** host service (start a host Postgres,
     allow-list `gateway:5432`, connect from inside the microVM);
  2. under `egress deny`, a **non-allow-listed** host/internet destination is
     **denied** (default-deny); under the **default "public"**, the open internet
     is reachable while **private ranges** stay blocked by the default-deny
     fallthrough — verify both;
  3. a **published** guest port (`network.publish_ports`) is reachable from the host;
  4. the model-gateway path (nginx → Headroom → LiteLLM) remains reachable in
     every mode (the always-on host-gateway allow rule).

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

`realServices.serviceHealthy("proxy")` now runs a **live** readiness probe
(`proxyReachable` → GET `http://127.0.0.1:18787/health/liveliness` through the
nginx → Headroom → LiteLLM chain; any HTTP response = up-and-forwarding, only a
transport error = down). What remains is live-host verification of the forward
chain itself. The remaining verification work:

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
      mode verify the 0.0.0.0:18787 bind reaches a remote client. (The live
      `serviceHealthy("proxy")` readiness probe is already wired — see §2.3 intro;
      this item is the live end-to-end confirmation of the forward chain.)
- [ ] **nginx as the SOLE host entry — the new routes** — every service container
      is now INTERNAL-ONLY on `aip-net` (LiteLLM, Ollama, Presidio, Headroom no
      longer host-publish); only nginx publishes. Verify on a live
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

- [ ] **UI Host-based vhost on the single :18787 (the new topology)** — the single
      host UI is now a subdomain (NOT a separate host port): nginx matches it by
      `server_name` on the same :18787. Verify on a live host:
  - `http://litellm.<domain>:18787/` serves the LiteLLM admin UI (redirects `/` →
    `/ui`; proxies to `aip-litellm:4000`, bypassing Headroom);
  - `<domain>` is the resolved platform base domain (default `aip.local`; `ai domain`).
  - (Open WebUI is now a per-workspace in-VM app, not a host vhost; Odysseus was removed.)
- [ ] **Standalone `/etc/hosts` write (the sudo seam)** — `ai setup` in standalone
      mode prompts for consent and writes the managed block via
      `uihosts.sudoWriteHosts` (temp file → `sudo cp <tmp> /etc/hosts`). Confirm on a
      live host the sudo prompt appears, the block is written (`litellm.<domain>` →
      127.0.0.1), the name then resolves, and the
      no-TTY/`--json`/declined paths print the manual block instead (never fail
      setup). `ai uninstall` removes the block the same way (standalone only).
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
  - standalone/client (loopback) are OPEN: `ai setup` does NOT prompt for a
    LiteLLM password.
  - server (`0.0.0.0`) requires auth: `ai setup` forces a non-empty LiteLLM admin
    password (generated if not entered).
  - **`ai litellm password`** relaunches the live LiteLLM with the new password —
    verify the relaunch + login work end-to-end on a provisioned host (the
    relaunch is mocked in unit tests).
- [ ] **`~/.ai-platform/.ai-platform.env` auto-load** — confirm that a password saved via the
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

### 2.5b LiteLLM Postgres data-dir on a host bind mount

Every host-persisted **system** volume now lives under `~/.ai-platform/volumes/<name>`
(so it is discoverable in one place and removed by `ai uninstall --purge`, which
`RemoveAll`s `~/.ai-platform`). Two volumes today:

- `~/.ai-platform/volumes/litellm-db` → bind-mounted into `aip-litellm-db` at
  `/var/lib/postgresql` (the LiteLLM Postgres data dir). This replaces the former
  `aip-litellm-db-data` Docker **named volume**.
- `~/.ai-platform/volumes/models` → the Ollama model store (moved from the old
  `~/.ai-platform/models`).

- [ ] **Verify `initdb` succeeds on the bind mount.** Postgres on a host bind mount
      has data-dir **ownership** quirks: the container's `postgres` UID must own (or
      be able to `chown`) the host dir, and macOS Docker Desktop's gRPC-FUSE mount vs
      Linux **rootless** (userns-remapped UIDs) handle this differently. The dir is
      created `0700` before `docker run`. If `initdb` fails on a given host, a
      uid/`:Z` (SELinux relabel) tweak on the `-v` may be needed — wire it here.
- [ ] **Migration caveat (acceptable for this dev platform):** data in the OLD
      `aip-litellm-db-data` named volume and the OLD `~/.ai-platform/models` does
      **not** auto-migrate — the next `ai setup` re-`initdb`s the Postgres data dir
      and re-pulls Ollama models into the fresh dirs. `ai uninstall` still
      best-effort `docker volume rm aip-*`s the legacy named volume on upgrade.

### 2.6 In-VM container runtime (Phase 0 — arch §7)

Every workspace microVM image now ships a **rootful** OCI container runtime —
containerd + nerdctl + runc + CNI plugins + buildkit — installed from the pinned
`nerdctl-full` release tarball (`NERDCTL_VERSION=2.3.3`, arch-aware amd64/arm64,
extracted to `/usr/local`) in every OS base Dockerfile, plus the runtime OS deps
CNI needs (`ca-certificates`, `iptables`/`iptables-nft`, `iproute`/`iproute2`).
The runtime is **started at workspace start**, not baked running into the image:
`Manager.Start` calls `ensureContainerd`, which probes `nerdctl info` (as root via
`Sandbox.ExecRoot` = `msb exec -u root`) and, if the daemon is not up, boots it
**detached** (`setsid sh -c 'containerd >/var/log/containerd.log 2>&1 &'`) then
**polls `nerdctl info` for true readiness** (bounded to ~30s) to keep the boot exec
alive until the daemon both establishes and serves requests — msb tears down the
exec's process group on return, which would otherwise kill the just-forked daemon.
`ensureContainerd` returns whether the runtime came up ready, and `Start` runs the
in-VM apps only when it did (else it skips them with one warning). This is
**best-effort** — a failure logs a warning and does NOT fail the workspace start.

The containerd boot and its daemon-persistence are **verified on the Apple Silicon
host** (a plain `ai start` leaves containerd running, which also unblocks the in-VM
apps); the host-side wiring (Dockerfile install lines, `ExecRoot` argv assembly,
the probe + boot argv with the socket poll, the best-effort swallowing) is
unit-tested. What remains:

- [x] **containerd boots and persists in the microVM** — verified: `nerdctl info`
      reports a running daemon after `ai start` (the `nerdctl info` readiness poll
      keeps the detached daemon alive past the boot exec's return), and `ExecRoot`
      running as `-u root` gives the rootful runtime the uid 0 it needs.
- [ ] **`nerdctl run` end-to-end** — confirm `msb exec <name> -- nerdctl run --rm
      hello-world` works (proves runc + CNI + image pull through the now-`public`
      egress) — the live `nerdctl run`/`pull` is still a bring-up item (it overlaps
      §2.7's in-VM apps).
- [ ] **arch-aware tarball** — confirm `uname -m` → `arm64` on Apple Silicon
      selects `nerdctl-full-2.3.3-linux-arm64.tar.gz` (and `amd64` on Linux x86_64).

### 2.7 In-VM apps (Phase 1 — arch §7, CLI §4.5c)

On the Phase-0 runtime the platform runs opt-in AI apps as `nerdctl` containers
**inside** the workspace microVM — Open WebUI and AnythingLLM (`internal/apps`).
Each app is a declarative manifest (image+tag pin, container port, persisted data
dir, optional `/workspace` mount, gateway env). The host-side orchestration is
fully unit-tested with fakes:

- the manifest env (each app points at the resolved gateway
  `http://host.microsandbox.internal:18787/v1` with the workspace's scoped virtual
  key — the catalog-driven system has no default model, so the model handle is
  empty);
- **port allocation** — a unique host port per `(workspace, app)`, reserved
  machine-wide across all workspaces' configs and persisted in the project
  `config.yaml`'s `apps:` block, published ONLY while installed via the existing
  `egress.MsbNetworkArgs` `-p <port>:<port>` plumbing (`apps.PublishedPorts` merged
  into the network publish set at `Manager.Start`);
- the lifecycle Manager (install/remove/update/start/stop/restart/list) and the
  `nerdctl run -d` argv (`-p <port>:<containerPort>`, `-v
  /persist/apps/<key>:<DataDir>`, optional `-v /workspace:/workspace`, the gateway
  `-e` env, `--restart always`);
- **on-demand** start: apps are NOT auto-started at `ai start` (a heavy image pull
  is a long in-VM exec that would block the workspace start and every other exec for
  its duration). `Manager.Start` only publishes their ports and brings containerd up;
  each app starts when requested via `ai apps start`/`add`/`restart` (`runContainer`
  calls `EnsureRuntime` to bring containerd up first, retrying once if the runtime is
  unreachable). The microVM is created with 4G memory so a heavy app pull does not
  OOM-kill the in-VM containerd.

The LIVE `nerdctl` behaviour is the bring-up item (grep `hardware bring-up` in
`internal/apps` and `internal/workspace`):

- [ ] **app containers run in the microVM** — on a provisioned host, `ai apps add
      openwebui` then `ai restart` should publish the host port and run
      `aip-app-openwebui`; confirm `nerdctl ps` in the VM shows it and the app is
      reachable on `http://localhost:<port>` from the host, routed through the
      gateway (`ai apps list` STATUS → `running`).
- [ ] **`nerdctl pull` on update** — `ai apps update <app>` pulls the latest image
      and recreates the container.
- [ ] **persisted data survives restart** — data written under
      `/persist/apps/<key>` (the overlay) is retained across `ai restart`.

### 2.8 SDK workspace backend — live validation (arch §7; docs/MSB-SDK-MIGRATION.md)

The default workspace backend is now the in-process **Microsandbox Go SDK**
(`internal/workspace/workspace_sdk.go`; `AIP_WORKSPACE_BACKEND=cli` reverts to the
msb-CLI backend). A bounded lifecycle smoke (create/boot, exec incl. root, FS
write/read, `LogStream` history+follow, Detach→reconnect, stop/remove, and an
`msb load`-ed local image booting via `WithImage` + `PullPolicy=Never`) **passed**
on Apple Silicon (msb 0.6.1). The seams that still need validation **by real use**
(a TTY and/or the full service tier — not reproducible headless):

- [ ] **Interactive Attach (P3)** — `ai shell` / `ai attach` / `ai agent` route
      through `sdkSandbox.ExecInteractive` → `sb.Attach(ctx, tmux…)` (a relay-native
      PTY; no guest sshd). Validate on a real terminal: tmux truecolor, CSI-u
      extended keys (OpenCode shift+enter / ctrl-combos), and **window-resize
      forwarding** on SIGWINCH. If `sb.Attach` does not relay resize as cleanly as the
      SDK's `SSH().OpenClient().Attach` (which documents PTY-resize relay), switch the
      interactive path to the SSH client. The CLI already guards a non-TTY caller
      (`interactive(emitter)`), so `ExecInteractive` is only reached with a real TTY.
- [ ] **Full create→start→agent-config flow (item 4)** — `ai create` (SDK-created
      microVM with the resolved `--cpus`/`--memory`/`--ports`/`--location`) →
      `ai start` with a live aip-dns/gateway + overlay mounts → agent provider config
      injection (`registerAgentProviders` via `Sandbox.WriteFile`/`Exec`) → in-VM apps
      (`ai apps`). Confirm the published ports reach the guest and the resource caps
      apply.
- [ ] **Streaming log + metrics tabs live** — the Workspace **Sandbox Logs** tab
      (SDK `LogStream`, relay-free) streams history then new entries; the **Metrics**
      tab (`sb.MetricsStream`) updates the table every 2s; the **Sandbox
      Configuration** block shows the live `SandboxConfig`.
- [ ] **Single-handle churn fix** — confirm a long `ai ui` session holds at most one
      relay client per workspace (no "max clients" growth) and releases it on project
      switch / exit.

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
2. `ai setup` → exit 0; the core service tier (DNS, Ollama, Presidio, LiteLLM +
   DB, Headroom, and the nginx proxy LAST) up; templates installed. There are no
   optional host services.
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
6. `ai keys add openai --stdin` (registers that provider's catalog models in the
   gateway); `ai models test gpt-5` → works (real key lives encrypted in LiteLLM,
   only a scoped virtual key in the workspace).
7. The default egress mode is **"public"** — from a fresh workspace the open
   internet is reachable (in-VM `nerdctl` can pull images) while private ranges stay
   blocked, and `ai network log` shows the resolved names. Re-lock with `ai network
   egress deny`, then confirm a non-allowlisted destination is **denied**
   (default-deny Microsandbox net-rules) while the gateway stays reachable.
8. `make test-acceptance` → the `[S1]` suite is green.

## 5. Deferred beyond Slice 1

- Podman / Linux hardening (S6), overlay persistence hardening (S4), extended OS
  templates (S5).
