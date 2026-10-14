// Package multica manages the HOST half of the Multica integration
// (https://multica.ai): the `multica` CLI + background daemon that registers this
// machine with the self-hosted Multica server and runs agent CLIs for it. (The
// server itself is three containers — see internal/setup/multica_server.go.)
//
// The daemon discovers agent CLIs by name on PATH and launches them itself, which
// would bypass the nono sandbox and the LiteLLM gateway. So the daemon is started
// with a PATH that begins with ShimDir: one tiny script per supported agent CLI
// that re-enters `ai multica-exec`, which launches the REAL CLI inside nono wired
// to the gateway (see internal/workspace/multica.go). Multica documents no
// supported gemini runtime, so gemini gets no shim.
//
// hardware bring-up: install, `multica config set`, browser login, daemon start,
// and the shim round-trip are exercised only on a provisioned host.
package multica

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/releasepkg"
)

const (
	binSubdir  = "multica"
	shimSubdir = "multica-shims"
	// latestReleaseAPI is the GitHub API endpoint for the newest Multica release.
	latestReleaseAPI = "https://api.github.com/repos/multica-ai/multica/releases/latest"
)

// StoreDir is ~/.ai-platform/bin/multica — where the verified release archive's
// `multica` binary is extracted.
func StoreDir() (string, error) {
	binDir, err := paths.BinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(binDir, binSubdir), nil
}

// ShimDir is ~/.ai-platform/bin/multica-shims — prepended to the daemon's PATH.
func ShimDir() (string, error) {
	binDir, err := paths.BinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(binDir, shimSubdir), nil
}

// lookPath is the injectable seam locating the multica binary.
var lookPath = exec.LookPath

// BinaryPath resolves the `multica` binary: the platform store's copy when present,
// else the bare name for a PATH lookup (a brew/manual install is honored too).
func BinaryPath() string {
	if storeDir, err := StoreDir(); err == nil {
		candidate := filepath.Join(storeDir, "multica")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
	}
	return "multica"
}

// Detect reports whether a usable multica CLI is present. Read-only.
func Detect() bool {
	_, err := lookPath(BinaryPath())
	return err == nil
}

// archiveName returns the release asset for a platform — goreleaser publishes
// unversioned multica_<os>_<arch>.tar.gz alongside the versioned names — or ""
// (Windows ships a .zip and is not a target OS for this platform).
func archiveName(goos, goarch string) string {
	if (goos != "darwin" && goos != "linux") || (goarch != "amd64" && goarch != "arm64") {
		return ""
	}
	return "multica_" + goos + "_" + goarch + ".tar.gz"
}

// Install downloads the latest release, verifies it against the release's
// checksums.txt (fail-closed), and extracts it into StoreDir.
func Install() error {
	storeDir, err := StoreDir()
	if err != nil {
		return err
	}
	return releasepkg.Install(releasepkg.Source{
		LatestAPI:     latestReleaseAPI,
		ArchiveName:   archiveName,
		ChecksumsName: "checksums.txt",
		DestDir:       storeDir,
		Executables:   []string{"multica"},
	}, runtime.GOOS, runtime.GOARCH)
}

// Runner runs the multica CLI. env, when non-empty, REPLACES the child's
// environment; nil inherits the current process's.
type Runner func(env []string, args ...string) ([]byte, error)

// RealRunner shells out to the resolved multica binary.
func RealRunner(env []string, args ...string) ([]byte, error) {
	command := exec.Command(BinaryPath(), args...) //nolint:gosec // fixed platform-managed binary, constant subcommands
	command.Env = env
	return command.CombinedOutput()
}

// Configure points the CLI at the self-hosted server. The proxy vhost serves the
// web UI, the API (via the web UI's rewrites) and /ws on one origin, so a single URL
// is both server_url and app_url. Non-interactive; the browser login that follows
// (`multica login`) is the one step this platform cannot automate.
func Configure(run Runner, appURL string) error {
	for _, key := range []string{"server_url", "app_url"} {
		if out, err := run(nil, "config", "set", key, appURL); err != nil {
			return fmt.Errorf("multica config set %s: %w (%s)", key, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// SetupSelfHost runs `multica setup self-host --server-url <url> --app-url <url>` — the
// documented one-command install (SELF_HOSTING_AI.md) — attached to the terminal: it opens
// the browser to sign in, discovers workspaces and starts the daemon in the background. The
// whole command (and so the daemon it starts, which inherits this environment) runs with
// DaemonEnv: shim dir first on PATH so its agents are sandboxed, and platform secrets
// stripped. serverURL and appURL are the same origin here (the proxy vhost serves the web
// UI, the API via its rewrites, and /ws).
func SetupSelfHost(shimDir, appURL string) error {
	command := exec.Command(BinaryPath(), "setup", "self-host", "--server-url", appURL, "--app-url", appURL) //nolint:gosec // fixed platform-managed binary, platform-built URL
	command.Env = DaemonEnv(shimDir)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

// LoggedIn reports whether `multica auth status` succeeds.
func LoggedIn(run Runner) bool {
	_, err := run(nil, "auth", "status")
	return err == nil
}

// DaemonRunning reports whether `multica daemon status` succeeds (exits 0 when up).
func DaemonRunning(run Runner) bool {
	_, err := run(nil, "daemon", "status")
	return err == nil
}

// platformSecrets are env vars `ai` itself holds (container passthroughs and host-backend
// keys). The daemon spawns agent CLIs that inherit its environment, so none may reach it.
var platformSecrets = []string{
	"JWT_SECRET", "LANGFLOW_SUPERUSER_PASSWORD", "LITELLM_MASTER_KEY", "LITELLM_SALT_KEY", "UI_PASSWORD", "OMLX_API_KEY", "COLI_API_KEY",
}

// DaemonEnv is the environment the daemon is started with: the current environment
// minus platformSecrets, with ShimDir prepended to PATH so agent-CLI discovery lands on
// the sandboxing shims.
func DaemonEnv(shimDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || isPlatformSecret(entry) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func isPlatformSecret(entry string) bool {
	for _, name := range platformSecrets {
		if strings.HasPrefix(entry, name+"=") {
			return true
		}
	}
	return false
}

// StartDaemon starts the background daemon with the shim PATH.
func StartDaemon(run Runner, shimDir string) error {
	if out, err := run(DaemonEnv(shimDir), "daemon", "start"); err != nil {
		return fmt.Errorf("multica daemon start: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StopDaemon stops the daemon (a not-running daemon is not an error worth surfacing).
func StopDaemon(run Runner) {
	_, _ = run(nil, "daemon", "stop")
}

// WriteShims (re)writes one POSIX shim per binary into dir, each re-entering
// `<aiBinary> multica-exec <cli>`. binaries maps platform CLI name → PATH binary name.
// Stale shims for CLIs no longer in the map are left alone (harmless).
func WriteShims(dir, aiBinary string, binaries map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	clis := make([]string, 0, len(binaries))
	for cli := range binaries {
		clis = append(clis, cli)
	}
	sort.Strings(clis)
	for _, cli := range clis {
		script := "#!/bin/sh\n# Generated by `ai setup` — re-enters the nono sandbox launcher for Multica tasks.\n" +
			"exec " + shellQuote(aiBinary) + " multica-exec " + shellQuote(cli) + " -- \"$@\"\n"
		path := filepath.Join(dir, binaries[cli])
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // must be executable
			return err
		}
	}
	return nil
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PathWithout returns pathList with every occurrence of dir removed — how the shim
// launcher finds the REAL agent CLI behind its own shim.
func PathWithout(pathList, dir string) string {
	var kept []string
	for _, entry := range filepath.SplitList(pathList) {
		if filepath.Clean(entry) != filepath.Clean(dir) {
			kept = append(kept, entry)
		}
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// InstallGuidance is the manual follow-up printed when automation cannot finish.
func InstallGuidance(appURL string) []string {
	return []string{
		"`ai setup` installs the multica CLI (SHA-256 verified) into " + displayStoreDir() + " and points it at " + appURL + ".",
		"Sign in once: re-run `ai setup` (it offers to run `multica setup self-host` for you) or run `multica login` yourself — open " + appURL + " first and create your account; the verification code is in `ai logs --service multica`.",
		"Then `ai services start multica` starts the daemon with every agent CLI wrapped in the nono sandbox.",
	}
}

func displayStoreDir() string {
	if dir, err := StoreDir(); err == nil {
		return dir
	}
	return "~/.ai-platform/bin/multica"
}
