package setup

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/logs"
)

// The service set is, by necessity, expressed at several granularities across
// packages — logical services here (desiredServices), per-image pins in
// internal/versions, host endpoints in internal/console, and per-container log
// sources in internal/logs. A full single registry would be a larger refactor of
// the verified reconcile; instead these guards FAIL when those lists drift apart,
// so adding a service forces updating every surface. (Image coverage is already
// asserted by TestRequiredImagesCoversEveryService.)

// Every manageable logical service must have a console endpoint entry (even an
// address-less one) so `ai services console` / the ai ui Services view can
// resolve it.
func TestEveryServiceHasAConsoleEntry(test *testing.T) {
	for _, spec := range desiredServices() {
		if _, ok := console.EndpointFor(spec.Name); !ok {
			test.Errorf("service %q has no console registry entry — add one to internal/console (an address-less {} is fine)", spec.Name)
		}
	}
}

// Every manageable logical service must be tailable, i.e. accepted by
// `ai logs --service` (internal/logs), so the two user-facing service surfaces
// stay in sync.
func TestEveryServiceIsLoggable(test *testing.T) {
	loggable := make(map[string]bool, len(logs.Services()))
	for _, name := range logs.Services() {
		loggable[name] = true
	}
	for _, spec := range desiredServices() {
		if !loggable[spec.Name] {
			test.Errorf("service %q is not in logs.Services() — `ai logs --service %s` would reject it", spec.Name, spec.Name)
		}
	}
}
