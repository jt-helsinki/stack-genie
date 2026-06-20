package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
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
			// On first gateway seed, offer to set the ClawPatrol dashboard password
			// — but only interactively (a real TTY, not --json/automation), so
			// scripted/JSON setup stays non-interactive.
			if report.GatewayConfigCreated && !em.JSON && term.IsTerminal(os.Stdin.Fd()) {
				promptClawPatrolDashboardPassword(em)
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
// (hidden input) and sets it via the documented `clawpatrol gateway
// --set-dashboard-password` CLI (it is not an HCL field). The password is never
// logged or written to platform disk. Best-effort: a blank entry, a missing
// clawpatrol binary, or a command failure just prints a hint and continues.
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
	// #nosec G204 — fixed argv; the password is a value, not shell-interpolated.
	if err := exec.Command("clawpatrol", "gateway", "--set-dashboard-password", password, gatewayConfig).Run(); err != nil {
		_, _ = fmt.Fprintf(em.Err, "warning: could not set ClawPatrol dashboard password: %s\n", err)
		return
	}
	_, _ = fmt.Fprintln(em.Err, "ClawPatrol dashboard password set.")
}
