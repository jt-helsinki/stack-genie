package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// gatewayResult is the `ai gateway show`/`set`/`clear` payload: the machine-wide
// model-gateway address every workspace microVM routes through (arch §29.2).
// Configured is the raw AIPlatformHost (empty in standalone/local mode); Host,
// Port, and URL are the values resolved from it.
type gatewayResult struct {
	// Configured is the raw configured address (host or host:port); empty means
	// local standalone (host.microsandbox.internal).
	Configured string `json:"configured"`
	// Local is true when no AIPlatformHost is set (standalone/local fallback).
	Local bool   `json:"local"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	// URL is the base URL workspaces use to reach the gateway (http://host:port/v1).
	URL string `json:"url"`
}

// Human renders the resolved gateway so an operator can confirm where every
// microVM on this machine will route its model traffic.
func (result gatewayResult) Human() string {
	var builder strings.Builder
	if result.Local {
		builder.WriteString(ui.Label.Render("gateway:") + " " + ui.Muted.Render("local (standalone) —") + " ")
		builder.WriteString(ui.Value.Render("host.microsandbox.internal:" + strconv.Itoa(result.Port)))
		builder.WriteString("\n")
	} else {
		builder.WriteString(ui.Label.Render("gateway:") + " ")
		builder.WriteString(ui.Value.Render(result.Configured))
		builder.WriteString(" " + ui.Muted.Render("(client mode)") + "\n")
	}
	builder.WriteString(ui.Label.Render("microVMs route to:") + " ")
	builder.WriteString(ui.Value.Render(result.URL))
	return builder.String()
}

// newGatewayCmd builds `ai gateway` and its subcommands (CLI §gateway). The
// configured address is machine-wide (global config/runtime.yaml): it selects
// the model gateway every workspace microVM on this host routes through.
func newGatewayCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Configure the model gateway every workspace on this machine routes through",
		Long: "Configure the model gateway address every workspace microVM on this machine\n" +
			"routes through (machine-wide, config/runtime.yaml). Leave it unset for the\n" +
			"local standalone gateway (host.microsandbox.internal); set it to a remote\n" +
			"server (client mode) so every microVM reaches that server's LiteLLM.\n\n" +
			"The address is a bare host or host:port (NOT a URL); the default port is\n" +
			"18787 (the Headroom port). microVMs reach it at http://<host>:<port>/v1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help() // `ai gateway` with no subcommand prints help (§17.0)
		},
	}
	cmd.AddCommand(
		newGatewayShowCmd(emitter, exit),
		newGatewaySetCmd(emitter, exit),
		newGatewayClearCmd(emitter, exit),
	)
	return cmd
}

func newGatewayShowCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the configured model gateway and the URL microVMs will use",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info, err := runtime.Load()
			if err != nil {
				*exit = emitter.Failure("gateway.show", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if info == nil {
				*exit = emitter.Failure("gateway.show", output.Errorf(output.ExitMissingDep, "no runtime config yet — run `ai setup` first"))
				return nil
			}
			*exit = emitter.Success("gateway.show", resolveGatewayResult(info.AIPlatformHost))
			return nil
		},
	}
}

func newGatewaySetCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "set [host[:port]]",
		Short: "Route every workspace on this machine through a remote model gateway (client mode)",
		Long: "Set the machine-wide model gateway to a remote server (client mode): every\n" +
			"workspace microVM routes its model traffic through that server's LiteLLM.\n" +
			"The address is a bare host or host:port (NOT a URL); the default port is 18787.\n\n" +
			"Run on a terminal to be prompted for the address (pre-seeded with any value\n" +
			"you pass); pass it as an argument under --json/no TTY for non-interactive use.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// On a terminal always prompt, PRE-SEEDED with any address given on the
			// command line (the user confirms/edits). Under --json / no TTY the
			// positional arg is used directly and is required (§21, §1.8).
			address := ""
			if len(args) == 1 {
				address = strings.TrimSpace(args[0])
			}
			if interactive(emitter) {
				entered, err := promptText(
					"Remote gateway address (host or host:port)",
					"every workspace on this machine routes its model traffic through this server's LiteLLM",
					address,
					func(candidate string) error { return validateGatewayAddress(strings.TrimSpace(candidate)) },
				)
				if err != nil {
					*exit = emitter.Failure("gateway.set", err)
					return nil
				}
				address = entered
			} else if address == "" {
				*exit = emitter.Failure("gateway.set", output.Errorf(output.ExitInvalidInput,
					"provide a gateway address (e.g. `ai gateway set server:18787`)"))
				return nil
			}
			if err := validateGatewayAddress(address); err != nil {
				*exit = emitter.Failure("gateway.set", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}
			info, err := runtime.Load()
			if err != nil {
				*exit = emitter.Failure("gateway.set", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if info == nil {
				*exit = emitter.Failure("gateway.set", output.Errorf(output.ExitMissingDep, "no runtime config yet — run `ai setup` first"))
				return nil
			}
			info.AIPlatformHost = address
			if err := runtime.Persist(info); err != nil {
				*exit = emitter.Failure("gateway.set", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("gateway.set", resolveGatewayResult(info.AIPlatformHost))
			return nil
		},
	}
}

func newGatewayClearCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Clear the configured gateway and fall back to the local standalone gateway",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info, err := runtime.Load()
			if err != nil {
				*exit = emitter.Failure("gateway.clear", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if info == nil {
				*exit = emitter.Failure("gateway.clear", output.Errorf(output.ExitMissingDep, "no runtime config yet — run `ai setup` first"))
				return nil
			}
			info.AIPlatformHost = ""
			if err := runtime.Persist(info); err != nil {
				*exit = emitter.Failure("gateway.clear", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("gateway.clear", resolveGatewayResult(info.AIPlatformHost))
			return nil
		},
	}
}

// resolveGatewayResult builds the command payload from a configured AIPlatformHost.
func resolveGatewayResult(aiPlatformHost string) gatewayResult {
	host, port, url := runtime.ResolveGateway(aiPlatformHost)
	return gatewayResult{
		Configured: aiPlatformHost,
		Local:      strings.TrimSpace(aiPlatformHost) == "",
		Host:       host,
		Port:       port,
		URL:        url,
	}
}

// validateGatewayAddress checks <addr> is a plausible host or host:port — a
// non-empty host with no scheme/whitespace/slashes and, if present, a positive
// integer port. It is deliberately light (catch typos, not enforce RFC syntax).
func validateGatewayAddress(address string) error {
	if address == "" {
		return fmt.Errorf("gateway address is required — pass a host or host:port")
	}
	if strings.ContainsAny(address, " \t\r\n") {
		return fmt.Errorf("gateway address %q contains whitespace", address)
	}
	if strings.Contains(address, "://") {
		return fmt.Errorf("gateway address %q looks like a URL — pass just the host or host:port", address)
	}
	if strings.Contains(address, "/") {
		return fmt.Errorf("gateway address %q must not contain '/'", address)
	}
	host, portPart, hasPort := strings.Cut(address, ":")
	if host == "" {
		return fmt.Errorf("gateway address %q has an empty host", address)
	}
	if hasPort {
		port, err := strconv.Atoi(portPart)
		if err != nil || port <= 0 {
			return fmt.Errorf("gateway address %q has an invalid port (must be a positive integer)", address)
		}
	}
	return nil
}
