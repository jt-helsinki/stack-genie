package cli

import (
	goruntime "runtime"

	"github.com/jt-helsinki/ideal-robot/internal/doctor"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/spf13/cobra"
)

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
				GOOS:   goruntime.GOOS,
				GOARCH: goruntime.GOARCH,
				Prober: runtime.RealProber(),
				Model:  litellm.RealClient(),
			})
			*exit = emitter.Success("doctor", report)
			return nil
		},
	}
}
