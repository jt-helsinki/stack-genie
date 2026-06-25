package cli

import (
	"fmt"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/spf13/cobra"
)

// domainResult is the `ai domain [name]` payload: the platform base domain the
// nginx UI subdomains hang off (litellm.<domain>, chat.<domain>,
// odysseus.<domain>). Configured is the raw runtime.yaml value (empty means the
// default); Domain is the resolved value; Default is true when unset.
type domainResult struct {
	// Configured is the raw configured domain; empty means the default fallback.
	Configured string `json:"configured"`
	// Domain is the resolved platform base domain (Configured or the default).
	Domain string `json:"domain"`
	// Default is true when no domain is configured (resolved to the default).
	Default bool `json:"default"`
}

// Human renders the resolved platform base domain and the UI subdomains it serves.
func (result domainResult) Human() string {
	var builder strings.Builder
	builder.WriteString("Platform domain: ")
	builder.WriteString(result.Domain)
	if result.Default {
		builder.WriteString(" (default)")
	}
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf(
		"UIs are served at litellm.%s, chat.%s, odysseus.%s (configure DNS/hosts accordingly).",
		result.Domain, result.Domain, result.Domain))
	return builder.String()
}

// newDomainCmd builds `ai domain [name]` (CLI §domain): show or set the platform
// base domain every nginx UI subdomain hangs off (litellm.<domain>, chat.<domain>,
// odysseus.<domain>). It is machine-wide (config/runtime.yaml); the default is
// aip.local for local/standalone and operators override it in server mode.
//
// On a terminal with a [name] it ALWAYS prompts, pre-seeded with the name (the
// user confirms/edits); with no arg it SHOWS the resolved domain. Under --json /
// no TTY a provided name is applied directly (invalid → exit 2); no arg reports
// the current domain. Missing runtime.yaml → exit 3 (run `ai setup`); persist/read
// failure → exit 4.
func newDomainCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "domain [name]",
		Short: "Show or set the platform base domain the UI subdomains hang off",
		Long: "Show or set the platform base domain (machine-wide, config/runtime.yaml). The\n" +
			"nginx UI subdomains hang off it: litellm.<domain>, chat.<domain>,\n" +
			"odysseus.<domain>. The default is aip.local for local/standalone; operators\n" +
			"override it in server mode.\n\n" +
			"With no argument it shows the resolved domain. Run on a terminal with a name\n" +
			"to be prompted (pre-seeded with it); pass it as an argument under --json / no\n" +
			"TTY for non-interactive use. A name is a DNS hostname (e.g. aip.example.com),\n" +
			"NOT a URL.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			info, err := runtime.Load()
			if err != nil {
				*exit = emitter.Failure("domain", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if info == nil {
				*exit = emitter.Failure("domain", output.Errorf(output.ExitMissingDep, "no runtime config yet — run `ai setup` first"))
				return nil
			}

			provided := ""
			if len(args) == 1 {
				provided = strings.TrimSpace(args[0])
			}
			// No name given → SHOW the resolved domain, change nothing (a bare
			// `ai domain` is show, never a prompt-to-set, on a TTY or otherwise).
			if provided == "" {
				*exit = emitter.Success("domain", domainResultFor(info))
				return nil
			}

			domain := provided
			if interactive(emitter) {
				entered, promptErr := promptText(
					"Platform base domain",
					"the nginx UI subdomains hang off this (litellm.<domain>, chat.<domain>, odysseus.<domain>)",
					domain,
					func(candidate string) error { return validateDomain(strings.TrimSpace(candidate)) },
				)
				if promptErr != nil {
					*exit = emitter.Failure("domain", promptErr)
					return nil
				}
				domain = strings.TrimSpace(entered)
			}
			if err := validateDomain(domain); err != nil {
				*exit = emitter.Failure("domain", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}
			info.Domain = domain
			if err := runtime.Persist(info); err != nil {
				*exit = emitter.Failure("domain", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("domain", domainResultFor(info))
			return nil
		},
	}
}

// domainResultFor builds the command payload from a runtime Info.
func domainResultFor(info *runtime.Info) domainResult {
	return domainResult{
		Configured: info.Domain,
		Domain:     info.ResolveDomain(),
		Default:    strings.TrimSpace(info.Domain) == "",
	}
}

// validateDomain checks <name> is a syntactically valid lowercase DNS hostname:
// one or more dot-separated labels, no scheme/path/whitespace, no leading/trailing
// dot, each label 1-63 chars of [a-z0-9-] not starting/ending with a hyphen. A
// dotted name like aip.local or aip.example.com is fine; a bare label (aip) is too.
func validateDomain(name string) error {
	if name == "" {
		return fmt.Errorf("domain is required — pass a DNS hostname (e.g. aip.example.com)")
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("domain %q contains whitespace", name)
	}
	if strings.Contains(name, "://") {
		return fmt.Errorf("domain %q looks like a URL — pass just the hostname", name)
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("domain %q must not contain '/'", name)
	}
	if name != strings.ToLower(name) {
		return fmt.Errorf("domain %q must be lowercase", name)
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("domain %q must not start or end with a dot", name)
	}
	if len(name) > 253 {
		return fmt.Errorf("domain %q is too long (max 253 characters)", name)
	}
	for _, label := range strings.Split(name, ".") {
		if err := validateDomainLabel(name, label); err != nil {
			return err
		}
	}
	return nil
}

// validateDomainLabel checks one dot-separated label of a hostname.
func validateDomainLabel(name, label string) error {
	if label == "" {
		return fmt.Errorf("domain %q has an empty label", name)
	}
	if len(label) > 63 {
		return fmt.Errorf("domain %q has a label longer than 63 characters", name)
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return fmt.Errorf("domain %q has a label that starts or ends with a hyphen", name)
	}
	for _, char := range label {
		isLower := char >= 'a' && char <= 'z'
		isDigit := char >= '0' && char <= '9'
		if !isLower && !isDigit && char != '-' {
			return fmt.Errorf("domain %q has an invalid character %q (use letters, digits, hyphens)", name, char)
		}
	}
	return nil
}
