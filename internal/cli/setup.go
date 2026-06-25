package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	goruntime "runtime"
	"slices"
	"time"

	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/envfile"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/uihosts"
	"github.com/spf13/cobra"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// newSetupCmd builds `ai setup` (CLI §2.1).
func newSetupCmd(em *output.Emitter, exit *int) *cobra.Command {
	var providerConfig string
	var upgrade bool
	var mode string
	var serverAddr string
	var optional string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install, configure, and start the platform host services (idempotent)",
		Long: "Install, configure, and start the platform host services (idempotent).\n\n" +
			"On a TTY, setup prompts for this host's configuration in a single form with\n" +
			"back-navigation: the deployment role and the remote server address (client\n" +
			"role). The --mode/--server flags are NOT required — they just pre-seed the\n" +
			"form's defaults. Under --json / no TTY the flags drive setup directly with no\n" +
			"prompt (so automation stays scriptable).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			// On a TTY, ALWAYS prompt for this host's configuration — deployment
			// role, the remote server address (client only), and the enabled
			// optional host services — in a single back-navigable form, every field
			// PRE-SEEDED from the --mode/--server/--optional flags (which set the
			// defaults but are never required, CLI §21). Under --json / no TTY the
			// flags drive it directly with no prompt: --mode (else standalone) and
			// --optional (else leave the persisted/default optional set untouched).
			interactive := !em.JSON && term.IsTerminal(os.Stdin.Fd())
			var optionalSet bool
			var optionalServices []string
			// serverDomain is the hostname clients/browsers reach a SERVER-role host
			// at — it drives the UI subdomains + DNS guidance. Empty for
			// standalone/client; the server branch prompts for it (default localhost)
			// on a TTY, or uses the persisted/default under --json / no TTY.
			var serverDomain string
			if interactive {
				selectedMode, selectedServer, selectedOptional, selectedDomain, promptErr := promptSetupConfig(mode, serverAddr, optional)
				if promptErr != nil {
					*exit = em.Failure("setup", promptErr)
					return nil
				}
				mode, serverAddr = selectedMode, selectedServer
				optionalServices, optionalSet = selectedOptional, true
				serverDomain = selectedDomain
			} else {
				var optErr error
				optionalSet, optionalServices, optErr = optionalServicesFromFlag(optional)
				if optErr != nil {
					*exit = em.Failure("setup", optErr)
					return nil
				}
				// Non-interactive server hostname: keep the persisted domain (or the
				// default) — no prompt. Run does a load-modify-save, so leaving
				// Options.Domain empty preserves a domain set via `ai domain`.
				serverDomain = nonInteractiveServerDomain(mode)
			}
			// Stream step-by-step progress to stderr so setup doesn't look hung
			// during the (several-second) container bring-up. On a TTY this is a
			// live stepper checklist; on a non-TTY (but non-JSON) we keep the plain
			// stderr streaming; under --json/automation it stays quiet (progress
			// isn't part of the envelope).
			opts := setup.Options{
				ProviderConfig: providerConfig,
				Upgrade:        upgrade,
				Mode:           mode,
				ServerAddr:     serverAddr,
				Optional:       optionalServices,
				OptionalSet:    optionalSet,
				Domain:         serverDomain,
			}
			// Preflight: check prerequisites FIRST — before pulling any images. On a
			// TTY, missing-but-auto-installable prerequisites are offered for install
			// (the installer streams its native output); declining, or a missing
			// instruct-only prerequisite (e.g. Docker Desktop on macOS, virtualization),
			// prints the instructions + web URL and exits 3. Under --json / no-TTY this
			// stays non-interactive: a missing blocking prerequisite is exit 3 with the
			// instructions message (no prompt, no install, no image pull).
			if proceed := preflightPrerequisites(em, exit, opts, deps, interactive); !proceed {
				return nil
			}
			// Pre-pull the service-tier images with STREAMED native progress before
			// the reconcile, on a human (non-JSON) run that actually runs the service
			// tier (standalone/server, not client). Each ensure* launches a container
			// with `docker run -d`, whose implicit pull output the prober CAPTURES
			// (invisible) — so a multi-GB first-run pull (e.g. ollama) looks hung.
			// Pre-pulling renders docker's native progress bars to the terminal; the
			// later `docker run -d` then finds the image present and returns instantly.
			// This MUST be sequential with (and before) the bubbletea RunSteps below —
			// native docker progress and a bubbletea program cannot both own the
			// terminal at once. Under --json we skip it (keep stdout a clean envelope;
			// the reconcile's implicit pull stays as-is). On error we warn and
			// continue — the reconcile re-pulls anything still missing.
			if !em.JSON && mode != runtime.RoleClient {
				_, _ = fmt.Fprintln(em.Err, "Pulling container images (first run may take a few minutes)…")
				// Resolve the effective enabled optional-service set so the pre-pull
				// includes a disabled optional service's (large) images ONLY when it
				// is enabled (same precedence as the reconcile: explicit choice >
				// persisted > first-run default).
				persisted, _ := runtime.Load()
				enabledOptional := setup.ResolveOptional(opts, persisted)
				if err := deps.Services.PullImages(enabledOptional, em.Err, func(line string) { _, _ = fmt.Fprintln(em.Err, line) }); err != nil {
					_, _ = fmt.Fprintf(em.Err, "warning: image pre-pull incomplete (%s) — continuing; the reconcile will retry\n", err)
				}
			}
			var report *setup.Report
			var err error
			if ui.Enabled(em) {
				err = ui.RunSteps(em.Err, "Setting up the AI Development Platform", func(emit func(step string)) error {
					deps.Progress = emit
					var runErr error
					report, runErr = setup.Run(opts, deps)
					return runErr
				})
			} else {
				if !em.JSON {
					_, _ = fmt.Fprintln(em.Err, "Setting up the AI Development Platform…")
					deps.Progress = func(line string) { _, _ = fmt.Fprintln(em.Err, line) }
				}
				report, err = setup.Run(opts, deps)
			}
			if err != nil {
				*exit = em.Failure("setup", err) // err is *output.Error (carries the exit code)
				return nil
			}
			// Interactive credential prompts — only on a real TTY (not
			// --json/automation), so scripted/JSON setup stays non-interactive. The
			// LiteLLM admin-UI password only matters where LiteLLM runs locally
			// (standalone/server), so it is skipped in client mode.
			if report.Runtime != nil && report.Runtime.Role != runtime.RoleClient {
				setupLiteLLMUIPassword(em, interactive, report.Runtime.Role, report.Runtime.ResolveDomain())
			}
			// Domain wiring: standalone points the UI subdomains at 127.0.0.1 in
			// /etc/hosts (with consent + sudo, else a manual block); server prints the
			// DNS/cert operator contract (no /etc/hosts editing). Best-effort — neither
			// fails setup.
			syncUISubdomains(em, interactive, report.Runtime)
			*exit = em.Success("setup", report, report.Warnings...)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerConfig, "provider-config", "",
		"point LiteLLM at a provider/endpoint config file")
	cmd.Flags().BoolVar(&upgrade, "upgrade", false,
		"re-pin service versions to this binary's defaults and re-reconcile")
	cmd.Flags().StringVar(&mode, "mode", "",
		"deployment role: standalone (default) | server | client; seeds the role prompt on a TTY")
	cmd.Flags().StringVar(&serverAddr, "server", "",
		"client mode: address of the remote service tier to route to (host or host:port); seeds the prompt on a TTY")
	cmd.Flags().StringVar(&optional, "optional", "",
		"comma-separated opt-in host services to enable, or \"none\" to disable them all; seeds the checkbox on a TTY (there are currently no optional host services, so this is a no-op)")
	return cmd
}

// preflightPrerequisites checks the host prerequisites BEFORE any image pre-pull
// or the setup.Run reconcile (goal: never print "Pulling images…" when a
// prerequisite is missing). It returns true when setup should proceed (everything
// satisfied) and false when it set *exit and the RunE should return.
//
//   - Non-interactive (--json or no TTY): a missing blocking prerequisite → the
//     instructions message at exit 3 (no prompt, no install, no image pull).
//   - Interactive (TTY, not --json): each missing blocking prerequisite, in order:
//   - auto-installable (InstallCommand != "") → prompt (default Yes, showing the
//     exact command). Yes runs the installer streaming to stderr, then re-scans;
//     still missing → exit 3 with instructions. No → instructions + exit 3.
//   - instruct-only (InstallCommand == "") → print instructions + URL, exit 3.
//
// The installer runs here (sequential, outside the bubbletea stepper) like the
// image pre-pull — native curl|sh output and a bubbletea program cannot both own
// the terminal at once.
func preflightPrerequisites(em *output.Emitter, exit *int, opts setup.Options, deps setup.Deps, interactive bool) bool {
	role, err := setup.RoleFor(opts)
	if err != nil {
		*exit = em.Failure("setup", err)
		return false
	}
	missing := blockingMissing(deps, role)
	if len(missing) == 0 {
		return true
	}

	// Non-interactive: no prompts, no installs — just the actionable error (exit 3/4).
	if !interactive {
		*exit = em.Failure("setup", setup.PrerequisiteError(missing))
		return false
	}

	// Interactive: resolve each missing blocking prerequisite in order.
	for _, prereq := range missing {
		if prereq.InstallCommand == "" {
			// No clean auto-installer on this OS (Docker Desktop on macOS, a
			// host-capability gap, …) — instruct only and stop.
			printPrerequisiteInstructions(em, prereq)
			*exit = em.Failure("setup", setup.PrerequisiteError([]setup.Prerequisite{prereq}))
			return false
		}
		install, promptErr := promptConfirmDefault(
			"Install "+prereq.Name+"?",
			"Runs: "+prereq.InstallCommand+"  ("+prereq.Detail+")",
			true,
		)
		if promptErr != nil {
			*exit = em.Failure("setup", promptErr)
			return false
		}
		if !install {
			_, _ = fmt.Fprintf(em.Err, "skipping %s — install it and re-run `ai setup`\n", prereq.Name)
			printPrerequisiteInstructions(em, prereq)
			*exit = em.Failure("setup", setup.PrerequisiteError([]setup.Prerequisite{prereq}))
			return false
		}
		_, _ = fmt.Fprintf(em.Err, "Installing %s: %s\n", prereq.Name, prereq.InstallCommand)
		if installErr := deps.Services.InstallPrerequisite(prereq, em.Err); installErr != nil {
			_, _ = fmt.Fprintf(em.Err, "warning: installer for %s failed: %s\n", prereq.Name, installErr)
		}
	}

	// Re-scan after the install attempts; anything still missing stops setup.
	if stillMissing := blockingMissing(deps, role); len(stillMissing) > 0 {
		for _, prereq := range stillMissing {
			printPrerequisiteInstructions(em, prereq)
		}
		*exit = em.Failure("setup", setup.PrerequisiteError(stillMissing))
		return false
	}
	return true
}

// blockingMissing returns the missing BLOCKING prerequisites for the role (the
// non-blocking gaps are left to setup.Run, which surfaces them as warnings).
func blockingMissing(deps setup.Deps, role string) []setup.Prerequisite {
	var blocking []setup.Prerequisite
	for _, prereq := range setup.MissingPrerequisites(deps, role) {
		if prereq.Blocking {
			blocking = append(blocking, prereq)
		}
	}
	return blocking
}

// printPrerequisiteInstructions writes a missing prerequisite's manual-install
// hint + web URL to stderr (reused for declined / instruct-only / still-missing).
func printPrerequisiteInstructions(em *output.Emitter, prereq setup.Prerequisite) {
	_, _ = fmt.Fprintf(em.Err, "%s must be installed manually:\n", prereq.Name)
	if prereq.Suggestion != "" {
		_, _ = fmt.Fprintf(em.Err, "  how to install: %s\n", prereq.Suggestion)
	}
	if prereq.DocsURL != "" {
		_, _ = fmt.Fprintf(em.Err, "  web:            %s\n", prereq.DocsURL)
	}
}

// promptSetupConfig collects this host's setup configuration on a TTY in ONE huh
// form (so huh's built-in back-navigation works across all of it): the deployment
// role, the remote server address (client only), and the enabled optional host
// services (standalone/server only). Every field is PRE-SEEDED from the
// command-line flags, falling back to the persisted choice, then the first-run
// default — so --mode/--server/--optional set defaults without being required
// (CLI §21). Known choices are presented as a Select / checkbox (not free text),
// and the free-text server address is validated (reusing validateGatewayAddress).
//
// The hide funcs are re-evaluated by huh as the user navigates: the server-address
// group is shown only for the client role, the server-hostname group only for the
// server role, and the optional-tools group only for standalone/server (a client
// runs no service tier here), so stepping back to change the role reveals/hides the
// right follow-up fields.
//
// The returned serverDomain is the hostname clients/browsers reach a SERVER-role
// host at (it drives the UI subdomains + DNS guidance); it is "" for
// standalone/client (those use their own defaults and are not newly prompted).
func promptSetupConfig(seedMode, seedServer, seedOptional string) (mode, serverAddr string, optional []string, serverDomain string, err error) {
	persisted, _ := runtime.Load()

	// Role seed: flag > persisted > standalone.
	mode = strings.TrimSpace(seedMode)
	if mode == "" {
		if persisted != nil && persisted.Role != "" {
			mode = persisted.Role
		} else {
			mode = runtime.RoleStandalone
		}
	}
	// Remote-server-address seed: flag > persisted.
	serverAddr = strings.TrimSpace(seedServer)
	if serverAddr == "" && persisted != nil {
		serverAddr = persisted.AIPlatformHost
	}
	// Server-hostname seed: the persisted domain if set, else localhost (the
	// clients/browsers default). Standalone keeps its aip.local default and is NOT
	// newly prompted, so this is only surfaced for the server role.
	serverDomain = defaultServerDomain(persisted)
	// Optional-services seed (the pre-checked boxes): flag > persisted > first-run
	// default. A bad --optional name is exit 2 even on a TTY (validate inputs).
	var preChecked []string
	switch {
	case strings.TrimSpace(seedOptional) != "":
		parsed, parseErr := parseOptionalFlag(seedOptional)
		if parseErr != nil {
			return "", "", nil, "", parseErr
		}
		preChecked = parsed
	case persisted != nil && persisted.OptionalServices != nil:
		preChecked = persisted.OptionalServices
	default:
		preChecked = setup.DefaultOptionalServices()
	}
	selectedOptional := append([]string(nil), preChecked...)

	roleGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Deployment role for this host").
			Description("standalone: full stack here · server: shared service tier only (0.0.0.0) · client: workspaces only, route to a remote server").
			Options(
				huh.NewOption("Standalone — run the full service tier + workspaces here", runtime.RoleStandalone),
				huh.NewOption("Server — run only the shared service tier (other machines connect)", runtime.RoleServer),
				huh.NewOption("Client — run only workspaces; route to a remote server", runtime.RoleClient),
			).
			Value(&mode),
	)

	serverGroup := huh.NewGroup(
		huh.NewInput().
			Title("Remote server address (host or host:port)").
			Description("The machine running the shared service tier (a `server`-role host).").
			Value(&serverAddr).
			Validate(validateGatewayAddress),
	).WithHideFunc(func() bool { return mode != runtime.RoleClient })

	// Server-hostname group (server role only): the name clients + browsers reach
	// THIS server at — it drives the UI subdomains (litellm.<host> …) + DNS
	// guidance. Validated with the shared `ai domain` hostname validator.
	serverHostnameGroup := huh.NewGroup(
		huh.NewInput().
			Title("Server hostname / domain (how clients and browsers reach this server)").
			Description("The UI subdomains hang off this (litellm.<host>). Default localhost only resolves on this machine.").
			Value(&serverDomain).
			Validate(func(candidate string) error { return validateDomain(strings.TrimSpace(candidate)) }),
	).WithHideFunc(func() bool { return mode != runtime.RoleServer })

	// The optional-tools group is shown only when there ARE optional host services
	// to offer. There are none today (Open WebUI moved to a per-workspace in-VM app
	// and Odysseus was removed), so it is always hidden — the mechanism is retained
	// for future host optional services.
	hasOptional := len(setup.OptionalServiceNames()) > 0
	optionalGroup := huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Optional tools").
			Description(optionalServicesWarning).
			Options(optionalServiceOptions()...).
			Value(&selectedOptional),
	).WithHideFunc(func() bool { return !hasOptional || mode == runtime.RoleClient })

	if formErr := runForm(roleGroup, serverGroup, serverHostnameGroup, optionalGroup); formErr != nil {
		return "", "", nil, "", formErr
	}
	switch mode {
	case runtime.RoleClient:
		// The optional group was hidden; selectedOptional is the untouched seed —
		// return it so a later switch to standalone doesn't clobber the choice. A
		// client has no local UIs, so no server domain.
		return mode, strings.TrimSpace(serverAddr), selectedOptional, "", nil
	case runtime.RoleServer:
		// Thread the chosen server hostname through to Options.Domain.
		return mode, "", selectedOptional, strings.TrimSpace(serverDomain), nil
	default:
		// Standalone: keep the aip.local default (Domain empty → preserved/default).
		return mode, "", selectedOptional, "", nil
	}
}

// defaultServerDomain is the server-hostname seed: the persisted domain when set,
// else "localhost" (the address clients/browsers reach the server at by default).
func defaultServerDomain(persisted *runtime.Info) string {
	if persisted != nil {
		if domain := strings.TrimSpace(persisted.Domain); domain != "" {
			return domain
		}
	}
	return "localhost"
}

// nonInteractiveServerDomain is the server hostname used under --json / no TTY: for
// a server role, the persisted domain (else "" so Run's load-modify-save preserves
// or defaults it — never clobbering a domain set via `ai domain`); for any other
// role, "" (standalone keeps aip.local, a client has no local UIs).
func nonInteractiveServerDomain(mode string) string {
	if strings.TrimSpace(mode) != runtime.RoleServer {
		return ""
	}
	persisted, _ := runtime.Load()
	if persisted != nil {
		return strings.TrimSpace(persisted.Domain)
	}
	return ""
}

// optionalServicesFromFlag resolves the enabled optional-service set from the
// --optional flag alone (the non-interactive path: --json / no TTY). It returns
// (set, services): set reports whether an explicit choice was made, so setup.Run
// can tell a deliberate "none" from an unspecified set. An empty flag → set=false
// (keep the persisted/default); "none" → set=true, empty; a CSV → set=true with
// every name validated against the optional-service universe (unknown → exit 2).
func optionalServicesFromFlag(flagValue string) (bool, []string, error) {
	if strings.TrimSpace(flagValue) == "" {
		return false, nil, nil
	}
	selected, err := parseOptionalFlag(flagValue)
	if err != nil {
		return false, nil, err
	}
	return true, selected, nil
}

// parseOptionalFlag turns the --optional CSV into a validated service list. The
// sentinel "none" (case-insensitive) yields an empty set (disable them all); any
// unknown name is exit 2.
func parseOptionalFlag(flagValue string) ([]string, error) {
	if strings.EqualFold(strings.TrimSpace(flagValue), "none") {
		return []string{}, nil
	}
	universe := setup.OptionalServiceNames()
	var selected []string
	for _, part := range strings.Split(flagValue, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !slices.Contains(universe, name) {
			return nil, output.Errorf(output.ExitInvalidInput,
				"unknown optional service %q (one of %v, or \"none\")", name, universe)
		}
		selected = append(selected, name)
	}
	return selected, nil
}

// optionalServicesWarning is the SECURITY WARNING shown on the optional-tools
// checkbox: opt-in host services run OUTSIDE the workspace microVM sandbox. There
// are currently no optional host services, so the checkbox is not shown — this is
// retained for future host optional services.
const optionalServicesWarning = "SECURITY WARNING: these tools run OUTSIDE the workspace microVM sandbox, on the host, " +
	"with elevated privileges — they are a security risk and are not isolated like workspaces. " +
	"Leave them unchecked unless you need them."

// optionalServiceOptions builds the checkbox options for the optional host
// services, labelling each with its human hint (OptionalServiceLabel), in the
// canonical OptionalServiceNames order.
func optionalServiceOptions() []huh.Option[string] {
	universe := setup.OptionalServiceNames()
	options := make([]huh.Option[string], 0, len(universe))
	for _, name := range universe {
		label := name
		if hint := setup.OptionalServiceLabel(name); hint != "" {
			label = name + " — " + hint
		}
		options = append(options, huh.NewOption(label, name))
	}
	return options
}

// setupLiteLLMUIPassword applies this host's role-based LiteLLM admin-UI auth
// policy at the end of `ai setup` (runtime.RequireUIAuth):
//
//   - SERVER → the password is REQUIRED (the gateway binds 0.0.0.0 and is
//     network-exposed). On a TTY we loop until a non-empty password is entered;
//     non-interactively (or if the user keeps aborting) we GENERATE one rather
//     than leave the server open. The short-circuits below (already secured /
//     env-provided) still apply — those already secure it.
//   - STANDALONE / CLIENT → we do NOT prompt at all (open local access; a user
//     can opt into a password later with `ai litellm password`).
//
// It returns having either secured the UI or decided not to; it never fails
// setup (best-effort), and reuses secureLiteLLMUI for the relaunch + persist
// offer shared with `ai litellm password`.
func setupLiteLLMUIPassword(em *output.Emitter, interactive bool, role, domain string) {
	// Already supplied via the environment (the standard LiteLLM .env pattern, or
	// auto-loaded from ~/.ai-platform.env)? The container launch already picked
	// them up — nothing to do, for any role.
	if os.Getenv("UI_PASSWORD") != "" && os.Getenv("LITELLM_MASTER_KEY") != "" {
		return
	}
	// Already secured on a previous run (the container carries a UI password)?
	// Don't re-prompt — that turned `ai setup` re-runs into a "hang".
	if setup.LiteLLMUISecured() {
		return
	}

	if !liteLLMPasswordRequiredAtSetup(role) {
		// standalone/client: open access. Don't prompt — `ai litellm password`
		// lets a user opt into one later.
		return
	}

	// Server: a password is REQUIRED. Obtain a non-empty one.
	password := serverLiteLLMPassword(em, interactive)
	if password == "" {
		// Could not obtain a password (e.g. a generation failure was warned about).
		return
	}
	secureLiteLLMUI(em, interactive, password, domain)
}

// liteLLMPasswordRequiredAtSetup is the pure role decision for whether `ai setup`
// must collect a LiteLLM admin-UI password: ONLY the server role (its gateway is
// network-exposed on 0.0.0.0). standalone/client run open access (a user opts in
// later via `ai litellm password`). It is runtime.RequireUIAuth at the setup
// site, factored out so the decision is unit-testable without the live container.
func liteLLMPasswordRequiredAtSetup(role string) bool {
	return runtime.RequireUIAuth(role)
}

// serverLiteLLMPassword obtains a non-empty LiteLLM admin-UI password for the
// server role. On a TTY it loops until the user enters a non-empty value (a
// server gateway must not be left open). Non-interactively — or if interactive
// entry keeps failing/aborting — it GENERATES a strong random password so the
// server is never left unauthenticated. Returns "" only when even generation
// fails (warned to the caller).
func serverLiteLLMPassword(em *output.Emitter, interactive bool) string {
	if interactive {
		_, _ = fmt.Fprintln(em.Err, "\nServer mode: the LiteLLM admin UI is network-exposed and MUST have a password.")
		for attempt := 0; attempt < 3; attempt++ {
			password, err := promptSecret("Set the LiteLLM admin UI password (required)", "", nil)
			if err == nil && password != "" {
				return password
			}
			_, _ = fmt.Fprintln(em.Err, "a password is required in server mode — please enter one")
		}
		_, _ = fmt.Fprintln(em.Err, "no password entered — generating a random one so the server is not left open")
	}
	generated, err := generateMasterKey()
	if err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not generate a LiteLLM admin password: %s\n", err)
		return ""
	}
	if !interactive {
		_, _ = fmt.Fprintln(em.Err, "server mode: generated a random LiteLLM admin UI password (shown below)")
	}
	return generated
}

// secureLiteLLMUI relaunches LiteLLM with the given admin-UI password (reusing the
// running container's master key, else minting one), prints the login details
// once, then offers to persist the secrets to ~/.ai-platform.env. It is the
// shared securing path for `ai setup` (server role) and `ai litellm password`.
// Best-effort: a master-key/relaunch failure prints a warning and returns.
func secureLiteLLMUI(em *output.Emitter, interactive bool, password, domain string) {
	masterKey := setup.CurrentLiteLLMMasterKey()
	if masterKey == "" {
		generated, err := generateMasterKey()
		if err != nil {
			_, _ = fmt.Fprintf(em.Err, "warning: could not generate a LiteLLM master key: %s\n", err)
			return
		}
		masterKey = generated
	}
	if err := setup.RelaunchLiteLLMWithAuth(password, masterKey); err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not secure the LiteLLM UI: %s\n", err)
		return
	}
	_, _ = fmt.Fprintf(em.Err,
		"LiteLLM admin UI secured — log in as %q at http://litellm.%s:18787/ui\n"+
			"  (reachable once the /etc/hosts or DNS step maps litellm.%s to this host)\n"+
			"  master key (also the API key): %s\n",
		"admin", domain, domain, masterKey)
	offerPersistLiteLLMSecrets(em, interactive, password, masterKey)
}

// offerPersistLiteLLMSecrets offers (on a TTY) to save the LiteLLM UI password +
// master key to ~/.ai-platform.env, a 0600 file the `ai` CLI auto-loads at
// startup, so they persist across restarts without the user editing their shell
// rc. On YES it writes the file and confirms the path; on NO / non-TTY it falls
// back to printing the manual `export …` block (the previous behaviour). Shared
// by `ai setup` (server) and `ai litellm password`.
func offerPersistLiteLLMSecrets(em *output.Emitter, interactive bool, password, masterKey string) {
	if interactive {
		save, err := promptConfirmDefault(
			"Save these to ~/.ai-platform.env so they persist across restarts?",
			"writes a 0600 file that `ai` loads automatically",
			true,
		)
		if err == nil && save {
			if writeErr := envfile.Write(map[string]string{
				"UI_PASSWORD":        password,
				"LITELLM_MASTER_KEY": masterKey,
			}); writeErr != nil {
				_, _ = fmt.Fprintf(em.Err, "warning: could not write the env file: %s\n", writeErr)
			} else {
				path, _ := envfile.Path()
				_, _ = fmt.Fprintf(em.Err,
					"Saved to %s (loaded automatically by ai; add `source ~/.ai-platform.env` "+
						"to your shell rc if other tools need these)\n", path)
				return
			}
		}
	}
	// No / non-TTY / write failure: print the manual export block to keep them.
	_, _ = fmt.Fprintf(em.Err,
		"  Secrets are not stored on platform disk; to keep them across restarts, export\n"+
			"  them before `ai setup` / `ai services start`:\n"+
			"    export UI_PASSWORD='<the password you just set>' LITELLM_MASTER_KEY=%s\n",
		masterKey)
}

// syncUISubdomains wires the platform UI subdomains for this host's role after a
// successful setup. Standalone: point litellm.<domain> at
// 127.0.0.1 in /etc/hosts — on a TTY with consent (the write needs sudo), else
// print the exact block to add manually. Server: print the DNS + TLS operator
// contract (no /etc/hosts editing). Client: nothing (no local UIs). It is
// best-effort and never fails setup.
func syncUISubdomains(em *output.Emitter, interactive bool, info *runtime.Info) {
	if info == nil {
		return
	}
	domain := info.ResolveDomain()
	switch info.Role {
	case runtime.RoleClient:
		return
	case runtime.RoleServer:
		_, _ = fmt.Fprintln(em.Err)
		_, _ = fmt.Fprint(em.Err, uihosts.ServerGuidance(domain))
		_, _ = fmt.Fprintln(em.Err)
		_, _ = fmt.Fprint(em.Err, uihosts.ServerCredentialsGuide(domain))
		if domain == "localhost" {
			_, _ = fmt.Fprintln(em.Err,
				"Note: the server hostname is localhost — the UI subdomains "+
					"(litellm.localhost, …) resolve only ON this server (or wherever DNS / "+
					"/etc/hosts maps them). Set a real hostname with `ai domain <name>` (and "+
					"the DNS records above) so clients and browsers can reach them.")
		}
	default: // standalone (and the empty role)
		action, _ := uihosts.SyncHosts(uihosts.HostsSync{
			Path:        uihosts.DefaultHostsPath,
			Domain:      domain,
			Interactive: interactive,
			Out:         em.Err,
			Consent: func(prompt string) (bool, error) {
				return promptConfirmDefault(prompt, "writes a managed block to /etc/hosts; you may be asked for your sudo password", true)
			},
		})
		if action == uihosts.HostsWritten {
			_, _ = fmt.Fprintf(em.Err, "Updated /etc/hosts — the platform UI resolves at litellm.%s on :18787\n", domain)
		}
	}
}

// generateMasterKey returns a random LiteLLM master key (`sk-` + 48 hex chars).
func generateMasterKey() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(buffer), nil
}
