// Package egress edits a project's nono network policy in its config.yaml:
// the default outbound posture (deny|public) plus explicit allow/deny domain
// lists. There is no published-port or host-service-port concept anymore —
// nono sandboxes a single host process directly (no guest network to publish
// a port from); this package only manages the declaration. It is applied by
// internal/workspace.ensureProfile when generating the project's nono profile
// at the next `ai agent`/`ai shell`/`ai start` — there is no persistent
// sandbox to restart to pick up a change.
package egress

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// ErrInvalidMode is returned for an egress mode outside config.EgressModes.
var ErrInvalidMode = errors.New("invalid egress mode")

// ErrInvalidHost flags a clearly-invalid allow/deny-list host (empty,
// containing whitespace, or carrying a URL scheme).
var ErrInvalidHost = errors.New("invalid host")

// ValidateHost rejects clearly-invalid hosts while staying permissive — the
// goal is catching typos, not enforcing strict RFC syntax. It accepts IPv4
// addresses, hostnames/domains, and "*.suffix" wildcards (a wildcard must have
// at least two labels, e.g. "*.npmjs.org"). It rejects an empty host, any host
// containing whitespace, and anything carrying a URL scheme like "http://".
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: host is empty", ErrInvalidHost)
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("%w: %q contains whitespace", ErrInvalidHost, host)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("%w: %q looks like a URL — pass just the host", ErrInvalidHost, host)
	}
	if strings.Contains(host, "/") {
		return fmt.Errorf("%w: %q must not contain '/'", ErrInvalidHost, host)
	}
	if strings.HasPrefix(host, "*.") {
		// A suffix wildcard needs a real suffix: "*.npmjs.org" (>=2 labels), not
		// bare "*." or "*.com".
		suffix := strings.TrimPrefix(host, "*.")
		if suffix == "" || !strings.Contains(suffix, ".") {
			return fmt.Errorf("%w: wildcard %q needs at least two suffix labels (e.g. *.npmjs.org)", ErrInvalidHost, host)
		}
		return nil
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf("%w: %q — only a leading \"*.\" suffix wildcard is supported", ErrInvalidHost, host)
	}
	return nil
}

// Get returns the project's declared network policy for `ai network show`.
func Get(projectRoot string) (config.NetworkConfig, error) {
	projectConfig, err := config.Load(projectRoot)
	if err != nil {
		return config.NetworkConfig{}, err
	}
	return projectConfig.Network, nil
}

// SetMode sets the default egress posture.
func SetMode(projectRoot, mode string) error {
	if !slices.Contains(config.EgressModes, mode) {
		return fmt.Errorf("%w: %q (one of %v)", ErrInvalidMode, mode, config.EgressModes)
	}
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		network.Egress = mode
	})
}

// Allow adds (idempotently) a domain the workspace may reach, removing it from
// the deny list if present there.
func Allow(projectRoot, host string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		if !slices.Contains(network.AllowDomain, host) {
			network.AllowDomain = append(network.AllowDomain, host)
		}
		network.DenyDomain = removeDomain(network.DenyDomain, host)
	})
}

// Deny adds (idempotently) a domain the workspace may NOT reach, removing it
// from the allow list if present there.
func Deny(projectRoot, host string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		if !slices.Contains(network.DenyDomain, host) {
			network.DenyDomain = append(network.DenyDomain, host)
		}
		network.AllowDomain = removeDomain(network.AllowDomain, host)
	})
}

// Clear removes host from both the allow and deny lists (back to the default
// egress posture for that domain).
func Clear(projectRoot, host string) error {
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		network.AllowDomain = removeDomain(network.AllowDomain, host)
		network.DenyDomain = removeDomain(network.DenyDomain, host)
	})
}

func removeDomain(list []string, host string) []string {
	index := slices.Index(list, host)
	if index < 0 {
		return list
	}
	return slices.Delete(slices.Clone(list), index, index+1)
}

// mutate loads the project config, applies edit to its Network block, validates,
// and writes it back.
func mutate(projectRoot string, edit func(*config.NetworkConfig)) error {
	projectConfig, err := config.LoadProjectConfig(projectRoot)
	if err != nil {
		return err
	}
	edit(&projectConfig.Network)
	if err := projectConfig.Network.Validate(); err != nil {
		return err
	}
	return config.WriteProject(projectRoot, projectConfig)
}
