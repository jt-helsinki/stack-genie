package nono

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Session mirrors the JSON shape of `nono ps --json` / `nono inspect --json`
// (captured live against nono v0.79.0 — field names are nono's own, not
// renamed). Status is one of "running"/"exited" (nono's own vocabulary);
// Attachment is "attached"/"detached".
type Session struct {
	SessionID     string   `json:"session_id"`
	Name          string   `json:"name"`
	SupervisorPID int      `json:"supervisor_pid"`
	ChildPID      int      `json:"child_pid"`
	Started       string   `json:"started"`
	Status        string   `json:"status"`
	Attachment    string   `json:"attachment"`
	ExitCode      *int     `json:"exit_code"`
	Command       []string `json:"command"`
	Profile       *string  `json:"profile"`
	Workdir       string   `json:"workdir"`
	Network       string   `json:"network"`
}

// RunOptions describes one `nono run`/`nono shell` invocation.
type RunOptions struct {
	// Profile names a profile to use (by name, resolved from
	// ~/.config/nono/profiles/, or an absolute path) — typically a project's
	// generated profile (see ProfilePath), optionally extending a built-in or
	// registry pack profile.
	Profile string
	// Name assigns a human-friendly session name (nono's `--name`).
	Name string
	// AllowCWD grants the invocation's working directory without prompting
	// (nono's `--allow-cwd`).
	AllowCWD bool
	// Command is the program + args to run inside the sandbox. Required for
	// RunDetached/Run; unused for Shell.
	Command []string
	// Dir is the working directory the nono process itself is launched from
	// (also the directory --allow-cwd grants, and what $WORKDIR expands to in
	// the profile). Empty uses the current process's working directory.
	Dir string
	// EnvCredentialMap maps a credential reference (resolved from the OS
	// keystore via the profile's env_credentials, see Keychain* helpers) to the
	// destination environment variable name the sandboxed process sees it as
	// (nono's `--env-credential-map <ref> <ENV_VAR>`, repeatable). This is how
	// the LiteLLM scoped key reaches an agent CLI's process environment without
	// ever being written to a config file or profile on disk — only the
	// credential NAME (not the secret) appears in argv/the profile.
	EnvCredentialMap map[string]string
}

// ErrSessionNotFound is returned when a session id/prefix matches nothing.
var ErrSessionNotFound = errors.New("nono: session not found")

// Runner is the real-vs-fake seam for every `nono` subcommand this package
// drives, mirroring the project's established Runner pattern (e.g.
// internal/omlx.Runner) so Manager-level code stays unit-testable without a
// real nono binary.
type Runner interface {
	// RunDetached starts a session with `nono run --detached ...` and returns
	// its session ID, parsed from nono's own "Started detached session <id>."
	// confirmation line — fail-closed: nono only prints it once the session
	// registry entry and attach socket are both ready.
	RunDetached(ctx context.Context, opts RunOptions) (sessionID string, err error)
	// Run runs `nono run ... -- <command>` in the FOREGROUND, wiring the current
	// process's stdin/stdout/stderr straight through (no session, no PTY of our
	// own) and returning the wrapped command's failure as an *exec.ExitError. It is
	// for callers that are themselves a pipe-driven parent — the Multica daemon
	// streaming an agent CLI's stdio — where a detached session cannot work.
	Run(ctx context.Context, opts RunOptions) error
	// Attach runs `nono attach <sessionID>` with the current process's
	// stdin/stdout/stderr, giving the caller the session's real PTY.
	Attach(ctx context.Context, sessionID string) error
	// Shell runs `nono shell` (interactive) with the current process's
	// stdin/stdout/stderr.
	Shell(ctx context.Context, opts RunOptions) error
	// ListSessions runs `nono ps --json` (optionally `--all` to include
	// exited sessions).
	ListSessions(ctx context.Context, all bool) ([]Session, error)
	// Inspect runs `nono inspect --json <sessionID>`.
	Inspect(ctx context.Context, sessionID string) (Session, error)
	// Stop runs `nono stop <sessionID>` (or `--force` for SIGKILL).
	Stop(ctx context.Context, sessionID string, force bool) error
}

// RealRunner is the production Runner: it shells out to the `nono` binary on
// PATH. It is gated UPSTREAM by Detect (callers should not construct one on a
// host without nono installed) — this type simply reports an exec error if
// the binary is missing.
type RealRunner struct{}

var startedSessionPrefix = "Started detached session "

func (RealRunner) RunDetached(ctx context.Context, opts RunOptions) (string, error) {
	args := []string{"run", "--detached"}
	args = append(args, runOptionFlags(opts)...)
	if len(opts.Command) > 0 {
		args = append(args, "--")
		args = append(args, opts.Command...)
	}

	command := exec.CommandContext(ctx, "nono", args...)
	command.Dir = opts.Dir
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("nono run --detached: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	// nono prints "Started detached session <id>." to STDERR (verified live against
	// v0.79.0), not stdout — stdout is reserved for the wrapped command's own output.
	for _, line := range strings.Split(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, startedSessionPrefix) {
			id := strings.TrimPrefix(line, startedSessionPrefix)
			id = strings.TrimSuffix(strings.TrimSpace(id), ".")
			if id != "" {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("nono run --detached: session id not found in output: %q", stderr.String())
}

func (RealRunner) Run(ctx context.Context, opts RunOptions) error {
	args := []string{"run"}
	args = append(args, runOptionFlags(opts)...)
	args = append(args, "--")
	args = append(args, opts.Command...)
	command := exec.CommandContext(ctx, "nono", args...)
	command.Dir = opts.Dir
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func (RealRunner) Attach(ctx context.Context, sessionID string) error {
	command := exec.CommandContext(ctx, "nono", "attach", sessionID)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func (RealRunner) Shell(ctx context.Context, opts RunOptions) error {
	args := []string{"shell"}
	args = append(args, runOptionFlags(opts)...)
	command := exec.CommandContext(ctx, "nono", args...)
	command.Dir = opts.Dir
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func (RealRunner) ListSessions(ctx context.Context, all bool) ([]Session, error) {
	args := []string{"ps", "--json"}
	if all {
		args = append(args, "--all")
	}
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, "nono", args...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("nono ps: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	var sessions []Session
	if err := json.Unmarshal(stdout.Bytes(), &sessions); err != nil {
		return nil, fmt.Errorf("nono ps: parse JSON: %w", err)
	}
	return sessions, nil
}

func (RealRunner) Inspect(ctx context.Context, sessionID string) (Session, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, "nono", "inspect", "--json", sessionID)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if strings.Contains(stderr.String(), "not found") {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, fmt.Errorf("nono inspect: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	var session Session
	if err := json.Unmarshal(stdout.Bytes(), &session); err != nil {
		return Session{}, fmt.Errorf("nono inspect: parse JSON: %w", err)
	}
	return session, nil
}

func (RealRunner) Stop(ctx context.Context, sessionID string, force bool) error {
	args := []string{"stop", sessionID}
	if force {
		args = append(args, "--force")
	}
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, "nono", args...)
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if strings.Contains(stderr.String(), "not found") {
			return ErrSessionNotFound
		}
		return fmt.Errorf("nono stop: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runOptionFlags renders the flag portion (not the subcommand, not the "--
// command" trailer) shared by `nono run`/`nono shell`.
func runOptionFlags(opts RunOptions) []string {
	var args []string
	if opts.Profile != "" {
		args = append(args, "--profile", opts.Profile)
	}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.AllowCWD {
		args = append(args, "--allow-cwd")
	}
	// Sorted for deterministic argv (map iteration order is otherwise random).
	credentialRefs := make([]string, 0, len(opts.EnvCredentialMap))
	for ref := range opts.EnvCredentialMap {
		credentialRefs = append(credentialRefs, ref)
	}
	sort.Strings(credentialRefs)
	for _, ref := range credentialRefs {
		args = append(args, "--env-credential-map", ref, opts.EnvCredentialMap[ref])
	}
	return args
}

// FakeRunner is an in-memory Runner for tests. It never shells out to a real
// nono binary. Safe for concurrent use is not required (test-only, single
// goroutine); callers inspect the recorded *Calls fields directly.
type FakeRunner struct {
	// RunDetachedSessionID is returned by RunDetached (default "fake-session").
	RunDetachedSessionID string
	// RunDetachedErr, when non-nil, fails every RunDetached call.
	RunDetachedErr error
	// RunErr, when non-nil, fails every Run call.
	RunErr error
	// AttachErr, when non-nil, fails every Attach call.
	AttachErr error
	// ShellErr, when non-nil, fails every Shell call.
	ShellErr error
	// Sessions is returned by ListSessions (filtered by the all flag only if
	// a test wants that nuance; by default returned as-is).
	Sessions []Session
	// ListSessionsErr, when non-nil, fails every ListSessions call.
	ListSessionsErr error
	// InspectSessions maps a session ID to the Session Inspect returns; a
	// missing key yields ErrSessionNotFound.
	InspectSessions map[string]Session
	// StopErr, when non-nil, fails every Stop call.
	StopErr error

	RunCalls          []RunOptions
	RunDetachedCalls  []RunOptions
	AttachCalls       []string
	ShellCalls        []RunOptions
	ListSessionsCalls []bool
	InspectCalls      []string
	StopCalls         []FakeStopCall
}

// FakeStopCall captures the arguments of one Runner.Stop invocation.
type FakeStopCall struct {
	SessionID string
	Force     bool
}

func (fake *FakeRunner) RunDetached(_ context.Context, opts RunOptions) (string, error) {
	fake.RunDetachedCalls = append(fake.RunDetachedCalls, opts)
	if fake.RunDetachedErr != nil {
		return "", fake.RunDetachedErr
	}
	if fake.RunDetachedSessionID != "" {
		return fake.RunDetachedSessionID, nil
	}
	return "fake-session", nil
}

func (fake *FakeRunner) Run(_ context.Context, opts RunOptions) error {
	fake.RunCalls = append(fake.RunCalls, opts)
	return fake.RunErr
}

func (fake *FakeRunner) Attach(_ context.Context, sessionID string) error {
	fake.AttachCalls = append(fake.AttachCalls, sessionID)
	return fake.AttachErr
}

func (fake *FakeRunner) Shell(_ context.Context, opts RunOptions) error {
	fake.ShellCalls = append(fake.ShellCalls, opts)
	return fake.ShellErr
}

func (fake *FakeRunner) ListSessions(_ context.Context, all bool) ([]Session, error) {
	fake.ListSessionsCalls = append(fake.ListSessionsCalls, all)
	if fake.ListSessionsErr != nil {
		return nil, fake.ListSessionsErr
	}
	return fake.Sessions, nil
}

func (fake *FakeRunner) Inspect(_ context.Context, sessionID string) (Session, error) {
	fake.InspectCalls = append(fake.InspectCalls, sessionID)
	session, ok := fake.InspectSessions[sessionID]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return session, nil
}

func (fake *FakeRunner) Stop(_ context.Context, sessionID string, force bool) error {
	fake.StopCalls = append(fake.StopCalls, FakeStopCall{SessionID: sessionID, Force: force})
	return fake.StopErr
}
