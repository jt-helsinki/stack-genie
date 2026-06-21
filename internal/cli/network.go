package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
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
	)
	return cmd
}

func mapEgressErr(err error) error {
	switch {
	case errors.Is(err, egress.ErrInvalidMode),
		errors.Is(err, egress.ErrInvalidPort),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// networkResult is the `ai network show` payload.
type networkResult struct {
	Egress            string               `json:"egress"`
	AllowHostServices []config.HostService `json:"allow_host_services,omitempty"`
	PublishPorts      []config.PortMapping `json:"publish_ports,omitempty"`
}

// Human renders the egress policy readably.
func (result networkResult) Human() string {
	lines := []string{"egress: " + result.Egress}
	if len(result.AllowHostServices) == 0 {
		lines = append(lines, "allow:  (none — only the model gateway is reachable)")
	} else {
		lines = append(lines, "allow:")
		for _, service := range result.AllowHostServices {
			lines = append(lines, fmt.Sprintf("  - %s:%d", service.Host, service.Port))
		}
	}
	if len(result.PublishPorts) > 0 {
		lines = append(lines, "publish (host→guest):")
		for _, mapping := range result.PublishPorts {
			lines = append(lines, fmt.Sprintf("  - %d→%d", mapping.Host, mapping.Guest))
		}
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
			root, err := networkResolve(cmd, args, 0)
			if err != nil {
				*exit = emitter.Failure("network.show", mapEgressErr(err))
				return nil
			}
			network, err := egress.Get(root)
			if err != nil {
				*exit = emitter.Failure("network.show", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("network.show", networkResult{
				Egress:            network.ResolvedEgress(),
				AllowHostServices: network.AllowHostServices,
				PublishPorts:      network.PublishPorts,
			})
			return nil
		},
	}
}

func newNetworkEgressCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "egress <deny|public|unrestricted> [project]",
		Short: "Set the default egress posture",
		Args:  cobra.RangeArgs(1, 2),
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
			if err := egress.SetMode(root, args[0]); err != nil {
				*exit = emitter.Failure("network.egress", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.egress", map[string]any{"egress": args[0]})
			return nil
		},
	}
}

func newNetworkAllowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "allow <host:port> [project]",
		Short: "Allow the workspace to reach an external service (--remove to revoke)",
		Long: "Allow an egress destination the workspace may reach directly (a database,\n" +
			"Kafka broker, or a specific API/domain). host may be a hostname/IP/domain,\n" +
			"or \"gateway\" for a service on the host machine. Use --remove to revoke.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			host, port, err := splitHostPort(args[0])
			if err != nil {
				*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
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
		Use:   "publish <guest:host> [project]",
		Short: "Publish a workspace port to the host (--remove to revoke)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			guest, host, err := splitPortPair(args[0])
			if err != nil {
				*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.publish", mapEgressErr(err))
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

// splitHostPort parses "host:port" (host may be a domain/IP/"gateway").
func splitHostPort(value string) (string, int, error) {
	index := strings.LastIndex(value, ":")
	if index <= 0 || index == len(value)-1 {
		return "", 0, fmt.Errorf("expected host:port, got %q", value)
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
