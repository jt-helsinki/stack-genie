# Hardware bring-up checklist

**Post-nono-migration note:** the workspace runtime changed from
hardware-isolated **Microsandbox microVMs** to **[nono](https://nono.sh)**
kernel-capability sandboxing (Landlock LSM on Linux, Seatbelt on macOS) — a
same-kernel, per-invocation process sandbox, not a VM. This is a deliberate,
accepted security-model change (same-kernel sandboxing, not hardware isolation).
A large share of the items this checklist used to track no longer apply at
all, because the capability they describe (a guest OS/image, in-VM container
runtime, in-VM apps, VM resource sizing, VM-scoped mounts, the Microsandbox Go
SDK) **no longer exists** — those sections are marked **REMOVED / MOOT** below
rather than left as open bring-up items. A few *new* seams exist in their place
(nono session lifecycle, OS-keychain credential injection, host-native agent-CLI
installers) and are called out where relevant. The Docker-based service tier
(Caddy, LiteLLM, guardrails, Valkey, DNS audit, omlx) is **unaffected** by this
migration and its bring-up status below is unchanged.

Slice 1 was implemented host-side **and** verified end-to-end on a provisioned
Apple Silicon host under the **previous** Microsandbox-microVM design: `ai setup`
launched the full service tier; LiteLLM minted scoped agent virtual keys and held
provider keys; workspace OCI images built and booted as Microsandbox microVMs;
commands ran inside them; the per-project `ai network` egress policy was applied
as `msb` net-rules at workspace create; and the DNS egress audit (`ai network
log`) worked. That verification record is kept below for history, but it no
longer describes the live code — **the nono-based workspace model has not yet
had the equivalent live-hardware verification pass** (nono session
create/attach/stop, OS-keychain credential injection, per-session nono-profile
network enforcement, and the host-native agent-CLI installers in
`internal/setup/agentcli_host.go`/`nono_host.go` are unit-tested with fakes but
not yet confirmed end-to-end on a live host). The service-tier items (service
launch, virtual-key minting, the DNS audit log, the guardrails) remain done as
described. The few genuinely-unwired spots are grep-able as `hardware bring-up`
in the code.

## REMOVED / MOOT (the retired Microsandbox-microVM design — historical record)

These items were verified live under the **previous** workspace runtime and are
kept here only as a historical record; the capability itself no longer exists,
so there is nothing left to bring up for them:

- **Workspace build + microVM lifecycle** — GONE. There is no OCI image build, no
  `msb load`, no microVM. `internal/workspace/workspace_real.go`,
  `workspace_sdk.go`, and `msb.go` (the Microsandbox Go SDK integration) were
  deleted entirely, along with `internal/envimage` (OCI image build) and
  `internal/overlay` (the persistent VM-disk overlay — the project directory on
  host IS the persistent state now). The replacement is a nono session per
  `ai agent <cli>`/`ai shell` launch (`internal/nono`, `internal/workspace`) — see
  the "Not yet verified" section below.
- **Egress net-rules via `msb`** — GONE. `egress.MsbNetworkArgs` and the whole
  `host`-GROUP-token / always-on-DNS-pair mechanism it implemented no longer
  exist. The replacement is a nono profile's `network` block (egress
  `deny`/`public` + allow/deny domain lists), regenerated on every session
  launch — see "Not yet verified" below.
- **In-VM container runtime (containerd + nerdctl + runc + CNI)** — GONE. There
  is no guest VM to run it in. `Manager.ensureContainerd`, `Sandbox.ExecRoot`,
  and the whole rootful-runtime-boot dance are deleted.
- **In-VM apps (Open WebUI, agent-CLI dashboards)** — GONE. `internal/apps` was
  deleted entirely; there is no `ai apps` command, no published app ports, and
  no in-VM app concept. Not replaced.
- **Per-project host↔guest mounts (`ai mounts`)** — GONE. nono does not
  virtualize the filesystem, so there is no guest/host mount concept, no
  isolated dirs, no shared mounts.
- **VM resource sizing (`ai resize`, vCPU/memory/disk)** — GONE. There is no VM
  to size; a nono session runs with whatever resources the host process gets.
- **OS/software-stack selection at `ai create`** (debian-trixie/ubuntu/alma;
  go/rust/java/maven/deno) — GONE. A workspace runs on whatever OS/toolchains the
  host actually has; the platform no longer provisions them.
- **tmux-based in-VM session multiplexing** — GONE. Sessions are now native nono
  sessions (`nono run --detached`/`nono attach`/`nono ps`/`nono stop`), not tmux
  inside a VM.
- **`docs/MSB-SDK-MIGRATION.md`** (the Microsandbox CLI-backend→SDK-backend
  migration) — deleted outright; it described a now-twice-superseded migration of
  a runtime that no longer exists.

## Still true / unaffected by the nono migration

The Docker-based service tier did not change:

- **Service-tier launch** — `internal/setup/setup_real.go`: `realServices.Reconcile`
  brings up the container tier on `aip-net` in order **network → DNS →
  Presidio (only when secret-masking is enabled) → Valkey (+ RedisInsight) →
  Headroom → LiteLLM (+ DB) → Caddy proxy (LAST)**, via the detected runtime
  (docker|podman). Headroom PRECEDES LiteLLM because LiteLLM's `headroom`
  compression guardrail calls it in-process. Caddy (`aip-proxy`) is the sole host
  entry — all other containers are internal-only. **Separately** (not part of the
  container reconcile loop), `Reconcile` also brings up **omlx** — the SOLE
  local-inference runtime, a HOST-NATIVE process, not an `aip-*` container — between
  `ensureLiteLLM` and `ensureProxy`: `ensurePlatformVenv` + `ensureOmlxInstalled` then
  `startOmlxServerHost` best-effort starts the ONE shared `omlx serve` process (it
  scans its own model directory and serves every model it finds — there is no
  per-model launch or port allocation), reached by the containers through the host
  gateway.
- **DNS egress audit** — `aip-dns` (CoreDNS) logs every queried name, host-wide;
  `ai network log` surfaces it (names only, not a connection verdict — a
  project's own egress enforcement is separate, see "Not yet verified" below).
- **Virtual-key minting + provider-key storage** — `litellm.KeyManager` mints the
  scoped agent virtual key and stores provider keys in LiteLLM's credential store
  (keys-in-LiteLLM, §17), fronted by `ai keys`.
- **Live omlx health probe + status** — `omlxHealthy`/`omlxStatus`
  (`internal/setup/omlx_host.go`) HTTP-probe the ONE shared omlx server's
  `GET /v1/models` endpoint (there is no persisted model-runtime store — omlx is
  daemonless-safe via its own OS-level flock, `internal/omlx/lock.go`). `ai services
  status`/`ai doctor` surface a single `omlx` `host`-mode line (running when the
  server answers, else stopped).

## NEW seams to verify (nono-based workspace model)

These replace the microVM-specific bring-up items removed above. The host-side
wiring is unit-tested with fakes (`internal/nono`, `internal/workspace`,
`internal/setup/nono_host.go`, `internal/setup/agentcli_host.go`); none of it has
had a live-hardware verification pass yet:

- [ ] **nono session lifecycle** — `nono run --detached` (launch), `nono attach`,
      `nono shell`, `nono ps --json` (list), `nono inspect --json`, `nono stop
      [--force]` against a real `nono` install. Confirm `ai agent <cli>`,
      `ai shell`, `ai sessions`, `ai attach`, `ai stop`, `ai restart` all work
      end-to-end on a live host (both macOS/Seatbelt and Linux/Landlock).
- [ ] **OS-keychain credential injection** — the scoped LiteLLM virtual key is
      stored via `internal/nono.StoreCredential`/`DeleteCredential` (macOS
      Keychain via `security`, Linux via `secret-tool`) and handed to the
      sandboxed process only via nono's `--env-credential-map`. Confirm this
      round-trip on both macOS and a Linux host with `secret-tool`/a provider
      (e.g. gnome-keyring) available, and confirm `ai delete` revokes the key
      and removes the keychain entry.
- [ ] **nono profile network enforcement** — a project's `network.egress`
      (`deny`/`public`) and allow/deny domain lists are rendered into the
      generated nono profile (`internal/workspace/profile.go`) and enforced by
      nono itself. Confirm live: `egress deny` blocks a non-allow-listed
      destination while the gateway stays reachable; `ai network allow
      <domain>`/`disallow` take effect on the *next* session launch with no
      restart step.
- [ ] **Registry-pack profile extension** — `nonoPackForCLI` extends a fresh
      profile from `nolabs-ai/opencode`/`nolabs-ai/claude`/`nolabs-ai/codex`
      (`nono pull <pack>`, best-effort/20s-bounded). Confirm the packs resolve
      against the real nono registry and the resulting profile behaves as
      expected (falls back to nono's built-in `default` profile when the pull
      fails or is slow).
- [ ] **Host-native agent-CLI installers** (`internal/setup/agentcli_host.go`) —
      `ai setup` can install claude-code, codex, and opencode directly onto the
      host via each CLI's own official installer (`claude.ai/install.sh`,
      `chatgpt.com/codex/install.sh`, `opencode.ai/install`). Confirm each
      resolves and installs cleanly, and that `nono` itself installs via `brew
      install nono` / `nono.sh/install.sh` (`internal/setup/nono_host.go`).
      gemini/omp/hermes are not in this installer set — confirm what install
      path (if any) `ai doctor`/`ai setup` guidance points them to.
- [ ] **PTY-driven live-session testing** — unlike the old `msb exec -t`-backed
      acceptance harness, there is not yet an automated integration test that
      drives `ai agent <cli>`/`ai shell` through a real PTY against a live
      `nono` session (tmux/terminfo concerns from the old design don't apply —
      nono runs the CLI directly in the caller's own terminal — but line
      editing, Ctrl-C, SIGWINCH resize, and a clean exit still need a live
      check). This is a genuinely new gap, not a carried-over one.
- [ ] **Multica service (server containers + host daemon)** —
      `internal/setup/multica_server.go`, `multica_host.go`, `internal/multica`,
      `internal/workspace/multica.go`. Verify on a provisioned host:
      (a) the two containers (`aip-multica-backend`/`aip-multica`) boot against the
      `multica` database in `aip-litellm-db` — Multica's docs call for PostgreSQL 17 and
      the LiteLLM Postgres is 18.4, so confirm every migration applies and
      `SELECT extname FROM pg_extension` (in the `multica` db) lists pgcrypto + pg_trgm;
      the backend's `/readyz` passes; uploads land in `~/.ai-platform/volumes/multica-uploads`
      (bind-mount ownership on Linux); first-account signup at `multica.<domain>:18787` works
      (the verification code is in `ai logs --service multica` until an email provider
      exists) and the browser WebSocket survives the proxy's `/ws` route;
      (b) `multica setup self-host --server-url <vhost> --app-url <vhost>` (offered by `ai setup`) signs in and starts a daemon that is already shim-first, and that daemon
      reaches the API through the web UI's rewrites on that single origin (if not, the
      backend needs its own vhost);
      (c) the release archive really holds `multica` at its root (`StoreDir()/multica`);
      (d) **the shim round-trip**: the daemon finds `claude`/`codex`/`opencode`/`omp`/
      `hermes` shims first on PATH, passes its `--version` probe through, and a real
      task runs under `nono run` (foreground, stdio passthrough with no TTY — nono must
      not write banners to stdout, or it corrupts the agent protocol the daemon parses);
      (e) the profile's workspaces-root + temp grants are enough for git worktrees
      (`.git` points into `<root>/.repos`) and Codex's home; per-CLI gateway config
      written into the task worktree (`.claude/`, `.codex/`, `.opencode/`) does not get
      committed by agents; (f) one shared scoped key (`aip-gateway-multica`) holds under
      concurrent tasks. gemini has no Multica runtime and gets no shim.

## 1. Provision the host (prerequisites)

- [ ] `nono` (https://nono.sh) — the kernel-capability sandboxing CLI that
      replaces Microsandbox as the workspace runtime (Landlock LSM on Linux,
      Seatbelt on macOS). No Apple-Silicon-only restriction and no hypervisor
      requirement — unlike the runtime it replaces, nono has no
      virtualization-capability gate; any host the binary runs on can use it.
      `ai setup` best-effort auto-installs it (`EnsureNonoInstalled`:
      `brew install nono`, else `curl -fsSL https://nono.sh/install.sh | sh`);
      `ai doctor` reports it under "nono runtime".
- [ ] Go 1.26+.
- [ ] **Docker**, rootless. `ai doctor` → `container runtime: ok (docker)`;
      `ai setup` must verify rootless (`runtime.Verify`, exit 4 if not).
- [ ] **omlx** (host-native, the SOLE local-model backend, **macOS/Apple-Silicon
      only**) — installed into the platform-managed host venv `~/.ai-platform/venv`
      by `ai setup`'s best-effort auto-install (`ensureOmlxInstalled` → `omlx.Install`,
      Detect-gated, never failing setup); a manual `ai services start omlx` retries the
      same install on demand. `omlx.Install` resolves the LATEST
      `github.com/jundot/omlx` GitHub release, picks the newest-Python-version
      prebuilt wheel, and pins the venv to that Python version via
      `pyenv.EnsureVersion`. Model download/add/remove/tune all live in omlx's OWN
      admin panel (`ai services console omlx`) — there is no `ai models pull`
      equivalent and no separate Hugging Face CLI install. omlx weights live at
      `~/.ai-platform/volumes/models/omlx`. See §2.9.
- [ ] `git`, `gh` — host dev tooling the agent CLIs may expect to find on PATH,
      and the acceptance suite's `requireGit` gate. `ai create` does **not** use
      git itself. (Whether a per-project `git init`/Graphify-hook step still runs
      at session launch under the nono model, as it did at microVM workspace
      start in the previous design, needs re-confirming against current code —
      not re-verified as part of this doc pass.)

## 2. Remaining seams to wire/verify

### 2.1 Host-gateway reachability — MOOT (REMOVED)

This whole spike was about guest↔host networking inside a Microsandbox microVM
(the pinned `host.microsandbox.internal` DNS name, `msb` net-rules resolving a
`gateway` token, and proving a published guest port was reachable from the
host). None of that applies anymore: the agent CLI runs **directly on the host**,
so reaching the gateway is a plain loopback connection
(`http://127.0.0.1:18787/v1`, `runtime.DefaultGatewayHost`/`DefaultGatewayPort`)
with no guest-network translation layer at all. The property still worth
confirming live — that `egress deny` actually blocks a non-allow-listed
destination while the gateway and allow-listed domains stay reachable — is
tracked under nono's own profile enforcement in the "NEW seams to verify"
section above, not here.

### 2.2 Live service log capture (`ai logs`)

- [x] **Live service-log viewing is done.** `ai logs --service <svc> --follow`
      streams `<runtime> logs -f` to stdout, and the `ai ui` Services view shows a
      service's logs full-pane, auto-refreshing (the `l` key). The on-disk
      snapshot (`ai setup` / `ai services status` → `~/.ai-platform/logs`) remains
      for the non-follow path.
- **microVM (`msb`) log follow** — MOOT (REMOVED). There is no persistent microVM
  log to tail anymore (no SDK `LogStream`, no CLI-backend `msb logs -f`, no
  workspace Logs/Metrics TUI sub-tabs — a nono session's output is just the
  sandboxed process's own stdout/stderr in whatever terminal launched it).
- [ ] **Service detail metrics round-trip** — the Services detail **Service**
      sub-tab's live per-container stats (`setup.ServiceStats`, `<runtime> stats
      --no-stream` + `inspect`, polled ~2s) render a `── <container> ──` block per
      container for a multi-container service (Presidio, LiteLLM+DB). The argv
      assembly + JSON parsing are unit-tested against a fake prober; the live
      round-trip against a running docker/podman engine has not been confirmed on
      a provisioned host.

### 2.3 Tool-firewall verification + Caddy proxy readiness probe

`realServices.serviceHealthy("proxy")` now runs a **live** readiness probe
(`proxyReachable` → GET `http://127.0.0.1:18787/health/liveliness` through the
Caddy → LiteLLM chain (LiteLLM calls Headroom in-process as a `pre_call`
guardrail); any HTTP response = up-and-forwarding, only a transport error = down).
What remains is live-host verification of the forward chain itself. The remaining
verification work:

- [ ] **Tool-firewall (`tool_permission`) live verification** — the firewall denies
      destructive command tool-calls (`rm -rf`, `git push --force`, `git reset
      --hard`, `terraform destroy`, `kubectl delete`, `dd`, `mkfs`) at the gateway.
      The config was corrected against LiteLLM's verified semantics
      (`tool_permission` matches `allowed_param_patterns` with **re.fullmatch**, so
      `destructiveCommandRegex` is `.*( … ).*`-wrapped; `default_action: allow` +
      `decision: deny` rules block on match). `shellToolNameRegex` and the command
      param paths (`command`, `command[]`, `cmd`) were verified against each agent's
      tool schema (opencode/omp `bash`→`command`, claude-code `Bash`→`command`, codex
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
  (`rm -r -f`, `$(echo rm) -rf`) are invisible. The nono sandbox (filesystem scope
  + egress policy) remains the hard boundary; the firewall is thin defence-in-depth
  — note this boundary is now same-kernel process sandboxing, not VM-level
  hardware isolation (a deliberate, accepted trade-off of the nono migration).
  (There is no prompt-injection callback to verify — the in-process
  `detect_prompt_injection` and the unmaintained LLM Guard were both removed as
  false-positive sources on ordinary coding traffic; see arch §15.)
- [ ] **Caddy → LiteLLM gateway path (LiteLLM calls Headroom in-process)** —
      confirm the `aip-proxy` Caddy entry on host :18787 forwards `/v1` **directly**
      to LiteLLM (`aip-litellm:4000`) end-to-end, including **streamed (SSE)**
      responses flushing through (buffering off, long timeouts), AND that LiteLLM in
      turn reaches the internal-only Headroom (`aip-headroom:8787/v1/compress`) as
      its `pre_call` compression guardrail (Caddy no longer routes to Headroom
      itself). In server mode verify the 0.0.0.0:18787 bind reaches a remote client.
      (The live `serviceHealthy("proxy")` readiness probe is already wired — see
      §2.3 intro; this item is the live end-to-end confirmation of the forward chain.)
- [ ] **Caddy as the SOLE host entry — the new routes** — every service container
      is now INTERNAL-ONLY on `aip-net` (LiteLLM, Presidio, Headroom no
      longer host-publish); only Caddy publishes (omlx is host-native,
      reached via the host gateway). Verify on a live
      host that the new Caddy routes work end-to-end:
  - `localhost:18787/llm/*` reaches the LiteLLM admin surface (e.g.
    `GET /llm/model/info`, `/llm/v1/models`, `/llm/health/liveliness`, the
    `/llm/key/*` + `/llm/credentials` paths) — the prefix is stripped to
    `aip-litellm:4000/*`. This is what the host CLI uses (`litellm.AdminBaseURL`,
    `secrets` broker, `litellm.KeyManager`);
  - the chat test (`ai models test`) on `localhost:18787/v1/chat/completions` still
    goes **directly to LiteLLM** (the real model path — LiteLLM applies the Headroom
    compression guardrail in-process), NOT `/llm`;
  - the host-sandboxed agent's `/v1` path (now plain loopback
    `http://127.0.0.1:18787/v1` → LiteLLM, not a guest-network DNS name) still
    routes correctly from a nono session.

- [ ] **UI Host-based vhosts on the single :18787 (the new topology)** — the host
      UIs are now subdomains (NOT separate host ports): Caddy matches each by
      `server_name` on the same :18787, data-driven from `services.UIVhosts()`. Verify
      on a live host, for BOTH always-on vhosts:
  - `http://litellm.<domain>:18787/` serves the LiteLLM admin UI (redirects `/` →
    `/ui`; proxies to `aip-litellm:4000`);
  - `http://valkey.<domain>:18787/` serves the RedisInsight UI (proxies to
    `aip-redisinsight:5540`), preconfigured with the standalone `aip-valkey` connection;
  - `<domain>` is the resolved platform base domain (default `aip.local`; `ai domain`).
  - (Open WebUI and Odysseus were both removed from the platform entirely —
    neither is a host vhost nor an in-VM app anymore; there is no bundled
    web-chat UI today.)
- [ ] **Standalone `/etc/hosts` write (the sudo seam)** — `ai setup` in standalone
      mode prompts for consent and writes the managed block via
      `uihosts.sudoWriteHosts` (temp file → `sudo cp <tmp> /etc/hosts`). Confirm on a
      live host the sudo prompt appears, the block is written (`litellm.<domain>` →
      127.0.0.1), the name then resolves, and the
      no-TTY/`--json`/declined paths print the manual block instead (never fail
      setup). `ai uninstall` removes the block the same way (standalone only).
- [ ] **Server-mode DNS/TLS contract** — in server mode `ai setup`/`ai doctor` print
      the operator contract (create `*.<domain>` or per-host DNS → this server, and
      a TLS cert terminated at Caddy). Confirm a remote client can reach the UI
      subdomains once real DNS + cert are in place (the platform does NOT edit
      `/etc/hosts` on a server). The server hostname is collected at `ai setup`
      (default `localhost`, which only resolves on the server itself) and persisted
      as runtime.yaml's `domain`; a real DNS name is set there or via `ai domain`.
- [ ] **http→https redirect (TLS-time requirement)** — TLS terminates PER-VHOST at
      Caddy. Today Caddy serves plain http on the single `:18787` entry; TLS is
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

- [ ] Headroom runs as the shared host container `aip-headroom`, now called
      IN-PROCESS by LiteLLM's `headroom` compression guardrail
      (`pre_call`, POSTing to `aip-headroom:8787/v1/compress`) — no longer a Caddy
      proxy. The per-project `context.strategy`
      (conservative/balanced/aggressive) maps to Headroom's two real per-request body
      knobs `keep_turns`/`output_buffer_tokens` — (8,12000)/(5,8000)/(2,4000),
      `internal/contextopt.HeadroomParams` — baked into the agent CLI's request
      `extra_body` at session launch. Verify the knobs take effect live, and confirm
      whether LiteLLM's `headroom` guardrail forwards the `extra_body` knobs to
      `/v1/compress`.

### 2.5 Secret/credential masking round-trip (arch §15, §17)

- [ ] Guardrails are now **user-selectable** (chosen at `ai setup` via a picker /
      `--guardrails`, persisted machine-wide in `runtime.yaml` `guardrails:`; only
      the enabled ones are rendered into the LiteLLM config, and an unselected one is
      omitted entirely). The security guardrails — secret-masking (Presidio),
      hide-secrets, and the tool-firewall — are **opt-in** (Headroom compression is
      the only default-on guardrail). When **secret-masking** is enabled, verify the
      live guardrails on every route (cloud included): the
      Presidio **secret** guardrails (`presidio-secrets-input` pre_call /
      `presidio-secrets-output` post_call, scoped to CREDIT_CARD, US_SSN,
      US_BANK_NUMBER, IBAN_CODE, CRYPTO; action MASK, score threshold 0.6) plus
      `hide-secrets` mask secrets/credentials, while ordinary coding prompts pass
      untouched (general PII masking is deliberately not done). Because each ENABLED
      guardrail is rendered `default_on: true` and every route traverses the proxy, a
      cloud route cannot bypass an enabled guardrail.
- [ ] **Presidio gated on secret-masking.** The Presidio containers
      (`aip-presidio-analyzer`, `aip-presidio-anonymizer`) are pulled AND started
      **only when the `secret-masking` guardrail is selected** (`presidioSelected`
      gates `requiredImages`/`PullImages`/`UpdateImages`, the reconcile's
      `ensurePresidio`, and the Control "all" path). Verify: with secret-masking OFF,
      `ai setup` does not pull/start Presidio and `ai services status` lists it as
      **"disabled"** (present but not probed — discoverable); an explicit
      `ai services start presidio` STILL forces it (bypasses the gate). Presidio
      remains a CORE service, now conditionally reconciled.
- [ ] **`postgres` status line.** The LiteLLM Postgres (`aip-litellm-db`, reconciled
      by `ensureLiteLLMDB` as part of litellm) surfaces its OWN **display-only**
      `postgres` line in `ai services` / the TUI, immediately AFTER the litellm line:
      State "running" when the container is up else "stopped"; Mode "container"; no
      host endpoint (internal-only, `aip-litellm-db:5432`). Managed WITH litellm — there is
      NO separate start/stop/restart verb (`ai services <action> litellm-db` →
      "unknown service"). Verify the line appears and tracks the container state.
- [ ] **LiteLLM admin API round-trip shapes.** `litellm.KeyManager`'s DB-backed
      model/credential management (`POST /model/new`, `POST /model/delete
      {"id": ...}`, `POST`/`GET`/`DELETE /credentials`) is unit-tested against an
      httptest fake to the documented shapes only; verify the `/model/delete`
      id-keyed body and the `/credentials` list/delete shapes against a LIVE
      `aip-litellm` on a provisioned host
      (docs.litellm.ai/docs/proxy/model_management, /docs/proxy/credentials).

### 2.5b LiteLLM Postgres data-dir on a host bind mount

Every host-persisted **system** volume now lives under `~/.ai-platform/volumes/<name>`
(so it is discoverable in one place and removed by `ai uninstall --purge`, which
`RemoveAll`s `~/.ai-platform`). Two volumes today:

- `~/.ai-platform/volumes/litellm-db` → bind-mounted into `aip-litellm-db` at
  `/var/lib/postgresql` (the LiteLLM Postgres data dir). This replaces the former
  `aip-litellm-db-data` Docker **named volume**.
- `~/.ai-platform/volumes/models` → the local-model store; the omlx weights live in
  the `volumes/models/omlx` subdir, entirely managed by omlx's own admin panel.

- [ ] **Verify `initdb` succeeds on the bind mount.** Postgres on a host bind mount
      has data-dir **ownership** quirks: the container's `postgres` UID must own (or
      be able to `chown`) the host dir, and macOS Docker Desktop's gRPC-FUSE mount vs
      Linux **rootless** (userns-remapped UIDs) handle this differently. The dir is
      created `0700` before `docker run`. If `initdb` fails on a given host, a
      uid/`:Z` (SELinux relabel) tweak on the `-v` may be needed — wire it here.
- [ ] **Migration caveat (acceptable for this dev platform):** data in the OLD
      `aip-litellm-db-data` named volume and the OLD `~/.ai-platform/models` does
      **not** auto-migrate — the next `ai setup` re-`initdb`s the Postgres data dir
      and local models must be re-downloaded into the fresh dirs. `ai uninstall` still
      best-effort `docker volume rm aip-*`s the legacy named volume on upgrade.

### 2.6 In-VM container runtime — MOOT (REMOVED)

There is no guest VM, so there is nothing to run a container runtime inside.
The whole rootful containerd+nerdctl+runc+CNI install (every OS base Dockerfile),
`Manager.ensureContainerd`'s boot-and-poll dance, and `Sandbox.ExecRoot` (`msb
exec -u root`) are gone with the Dockerfile/image/`msb` layer itself. Nothing to
bring up here.

### 2.6b Base-image per-user dev tooling — rtk

**MOOT / possibly dropped, needs confirming.** rtk ("Rust Token Killer",
`github.com/rtk-ai/rtk`) was previously baked into every OS base image via its
`install.sh`. There is no OS image to bake it into anymore, and no reference to
`rtk` was found anywhere in the current Go source (no host-install wiring
equivalent to `internal/setup/agentcli_host.go`'s pattern) — it appears to have
been dropped in the migration rather than carried over as a host install.
Confirm whether rtk support is intentionally gone or a still-pending port; if
the latter, wire a host-side installer analogous to `EnsureNonoInstalled`.

### 2.6c Shared skills pool — hermes skills directory

**Needs re-confirming against current code.** Under the previous design, hermes
(a gateway agent like omp that reads skills from its own config's
`external_dirs` rather than a per-CLI symlink) had its `.hermes/skills` entry
symlinked to the shared `.ai-platform/skills` pool at workspace start via a
`linkSharedResources`/`sharedResourceLinks` step. `HermesConfig`
(`internal/agentcfg/agentcfg.go`) still takes a `skillsDir` parameter pointing at
`<project>/.ai-platform/skills`, but the symlink-creation step itself was not
found in the current `internal/workspace` source during this doc pass — it is
unclear whether it (and the equivalent pool-symlink wiring for the other
skill-capable CLIs, and thus the host-install story for caveman/graphify/
ponytail/agent-skills described in `docs/ARCHITECTURE.md`) has been ported to
the nono/host-process model yet, or is a gap. Worth an explicit code check
before relying on it.

### 2.6d Agent CLI installs — MOOT (REPLACED)

The per-CLI Dockerfile snippets (`internal/templates/files/agentclis/<cli>/`),
`npm install -g` at image build, omp's binary-install snippet, and
`Manager.registerHermes`'s workspace-start curl install are all gone along with
`internal/templates` and the image itself. The replacement is host-native:
`internal/setup/agentcli_host.go` installs **claude-code, codex, and opencode**
directly onto the host via each CLI's own official installer
(`claude.ai/install.sh`, `chatgpt.com/codex/install.sh`, `opencode.ai/install`)
when `ai setup` is run with them selected — see "NEW seams to verify" above for
what still needs a live check. gemini/omp/hermes are not in that installer set;
how (or whether) the platform now installs/verifies those three on the host is
unconfirmed and worth a follow-up code check.

### 2.6e Software stack installs — MOOT (REMOVED)

There is no `--stacks` selector, no Dockerfile, and no `internal/templates`
package anymore, so there is nothing to install per-OS. A workspace uses
whatever `go`/`java`/`maven`/`deno`/`rust` toolchains are already on the host.

### 2.7 In-VM apps — MOOT (REMOVED)

`internal/apps` (opt-in AI apps as `nerdctl` containers inside the workspace
microVM — Open WebUI — plus agent-CLI web dashboards like hermes's) was deleted
entirely. There is no `ai apps` command, no per-`(workspace, app)` published
port, no app autostart, and no in-VM container runtime for it to run on. Not
replaced — in-VM apps are simply out of scope now.

### 2.8 Microsandbox Go SDK workspace backend — MOOT (REMOVED)

`internal/workspace/workspace_sdk.go` (the in-process Microsandbox Go SDK
backend), `workspace_real.go` (the `msb`-CLI backend), and `msb.go` (the pinned
`msb` binary download) are all deleted, along with everything this section used
to track: SDK create/boot/exec/FS/LogStream/Detach-reconnect, interactive
`Attach`'s tmux/CSI-u/SIGWINCH handling, the Sandbox Logs/Metrics TUI sub-tabs,
and the single-relay-handle lifecycle. `docs/MSB-SDK-MIGRATION.md`, which this
section used to point at, has been deleted for the same reason. The nearest
live-hardware equivalents that still need verification — nono session
create/attach/stop and a real PTY through `ai agent`/`ai shell` — are tracked
under "NEW seams to verify" above, not here.

### 2.9 Host-native model runtime — omlx (arch §17)

omlx ([jundot/omlx](https://github.com/jundot/omlx)) is the **SOLE** local model
backend, a **host-native** process (macOS/Apple-Silicon only), not an `aip-*`
container: ONE shared `omlx serve --model-dir <dir> --port 8100` process that scans
its own model directory and serves every model it finds. There is no per-model
runtime choice, no per-model port allocation, and no LRU eviction (unlike the vLLM
design this replaced) — model download/add/remove/tune all live entirely in omlx's
OWN admin panel (`ai services console omlx`), never this CLI; `ai models` is
read-only gateway inspection (`status`/`test`/`refresh`). The host-side wiring —
status/health probing, `omlx.Manager.EnsureServed` (adopt/wait/start, OS-level
flock-serialized across `ai` invocations), and the automatic LiteLLM `omlx/*`
registration sync (`litellm.SyncOmlxModels`) against omlx's live model list — is
fully unit-tested with fakes. The seams that MUTATE the host are `hardware
bring-up` (grep `hardware bring-up` in `internal/setup` and `internal/omlx`):

- [ ] **omlx GitHub-release resolution + install** — `omlx.Install`
      (`internal/omlx/omlx.go`) resolves the LATEST `github.com/jundot/omlx` GitHub
      release, picks the newest-Python-version prebuilt
      `omlx-X.Y.Z-cpNNN-...-macosx_15_0_universal2.whl` asset, pins the platform venv
      (`~/.ai-platform/venv`) to that exact Python version via `pyenv.EnsureVersion`
      (recreating a mismatched venv), and `pyenv.PipInstall`s the wheel. `ai setup`
      best-effort auto-installs it when absent (`ensureOmlxInstalled`, Detect-gated,
      never failing setup); `ai services start omlx` retries the same install+start on
      demand. Verify the real GitHub API resolution + wheel pip-install on a live host
      (behind the injectable `releaseGet`/`pyenv` seams).
- [ ] **omlx shared server launch** — `startOmlxServerHost`
      (`internal/setup/omlx_host.go`) → `omlx.Manager.EnsureServed` launches the real
      DETACHED `omlx serve --model-dir <dir> --port 8100` process (stdout+stderr to
      `omlx.LogPath`, a sibling of the model directory), then `SyncOmlxModels`
      registers its live model list into LiteLLM as `omlx/<id>` routed to
      `openai/<id>` against the shared `api_base`. Verify a real server launch + a
      served model's gateway round-trip (`ai models test omlx/<name>`) on a live host.
- [ ] **Uninstall host-runtime removal** — a plain `ai uninstall` ALWAYS stops the
      running `omlx serve` server best-effort (`stopOmlxServer` — an unqualified
      `pkill -f "omlx serve"`), and by default also attempts to remove the host-native
      omlx runtime (`Options.RemoveRuntimes` defaults to true; prompt defaults to yes;
      `--keep-runtimes` opts out; the downloaded model store
      `volumes/models/omlx` survives unless `--purge`). `removeOmlx`
      (`internal/uninstall/uninstall.go`) is currently a **documented STUB**: it
      reports `Report.RemovedOmlx=true` and logs a message but does not actually run a
      per-OS uninstall of the omlx install — only stopping the running server is
      actually wired. Wire the real per-OS removal (e.g. `pip uninstall omlx` from the
      platform venv) and verify.

### 2.10 Per-project host↔guest mounts (`ai mounts`) — MOOT (REMOVED)

`ai mounts` (isolated dirs backed by a private volume, shared mounts bind-mounted
from an arbitrary host dir) was deleted along with `WorkspaceConfig.IsolatedDirs`/
`SharedMounts` and the SDK-level mount mechanism it configured. nono does not
virtualize the filesystem — a session simply sees the host filesystem through its
profile's grants — so there is no second "guest view" to mount a second thing
into, and nothing left to bring up here.

## 3. Turn on the remaining acceptance tests

**Partially updated for nono; details below are from before the migration and
need re-confirming.** `test/acceptance/harness_test.go`,
`acceptance_test.go`, and `s1_remaining_test.go` already reference nono rather
than Microsandbox/`msb`, so the suite was at least partly ported — but the
specific checks itemized below (Dockerfile/agent-CLI/stack probes,
workspace-isolation framed around a microVM) were written against the old
design and were not individually re-verified against current test file
contents as part of this doc pass.

In `test/acceptance/` (harness, PTY driver, and the runnable `[S1]` subset already
pass):

- [x] **HTTPS mock-provider** fixture — `test/acceptance/mockprovider_test.go`
      (OpenAI-compatible `/health` + `/v1/chat/completions`, records the credential,
      serves TLS via `httptest`, exposes its cert via `writeCertPEM`).
- [x] Core service `[S1]` tests written (gated by `hardwareAvailable()`) —
      `test/acceptance/s1_hardware_test.go`: §9.1 credentialed request, §16.2
      workspace isolation, §16.3 egress confinement.
- [x] **Stale `[S1]` hardware tests updated to the current surface.**
      `s1_hardware_test.go` now uses `ai keys add openai --value <sentinel>`
      (keys-in-LiteLLM) instead of the retired `ai secrets set`/`secrets map`; and
      `s1_remaining_test.go` `TestModelStatusOnHardware` no longer asserts a non-empty
      default model (`litellm.DefaultRouting()` returns the zero `Routing{}` —
      catalog-driven, no default), keeping only the gateway-health + provider-set
      checks.
- [x] Remaining `[S1]` tests written — `test/acceptance/s1_remaining_test.go`:
      §6.1/§6.3/§6.4 Dockerfile/agent-CLI/stack probes (run host-side), §2.1–2.3
      setup/idempotency and §7.1/§7.2 model status/test (gated by
      `hardwareAvailable()`). The hardware-gated ones still need live-host
      verification (next item) after the updates above land.
- [ ] Run/verify the acceptance suite on hardware — `make test-acceptance` already
      runs `go test -count=1 ./test/acceptance/` (the package passes `go test ./...`);
      confirm the `hardwareAvailable()`-gated `[S1]` tests go green on a live host and
      enable the self-hosted Apple Silicon job in `.github/workflows/ci.yml`
      (`acceptance-s1`).

## 4. Smoke sequence (manual, on the host)

Rewritten for the nono-based CLI surface (the old sequence drove `ai create
--os`, `ai exec`, and a tmux-in-microVM session model — all retired).

1. `ai doctor` → all checks green, including the "nono runtime" check
   (`nono.Detect`).
2. `ai setup` → exit 0 (choose the guardrails at the picker / `--guardrails`); the
   core service tier (DNS, Valkey (+ RedisInsight), Headroom, LiteLLM + DB,
   and the Caddy proxy LAST) comes up; omlx is best-effort installed + started
   (the one shared server) into the platform venv — Presidio starts **only
   when secret-masking is selected** (`ai services status` shows it "disabled"
   otherwise); `nono` is best-effort installed (`EnsureNonoInstalled`) if
   absent; claude-code/codex/opencode are best-effort installed onto the host
   if selected (`EnsureHostAgentCLIsInstalled`). There are no optional host
   services (omlx is host-native and serves every model it finds from ONE
   shared process).
3. `ai create demo` (no `--os` anymore) → scaffold-only: writes the project's
   `.ai-platform/` files (`config.yaml`, `project.yaml`, `.gitignore`) in the
   target directory and registers it in the global index. There is no image to
   build and nothing to download — this finishes in milliseconds.
4. `ai agent opencode demo` (or `cd demo && ai agent opencode`) → mints a
   scoped LiteLLM virtual key, stores it in the OS keychain, writes opencode's
   keyless project config, generates the project's nono profile, and launches
   opencode as a detached nono session, then attaches to it interactively in
   the real terminal. Verify: line editing, Ctrl-C, and a clean exit/detach
   back to the host.
5. `ai sessions demo` → lists the live nono session(s) for the project
   (NAME/STATUS/ATTACHED/STARTED; zero sessions lists cleanly, not an error).
   `ai attach <session> demo` reattaches an existing one; `ai shell demo` opens
   a second, independent sandboxed shell session concurrently; `ai stop demo`
   stops every live session for the project. The TUI **Shell** sub-tab mirrors
   this (list/attach/new/stop).
6. `ai keys add openai --stdin` (registers that provider's catalog models in the
   gateway); `ai models test gpt-5` → works (real key lives encrypted in LiteLLM,
   only a scoped virtual key ever reaches the sandboxed session, via the OS
   keychain).
7. The default egress mode is **"public"** (nono's own unrestricted-outbound
   default) — from a fresh session the open internet is reachable while
   private ranges stay blocked. `ai network log` shows DNS names resolved
   host-wide. Re-lock with `ai network egress deny`, then confirm a
   non-allow-listed destination is **denied** by nono's own network
   enforcement (`ai network show`) while the gateway stays reachable; `ai
   network allow <domain>` opens one exception without a restart.
8. `make test-acceptance` → the `[S1]` suite is green (see §3's caveat above
   about re-confirming the suite's content against the current surface).

## 5. Deferred beyond Slice 1

- Podman / Linux hardening (S6), overlay persistence hardening (S4), extended OS
  templates (S5).
