package cli

import (
	"fmt"
	"net/http"
	goruntime "runtime"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/doctor"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/spf13/cobra"
)

// openWebUIProbe is the live health probe for the optional Open WebUI chat UI: it
// GETs http://localhost:18090/health with a short timeout (doctor must not hang).
type openWebUIProbe struct{}

func (openWebUIProbe) Reachable() error {
	httpClient := &http.Client{Timeout: 3 * time.Second}
	response, err := httpClient.Get("http://localhost:18090/health")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("open-webui: unexpected status %d", response.StatusCode)
	}
	return nil
}

// newDoctorCmd builds `ai doctor` (CLI §10.1): dependency + health checks with
// repair suggestions. It always runs to completion and exits 0 with a report;
// per-check `status` (and the top-level `ok`) convey health.
func newDoctorCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check platform dependencies and health, with repair suggestions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report := doctor.Run(doctor.Deps{
				GOOS:      goruntime.GOOS,
				GOARCH:    goruntime.GOARCH,
				Prober:    runtime.RealProber(),
				Model:     litellm.RealClient(),
				Ollama:    ollama.RealProbe(),
				OpenWebUI: openWebUIProbe{},
			})
			*exit = emitter.Success("doctor", report)
			return nil
		},
	}
}
