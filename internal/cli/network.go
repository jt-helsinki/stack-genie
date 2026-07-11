package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/egress"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
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
		newNetworkDisallowCmd(emitter, exit),
		newNetworkPublishCmd(emitter, exit),
		newNetworkUnpublishCmd(emitter, exit),
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
	lines := []string{ui.Heading.Render("declared (config.yaml):"), "  " + ui.Label.Render("egress:") + " " + ui.Value.Render(result.Egress)}
	if len(result.AllowHostServices) == 0 {
		lines = append(lines, "  "+ui.Label.Render("allow:")+"  "+ui.Muted.Render("(none — only the model gateway is reachable)"))
	} else {
		lines = append(lines, "  "+ui.Label.Render("allow:"))
		for _, service := range result.AllowHostServices {
			lines = append(lines, "    - "+ui.Value.Render(fmt.Sprintf("%s:%d", service.Host, service.Port)))
		}
	}
	if len(result.PublishPorts) > 0 {
		lines = append(lines, "  "+ui.Label.Render("publish (host→guest):"))
		for _, mapping := range result.PublishPorts {
			lines = append(lines, "    - "+ui.Value.Render(fmt.Sprintf("%d→%d", mapping.Host, mapping.Guest)))
		}
	}
	if result.InForce == nil {
		lines = append(lines, "", ui.Heading.Render("in force (live):")+" "+ui.Muted.Render("(workspace not running — showing declared policy only)"))
		return strings.Join(lines, "\n")
	}
	lines = append(lines,
		"",
		ui.Heading.Render("in force (live, on the running microVM):"),
		"  "+ui.Muted.Render("(applied policy — what WOULD be blocked, not a record of blocked connections)"),
		"  "+ui.Label.Render("default egress:")+" "+ui.Value.Render(result.InForce.DefaultEgress))
	if len(result.InForce.Rules) == 0 {
		lines = append(lines, "  "+ui.Label.Render("rules:")+"  "+ui.Muted.Render("(none)"))
	} else {
		lines = append(lines, "  "+ui.Label.Render("rules:"))
		for _, rule := range result.InForce.Rules {
			lines = append(lines, "    - "+ui.Value.Render(rule))
		}
	}
	if result.InForce.OnViolation != "" {
		lines = append(lines, "  "+ui.Label.Render("on violation:")+" "+ui.Value.Render(result.InForce.OnViolation))
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

// offerWorkspaceRestart, after a per-project network/config change that only takes
// effect when the microVM is (re)created, asks on a TTY whether to restart the
// workspace now to apply it — and does so. It is a no-op under --json / no-TTY (the
// change applies on the next `ai start`/`ai restart`) and when the workspace isn't
// currently running (there is nothing to re-apply against). It is best-effort: it
// runs AFTER the command has already reported success, so a restart decline or
// failure never changes the command's exit code.
func offerWorkspaceRestart(emitter *output.Emitter, root string) {
	if !interactive(emitter) {
		return
	}
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	if !manager.IsRunning(root) {
		return // not running: the change is picked up on the next start
	}
	restart, err := promptConfirmDefault(
		"Restart the workspace now to apply this change?",
		"The change takes effect when the workspace microVM is (re)created; restarting rebuilds and reboots it.",
		false,
	)
	if err != nil || !restart {
		_, _ = fmt.Fprintln(emitter.Err, ui.Muted.Render("Not restarted — the change applies on the next `ai restart`."))
		return
	}
	_, _ = fmt.Fprintln(emitter.Err, ui.Heading.Render("Restarting workspace")+ui.Muted.Render("…"))
	if _, err := manager.Restart(root); err != nil {
		_, _ = fmt.Fprintf(emitter.Err, "%s\n", ui.Failure.Render(ui.IconFail+" restart failed: "+err.Error()))
		return
	}
	_, _ = fmt.Fprintln(emitter.Err, ui.Success.Render(ui.IconOK+" workspace restarted"))
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
			offerWorkspaceRestart(emitter, root)
			return nil
		},
	}
}

// resolveAllowTarget resolves a host:port for the allow/disallow commands: on a
// terminal it prompts (pre-seeded with any arg); off-terminal it requires the arg.
// Parse/validation failures come back as exit-2 errors.
func resolveAllowTarget(emitter *output.Emitter, provided, prompt string) (string, int, error) {
	value := provided
	if interactive(emitter) {
		entered, err := promptText(
			prompt+" (host, host:port, '*.suffix' wildcard, or 'gateway')",
			"a bare host defaults to port 443 (e.g. api.github.com); use host:port for a specific port (e.g. gateway:5432)",
			provided,
			func(candidate string) error {
				_, _, validateErr := egress.SplitHostPort(strings.TrimSpace(candidate))
				return validateErr
			},
		)
		if err != nil {
			return "", 0, err
		}
		value = entered
	} else if value == "" {
		return "", 0, output.Errorf(output.ExitInvalidInput, "provide host:port, or run on a terminal to enter it")
	}
	host, port, err := egress.SplitHostPort(value)
	if err != nil {
		return "", 0, output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	return host, port, nil
}

// resolvePortPair resolves a guest:host pair for the publish/unpublish commands.
func resolvePortPair(emitter *output.Emitter, provided, prompt string) (int, int, error) {
	value := provided
	if interactive(emitter) {
		entered, err := promptText(
			prompt+" (guest:host)",
			"the guest port inside the workspace and the host port it is reachable at (e.g. 3000:3000)",
			provided,
			func(candidate string) error {
				_, _, validateErr := egress.SplitPortPair(strings.TrimSpace(candidate))
				return validateErr
			},
		)
		if err != nil {
			return 0, 0, err
		}
		value = entered
	} else if value == "" {
		return 0, 0, output.Errorf(output.ExitInvalidInput, "provide guest:host, or run on a terminal to enter the mapping")
	}
	guest, host, err := egress.SplitPortPair(value)
	if err != nil {
		return 0, 0, output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	return guest, host, nil
}

func newNetworkAllowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "allow [host[:port]] [project]",
		Short: "Allow the workspace to reach an external service (prompts on a terminal, pre-seeded)",
		Long: "Allow an egress destination the workspace may reach directly (a database,\n" +
			"Kafka broker, or a specific API/domain). host may be a hostname/IP/domain,\n" +
			"a \"*.suffix\" wildcard (e.g. *.npmjs.org), or \"gateway\" for a service on\n" +
			"the host machine. The port is optional and defaults to 443 (HTTPS), so a\n" +
			"bare domain like api.github.com allows it on 443. Omit the host on a terminal\n" +
			"to be prompted (pre-seeded with any value you pass). Revoke with\n" +
			"`ai network disallow`.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			host, port, err := resolveAllowTarget(emitter, firstArg(args), "Service to allow")
			if err != nil {
				*exit = emitter.Failure("network.allow", err)
				return nil
			}
			if err := egress.Allow(root, host, port); err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.allow", map[string]any{"host": host, "port": port})
			offerWorkspaceRestart(emitter, root)
			return nil
		},
	}
}

func newNetworkDisallowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "disallow [host[:port]] [project]",
		Short: "Revoke an allowed egress destination (prompts on a terminal, pre-seeded)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.disallow", mapEgressErr(err))
				return nil
			}
			host, port, err := resolveAllowTarget(emitter, firstArg(args), "Service to disallow")
			if err != nil {
				*exit = emitter.Failure("network.disallow", err)
				return nil
			}
			if err := egress.Deny(root, host, port); err != nil {
				*exit = emitter.Failure("network.disallow", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.disallow", map[string]any{"host": host, "port": port})
			offerWorkspaceRestart(emitter, root)
			return nil
		},
	}
}

func newNetworkPublishCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "publish [guest:host] [project]",
		Short: "Publish a workspace port to the host (prompts on a terminal, pre-seeded)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.publish", mapEgressErr(err))
				return nil
			}
			guest, host, err := resolvePortPair(emitter, firstArg(args), "Port mapping to publish")
			if err != nil {
				*exit = emitter.Failure("network.publish", err)
				return nil
			}
			if err := egress.Publish(root, guest, host); err != nil {
				*exit = emitter.Failure("network.publish", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.publish", map[string]any{"guest": guest, "host": host})
			offerWorkspaceRestart(emitter, root)
			return nil
		},
	}
}

func newNetworkUnpublishCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "unpublish [guest:host] [project]",
		Short: "Remove a published workspace port (prompts on a terminal, pre-seeded)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.unpublish", mapEgressErr(err))
				return nil
			}
			guest, host, err := resolvePortPair(emitter, firstArg(args), "Port mapping to remove")
			if err != nil {
				*exit = emitter.Failure("network.unpublish", err)
				return nil
			}
			if err := egress.Unpublish(root, host); err != nil {
				*exit = emitter.Failure("network.unpublish", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.unpublish", map[string]any{"guest": guest, "host": host})
			offerWorkspaceRestart(emitter, root)
			return nil
		},
	}
}

// splitHostPort parses an allow target. The host may be a hostname/IP/domain, a
// "*.suffix" wildcard, or the "gateway" token for the host machine. The port is
// optional: a bare host with no ":" (e.g. "api.github.com" or "*.npmjs.org")
// defaults to 443 (HTTPS). An explicit "host:port" (e.g. "db.internal:5432" or
// "*.npmjs.org:8443") is split on the LAST colon. IPv6 literals are not supported.

// splitPortPair parses "guest:host" (two ports).
