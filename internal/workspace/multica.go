package workspace

// multica.go launches an agent CLI for a Multica task INSIDE nono. The Multica
// daemon (internal/multica) is started with a PATH of shims that re-enter here via
// `ai multica-exec`, so every agent it spawns is sandboxed and routed through the
// LiteLLM gateway instead of running bare on the host.
//
// Unlike Manager.Agent there is no registered project: the task directory the
// daemon gives us (the process cwd) plays the project root. Two consequences:
//   - ONE scoped gateway key is shared by every task (multicaKeyProject), minted once
//     when the daemon starts (EnsureMulticaKey) and only INJECTED here. Re-minting per
//     task would revoke the key of every task still running (the daemon runs up to 20).
//   - The filesystem grant is wider than a project's: besides the task dir it must
//     cover the whole Multica workspaces root (a task's .git points into the shared
//     bare-repo cache there, and Codex keeps its home beside the task) and the temp base.
//
// hardware bring-up: stdio passthrough of `nono run` under a pipe-driven parent, the
// workspaces-root grant being sufficient for git/codex, and per-CLI gateway wiring in a
// git worktree are exercised only on a provisioned host with Multica running.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/multica"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// multicaKeyProject is the key alias / keychain-name stem of the single scoped
// gateway key shared by all Multica tasks (credentialName gives "aip-gateway-multica").
const multicaKeyProject = "multica"

// EnsureMulticaKey mints (revoking any prior) the shared Multica scoped gateway key
// and stores it in the OS keystore. Call once when the daemon starts — never per task.
func (manager Manager) EnsureMulticaKey() error {
	_, err := manager.ensureScopedKey(multicaKeyProject)
	return err
}

// AgentBinary returns the PATH binary name of a platform agent CLI.
func AgentBinary(cli string) (string, bool) {
	spec, ok := agentValidCLIs[cli]
	return spec.binary, ok
}

// MulticaWorkspacesRoot is where the daemon creates task directories: its own
// MULTICA_WORKSPACES_ROOT override, else ~/multica_workspaces.
func MulticaWorkspacesRoot() (string, error) {
	if root := os.Getenv("MULTICA_WORKSPACES_ROOT"); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "multica_workspaces"), nil
}

// isVersionProbe reports whether args are the daemon's CLI-detection probe, which
// must not mint keys or start a sandbox.
func isVersionProbe(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "--version", "-v", "-V", "version":
		return true
	}
	return false
}

// RunMulticaTask runs agent CLI cli with args in workdir, sandboxed. It resolves the
// real binary from PATH with the shim directory removed (the shim IS on the daemon's
// PATH, so a plain lookup would find itself). A version probe bypasses the sandbox.
func (manager Manager) RunMulticaTask(cli, workdir string, args []string) error {
	spec, ok := agentValidCLIs[cli]
	if !ok {
		return fmt.Errorf("%w %q (valid: %s)", ErrUnknownAgentCLI, cli, strings.Join(AgentCLINames(), ", "))
	}
	shimDir, err := multica.ShimDir()
	if err != nil {
		return err
	}
	realBinary, err := lookPathIn(spec.binary, multica.PathWithout(os.Getenv("PATH"), shimDir))
	if err != nil {
		return fmt.Errorf("%s is not installed on this host (run `ai setup` to install it): %w", cli, err)
	}

	if isVersionProbe(args) {
		probe := exec.Command(realBinary, args...) //nolint:gosec // the user's own agent CLI, fixed probe arg
		probe.Stdin, probe.Stdout, probe.Stderr = os.Stdin, os.Stdout, os.Stderr
		return probe.Run()
	}

	_, _, gatewayURL := resolveGateway()
	// A zero Config is the default project posture (api-key mode, public egress). The
	// key VALUE is irrelevant to writeAgentConfig (it only picks the config shape), so a
	// non-empty placeholder stands in — the real key only ever travels via the keystore.
	projectConfig := &config.Config{}
	if err := manager.writeAgentConfig(workdir, cli, projectConfig, gatewayURL, "keystore"); err != nil {
		return err
	}
	profilePath, err := ensureMulticaProfile(cli, gatewayURL)
	if err != nil {
		return err
	}
	return manager.Runner.Run(context.Background(), nono.RunOptions{
		Profile:          profilePath,
		AllowCWD:         true,
		Dir:              workdir,
		Command:          append([]string{realBinary}, args...),
		EnvCredentialMap: map[string]string{credentialName(multicaKeyProject): spec.credentialEnvVar},
	})
}

// ensureMulticaProfile writes the shared nono profile for cli's Multica tasks under
// ~/.ai-platform/multica (never into a task worktree, which the agent may commit from)
// and returns its path. Filesystem: the workspaces root + temp base on top of the CLI
// pack's own grants and the task cwd; network: default-open plus the gateway host.
func ensureMulticaProfile(cli, gatewayURL string) (string, error) {
	workspacesRoot, err := MulticaWorkspacesRoot()
	if err != nil {
		return "", err
	}
	platformDir, err := paths.PlatformDir()
	if err != nil {
		return "", err
	}
	allow := []string{workspacesRoot, os.TempDir()}
	if os.TempDir() != "/tmp" {
		allow = append(allow, "/tmp")
	}
	profile := nono.Profile{
		Meta:       nono.Meta{Name: "aip-multica-" + cli},
		Extends:    profileBase(cli),
		Filesystem: nono.FilesystemCapabilities{Allow: allow},
	}
	if host := gatewayHostFromURL(gatewayURL); host != "" {
		profile.Network.AllowDomain = []string{host}
	}
	path := filepath.Join(platformDir, "multica", "nono-profile-"+cli+".json")
	if err := nono.WriteProfile(path, profile); err != nil {
		return "", err
	}
	return path, nil
}

// lookPathIn finds name in the given PATH list (exec.LookPath always uses the
// process's own PATH).
func lookPathIn(name, pathList string) (string, error) {
	for _, dir := range filepath.SplitList(pathList) {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("executable file not found in PATH")
}
