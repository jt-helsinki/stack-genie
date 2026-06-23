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
