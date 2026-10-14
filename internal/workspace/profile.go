package workspace

import (
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/nono"
)

// profilePackPullTimeout bounds the best-effort `nono pull` a fresh profile's
// base pack triggers — never blocks a launch for long if the registry is slow
// or unreachable (the profile still works, extending "default" at worst).
const profilePackPullTimeout = 20 * time.Second

// pullPackFn is the injectable seam over nono.PullPack (mirrors the project's
// established *Fn-variable pattern, e.g. internal/setup's installNonoFn) so
// unit tests never shell out to the real `nono` binary / hit its registry.
var pullPackFn = nono.PullPack

// profileBase returns the profile a CLI's generated profile extends: its official
// registry pack (best-effort pulled) when one exists, else nono's built-in "default".
func profileBase(cli string) string {
	base := nonoPackForCLI[cli]
	if base == "" {
		return "default"
	}
	// Best-effort: an offline/slow registry must never block a launch — the
	// profile's `extends` reference still resolves fine once the pack IS
	// pulled, and nono itself prompts/degrades sensibly if it never is.
	ctx, cancel := context.WithTimeout(context.Background(), profilePackPullTimeout)
	defer cancel()
	_ = pullPackFn(ctx, base)
	return base
}

// ensureProfile generates (or regenerates) the nono profile for one agent CLI
// invocation in project root, and returns its path. cli selects the base pack
// (nonoPackForCLI) when a dedicated one exists; an empty cli (Shell) falls back
// to nono's own built-in "default" profile. gatewayURL is only needed for
// gemini, which has no config-file base-URL setting (set directly as a
// non-secret profile environment variable instead). The profile is rewritten
// on every call (cheap — a few lines of JSON) so a config.yaml network change
// (`ai network allow/deny`) takes effect on the very next launch, no restart.
func ensureProfile(root, cli string, projectConfig *config.Config, gatewayURL string) (string, error) {
	base := profileBase(cli)
	profileKey := cli
	if profileKey == "" {
		profileKey = "shell"
	}

	allowDomain := projectConfig.Network.AllowDomain
	// The model gateway must stay reachable regardless of the project's own
	// egress policy — a `deny` profile is meant to fence off the INTERNET, not
	// the agent's own LiteLLM gateway, which every agent CLI needs for every
	// request. Without this, `ai network egress deny` silently breaks the
	// agent it's supposed to just sandbox.
	if gatewayHost := gatewayHostFromURL(gatewayURL); gatewayHost != "" && !containsString(allowDomain, gatewayHost) {
		allowDomain = append(append([]string{}, allowDomain...), gatewayHost)
	}

	profile := nono.Profile{
		Meta:    nono.Meta{Name: Name(filepath.Base(root)) + "-" + profileKey},
		Extends: base,
		Network: nono.NetworkCapabilities{
			Block:       projectConfig.Network.ResolvedEgress() == "deny",
			AllowDomain: allowDomain,
			DenyDomain:  projectConfig.Network.DenyDomain,
			OpenPort:    loopbackGatewayPorts(gatewayURL),
		},
	}
	if cli == "gemini" {
		gatewayRoot := strings.TrimSuffix(strings.TrimRight(gatewayURL, "/"), "/v1")
		profile.Environment.SetVars = map[string]string{agentcfg.GeminiBaseURLEnvVar: gatewayRoot}
	}

	if cli == "opencode" {
		// opencode keeps ONE shared database under ~/.local/share/opencode. When that file was
		// written by a different opencode build ("Database is not empty and has no session
		// table") opencode fails on start and the session shows a blank screen with no error.
		// A per-project database avoids the shared file; OPENCODE_DB is opencode's own override.
		profile.Environment.SetVars = map[string]string{"OPENCODE_DB": filepath.Join(root, ".ai-platform", "opencode.db")}
	}

	path := filepath.Join(root, ".ai-platform", "nono-profile-"+profileKey+".json")
	if err := nono.WriteProfile(path, profile); err != nil {
		return "", err
	}
	return path, nil
}

// gatewayHostFromURL extracts the bare hostname (no port, no scheme) from a
// gateway base URL for use as an AllowDomain entry — nono's domain allow-list
// matches on hostname, as seeded by its own documented examples (a bare
// "127.0.0.1", no port). Returns "" for an empty or unparsable URL (Shell
// launches pass no gatewayURL since there is no agent to reach the gateway).
func gatewayHostFromURL(gatewayURL string) string {
	if gatewayURL == "" {
		return ""
	}
	parsed, err := url.Parse(gatewayURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return parsed.Hostname()
}

// loopbackGatewayPorts returns the gateway's port when it is served on this machine's
// loopback (the standalone default): the sandbox can only reach it through an explicit
// open port, not through allow_domain.
func loopbackGatewayPorts(gatewayURL string) []int {
	if gatewayURL == "" {
		return nil
	}
	parsed, err := url.Parse(gatewayURL)
	if err != nil {
		return nil
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return nil
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return nil
	}
	return []int{port}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
