package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/spf13/cobra"
)

// interactive reports whether we can present a menu (a real TTY, not --json).
func interactive(emitter *output.Emitter) bool {
	return !emitter.JSON && term.IsTerminal(os.Stdin.Fd())
}

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
		Use:   "egress [deny|public|unrestricted] [project]",
		Short: "Set the default egress posture (prompts with the options if omitted)",
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
			mode := ""
			if len(args) >= 1 {
				mode = args[0]
			} else if interactive(emitter) {
				picked, cancelled, err := pickEgressMode()
				if err != nil || cancelled {
					*exit = emitter.Success("network.egress", map[string]any{"cancelled": true})
					return nil
				}
				mode = picked
			} else {
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

// pickEgressMode presents the egress postures with descriptions.
func pickEgressMode() (string, bool, error) {
	mode := "deny"
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Default egress posture").
			Options(
				huh.NewOption("deny — only the model gateway + allowed services (most locked-down)", "deny"),
				huh.NewOption("public — open internet, private ranges still blocked", "public"),
				huh.NewOption("unrestricted — allow everything (least safe)", "unrestricted"),
			).Value(&mode),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", true, nil
		}
		return "", false, err
	}
	return mode, false, nil
}

func newNetworkAllowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "allow <host:port> [project]",
		Short: "Allow the workspace to reach an external service (--remove to revoke)",
		Long: "Allow an egress destination the workspace may reach directly (a database,\n" +
			"Kafka broker, or a specific API/domain). host may be a hostname/IP/domain,\n" +
			"or \"gateway\" for a service on the host machine. Use --remove to revoke.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			var host string
			var port int
			if len(args) >= 1 {
				host, port, err = splitHostPort(args[0])
				if err != nil {
					*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			} else if remove {
				*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput, "--remove needs a host:port"))
				return nil
			} else if interactive(emitter) {
				var cancelled bool
				host, port, cancelled, err = pickAllowService()
				if err != nil || cancelled {
					*exit = emitter.Success("network.allow", map[string]any{"cancelled": true})
					return nil
				}
			} else {
				*exit = emitter.Failure("network.allow", output.Errorf(output.ExitInvalidInput,
					"host:port required, or run on a terminal to choose a service"))
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

// pickAllowService presents common services (with default ports) and lets the
// user confirm/edit the host and port.
func pickAllowService() (string, int, bool, error) {
	portStr := "5432"
	host := "gateway"
	presets := huh.NewSelect[string]().
		Title("Service to allow (sets a default port — edit below)").
		Options(
			huh.NewOption("PostgreSQL (5432)", "5432"),
			huh.NewOption("MySQL / MariaDB (3306)", "3306"),
			huh.NewOption("Redis (6379)", "6379"),
			huh.NewOption("Kafka (9092)", "9092"),
			huh.NewOption("MongoDB (27017)", "27017"),
			huh.NewOption("RabbitMQ / AMQP (5672)", "5672"),
			huh.NewOption("Elasticsearch (9200)", "9200"),
			huh.NewOption("HTTPS API (443)", "443"),
			huh.NewOption("Custom…", ""),
		).Value(&portStr)
	form := huh.NewForm(
		huh.NewGroup(presets),
		huh.NewGroup(
			huh.NewInput().Title("Host (hostname/IP/domain, or 'gateway' for the host machine)").Value(&host),
			huh.NewInput().Title("Port").Value(&portStr).Validate(func(value string) error {
				port, err := strconv.Atoi(value)
				if err != nil || port < 1 || port > 65535 {
					return fmt.Errorf("port must be 1–65535")
				}
				return nil
			}),
		),
	)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", 0, true, nil
		}
		return "", 0, false, err
	}
	port, _ := strconv.Atoi(portStr)
	if host == "" {
		host = "gateway"
	}
	return host, port, false, nil
}

func newNetworkPublishCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "publish [guest:host] [project]",
		Short: "Publish a workspace port to the host (prompts if omitted; --remove to revoke)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.publish", mapEgressErr(err))
				return nil
			}
			var guest, host int
			if len(args) >= 1 {
				guest, host, err = splitPortPair(args[0])
				if err != nil {
					*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "%s", err))
					return nil
				}
			} else if remove {
				*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput, "--remove needs a guest:host"))
				return nil
			} else if interactive(emitter) {
				var cancelled bool
				guest, host, cancelled, err = pickPublishPorts()
				if err != nil || cancelled {
					*exit = emitter.Success("network.publish", map[string]any{"cancelled": true})
					return nil
				}
			} else {
				*exit = emitter.Failure("network.publish", output.Errorf(output.ExitInvalidInput,
					"guest:host required, or run on a terminal to enter the ports"))
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

// pickPublishPorts prompts for the guest and host ports to publish.
func pickPublishPorts() (int, int, bool, error) {
	guestStr, hostStr := "3000", "3000"
	portValidator := func(value string) error {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("port must be 1–65535")
		}
		return nil
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Guest port (inside the workspace)").Value(&guestStr).Validate(portValidator),
		huh.NewInput().Title("Host port (reachable at localhost:<port>)").Value(&hostStr).Validate(portValidator),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return 0, 0, true, nil
		}
		return 0, 0, false, err
	}
	guest, _ := strconv.Atoi(guestStr)
	host, _ := strconv.Atoi(hostStr)
	return guest, host, false, nil
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
