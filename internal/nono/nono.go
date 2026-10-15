// Package nono wraps the `nono` CLI (https://nono.sh) — the kernel-capability
// sandboxing tool (Landlock LSM on Linux, Seatbelt on macOS) that replaces
// Microsandbox microVMs as the workspace runtime. Unlike Microsandbox, nono
// sandboxes a single host process per invocation: there is no VM, no OS image,
// and no persistent boot. A "session" (internal/nono/session.go) exists only
// while its wrapped command is running or detached-but-alive; a "profile"
// (this file) is the declarative capability grant (filesystem/network access)
// a session is launched with.
package nono

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Prober abstracts host probes so detection can be faked in tests, mirroring
// internal/sandbox's pattern for the runtime it replaces.
type Prober interface {
	LookPath(file string) (string, error)
}

// Info is the detected nono-CLI state. Unlike the Microsandbox runtime it
// replaces, there is no separate "virtualization available" check — nono's
// Landlock/Seatbelt enforcement works on any supported host the binary runs
// on, with no Apple-Silicon-only restriction.
type Info struct {
	Installed bool
	// Path is the resolved nono binary location, empty when not Installed.
	Path string
}

// Detect reports whether the `nono` CLI is on PATH. It never installs.
func Detect(prober Prober) Info {
	path, err := prober.LookPath("nono")
	if err != nil {
		return Info{}
	}
	return Info{Installed: true, Path: path}
}

// Profile is the subset of nono's profile JSON schema (`nono profile schema`)
// this platform generates. Fields outside what we actually populate are
// deliberately omitted rather than modeled — see `nono profile schema` for
// the full schema if a future need requires more of it.
type Profile struct {
	Meta Meta `json:"meta"`
	// Extends names a base profile (a built-in preset, a pulled registry pack
	// like "nolabs-ai/claude", or another user profile) this one inherits from.
	// Every profile implicitly extends "default" even when this is empty.
	Extends        string                  `json:"extends,omitempty"`
	Filesystem     FilesystemCapabilities  `json:"filesystem"`
	Network        NetworkCapabilities     `json:"network"`
	EnvCredentials map[string]string       `json:"env_credentials,omitempty"`
	Environment    EnvironmentCapabilities `json:"environment,omitempty"`
}

// EnvironmentCapabilities sets plain (non-secret) environment variables for
// the sandboxed process — e.g. a gateway base URL, which is a local address,
// not a credential. Secrets belong in EnvCredentials/--env-credential-map
// instead, never here (this is written verbatim into the profile JSON).
type EnvironmentCapabilities struct {
	SetVars map[string]string `json:"set_vars,omitempty"`
}

// Meta identifies a profile.
type Meta struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// FilesystemCapabilities grants path-level access. Allow is read+write
// (recursive); Read/Write are their single-mode counterparts.
type FilesystemCapabilities struct {
	Allow []string `json:"allow,omitempty"`
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
	// UnixSocketSubtreeBind allows connect+bind of AF_UNIX sockets anywhere under these
	// directories. nono denies pathname unix sockets by default.
	UnixSocketSubtreeBind []string `json:"unix_socket_subtree_bind,omitempty"`
}

// NetworkCapabilities controls egress. Block denies all network access
// (sticky across profile inheritance, per nono's docs); AllowDomain/DenyDomain
// are proxy allow/deny lists (deny evaluated first); CustomCredentials maps a
// named credential service to an upstream + header-injection rule, used to
// route the agent CLI's gateway traffic through nono's reverse-proxy
// credential injection (see internal/nono's package doc and the migration
// plan's "Credential wiring" section) so the real LiteLLM scoped key never
// touches the agent process's environment or disk.
type NetworkCapabilities struct {
	Block       bool     `json:"block"`
	AllowDomain []string `json:"allow_domain,omitempty"`
	DenyDomain  []string `json:"deny_domain,omitempty"`
	// OpenPort allows bidirectional localhost TCP on these ports. Needed for a loopback
	// gateway: in nono's proxy network mode a direct connect to 127.0.0.1 is blocked, and
	// allow_domain only governs traffic that goes THROUGH the proxy.
	OpenPort          []int                       `json:"open_port,omitempty"`
	NetworkProfile    string                      `json:"network_profile,omitempty"`
	CustomCredentials map[string]CustomCredential `json:"custom_credentials,omitempty"`
}

// CustomCredential maps a named credential (referenced by `--credential
// <name>` / `network.credentials`) to an upstream service nono's reverse
// proxy injects the real secret into.
type CustomCredential struct {
	Upstream         string         `json:"upstream"`
	CredentialKey    string         `json:"credential_key"`
	InjectHeader     string         `json:"inject_header,omitempty"`
	CredentialFormat string         `json:"credential_format,omitempty"`
	EndpointRules    []EndpointRule `json:"endpoint_rules,omitempty"`
}

// EndpointRule scopes a CustomCredential to specific HTTP method+path
// patterns (nono's `--allow-endpoint SERVICE:METHOD:PATH` equivalent).
type EndpointRule struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// ProfilePath returns the on-disk path for a project's generated profile,
// kept alongside the rest of its tracked .ai-platform state.
func ProfilePath(projectRoot string) string {
	return filepath.Join(projectRoot, ".ai-platform", "nono-profile.json")
}

// WriteProfile atomically writes profile as JSON to path (temp file + rename,
// matching internal/conffile's safety guarantee for the one format conffile
// itself doesn't handle — nono requires JSON, not YAML).
func WriteProfile(path string, profile Profile) error {
	return writeAtomicJSON(path, profile)
}

// PullPack best-effort pulls a nono registry pack (e.g. "nolabs-ai/claude"),
// non-interactively (`NONO_AUTO_MIGRATE=1`, matching nono's own documented
// automation flag so the first-run install prompt never blocks). A failure
// (offline, unknown pack) is returned for the caller to log/swallow — pack
// packs are an optimization (OAuth-flow smoothing, hook wiring), not required
// for a profile to function, since every generated profile also falls back to
// extending the built-in "default" profile.
func PullPack(ctx context.Context, name string) error {
	command := exec.CommandContext(ctx, "nono", "pull", name)
	command.Env = append(os.Environ(), "NONO_AUTO_MIGRATE=1")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("nono pull %s: %w (output: %s)", name, err, strings.TrimSpace(output.String()))
	}
	return nil
}
