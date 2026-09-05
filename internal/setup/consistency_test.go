package setup

import (
	"slices"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/console"
	"github.com/jt-helsinki/stack-genie/internal/logs"
	"github.com/jt-helsinki/stack-genie/internal/services"
)

// The service set is, by necessity, expressed at several granularities across
// packages — logical services here (desiredServices), per-image pins in
// internal/versions, host endpoints in internal/console, and per-container log
// sources in internal/logs. A full single registry would be a larger refactor of
// the verified reconcile; instead these guards FAIL when those lists drift apart,
// so adding a service forces updating every surface. (Image coverage is already
// asserted by TestRequiredImagesCoversEveryService.)
//
// There are currently NO companion containers that are not also a logical service
// (Open WebUI moved to a per-workspace in-VM app and Odysseus — which owned the
// chromadb/searxng/ntfy companions — was removed). The REVERSE direction
// (logs.Services() → known targets) is still guarded by TestNoOrphanLogScopes,
// which catches a log scope with no real service behind it.

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

// Reverse direction of TestEveryServiceIsLoggable: every entry in logs.Services()
// must be a KNOWN target — a logical desiredService or the "microsandbox" microVM
// runtime (which has no service-tier container but is tailable). Drift caught: a
// log scope added to internal/logs with no real service behind it, so
// `ai logs --service <x>` would advertise a source that nothing ever writes. Fix:
// remove the orphan scope, or back it with a real service.
func TestNoOrphanLogScopes(test *testing.T) {
	known := map[string]bool{
		"microsandbox": true, // the microVM runtime — tailable, but not a service-tier container
	}
	for _, spec := range desiredServices() {
		known[spec.Name] = true
	}
	for _, name := range logs.Services() {
		if !known[name] {
			test.Errorf("log scope %q has no backing service — it is not a desiredService or the microsandbox runtime; remove it from internal/logs or add the service", name)
		}
	}
}

// The aip-* container-name consts in setup_real.go are still hand-written
// (they back the verified ensure*/Control/Reconcile path and are intentionally
// not derived). This guard pins them to internal/services (the single source of
// truth for service→container-names) so the two surfaces cannot drift: each
// logical service's consts must equal services.ContainerNames(<service>) in
// component order. Drift caught: a container renamed/added in one place but not
// the other.
func TestSetupContainerConstsMatchRegistry(test *testing.T) {
	expected := map[string][]string{
		// omlx is HOST-NATIVE (no aip-omlx container is reconciled) — it is the sole
		// local-inference backend. It has no container, so the setup const and the
		// registry must agree on an empty component set.
		"omlx":         {}, // host-native — no container
		"presidio":     {presidioAnalyzerContainer, presidioAnonymizerContainer},
		"litellm":      {litellmContainer, litellmDBContainer},
		"valkey":       {valkeyContainer},
		"redisinsight": {redisInsightContainer},
		"headroom":     {headroomContainer},
		"proxy":        {proxyContainer},
		"dns":          {dnsContainer},
	}
	for _, spec := range desiredServices() {
		want, ok := expected[spec.Name]
		if !ok {
			test.Errorf("service %q has no container-name const expectation — add it to this guard", spec.Name)
			continue
		}
		if got := services.ContainerNames(spec.Name); !slices.Equal(got, want) {
			test.Errorf("services.ContainerNames(%q) = %v, want %v (setup consts drifted from the registry)", spec.Name, got, want)
		}
	}
}
