// Package egress edits a project's workspace egress policy (arch §29.6) in its
// config.yaml: the default outbound posture (deny|public|unrestricted; the
// default is now "public" — allow-outbound, DNS-audited and re-lockable), the
// allow-list of external services the workspace may reach (databases, Kafka,
// specific APIs), and host→guest published ports. Enforced by the Microsandbox
// network policy at workspace start; this package only manages the declaration.
package egress

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/config"
)

// ErrInvalidMode is returned for an egress mode outside config.EgressModes.
var ErrInvalidMode = errors.New("invalid egress mode")

// SplitHostPort parses an allow target. The host may be a hostname/IP/domain, a
// "*.suffix" wildcard, or the "gateway" token for the host machine. The port is
// optional: a bare host with no ":" (e.g. "api.github.com" or "*.npmjs.org")
// defaults to 443 (HTTPS). An explicit "host:port" is split on the LAST colon. IPv6
// literals are not supported. Shared by `ai network allow/disallow` and the TUI.
func SplitHostPort(value string) (string, int, error) {
	value = strings.TrimSpace(value)
	index := strings.LastIndex(value, ":")
	if index < 0 {
		return value, 443, nil
	}
	if index == 0 || index == len(value)-1 {
		return "", 0, fmt.Errorf("expected host or host:port, got %q", value)
	}
	port, err := strconv.Atoi(value[index+1:])
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %s", value, err)
	}
	return value[:index], port, nil
}

// SplitPortPair parses "guest:host" (two ports). Shared by `ai network
// publish/unpublish` and the TUI.
func SplitPortPair(value string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected guest:host, got %q", value)
	}
	guest, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid guest port in %q: %s", value, err)
	}
	host, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid host port in %q: %s", value, err)
	}
	return guest, host, nil
}

// ErrInvalidPort is returned for a port outside 1–65535.
var ErrInvalidPort = errors.New("port out of range")

// ErrInvalidHost is returned for a clearly-invalid allow-rule host (empty, or
// containing whitespace or a URL scheme).
var ErrInvalidHost = errors.New("invalid host")

// ValidateHost rejects clearly-invalid allow-rule hosts while staying permissive
// — the goal is catching typos, not enforcing strict RFC syntax. It accepts the
// "gateway" token, IPv4 addresses, hostnames/domains, and "*.suffix" wildcards
// (a wildcard must have at least two labels, e.g. "*.npmjs.org"). It rejects an
// empty host, any host containing whitespace, and anything carrying a URL scheme
// such as "http://".
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
	if host == gatewayToken {
		return nil
	}
	if strings.HasPrefix(host, "*.") {
		// A suffix wildcard needs a real suffix: "*.npmjs.org" (>=2 labels), not
		// "*." or "*.com".
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

// Get returns the effective network config (global defaults merged with the
// project's config, with project values taking priority) for `ai network show`.
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

// Allow adds (idempotently) a host:port the workspace may reach. host may be a
// hostname/IP/domain, or "gateway" for a service on the host machine.
func Allow(projectRoot, host string, port int) error {
	if err := checkPort(port); err != nil {
		return err
	}
	if host == "" {
		host = "gateway"
	}
	if err := ValidateHost(host); err != nil {
		return err
	}
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		for _, service := range network.AllowHostServices {
			if service.Host == host && service.Port == port {
				return // already allowed
			}
		}
		network.AllowHostServices = append(network.AllowHostServices, config.HostService{Host: host, Port: port})
	})
}

// Deny removes a previously-allowed host:port.
func Deny(projectRoot, host string, port int) error {
	if host == "" {
		host = "gateway"
	}
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		kept := network.AllowHostServices[:0]
		for _, service := range network.AllowHostServices {
			if service.Host == host && service.Port == port {
				continue
			}
			kept = append(kept, service)
		}
		network.AllowHostServices = kept
	})
}

// Publish maps a guest port to a host port (host → workspace), replacing any
// existing mapping for the same host port.
func Publish(projectRoot string, guest, host int) error {
	if err := checkPort(guest); err != nil {
		return err
	}
	if err := checkPort(host); err != nil {
		return err
	}
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		kept := network.PublishPorts[:0]
		for _, mapping := range network.PublishPorts {
			if mapping.Host != host {
				kept = append(kept, mapping)
			}
		}
		network.PublishPorts = append(kept, config.PortMapping{Guest: guest, Host: host})
	})
}

// Unpublish removes the mapping for a host port.
func Unpublish(projectRoot string, host int) error {
	return mutate(projectRoot, func(network *config.NetworkConfig) {
		kept := network.PublishPorts[:0]
		for _, mapping := range network.PublishPorts {
			if mapping.Host != host {
				kept = append(kept, mapping)
			}
		}
		network.PublishPorts = kept
	})
}

func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: %d", ErrInvalidPort, port)
	}
	return nil
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
