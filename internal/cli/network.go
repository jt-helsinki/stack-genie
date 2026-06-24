package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// newNetworkCmd builds `ai network` (CLI §29.6): the per-project workspace egress
// policy — default posture, the allow-list of external services (DB/Kafka/APIs),
// and published ports. Enforced by the Microsandbox network policy at workspace
// start; these commands edit the declaration in the project's config.yaml.
func newNetworkCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Inspect and configure the workspace egress policy",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newNetworkShowCmd(emitter, exit),
		newNetworkEgressCmd(emitter, exit),
		newNetworkAllowCmd(emitter, exit),
		newNetworkPublishCmd(emitter, exit),
		newNetworkLogCmd(emitter, exit),
	)
	return cmd
}

func mapEgressErr(err error) error {
	// An error that already carries an exit code (e.g. resolveProjectName's exit-2
	// "no project" error) is passed through unchanged — don't re-map it to runtime.
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	switch {
	case errors.Is(err, egress.ErrInvalidMode),
		errors.Is(err, egress.ErrInvalidPort),
		errors.Is(err, egress.ErrInvalidHost),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// networkResult is the `ai network show` payload: the project's DECLARED egress
// policy (from config.yaml) plus, when the workspace is running, the policy
// actually IN FORCE on the microVM (read back from msb inspect).
type networkResult struct {
	Egress            string                   `json:"egress"`
	AllowHostServices []config.HostService     `json:"allow_host_services,omitempty"`
	PublishPorts      []config.PortMapping     `json:"publish_ports,omitempty"`
	InForce           *workspace.NetworkPolicy `json:"in_force,omitempty"`
}

// Human renders the declared egress policy and, when available, the live in-force
// policy. The in-force section is the applied POLICY (what would be blocked), not
// a record of blocked connections — msb 0.5.7 exposes only the policy config.
func (result networkResult) Human() string {
	lines := []string{"declared (config.yaml):", "  egress: " + result.Egress}
	if len(result.AllowHostServices) == 0 {
		lines = append(lines, "  allow:  (none — only the model gateway is reachable)")
	} else {
		lines = append(lines, "  allow:")
		for _, service := range result.AllowHostServices {
			lines = append(lines, fmt.Sprintf("    - %s:%d", service.Host, service.Port))
		}
	}
	if len(result.PublishPorts) > 0 {
		lines = append(lines, "  publish (host→guest):")
		for _, mapping := range result.PublishPorts {
			lines = append(lines, fmt.Sprintf("    - %d→%d", mapping.Host, mapping.Guest))
		}
	}
	if result.InForce == nil {
		lines = append(lines, "", "in force (live): (workspace not running — showing declared policy only)")
		return strings.Join(lines, "\n")
	}
	lines = append(lines,
		"",
		"in force (live, on the running microVM):",
		"  (applied policy — what WOULD be blocked, not a record of blocked connections)",
		"  default egress: "+result.InForce.DefaultEgress)
	if len(result.InForce.Rules) == 0 {
		lines = append(lines, "  rules:  (none)")
	} else {
		lines = append(lines, "  rules:")
		for _, rule := range result.InForce.Rules {
			lines = append(lines, "    - "+rule)
		}
	}
	if result.InForce.OnViolation != "" {
		lines = append(lines, "  on violation: "+result.InForce.OnViolation)
	}
	return strings.Join(lines, "\n")
}

// projectArg resolves the trailing optional [project] at args[index].
func networkResolve(cmd *cobra.Command, args []string, index int) (string, error) {
	explicit := ""
	if len(args) > index {
		explicit = args[index]
	}
	name, err := resolveProjectName(cmd, explicit)
	if err != nil {
		return "", err
	}
	return resolveProjectRoot(name)
}

func newNetworkShowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "show [project]",
		Short:             "Show the workspace egress policy",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			explicit := ""
			if len(args) > 0 {
				explicit = args[0]
			}
			name, err := resolveProjectName(cmd, explicit)
			if err != nil {
				*exit = emitter.Failure("network.show", mapEgressErr(err))
				return nil
			}
			root, err := resolveProjectRoot(name)
			if err != nil {
				*exit = emitter.Failure("network.show", mapEgressErr(err))
				return nil
			}
			network, err := egress.Get(root)
			if err != nil {
				*exit = emitter.Failure("network.show", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			result := networkResult{
				Egress:            network.ResolvedEgress(),
				AllowHostServices: network.AllowHostServices,
				PublishPorts:      network.PublishPorts,
			}
			// Best-effort: read the live in-force policy off the running microVM.
			// A not-running workspace (or missing msb) is NOT an error — we just
			// show the declared policy. Only a genuine runtime failure surfaces.
			policy, inspectErr := workspace.RealManager(goruntime.GOOS, nowRFC3339).InspectNetwork(name)
			switch {
			case inspectErr == nil:
				result.InForce = &policy
			case errors.Is(inspectErr, workspace.ErrNotRunning),
				errors.Is(inspectErr, workspace.ErrMsbMissing):
				// Declared-only view; leave InForce nil.
			default:
				*exit = emitter.Failure("network.show", output.Errorf(output.ExitRuntimeFailure, "%s", inspectErr))
				return nil
			}
			*exit = emitter.Success("network.show", result)
			return nil
		},
	}
}

func newNetworkEgressCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "egress [deny|public|unrestricted] [project]",
		Short: "Set the default egress posture (prompts on a terminal, pre-seeded)",
		Args:  cobra.MaximumNArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return config.EgressModes, cobra.ShellCompDirectiveNoFileComp
			}
			return completeProjectNames(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.egress", mapEgressErr(err))
				return nil
			}
			// On a terminal always prompt, PRE-SEEDED with any mode given on the
			// command line; under --json / no TTY the arg is used directly and is
			// required (§21, §1.8).
			mode := ""
			if len(args) >= 1 {
				mode = args[0]
			}
			if interactive(emitter) {
				initial := mode
				if initial == "" {
					initial = "deny"
				}
				picked, err := promptChoice("Default egress posture", "",
					[]huh.Option[string]{
						huh.NewOption("deny — only the model gateway + allowed services (most locked-down)", "deny"),
						huh.NewOption("public — open internet, private ranges still blocked", "public"),
						huh.NewOption("unrestricted — allow everything (least safe)", "unrestricted"),
					}, initial)
				if err != nil {
					*exit = emitter.Failure("network.egress", err)
					return nil
				}
				mode = picked
			} else if mode == "" {
				*exit = emitter.Failure("network.egress", output.Errorf(output.ExitInvalidInput,
					"egress mode required (deny|public|unrestricted), or run on a terminal to choose"))
				return nil
			}
			if err := egress.SetMode(root, mode); err != nil {
				*exit = emitter.Failure("network.egress", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.egress", map[string]any{"egress": mode})
			return nil
		},
	}
}

func newNetworkAllowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "allow [host[:port]] [project]",
		Short: "Allow the workspace to reach an external service (prompts on a terminal, pre-seeded; --remove to revoke)",
		Long: "Allow an egress destination the workspace may reach directly (a database,\n" +
			"Kafka broker, or a specific API/domain). host may be a hostname/IP/domain,\n" +
			"a \"*.suffix\" wildcard (e.g. *.npmjs.org), or \"gateway\" for a service on\n" +
			"the host machine. The port is optional and defaults to 443 (HTTPS), so a\n" +
			"bare domain like api.github.com allows it on 443. Omit the host on a terminal\n" +
			"to be prompted (pre-seeded with any value you pass). Use --remove to revoke.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			provided := ""
			if len(args) >= 1 {
				provided = args[0]
			}
			var host string
			var port int
			switch {
			case remove:
				// --remove targets an existing rule by host:port; it is given on the
				// command line (no seeded prompt — there is nothing to pre-fill).
				if provided == "" {
					*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "--remove needs a host:port"))
					return nil
				}
				host, port, err = splitHostPort(provided)
				if err != nil {
					*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			case interactive(emitter):
				// On a terminal always prompt, PRE-SEEDED with any value given on the
				// command line (§21, §1.8).
				entered, promptErr := promptText(
					"Service to allow (host, host:port, '*.suffix' wildcard, or 'gateway')",
					"a bare host defaults to port 443 (e.g. api.github.com); use host:port for a specific port (e.g. gateway:5432)",
					provided,
					func(candidate string) error {
						_, _, validateErr := splitHostPort(strings.TrimSpace(candidate))
						return validateErr
					},
				)
				if promptErr != nil {
					*exit = emitter.Failure("network.allow", promptErr)
					return nil
				}
				host, port, err = splitHostPort(entered)
				if err != nil {
					*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			case provided != "":
				host, port, err = splitHostPort(provided)
				if err != nil {
					*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			default:
				*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput,
					"provide host:port, or run on a terminal to enter it"))
				return nil
			}
			action := egress.Allow
			if remove {
				action = egress.Deny
			}
			if err := action(root, host, port); err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.allow", map[string]any{"host": host, "port": port, "removed": remove})
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "revoke this allow rule instead of adding it")
	return cmd
}

func newNetworkPublishCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "publish [guest:host] [project]",
		Short: "Publish a workspace port to the host (prompts on a terminal, pre-seeded; --remove to revoke)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.publish", mapEgressErr(err))
				return nil
			}
			provided := ""
			if len(args) >= 1 {
				provided = args[0]
			}
			var guest, host int
			switch {
			case remove:
				// --remove targets an existing mapping; given on the command line.
				if provided == "" {
					*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "--remove needs a guest:host"))
					return nil
				}
				guest, host, err = splitPortPair(provided)
				if err != nil {
					*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			case interactive(emitter):
				// On a terminal always prompt, PRE-SEEDED with any value given on the
				// command line (§21, §1.8).
				entered, promptErr := promptText(
					"Port mapping to publish (guest:host)",
					"the guest port inside the workspace and the host port it is reachable at (e.g. 3000:3000)",
					provided,
					func(candidate string) error {
						_, _, validateErr := splitPortPair(strings.TrimSpace(candidate))
						return validateErr
					},
				)
				if promptErr != nil {
					*exit = emitter.Failure("network.publish", promptErr)
					return nil
				}
				guest, host, err = splitPortPair(entered)
				if err != nil {
					*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			case provided != "":
				guest, host, err = splitPortPair(provided)
				if err != nil {
					*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			default:
				*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput,
					"provide guest:host, or run on a terminal to enter the mapping"))
				return nil
			}
			if remove {
				if err := egress.Unpublish(root, host); err != nil {
					*exit = emitter.Failure("network.publish", mapEgressErr(err))
					return nil
				}
			} else if err := egress.Publish(root, guest, host); err != nil {
				*exit = emitter.Failure("network.publish", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.publish", map[string]any{"guest": guest, "host": host, "removed": remove})
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove this published port instead of adding it")
	return cmd
}

// splitHostPort parses an allow target. The host may be a hostname/IP/domain, a
// "*.suffix" wildcard, or the "gateway" token for the host machine. The port is
// optional: a bare host with no ":" (e.g. "api.github.com" or "*.npmjs.org")
// defaults to 443 (HTTPS). An explicit "host:port" (e.g. "db.internal:5432" or
// "*.npmjs.org:8443") is split on the LAST colon. IPv6 literals are not supported.
func splitHostPort(value string) (string, int, error) {
	index := strings.LastIndex(value, ":")
	if index < 0 {
		// No port given — default to HTTPS. (A bare wildcard like "*.npmjs.org"
		// or a domain like "api.github.com" lands here.)
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

// splitPortPair parses "guest:host" (two ports).
func splitPortPair(value string) (int, int, error) {
	parts := strings.Split(value, ":")
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
