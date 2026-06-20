package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/spf13/cobra"
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// newSetupCmd builds `ai setup` (CLI §2.1).
func newSetupCmd(em *output.Emitter, exit *int) *cobra.Command {
	var providerConfig string
	var upgrade bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install, configure, and start the platform host services (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
			report, err := setup.Run(setup.Options{ProviderConfig: providerConfig, Upgrade: upgrade}, deps)
			if err != nil {
				*exit = em.Failure("setup", err) // err is *output.Error (carries the exit code)
				return nil
			}
			// Interactive credential prompts — only on a real TTY (not
			// --json/automation), so scripted/JSON setup stays non-interactive.
			// Announce them clearly so they aren't mistaken for a hang after the
			// dependency installers' noisy output.
			if !em.JSON && term.IsTerminal(os.Stdin.Fd()) {
				_, _ = fmt.Fprintln(em.Err, "\nSetup needs a couple of credentials — press Enter at a prompt to skip it.")
				if report.GatewayConfigCreated {
					promptClawPatrolDashboardPassword(em)
				}
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
	return cmd
}

// promptClawPatrolDashboardPassword asks for the ClawPatrol dashboard password
// (hidden input) and applies it via `clawpatrol gateway --set-dashboard-password`.
// That command upserts the password and then RUNS the gateway server in the
// foreground, so it is started **detached** (its own session, output to
// ~/.clawpatrol/gateway.log) — otherwise `ai setup` would block forever. The
// gateway then keeps running after setup exits; registering it as a managed OS
// service is the hardware-bring-up end-state. The password is never logged or
// written to platform disk. Best-effort: a blank entry, a missing clawpatrol
// binary, or a start failure just prints a hint and continues.
func promptClawPatrolDashboardPassword(em *output.Emitter) {
	if _, err := exec.LookPath("clawpatrol"); err != nil {
		return // can't set it without the binary; doctor already flags this
	}
	var password string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Set the ClawPatrol dashboard password (leave blank to skip)").
			EchoMode(huh.EchoModePassword).
			Value(&password),
	))
	if err := form.Run(); err != nil || password == "" {
		return
	}
	dir, err := paths.ClawPatrolDir()
	if err != nil {
		return
	}
	gatewayConfig := filepath.Join(dir, "gateway.hcl")
	logPath := filepath.Join(dir, "gateway.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not open %s: %s\n", logPath, err)
		return
	}
	defer func() { _ = logFile.Close() }()

	// #nosec G204 — fixed argv; the password is a value, not shell-interpolated.
	command := exec.Command("clawpatrol", "gateway", "--set-dashboard-password", password, gatewayConfig)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from setup's session
	if err := command.Start(); err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not start the ClawPatrol gateway: %s\n", err)
		return
	}
	_ = command.Process.Release() // run in the background; do not wait
	_, _ = fmt.Fprintf(em.Err, "ClawPatrol dashboard password set; gateway started in the background (log: %s).\n", logPath)
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
	var password string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Set the LiteLLM admin UI password (leave blank to skip)").
			EchoMode(huh.EchoModePassword).
			Value(&password),
	))
	if err := form.Run(); err != nil || password == "" {
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
		"LiteLLM admin UI secured — log in as %q at http://localhost:4000/ui\n"+
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
