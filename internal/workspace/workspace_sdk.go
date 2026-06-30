package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	microsandbox "github.com/superradcompany/microsandbox/sdk/go"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// workspace_sdk.go is the in-process Microsandbox Go SDK backend for the Sandbox
// interface, the durable fix for the relay-client-exhaustion wedge (see
// docs/MSB-SDK-MIGRATION.md). Where realSandbox shells out a fresh `msb` process
// per call — each one a NEW agent-relay client that a leaky/saturated relay never
// reclaims — sdkSandbox holds ONE live `*microsandbox.Sandbox` handle per workspace
// (a single Rust-side relay connection) and reuses it across every Exec/FS/SSH call.
// That collapses the per-call connection churn the TUI's polling otherwise creates.
//
// This backend is now the DEFAULT (see selectSandbox). Live-validated on Apple
// Silicon (msb 0.6.1): microVM create/boot, exec (incl. as root), FS write/read, log
// streaming (history + follow), Detach→reconnect, stop/remove, and that an `msb
// load`ed local image resolves via WithImage. Set AIP_WORKSPACE_BACKEND=cli to fall
// back to the msb-CLI backend. The interactive SSH Attach path (`ai shell`/agent) and
// the full create→start→agent-config flow are validated by real use on a TTY/host.

// Compile-time assertion that the SDK backend satisfies the Sandbox interface.
var _ Sandbox = (*sdkSandbox)(nil)

// sdkEnsureTimeout bounds the one-time runtime install (EnsureInstalled may download
// msb + libkrunfw from GitHub on first use), so a stalled download can't hang forever.
const sdkEnsureTimeout = 10 * time.Minute

// workspaceBackendEnv selects the Sandbox backend. The DEFAULT is now the in-process
// Go SDK backend (sdkSandbox) — one reused relay connection per workspace, the
// relay-exhaustion fix. Set AIP_WORKSPACE_BACKEND=cli (or "msb") to fall back to the
// msb-CLI backend (realSandbox) — an escape hatch if the SDK path misbehaves on a
// given host. Live-validated on Apple Silicon (msb 0.6.1): create/exec/root-exec/FS/
// log-stream/reconnect/stop + `msb load`→WithImage resolution + the workspace user.
// Still TTY/platform-validated by use: interactive SSH Attach (`ai shell`/agent) and
// the full create→start→agent-config flow.
const workspaceBackendEnv = "AIP_WORKSPACE_BACKEND"

// selectSandbox returns the Sandbox backend chosen by workspaceBackendEnv, defaulting
// to the in-process SDK backend; "cli"/"msb" force the msb-CLI backend.
func selectSandbox(prober runtime.Prober) Sandbox {
	switch strings.ToLower(os.Getenv(workspaceBackendEnv)) {
	case "cli", "msb":
		return realSandbox{prober: prober}
	default:
		return &sdkSandbox{}
	}
}

// connectionReleaser is the optional lifecycle surface a Sandbox backend may
// implement to release pooled per-workspace connections. The CLI backend holds no
// persistent connections (it shells out per call) and does not implement it; the SDK
// backend does. Manager.ReleaseConnection / Manager.Close fan out to it when present,
// so callers (notably the long-lived management TUI) can drop a workspace's live
// relay connection on project switch and close them all on exit.
type connectionReleaser interface {
	ReleaseConnection(name string)
	CloseConnections()
}

// ReleaseConnection drops the live SDK handle for project so only one workspace
// connection is held at a time — called on TUI project switch (and implicitly by
// Stop/Destroy). A no-op on the CLI backend, which keeps no connections.
func (manager Manager) ReleaseConnection(project string) {
	if releaser, ok := manager.Sandbox.(connectionReleaser); ok {
		releaser.ReleaseConnection(Name(project))
	}
}

// Close releases ALL live SDK connections — called when the management TUI exits, so
// no relay client lingers past the process's interactive session. A no-op on the CLI
// backend.
func (manager Manager) Close() {
	if releaser, ok := manager.Sandbox.(connectionReleaser); ok {
		releaser.CloseConnections()
	}
}

// LogStream is a live log subscription: Recv blocks for the next chunk (recent
// history first, then new entries as they land), and Close stops it. Backed by the
// SDK's relay-free host log channel (no agent-relay client). The CLI backend does not
// implement streaming; callers fall back to the LogTail poll path there.
type LogStream interface {
	Recv(ctx context.Context) (string, error)
	Close() error
}

// logStreamer is the optional capability a Sandbox backend may implement to open a
// live LogStream. Only the SDK backend does.
type logStreamer interface {
	OpenLogStream(ctx context.Context, name string) (LogStream, error)
}

// ErrLogStreamUnsupported is returned by OpenWorkspaceLogStream when the active
// Sandbox backend cannot stream (the CLI backend); the caller then polls instead.
var ErrLogStreamUnsupported = errors.New("log streaming is not supported by this workspace backend")

// SupportsLogStreaming reports whether the active backend can stream logs (the SDK
// backend yes, the CLI backend no). Callers use it to choose the streaming log view
// vs. the tailer poll fallback.
func (manager Manager) SupportsLogStreaming() bool {
	_, ok := manager.Sandbox.(logStreamer)
	return ok
}

// OpenWorkspaceLogStream opens a live log stream for the project's microVM (recent
// history then follow), or ErrLogStreamUnsupported on the CLI backend. It rides the
// relay-free host log channel, so it consumes no agent-relay client. The caller owns
// the returned stream and must Close it.
func (manager Manager) OpenWorkspaceLogStream(ctx context.Context, project string) (LogStream, error) {
	streamer, ok := manager.Sandbox.(logStreamer)
	if !ok {
		return nil, ErrLogStreamUnsupported
	}
	return streamer.OpenLogStream(ctx, Name(project))
}

// sdkLogStream adapts a microsandbox LogStreamHandle to LogStream, mapping a nil
// entry (end of stream) to io.EOF.
type sdkLogStream struct{ handle *microsandbox.LogStreamHandle }

func (stream *sdkLogStream) Recv(ctx context.Context) (string, error) {
	entry, err := stream.handle.Recv(ctx)
	if err != nil {
		return "", err
	}
	if entry == nil {
		return "", io.EOF
	}
	return entry.Text(), nil
}

func (stream *sdkLogStream) Close() error { return stream.handle.Close() }

// OpenLogStream opens a follow-from-beginning log stream for the named microVM —
// recent history first, then new entries — over the relay-free host log channel.
// LogStream works without a live agent connection (it reads the persisted exec.log),
// so it does not consume the single active relay handle.
func (sandbox *sdkSandbox) OpenLogStream(ctx context.Context, name string) (LogStream, error) {
	if err := sandbox.ensure(); err != nil {
		return nil, err
	}
	meta, err := microsandbox.GetSandbox(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("could not open the workspace log stream for %q", name)
	}
	handle, err := meta.LogStream(ctx, microsandbox.LogStreamOptions{
		Sources: []microsandbox.LogSource{
			microsandbox.LogSourceStdout,
			microsandbox.LogSourceStderr,
			microsandbox.LogSourceSystem,
		},
		Follow: true,
	})
	if err != nil {
		return nil, fmt.Errorf("could not open the workspace log stream for %q", name)
	}
	return &sdkLogStream{handle: handle}, nil
}

// sdkSandbox drives Microsandbox microVMs via the in-process Go SDK. It keeps ONE
// live (Connect-derived, non-lifecycle-owning) handle at a time — for the workspace
// currently being driven — so repeated calls reuse a single relay connection.
// Connecting to a different workspace detaches the previous handle first, so there is
// always at most one active connection (never an accumulating pool). Concurrency-safe:
// the SDK's *Sandbox is goroutine-safe, and mu guards the current handle.
type sdkSandbox struct {
	mu          sync.Mutex
	current     *microsandbox.Sandbox
	currentName string

	ensureOnce sync.Once
	ensureErr  error
}

// ensure performs the one-time runtime install. A failure is reported as
// ErrMsbMissing for parity with the CLI backend (the runtime is unavailable either
// way), so callers handle it identically.
func (sandbox *sdkSandbox) ensure() error {
	sandbox.ensureOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), sdkEnsureTimeout)
		defer cancel()
		sandbox.ensureErr = microsandbox.EnsureInstalled(ctx)
	})
	if sandbox.ensureErr != nil {
		return ErrMsbMissing
	}
	return nil
}

// handle returns the single live handle, connecting on first use for name. If a
// DIFFERENT workspace is currently connected, its handle is detached first so only
// one connection is ever active. The handle is Connect-derived, so it does NOT own
// the VM lifecycle — detaching releases only the relay connection, never stopping the
// VM (Close, by contrast, can stop a detached sandbox — see the Detach-vs-Close
// footgun in docs/MSB-SDK-MIGRATION.md).
func (sandbox *sdkSandbox) handle(ctx context.Context, name string) (*microsandbox.Sandbox, error) {
	if err := sandbox.ensure(); err != nil {
		return nil, err
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if sandbox.current != nil && sandbox.currentName == name {
		return sandbox.current, nil
	}
	// A different workspace (or none) is connected — enforce one active handle by
	// detaching the previous before connecting the new one.
	if sandbox.current != nil {
		_ = sandbox.current.Detach(context.Background())
		sandbox.current = nil
		sandbox.currentName = ""
	}
	meta, err := microsandbox.GetSandbox(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("could not reach workspace %q — is it running? start it with `ai start`", name)
	}
	live, err := meta.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not connect to workspace %q — is it running? start it with `ai start`", name)
	}
	sandbox.current = live
	sandbox.currentName = name
	return live, nil
}

// ReleaseConnection detaches the live handle if it is for name (a no-op otherwise).
// Detach releases the Rust-side handle WITHOUT stopping the VM. Called on TUI project
// switch and from Stop/Destroy so the connection drops promptly.
func (sandbox *sdkSandbox) ReleaseConnection(name string) {
	sandbox.mu.Lock()
	live := sandbox.current
	if live == nil || sandbox.currentName != name {
		sandbox.mu.Unlock()
		return
	}
	sandbox.current = nil
	sandbox.currentName = ""
	sandbox.mu.Unlock()
	_ = live.Detach(context.Background())
}

// CloseConnections detaches the live handle, if any. Called when the application
// closes so no relay connection lingers past the process's interactive session.
func (sandbox *sdkSandbox) CloseConnections() {
	sandbox.mu.Lock()
	live := sandbox.current
	sandbox.current = nil
	sandbox.currentName = ""
	sandbox.mu.Unlock()
	if live != nil {
		_ = live.Detach(context.Background())
	}
}

// Create creates AND boots a detached microVM, mounting the host project source at
// /workspace and the persistent overlay at /persist, applying the egress policy
// (translated from netArgs) plus the fixed aip-dns nameserver. The owning handle is
// Detached immediately so the VM survives this process exiting; later calls
// reconnect via handle().
func (sandbox *sdkSandbox) Create(name, imageRef, projectMount, overlayPath string, resources VMResources, netArgs []string) error {
	if err := sandbox.ensure(); err != nil {
		return err
	}
	options := []microsandbox.SandboxOption{
		microsandbox.WithImage(imageRef),
		// Workspace images are always built locally and loaded into the shared msb
		// store by the Builder (`msb load`); they are never in a registry. Never pull
		// — use the loaded image (verified: an `msb load`ed tag resolves via WithImage).
		microsandbox.WithPullPolicy(microsandbox.PullPolicyNever),
		microsandbox.WithReplace(),
		microsandbox.WithDetached(),
		microsandbox.WithMemory(parseMemoryMiB(resources.Memory)),
		microsandbox.WithIdleTimeout(parseIdleTimeout(resources.IdleTimeout)),
		microsandbox.WithWorkdir(workspaceWorkdir),
		microsandbox.WithMounts(map[string]microsandbox.MountConfig{
			workspaceWorkdir: microsandbox.Mount.Bind(projectMount, microsandbox.MountOptions{}),
			"/persist":       microsandbox.Mount.Bind(overlayPath, microsandbox.MountOptions{}),
		}),
		microsandbox.WithNetwork(buildNetworkConfig(netArgs)),
	}
	if resources.CPUs > 0 {
		options = append(options, microsandbox.WithCPUs(uint8(resources.CPUs)))
	}
	live, err := microsandbox.CreateSandbox(context.Background(), name, options...)
	if err != nil {
		return fmt.Errorf("could not create workspace %q — run `ai doctor` to check Microsandbox and disk space", name)
	}
	// Detach the owning handle: keep the VM running after this process exits.
	_ = live.Detach(context.Background())
	return nil
}

// Start ensures the microVM is booted (detached). An already-running VM is success,
// mirroring the CLI backend.
func (sandbox *sdkSandbox) Start(name string) error {
	if err := sandbox.ensure(); err != nil {
		return err
	}
	meta, err := microsandbox.GetSandbox(context.Background(), name)
	if err != nil {
		return fmt.Errorf("could not start workspace %q — run `ai doctor`", name)
	}
	if meta.Status() == microsandbox.SandboxStatusRunning {
		return nil
	}
	live, err := meta.StartDetached(context.Background())
	if err != nil {
		return fmt.Errorf("could not start workspace %q — run `ai doctor`", name)
	}
	_ = live.Detach(context.Background())
	return nil
}

// Stop stops the microVM (releasing any cached handle first).
func (sandbox *sdkSandbox) Stop(name string) error {
	if err := sandbox.ensure(); err != nil {
		return err
	}
	sandbox.ReleaseConnection(name)
	meta, err := microsandbox.GetSandbox(context.Background(), name)
	if err != nil {
		// Not present: nothing to stop.
		return nil
	}
	if err := meta.Stop(context.Background()); err != nil {
		return fmt.Errorf("could not stop workspace %q", name)
	}
	return nil
}

// Destroy stops (if needed) and removes the microVM.
func (sandbox *sdkSandbox) Destroy(name string) error {
	if err := sandbox.ensure(); err != nil {
		return err
	}
	sandbox.ReleaseConnection(name)
	if meta, err := microsandbox.GetSandbox(context.Background(), name); err == nil {
		_ = meta.Stop(context.Background())
	}
	if err := microsandbox.RemoveSandbox(context.Background(), name); err != nil {
		return fmt.Errorf("could not remove workspace %q", name)
	}
	return nil
}

// execAs runs argv inside the microVM as user (empty/"" → SDK default, the
// unprivileged workspace user). A non-zero inner exit is data in ExecResult, not a
// Go error; a ctx timeout/cancel surfaces as ErrWorkspaceUnresponsive. A transport
// failure evicts the cached handle so the next call reconnects.
func (sandbox *sdkSandbox) execAs(ctx context.Context, name, user string, argv []string) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, fmt.Errorf("no command given for workspace %q", name)
	}
	live, err := sandbox.handle(ctx, name)
	if err != nil {
		return ExecResult{}, err
	}
	output, err := live.Exec(ctx, argv[0], argv[1:], microsandbox.WithExecUser(user))
	if err != nil {
		if ctx.Err() != nil {
			return ExecResult{}, ErrWorkspaceUnresponsive
		}
		sandbox.ReleaseConnection(name)
		return ExecResult{}, fmt.Errorf("could not run the command in workspace %q — is it running? start it with `ai start`", name)
	}
	return ExecResult{ExitCode: output.ExitCode(), Stdout: output.Stdout(), Stderr: output.Stderr()}, nil
}

func (sandbox *sdkSandbox) Exec(name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(context.Background(), name, "workspace", argv)
}

func (sandbox *sdkSandbox) ExecContext(ctx context.Context, name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(ctx, name, "workspace", argv)
}

func (sandbox *sdkSandbox) ExecRoot(name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(context.Background(), name, "root", argv)
}

func (sandbox *sdkSandbox) ExecRootContext(ctx context.Context, name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(ctx, name, "root", argv)
}

// ExecInteractive bridges the caller's terminal to a guest PTY via the SDK's native
// Attach (no guest sshd needed). The inner session ending is not an error; only an
// infrastructure failure is returned.
func (sandbox *sdkSandbox) ExecInteractive(name string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command given for workspace %q", name)
	}
	live, err := sandbox.handle(context.Background(), name)
	if err != nil {
		return err
	}
	_, err = live.Attach(context.Background(), argv[0], argv[1:]...)
	return err
}

// WriteFile writes content to guestPath, creating parent directories (mkdir -p as
// the workspace user, matching the file owner) before the SDK filesystem write.
func (sandbox *sdkSandbox) WriteFile(name, guestPath string, content []byte) error {
	live, err := sandbox.handle(context.Background(), name)
	if err != nil {
		return err
	}
	if dir := path.Dir(guestPath); dir != "" && dir != "." && dir != "/" {
		_, _ = live.Exec(context.Background(), "mkdir", []string{"-p", dir}, microsandbox.WithExecUser("workspace"))
	}
	if err := live.FS().Write(context.Background(), guestPath, content); err != nil {
		return fmt.Errorf("could not write %s in workspace %q: %w", guestPath, name, err)
	}
	return nil
}

// logTail reads the persisted sandbox log (relay-free — backed by an on-disk
// exec.log, so it never consumes a relay client). lines ≤ 0 returns the full log.
func (sandbox *sdkSandbox) logTail(ctx context.Context, name string, lines int) (string, error) {
	if err := sandbox.ensure(); err != nil {
		return "", err
	}
	meta, err := microsandbox.GetSandbox(ctx, name)
	if err != nil {
		return "", fmt.Errorf("could not read the workspace log for %q", name)
	}
	options := microsandbox.LogOptions{}
	if lines > 0 {
		options.Tail = uint64(lines)
	}
	entries, err := meta.Logs(ctx, options)
	if err != nil {
		return "", fmt.Errorf("could not read the workspace log for %q", name)
	}
	var builder strings.Builder
	for _, entry := range entries {
		builder.WriteString(entry.Text())
	}
	return builder.String(), nil
}

func (sandbox *sdkSandbox) LogTail(name string, lines int) (string, error) {
	return sandbox.logTail(context.Background(), name, lines)
}

func (sandbox *sdkSandbox) LogTailContext(ctx context.Context, name string, lines int) (string, error) {
	return sandbox.logTail(ctx, name, lines)
}

// InspectNetwork reads the configured egress policy back from the sandbox config
// (the SDK has no live policy getter; the stored config carries the applied
// NetworkConfig). ErrNotRunning when no such sandbox exists.
func (sandbox *sdkSandbox) InspectNetwork(name string) (NetworkPolicy, error) {
	if err := sandbox.ensure(); err != nil {
		return NetworkPolicy{}, err
	}
	meta, err := microsandbox.GetSandbox(context.Background(), name)
	if err != nil {
		if microsandbox.IsKind(err, microsandbox.ErrSandboxNotFound) {
			return NetworkPolicy{}, ErrNotRunning
		}
		return NetworkPolicy{}, fmt.Errorf("could not inspect workspace %q network policy", name)
	}
	configuration, err := meta.Config()
	if err != nil {
		return NetworkPolicy{}, fmt.Errorf("could not inspect workspace %q network policy", name)
	}
	policy := NetworkPolicy{}
	if configuration.Network != nil {
		policy.DefaultEgress = string(configuration.Network.DefaultEgress)
		for _, rule := range configuration.Network.Rules {
			policy.Rules = append(policy.Rules, renderPolicyRule(rule))
		}
	}
	return policy, nil
}

// IsRunning reports whether the named microVM is running, via a metadata-only
// lookup (no in-VM exec, so it cannot wedge). A non-existent sandbox is (false, nil).
func (sandbox *sdkSandbox) IsRunning(ctx context.Context, name string) (bool, error) {
	if err := sandbox.ensure(); err != nil {
		return false, err
	}
	meta, err := microsandbox.GetSandbox(ctx, name)
	if err != nil {
		if microsandbox.IsKind(err, microsandbox.ErrSandboxNotFound) {
			return false, nil
		}
		return false, err
	}
	return meta.Status() == microsandbox.SandboxStatusRunning, nil
}

// SyncClock sets the guest clock to the host's current UTC time (best-effort, as
// root, bounded). A microVM's clock freezes during host sleep; correcting it keeps
// in-VM TLS working.
func (sandbox *sdkSandbox) SyncClock(name string) error {
	if err := sandbox.ensure(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), inVMProbeTimeout)
	defer cancel()
	live, err := sandbox.handle(ctx, name)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, _ = live.Exec(ctx, "date", []string{"-u", "-s", stamp}, microsandbox.WithExecUser("root"))
	return nil
}

// parseMemoryMiB converts a config memory string ("4G", "512M", "2Gi", "2048") to
// the MiB count the SDK's WithMemory wants. An unset or unparsable value falls back
// to the platform default (microVMMemory).
func parseMemoryMiB(value string) uint32 {
	value = strings.TrimSpace(value)
	if value == "" {
		value = microVMMemory
	}
	upper := strings.ToUpper(value)
	multiplier := 1.0
	number := upper
	switch {
	case strings.HasSuffix(upper, "GI"):
		multiplier, number = 1024, strings.TrimSuffix(upper, "GI")
	case strings.HasSuffix(upper, "G"):
		multiplier, number = 1024, strings.TrimSuffix(upper, "G")
	case strings.HasSuffix(upper, "MI"):
		multiplier, number = 1, strings.TrimSuffix(upper, "MI")
	case strings.HasSuffix(upper, "M"):
		multiplier, number = 1, strings.TrimSuffix(upper, "M")
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
	if err != nil || parsed <= 0 {
		return parseMemoryMiB(microVMMemory)
	}
	return uint32(parsed * multiplier)
}

// parseIdleTimeout parses a Go-style duration ("24h", "30m"), falling back to the
// platform default when unset/unparsable.
func parseIdleTimeout(value string) time.Duration {
	if value == "" {
		value = config.DefaultMicrosandboxIdleTimeout
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		if fallback, ferr := time.ParseDuration(config.DefaultMicrosandboxIdleTimeout); ferr == nil {
			return fallback
		}
		return 24 * time.Hour
	}
	return parsed
}

// buildNetworkConfig translates the msb-CLI net-rule fragment (from
// egress.MsbNetworkArgs) into the SDK's typed NetworkConfig. It is a transitional
// round-trip that keeps the Sandbox interface (and its test fakes) unchanged for
// Phase 1; a later phase can pass the structured egress policy directly. The fixed
// aip-dns nameserver (added by the CLI backend's Create, not by MsbNetworkArgs) is
// applied here so guest DNS is still audited.
func buildNetworkConfig(netArgs []string) *microsandbox.NetworkConfig {
	configuration := &microsandbox.NetworkConfig{
		DefaultEgress:  microsandbox.PolicyActionDeny,
		DefaultIngress: microsandbox.PolicyActionAllow,
		DNS:            &microsandbox.DNSConfig{Nameservers: []string{dnsNameserver}},
	}
	ports := make(map[uint16]uint16)
	for index := 0; index < len(netArgs); index++ {
		switch netArgs[index] {
		case "--net-rule":
			index++
			if index >= len(netArgs) {
				break
			}
			if rule, ok := parseNetRule(netArgs[index]); ok {
				configuration.Rules = append(configuration.Rules, rule)
			}
		case "--net-default-egress":
			index++
			if index >= len(netArgs) {
				break
			}
			if strings.EqualFold(netArgs[index], "allow") {
				configuration.DefaultEgress = microsandbox.PolicyActionAllow
			} else {
				configuration.DefaultEgress = microsandbox.PolicyActionDeny
			}
		case "-p":
			index++
			if index >= len(netArgs) {
				break
			}
			if host, guest, ok := parseHostGuestPort(netArgs[index]); ok {
				ports[host] = guest
			}
		}
	}
	if len(ports) > 0 {
		configuration.Ports = ports
	}
	return configuration
}

// parseNetRule parses an msb net-rule string ("allow:egress@host:tcp:18787",
// "allow:egress@public") into a typed PolicyRule.
func parseNetRule(value string) (microsandbox.PolicyRule, bool) {
	head, destination, found := strings.Cut(value, "@")
	if !found {
		return microsandbox.PolicyRule{}, false
	}
	actionToken, directionToken, _ := strings.Cut(head, ":")
	rule := microsandbox.PolicyRule{
		Action:    microsandbox.PolicyActionAllow,
		Direction: microsandbox.PolicyDirectionEgress,
	}
	if strings.EqualFold(actionToken, "deny") {
		rule.Action = microsandbox.PolicyActionDeny
	}
	switch strings.ToLower(directionToken) {
	case "ingress":
		rule.Direction = microsandbox.PolicyDirectionIngress
	case "any":
		rule.Direction = microsandbox.PolicyDirectionAny
	}
	// destination is "<dest>[:<proto>:<port>]".
	parts := strings.Split(destination, ":")
	rule.Destination = parts[0]
	if len(parts) >= 2 {
		if strings.EqualFold(parts[1], "udp") {
			rule.Protocol = microsandbox.PolicyProtocolUDP
		} else {
			rule.Protocol = microsandbox.PolicyProtocolTCP
		}
	}
	if len(parts) >= 3 {
		rule.Port = parts[2]
	}
	return rule, true
}

// parseHostGuestPort parses a "host:guest" port mapping.
func parseHostGuestPort(value string) (uint16, uint16, bool) {
	hostToken, guestToken, found := strings.Cut(value, ":")
	if !found {
		return 0, 0, false
	}
	host, herr := strconv.ParseUint(strings.TrimSpace(hostToken), 10, 16)
	guest, gerr := strconv.ParseUint(strings.TrimSpace(guestToken), 10, 16)
	if herr != nil || gerr != nil {
		return 0, 0, false
	}
	return uint16(host), uint16(guest), true
}

// renderPolicyRule renders a typed PolicyRule back to a single human line
// ("allow egress example.com tcp 443"), matching NetworkPolicy.Rules' shape.
func renderPolicyRule(rule microsandbox.PolicyRule) string {
	protocol := string(rule.Protocol)
	if protocol == "" && len(rule.Protocols) > 0 {
		protocol = string(rule.Protocols[0])
	}
	port := rule.Port
	if port == "" && len(rule.Ports) > 0 {
		port = rule.Ports[0]
	}
	parts := []string{string(rule.Action), string(rule.Direction), rule.Destination}
	if protocol != "" {
		parts = append(parts, protocol)
	}
	if port != "" {
		parts = append(parts, port)
	}
	return strings.Join(parts, " ")
}
