package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	goruntime "runtime"
	"time"

	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/spf13/cobra"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// newSetupCmd builds `ai setup` (CLI §2.1).
func newSetupCmd(em *output.Emitter, exit *int) *cobra.Command {
	var providerConfig string
	var upgrade bool
	var mode string
	var serverAddr string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install, configure, and start the platform host services (idempotent)",
		Long: "Install, configure, and start the platform host services (idempotent).\n\n" +
			"On a TTY (without --mode) the deployment role — and, for a client, the remote\n" +
			"server address — are prompted in a single form with back-navigation, so you can\n" +
			"step back to change the role before submitting.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			// On a TTY without an explicit --mode, ask which deployment role this
			// host plays (and, for a client, where the remote service tier lives).
			// --json/automation and non-TTY hosts stay non-interactive (default
			// standalone unless --mode was passed).
			interactive := !em.JSON && term.IsTerminal(os.Stdin.Fd())
			if mode == "" && interactive {
				selectedMode, selectedServer, err := promptDeploymentRole()
				if err != nil {
					*exit = em.Failure("setup", output.Errorf(output.ExitInvalidInput, "deployment role prompt: %s", err))
					return nil
				}
				mode, serverAddr = selectedMode, selectedServer
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
				if err := deps.Services.PullImages(em.Err, func(line string) { _, _ = fmt.Fprintln(em.Err, line) }); err != nil {
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
			if interactive && report.Runtime != nil && report.Runtime.Role != runtime.RoleClient {
				_, _ = fmt.Fprintln(em.Err, "\nSetup needs a credential — press Enter at the prompt to skip it.")
				promptLiteLLMUIPassword(em)
			}
			*exit = em.Success("setup", report, report.Warnings...)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerConfig, "provider-config", "",
		"point LiteLLM at a provider/endpoint config file")
	cmd.Flags().BoolVar(&upgrade, "upgrade", false,
		"re-pin service versions to this binary's defaults and re-reconcile")
	cmd.Flags().StringVar(&mode, "mode", "",
		"deployment role: standalone (default) | server | client (interactive prompt on a TTY)")
	cmd.Flags().StringVar(&serverAddr, "server", "",
		"client mode: address of the remote service tier to route to (host, host:port, or URL)")
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
			_, _ = fmt.Fprintf(em.Err, "Skipping %s — install it and re-run `ai setup`.\n", prereq.Name)
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

// promptDeploymentRole asks (on a TTY) which deployment role this host plays,
// defaulting to the persisted role (else standalone). When Client is chosen it
// also prompts for the remote service-tier address (defaulting to the persisted
// one). Both prompts live in ONE huh form (two groups) so huh's built-in
// back-navigation works: the user can step back from the server-address group to
// the role group and change their choice (see the back-navigation note in
// prompt.go). The second group is hidden unless the chosen role is client — huh
// re-evaluates the hide func as the user navigates, so picking Client reveals it
// and going back to pick a non-client role hides it again.
func promptDeploymentRole() (mode, serverAddr string, err error) {
	defaultRole := runtime.RoleStandalone
	defaultServer := ""
	if persisted, loadErr := runtime.Load(); loadErr == nil && persisted != nil {
		if persisted.Role != "" {
			defaultRole = persisted.Role
		}
		defaultServer = persisted.AIPlatformHost
	}

	mode = defaultRole
	serverAddr = defaultServer

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
			Title("Remote server address (host, host:port, or URL)").
			Value(&serverAddr).
			Validate(func(value string) error {
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf("a server address is required in client mode")
				}
				return nil
			}),
	).WithHideFunc(func() bool { return mode != runtime.RoleClient })

	if err := runForm(roleGroup, serverGroup); err != nil {
		return "", "", err
	}
	if mode != runtime.RoleClient {
		return mode, "", nil
	}
	return mode, strings.TrimSpace(serverAddr), nil
}

// promptLiteLLMUIPassword secures the LiteLLM admin UI: it asks for a password
// (hidden), generates a master key, and relaunches the LiteLLM container with
// both set (via the environment, never disk/argv). Skipped when the UI auth is
// already provided through the environment. Best-effort: a blank entry or a
// relaunch failure just prints a hint and continues. The chosen password is
// never echoed; the generated master key is shown once (it is also the API key).
func promptLiteLLMUIPassword(em *output.Emitter) {
	// Already supplied via the environment (the standard LiteLLM .env pattern)?
	// Then the container launch already picked them up — nothing to prompt.
	if os.Getenv("UI_PASSWORD") != "" && os.Getenv("LITELLM_MASTER_KEY") != "" {
		return
	}
	// Already secured on a previous run (the container carries a UI password)?
	// Don't re-prompt — that turned `ai setup` re-runs into a "hang".
	if setup.LiteLLMUISecured() {
		return
	}
	password, err := promptSecret("Set the LiteLLM admin UI password (leave blank to skip)", "", nil)
	if err != nil || password == "" {
		return
	}
	masterKey, err := generateMasterKey()
	if err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not generate a LiteLLM master key: %s\n", err)
		return
	}
	if err := setup.RelaunchLiteLLMWithAuth(password, masterKey); err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not secure the LiteLLM UI: %s\n", err)
		return
	}
	_, _ = fmt.Fprintf(em.Err,
		"LiteLLM admin UI secured — log in as %q at http://localhost:14000/ui\n"+
			"  master key (also the API key): %s\n"+
			"  Secrets are not stored on disk; to keep them across restarts, export them\n"+
			"  before `ai setup` / `ai services start`:\n"+
			"    export UI_PASSWORD='<the password you just set>' LITELLM_MASTER_KEY=%s\n",
		"admin", masterKey, masterKey)
}

// generateMasterKey returns a random LiteLLM master key (`sk-` + 48 hex chars).
func generateMasterKey() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(buffer), nil
}
