package cli

import (
	"errors"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/egress"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// newNetworkCmd builds `ai network`: the per-project nono egress policy —
// default posture plus an allow/deny domain list. Applied when the project's
// nono profile is (re)generated at the next `ai agent`/`ai shell`/`ai start` —
// there is no persistent sandbox to restart to pick up a change.
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
		errors.Is(err, egress.ErrInvalidHost),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// networkResult is the `ai network show` payload: the project's declared
// egress policy (from config.yaml).
type networkResult struct {
	Egress      string   `json:"egress"`
	AllowDomain []string `json:"allow_domain,omitempty"`
	DenyDomain  []string `json:"deny_domain,omitempty"`
}

// Human renders the declared egress policy.
func (result networkResult) Human() string {
	lines := []string{"  " + ui.Label.Render("egress:") + " " + ui.Value.Render(result.Egress)}
	if len(result.AllowDomain) == 0 {
		lines = append(lines, "  "+ui.Label.Render("allow:")+"  "+ui.Muted.Render("(none — only the model gateway is reachable in deny mode)"))
	} else {
		lines = append(lines, "  "+ui.Label.Render("allow:"))
		for _, host := range result.AllowDomain {
			lines = append(lines, "    - "+ui.Value.Render(host))
		}
	}
	if len(result.DenyDomain) > 0 {
		lines = append(lines, "  "+ui.Label.Render("deny:"))
		for _, host := range result.DenyDomain {
			lines = append(lines, "    - "+ui.Value.Render(host))
		}
	}
	return strings.Join(lines, "\n")
}

// networkResolve resolves the trailing optional [project] at args[index].
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
				Egress:      network.ResolvedEgress(),
				AllowDomain: network.AllowDomain,
				DenyDomain:  network.DenyDomain,
			})
			return nil
		},
	}
}

func newNetworkEgressCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "egress [deny|public] [project]",
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
			mode := firstArg(args)
			if interactive(emitter) {
				initial := mode
				if initial == "" {
					initial = "deny"
				}
				picked, err := promptChoice("Default egress posture", "",
					[]huh.Option[string]{
						huh.NewOption("deny — only the model gateway + allowed domains (most locked-down)", "deny"),
						huh.NewOption("public — open internet, private ranges still blocked", "public"),
					}, initial)
				if err != nil {
					*exit = emitter.Failure("network.egress", err)
					return nil
				}
				mode = picked
			} else if mode == "" {
				*exit = emitter.Failure("network.egress", output.Errorf(output.ExitInvalidInput,
					"egress mode required (deny|public), or run on a terminal to choose"))
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

// resolveAllowTarget resolves a domain for the allow/disallow commands: on a
// terminal it prompts (pre-seeded with any arg); off-terminal it requires the
// arg. Validation failures come back as exit-2 errors.
func resolveAllowTarget(emitter *output.Emitter, provided, prompt string) (string, error) {
	value := provided
	if interactive(emitter) {
		entered, err := promptText(
			prompt+" (domain, or a \"*.suffix\" wildcard)",
			"e.g. api.github.com or *.npmjs.org",
			provided,
			egress.ValidateHost,
		)
		if err != nil {
			return "", err
		}
		value = entered
	} else if value == "" {
		return "", output.Errorf(output.ExitInvalidInput, "provide a domain, or run on a terminal to enter it")
	}
	if err := egress.ValidateHost(value); err != nil {
		return "", output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	return value, nil
}

func newNetworkAllowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "allow [domain] [project]",
		Short: "Allow the workspace to reach a domain (prompts on a terminal, pre-seeded)",
		Long: "Allow an egress destination the workspace may reach directly. domain may be\n" +
			"a hostname/IP/domain, or a \"*.suffix\" wildcard (e.g. *.npmjs.org). Omit it\n" +
			"on a terminal to be prompted (pre-seeded with any value you pass). Revoke\n" +
			"with `ai network disallow`.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			host, err := resolveAllowTarget(emitter, firstArg(args), "Domain to allow")
			if err != nil {
				*exit = emitter.Failure("network.allow", err)
				return nil
			}
			if err := egress.Allow(root, host); err != nil {
				*exit = emitter.Failure("network.allow", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.allow", map[string]any{"host": host})
			return nil
		},
	}
}

func newNetworkDisallowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "disallow [domain] [project]",
		Short: "Clear a domain from the allow/deny lists (prompts on a terminal, pre-seeded)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := networkResolve(cmd, args, 1)
			if err != nil {
				*exit = emitter.Failure("network.disallow", mapEgressErr(err))
				return nil
			}
			host, err := resolveAllowTarget(emitter, firstArg(args), "Domain to clear")
			if err != nil {
				*exit = emitter.Failure("network.disallow", err)
				return nil
			}
			if err := egress.Clear(root, host); err != nil {
				*exit = emitter.Failure("network.disallow", mapEgressErr(err))
				return nil
			}
			*exit = emitter.Success("network.disallow", map[string]any{"host": host})
			return nil
		},
	}
}
