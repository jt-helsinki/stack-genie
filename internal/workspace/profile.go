package workspace

import (
	"context"
	"net/url"
	"path/filepath"
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

// ensureProfile generates (or regenerates) the nono profile for one agent CLI
// invocation in project root, and returns its path. cli selects the base pack
// (nonoPackForCLI) when a dedicated one exists; an empty cli (Shell) falls back
// to nono's own built-in "default" profile. gatewayURL is only needed for
// gemini, which has no config-file base-URL setting (set directly as a
// non-secret profile environment variable instead). The profile is rewritten
// on every call (cheap — a few lines of JSON) so a config.yaml network change
// (`ai network allow/deny`) takes effect on the very next launch, no restart.
func ensureProfile(root, cli string, projectConfig *config.Config, gatewayURL string) (string, error) {
	base := nonoPackForCLI[cli]
	profileKey := cli
	if profileKey == "" {
		profileKey = "shell"
	}
	if base != "" {
		// Best-effort: an offline/slow registry must never block a launch — the
		// profile's `extends` reference still resolves fine once the pack IS
		// pulled, and nono itself prompts/degrades sensibly if it never is.
		ctx, cancel := context.WithTimeout(context.Background(), profilePackPullTimeout)
		_ = pullPackFn(ctx, base)
		cancel()
	} else {
		base = "default"
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
		},
	}
	if cli == "gemini" {
		gatewayRoot := strings.TrimSuffix(strings.TrimRight(gatewayURL, "/"), "/v1")
		profile.Environment.SetVars = map[string]string{agentcfg.GeminiBaseURLEnvVar: gatewayRoot}
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

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
