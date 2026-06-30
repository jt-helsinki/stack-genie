package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// Sentinel errors for the real impls. Missing tools → exit 3 (mapped by the
// CLI's mapWorkspaceErr). Runtime failures are plain errors → exit 4.
var (
	ErrContainerRuntimeMissing = errors.New("no container runtime (docker or podman) found")
	ErrMsbMissing              = errors.New("microsandbox (msb) is not installed")
	// ErrNotRunning marks that no microVM exists for the requested name (the
	// workspace is not running). Callers of InspectNetwork treat this as "show the
	// declared policy only", not a failure.
	ErrNotRunning = errors.New("workspace microVM is not running")
)

// realBuilder builds the workspace OCI image with the detected container
// runtime (Docker or Podman, Slice 6 — never hardcoded), then loads it into
// Microsandbox. msb cannot read the container runtime's local image store, so
// the image is exported to a tar and fed to `msb load` (verified, msb 0.5.7).
type realBuilder struct{ prober runtime.Prober }

func (builder realBuilder) Build(projectRoot, imageRef string) error {
	containerRuntime, err := runtime.ContainerRuntimeName(builder.prober)
	if err != nil {
		return ErrContainerRuntimeMissing
	}
	if _, err := builder.prober.LookPath("msb"); err != nil {
		return ErrMsbMissing
	}

	// 1. Build the image from <projectRoot>/.ai-platform/Dockerfile.
	dockerfile := filepath.Join(projectRoot, ".ai-platform", "Dockerfile")
	buildArgs := containerRuntime.BuildArgs(imageRef, dockerfile, projectRoot)
	if err := runStreaming(containerRuntime.Name, buildArgs...); err != nil {
		return fmt.Errorf("could not build the workspace image — check disk space and the project Dockerfile (.ai-platform/Dockerfile)")
	}

	// 2. Export the freshly-built image to a temp tar. msb cannot read the
	//    runtime's local image store, so it is loaded from an image tar.
	tarFile, err := os.CreateTemp("", "aip-image-*.tar")
	if err != nil {
		return fmt.Errorf("create image tar: %w", err)
	}
	tarPath := tarFile.Name()
	_ = tarFile.Close()
	defer func() { _ = os.Remove(tarPath) }()

	if err := runStreaming(containerRuntime.Name, "save", "-o", tarPath, imageRef); err != nil {
		return fmt.Errorf("could not export the workspace image")
	}

	// 3. Load the image tar into Microsandbox, FORCING the tag onto this freshly
	//    imported image (`--tag <ref>`). Without an explicit tag, `msb load` does not
	//    re-point an existing `<name>:latest` reference to the new content, so
	//    `msb create --replace` (which rebuilds the rootfs from that tag every start)
	//    would keep booting a STALE earlier image — e.g. one built before a Dockerfile
	//    change such as adding tmux. `--tag` makes each start boot exactly what was
	//    just built. (Verified against msb 0.5.10: `load -t <REF>`.)
	if err := runStreaming("msb", "load", "--tag", imageRef, "--input", tarPath); err != nil {
		return fmt.Errorf("could not load the workspace image into Microsandbox")
	}
	return nil
}

// microVMMemory is the DEFAULT memory allocated to a workspace microVM when the
// project config does not set `workspace.memory_limit`. It must hold the guest OS +
// the rootful in-VM containerd + any in-VM app containers (Open WebUI / AnythingLLM
// are heavy); 1G was too small — pulling/running an app could OOM-kill containerd
// mid-pull ("connection refused" on its socket), so the fallback is 4G. (A freshly
// created project's config sets memory_limit to 8G; this only applies when it's empty.)
const microVMMemory = "4G"

// dnsNameserver is the fixed host-loopback address of the platform's aip-dns
// egress-audit resolver (arch §29). Every workspace microVM is booted with
// `--dns-nameserver` pointing here, so msb's netstack forwards guest DNS to it
// and CoreDNS logs the attempted names (surfaced by `ai network log`). This is a
// fixed platform setting, not policy-derived, so it lives in Create directly
// rather than in egress.MsbNetworkArgs. It mirrors setup.DNSNameserver.
const dnsNameserver = "127.0.0.1:15353"

// realSandbox drives Microsandbox microVMs via `msb`.
type realSandbox struct{ prober runtime.Prober }

func (sandbox realSandbox) ensureInstalled() error {
	if _, err := sandbox.prober.LookPath("msb"); err != nil {
		return ErrMsbMissing
	}
	return nil
}

// Create creates AND boots a detached microVM from imageRef, mounting the host
// project source at /workspace and the persistent overlay (arch §26) at
// /persist so writes outside the project mount survive stop/start and destroy
// recreation. The egress policy is applied via netArgs. `--replace` makes
// recreation idempotent.
func (sandbox realSandbox) Create(name, imageRef, projectMount, overlayPath string, resources VMResources, netArgs []string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	args := sandboxCreateArgs(name, imageRef, projectMount, overlayPath, resources, netArgs)
	if err := runStreaming("msb", args...); err != nil {
		return fmt.Errorf("could not create workspace %q — run `ai doctor` to check Microsandbox and disk space", name)
	}
	return nil
}

// sandboxCreateArgs renders the `msb create` argv for a workspace microVM. It is a
// helper so the important lifecycle flags (especially --idle-timeout) are unit-tested
// without spawning the real external tool.
func sandboxCreateArgs(name, imageRef, projectMount, overlayPath string, resources VMResources, netArgs []string) []string {
	memory := resources.Memory
	if memory == "" {
		memory = microVMMemory // fall back to the platform default when unset
	}
	idleTimeout := resources.IdleTimeout
	if idleTimeout == "" {
		idleTimeout = config.DefaultMicrosandboxIdleTimeout
	}
	args := []string{
		"create", imageRef,
		"--name", name,
		"--memory", memory,
		"--volume", projectMount + ":/workspace",
		"--volume", overlayPath + ":/persist",
		"--workdir", "/workspace",
		// Forward all guest DNS to the platform's aip-dns audit resolver (arch
		// §29). A fixed platform setting; egress enforcement stays on the
		// net-rules in netArgs (a resolver answer cannot bypass them).
		"--dns-nameserver", dnsNameserver,
		// Keep a development workspace alive across normal breaks WITHOUT a heartbeat
		// loop or host sleep prevention. This avoids msb's short idle reaping while not
		// taxing CPU/battery and not blocking real computer sleep.
		"--idle-timeout", idleTimeout,
		"--replace",
	}
	// Apply the configured vCPU count when set; ≤ 0 omits --cpus so msb uses its
	// default. msb's flags: `-m/--memory <e.g. 1G>`, `-c/--cpus <int>`.
	if resources.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(resources.CPUs))
	}
	args = append(args, netArgs...)
	return args
}

func (sandbox realSandbox) Start(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	// Start means "ensure the microVM is booted". `msb create --replace` (in
	// Create) already boots it, so a follow-up start reports "already running" —
	// that is success here, not a failure. Capture the output to tell that case
	// apart from a real error (and to keep msb's noise out of the caller's screen).
	if combined, err := runCaptured("msb", "start", name); err != nil {
		if strings.Contains(combined, "already running") {
			return nil
		}
		return fmt.Errorf("could not start workspace %q — run `ai doctor`", name)
	}
	return nil
}

func (sandbox realSandbox) Stop(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	if err := runStreaming("msb", "stop", "-f", name); err != nil {
		return fmt.Errorf("could not stop workspace %q", name)
	}
	return nil
}

func (sandbox realSandbox) Destroy(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	// `-f` stops the microVM first if running, then removes it.
	if err := runStreaming("msb", "remove", "-f", name); err != nil {
		return fmt.Errorf("could not remove workspace %q", name)
	}
	return nil
}

// Exec runs argv inside the running microVM. The inner command's exit code is
// faithfully propagated by msb to its own process exit; a non-zero inner exit
// is data carried in ExecResult, NOT a Go error. Only infrastructure failures
// (microVM down, msb missing) are returned as errors (§4.5).
func (sandbox realSandbox) Exec(name string, argv []string) (ExecResult, error) {
	// Run as the `workspace` user (the image's home owner, matching WriteFile) so
	// commands, shells, agents, and tmux all share that user's home + the agent
	// provider configs under /home/workspace.
	return sandbox.execAs(context.Background(), name, "workspace", argv)
}

// ExecContext is Exec bounded by ctx — used for the short in-VM probes (tmux/session
// listing) so they fail fast (and the hung msb process is killed) instead of hanging
// the CLI/TUI forever when the workspace is busy or wedged.
func (sandbox realSandbox) ExecContext(ctx context.Context, name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(ctx, name, "workspace", argv)
}

// ExecRoot runs argv as the image's ROOT user — `msb exec -u root <name> -- <argv>`
// — for privileged operations the unprivileged workspace user cannot perform
// (notably booting the rootful in-VM containerd). A non-zero inner exit is data in
// ExecResult; only an infra failure is a Go error (§4.5).
//
// The `-u root` is REQUIRED and verified live: `msb exec` with NO `-u` does NOT
// run as root — it defaults to the unprivileged `workspace` user (uid 1000), so a
// rootful boot fails with "Permission denied" (e.g. writing /var/log) and is
// silently swallowed. Passing `-u root` is the only way to get uid 0.
//
// hardware bring-up: msb's daemon-persistence (a setsid'd containerd surviving
// the exec) is verified on a provisioned host; the argv assembly is unit-tested.
func (sandbox realSandbox) ExecRoot(name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(context.Background(), name, "root", argv)
}

// ExecRootContext is ExecRoot bounded by ctx — used for the short root probes (the
// apps `nerdctl ps` listing) so they fail fast (killing the hung msb exec and
// returning ErrWorkspaceUnresponsive) instead of hanging when the workspace is busy
// or wedged. The unbounded ExecRoot stays for the deliberately-long containerd boot.
func (sandbox realSandbox) ExecRootContext(ctx context.Context, name string, argv []string) (ExecResult, error) {
	return sandbox.execAs(ctx, name, "root", argv)
}

// execAs runs argv inside the running microVM as a specific user, passed through
// as msb's `-u`. NOTE: an empty user means "omit -u", which is msb's DEFAULT exec
// user — the unprivileged `workspace` (uid 1000), NOT root; callers needing root
// pass "root" (see ExecRoot). The inner command's exit code is faithfully
// propagated by msb to its own process exit; a non-zero inner exit is data
// carried in ExecResult, NOT a Go error. Only infrastructure failures (microVM
// down, msb missing) are returned as errors (§4.5).
func (sandbox realSandbox) execAs(ctx context.Context, name, user string, argv []string) (ExecResult, error) {
	if err := sandbox.ensureInstalled(); err != nil {
		return ExecResult{}, err
	}
	args := []string{"exec"}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, name, "--")
	args = append(args, argv...)
	command := exec.CommandContext(ctx, "msb", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	result := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		result.ExitCode = 0
		return result, nil
	}
	// ctx timed out / was cancelled: the msb exec is killed (CommandContext) and we
	// report a clear "unresponsive" error rather than a generic failure.
	if ctx.Err() != nil {
		return ExecResult{}, ErrWorkspaceUnresponsive
	}
	// A non-zero inner exit surfaces as *exec.ExitError; that is the command's
	// own exit code (data), not a platform failure.
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	// Anything else (couldn't launch msb, signal, etc.) is an infra failure.
	return ExecResult{}, fmt.Errorf("could not run the command in workspace %q — is it running? start it with `ai start`", name)
}

// ExecInteractive runs argv inside the running microVM attached to the caller's
// terminal: `msb exec -t <name> -- <argv>` allocates a PTY, and stdin/stdout/
// stderr are wired straight through (no buffering), so interactive shells and
// agent CLIs work. The inner program's non-zero exit (the user ending the
// session, Ctrl-C, etc.) is NOT a platform error — only a failure to launch msb
// is. Caller is responsible for owning the terminal (the CLI runs it in the
// foreground; the TUI runs it via tea.ExecProcess).
func (sandbox realSandbox) ExecInteractive(name string, argv []string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	// Attach the PTY as the `workspace` user (the image's home owner, matching
	// Exec/WriteFile) so interactive shells, agent CLIs, and tmux sessions all
	// share that user's home + the agent provider configs under /home/workspace.
	args := append([]string{"exec", "-t", "-u", "workspace", name, "--"}, argv...)
	command := exec.Command("msb", args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil // the inner program exited non-zero — normal end of session
		}
		return fmt.Errorf("lost the connection to workspace %q — restart it with `ai start`", name)
	}
	return nil
}

// WriteFile writes content to guestPath inside the running microVM as the
// `workspace` user (the image's home owner), creating parent directories. It
// pipes the content to `cat` over stdin so no file payload appears in argv. A
// missing msb → exit 3; any write/exec failure → exit 4 (plain error mapped by
// the CLI's mapWorkspaceErr).
func (sandbox realSandbox) WriteFile(name, guestPath string, content []byte) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	guestDir := path.Dir(guestPath)
	// Single shell so the mkdir and the redirected cat share one exec; content
	// arrives on stdin (kept out of argv).
	shellScript := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(guestDir), shellQuote(guestPath))
	command := exec.Command("msb", "exec", name, "-u", "workspace", "--", "sh", "-c", shellScript)
	command.Stdin = bytes.NewReader(content)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("could not write to workspace %q", name)
	}
	return nil
}

// LogTail returns the microVM's captured output via `msb logs <name> --tail
// <lines>` (verified against msb 0.5.10: `logs <NAME> [--tail N] [-f]` shows the
// sandbox's captured stdout/stderr). lines ≤ 0 omits --tail (full log). The log
// content is on stdout; on a non-zero exit (e.g. the sandbox is not running) msb
// writes a diagnostic to stderr, which is surfaced in the error.
func (sandbox realSandbox) LogTail(name string, lines int) (string, error) {
	return sandbox.LogTailContext(context.Background(), name, lines)
}

func (sandbox realSandbox) LogTailContext(ctx context.Context, name string, lines int) (string, error) {
	if err := sandbox.ensureInstalled(); err != nil {
		return "", err
	}
	args := []string{"logs", name}
	if lines > 0 {
		args = append(args, "--tail", strconv.Itoa(lines))
	}
	command := exec.CommandContext(ctx, "msb", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("could not read the workspace log (msb logs %s): %s", name, message)
	}
	return stdout.String(), nil
}

// InspectNetwork reads the egress policy in force on the named microVM via
// `msb inspect <name> --format json` and parses the applied network policy. A
// missing msb returns ErrMsbMissing; a name that does not resolve to a sandbox
// (workspace not running) returns ErrNotRunning. Both let the caller fall back to
// the declared policy. The verified msb 0.5.7 shape nests the policy at
// config.network.policy (default_egress + rules) and the violation posture at
// config.network.secrets.on_violation.
func (sandbox realSandbox) InspectNetwork(name string) (NetworkPolicy, error) {
	if err := sandbox.ensureInstalled(); err != nil {
		return NetworkPolicy{}, err
	}
	command := exec.Command("msb", "inspect", name, "--format", "json")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		// A non-existent sandbox (workspace not running) surfaces as a non-zero
		// exit; treat it as ErrNotRunning so the caller shows the declared policy.
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return NetworkPolicy{}, ErrNotRunning
		}
		return NetworkPolicy{}, fmt.Errorf("msb inspect %s: %w", name, err)
	}
	return parseInspectNetwork(stdout.Bytes())
}

// IsRunning reports whether a microVM named name actually exists/runs, via a
// bounded `msb inspect <name> --format json` (the same verified metadata query
// InspectNetwork uses, here bounded by ctx). It is a fast LIVENESS probe — metadata
// only, NOT an in-VM `msb exec` — so it cannot wedge the way an exec can against a
// gone or still-booting VM. msb exits non-zero for a name that resolves to no
// sandbox, which we report as (false, nil) = stale handle; a clean inspect is
// (true, nil) = present. A missing msb is ErrMsbMissing; a ctx timeout/cancel is
// returned so the caller treats an indeterminate liveness as present-but-slow,
// never as a confident "stale".
//
// hardware bring-up: the live `msb inspect` is verified on a provisioned host; the
// argv + non-zero-exit handling are unit-tested with the fake sandbox.
func (sandbox realSandbox) IsRunning(ctx context.Context, name string) (bool, error) {
	if err := sandbox.ensureInstalled(); err != nil {
		return false, err
	}
	command := exec.CommandContext(ctx, "msb", "inspect", name, "--format", "json")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return true, nil
	}
	// ctx timed out / was cancelled: liveness is indeterminate — surface it so the
	// caller does NOT misclassify a slow host as a stale VM.
	if ctx.Err() != nil {
		return false, fmt.Errorf("liveness probe timed out for workspace %q: %w", name, ctx.Err())
	}
	// A non-existent sandbox (workspace not running / stale handle) surfaces as a
	// non-zero exit — that is the definitive "not running" answer.
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return false, nil
	}
	// Anything else (couldn't launch msb, signal, …) is an infra failure.
	return false, fmt.Errorf("msb inspect %s: %w", name, err)
}

// SyncClock sets the guest clock to the host's current UTC time, as root and
// bounded by inVMProbeTimeout. A microVM's clock freezes during host sleep and
// jumps backward on wake; correcting it keeps in-VM TLS (e.g. `nerdctl pull` cert
// validation) working. `date -u -s` accepts the ISO-8601 stamp; only root may set
// the clock. Best-effort for the caller — a non-zero inner exit is ignored here.
func (sandbox realSandbox) SyncClock(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05")
	ctx, cancel := context.WithTimeout(context.Background(), inVMProbeTimeout)
	defer cancel()
	_, err := sandbox.execAs(ctx, name, "root", []string{"date", "-u", "-s", stamp})
	return err
}

// msbInspect mirrors the subset of `msb inspect --format json` (msb 0.5.7) the
// network view needs. Unknown fields are ignored.
type msbInspect struct {
	Config struct {
		Network struct {
			Policy struct {
				DefaultEgress string           `json:"default_egress"`
				Rules         []msbInspectRule `json:"rules"`
			} `json:"policy"`
			Secrets struct {
				OnViolation string `json:"on_violation"`
			} `json:"secrets"`
		} `json:"network"`
	} `json:"config"`
}

// msbInspectRule is one applied net-rule. destination is a single-key object
// keyed by match kind (domain, domain_suffix, cidr, ip, …); it is decoded
// generically so any kind renders.
type msbInspectRule struct {
	Action      string            `json:"action"`
	Direction   string            `json:"direction"`
	Destination map[string]string `json:"destination"`
	Ports       []struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"ports"`
	Protocols []string `json:"protocols"`
}

// parseInspectNetwork extracts the applied egress policy from a raw `msb inspect`
// JSON document. It renders each rule as a single readable line.
func parseInspectNetwork(raw []byte) (NetworkPolicy, error) {
	var inspect msbInspect
	if err := json.Unmarshal(raw, &inspect); err != nil {
		return NetworkPolicy{}, fmt.Errorf("parse msb inspect json: %w", err)
	}
	network := inspect.Config.Network
	policy := NetworkPolicy{
		DefaultEgress: network.Policy.DefaultEgress,
		OnViolation:   network.Secrets.OnViolation,
	}
	for _, rule := range network.Policy.Rules {
		policy.Rules = append(policy.Rules, renderInspectRule(rule))
	}
	return policy, nil
}

// renderInspectRule turns one applied net-rule into a readable line, e.g.
// "allow egress example.com tcp 443" or "allow egress 10.0.0.0/8 tcp 5432".
func renderInspectRule(rule msbInspectRule) string {
	parts := []string{rule.Action, rule.Direction}
	if destination := renderDestination(rule.Destination); destination != "" {
		parts = append(parts, destination)
	}
	if protocols := strings.Join(rule.Protocols, "/"); protocols != "" {
		parts = append(parts, protocols)
	}
	for _, port := range rule.Ports {
		if port.Start == port.End {
			parts = append(parts, strconv.Itoa(port.Start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", port.Start, port.End))
		}
	}
	return strings.Join(parts, " ")
}

// renderDestination flattens the single-key destination object (domain,
// domain_suffix, cidr, ip, …) to a readable token. domain_suffix renders as a
// "*.suffix" wildcard; other kinds render as their value. Keys are sorted for a
// stable result if msb ever reports more than one.
func renderDestination(destination map[string]string) string {
	if len(destination) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(destination))
	for kind := range destination {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	rendered := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		value := destination[kind]
		if kind == "domain_suffix" {
			rendered = append(rendered, "*."+value)
		} else {
			rendered = append(rendered, value)
		}
	}
	return strings.Join(rendered, ",")
}

// shellQuote single-quotes a path for safe interpolation into the `sh -c`
// script (the guest paths are platform-controlled, but quoting keeps it robust).
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// runStreaming runs name with args, streaming stdout/stderr to the parent
// process so build/boot progress is visible.
func runStreaming(name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// runCaptured runs a command and returns its combined stdout+stderr, used when the
// caller needs to inspect the output (e.g. to treat an idempotent "already
// running" as success) rather than stream it to the terminal.
func runCaptured(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	err := command.Run()
	return combined.String(), err
}

// servedModelsClient adapts a *litellm.KeyManager to the ServedModels interface:
// it lists the gateway's live DB-backed models and returns their public model
// names for the in-VM agent picker. Any list error propagates so pickerModels can
// degrade to an empty picker (it never fails the workspace start).
type servedModelsClient struct{ manager *litellm.KeyManager }

func (client servedModelsClient) ServedModels() ([]string, error) {
	models, err := client.manager.ListModels()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(models))
	for _, model := range models {
		if model.Name != "" {
			names = append(names, model.Name)
		}
	}
	return names, nil
}

// RealManager builds a Manager wired to the actual host (used by the CLI).
func RealManager(goos string, now func() string) Manager {
	prober := runtime.RealProber()
	keyManager := litellm.NewKeyManager(prober)
	return Manager{
		Builder: realBuilder{prober: prober},
		Sandbox: selectSandbox(prober),
		Keys:    keyManager,
		Served:  servedModelsClient{manager: keyManager},
		Now:     now,
		GOOS:    goos,
		// Do not hold a host sleep assertion by default. Workspace idling is handled by
		// the msb --idle-timeout at create time; explicit/idle host sleep should remain
		// a real sleep and must not be blocked by the platform.
		Sleep: nil,
	}
}
