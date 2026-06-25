package setup

import (
	"slices"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/logs"
	"github.com/jt-helsinki/ideal-robot/internal/services"
)

// The service set is, by necessity, expressed at several granularities across
// packages — logical services here (desiredServices), per-image pins in
// internal/versions, host endpoints in internal/console, and per-container log
// sources in internal/logs. A full single registry would be a larger refactor of
// the verified reconcile; instead these guards FAIL when those lists drift apart,
// so adding a service forces updating every surface. (Image coverage is already
// asserted by TestRequiredImagesCoversEveryService.)
//
// Two further granularities are guarded below:
//   - Odysseus COMPANION containers (chromadb/searxng/ntfy): real containers that
//     are NOT logical desiredServices, so the desiredServices-driven guards above
//     don't reach them — TestCompanionContainersStayConsistent covers them.
//   - The REVERSE direction (logs.Services() → known targets):
//     TestNoOrphanLogScopes catches a log scope with no real service behind it.

// odysseusCompanions derives the Odysseus companion-container names from the same
// production source the reconcile/image code uses (serviceImageKeys("odysseus"),
// the app plus its companions) by dropping the "odysseus" app itself. Deriving it
// here — rather than hand-typing chromadb/searxng/ntfy — forces a newly added
// companion into serviceImageKeys, where production already consumes it.
func odysseusCompanions() []string {
	companions := make([]string, 0, 3)
	for _, name := range serviceImageKeys("odysseus") {
		if name == "odysseus" {
			continue // the app itself is a logical service, not a companion
		}
		companions = append(companions, name)
	}
	return companions
}

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

// The Odysseus companion containers (chromadb/searxng/ntfy) are real containers
// but are NOT logical desiredServices (they are owned by the "odysseus" service),
// so the desiredServices-driven guards above never reach them. This guard pins
// the two surfaces that must still know each companion individually: it must be
// tailable (`ai logs --service <companion>`), and it must resolve to a pinned
// image via the same path production pulls through (containerImage). Drift caught:
// a companion added to serviceImageKeys but missing from logs.Services() (its log
// scope would be rejected) or with no versions pin (containerImage empty → no
// image to pull). Fix: add the companion to internal/logs services / its
// internal/versions pin.
func TestCompanionContainersStayConsistent(test *testing.T) {
	loggable := make(map[string]bool, len(logs.Services()))
	for _, name := range logs.Services() {
		loggable[name] = true
	}
	companions := odysseusCompanions()
	if len(companions) == 0 {
		test.Fatal("odysseusCompanions() is empty — serviceImageKeys(\"odysseus\") should list the app plus its companions")
	}
	for _, companion := range companions {
		if !loggable[companion] {
			test.Errorf("companion %q is not in logs.Services() — `ai logs --service %s` would reject it", companion, companion)
		}
		if ref := containerImage(companion); ref == "" {
			test.Errorf("companion %q has no pinned image — containerImage(%q) is empty; add a pin in internal/versions", companion, companion)
		}
	}
}

// Reverse direction of TestEveryServiceIsLoggable: every entry in logs.Services()
// must be a KNOWN target — a logical desiredService, an Odysseus companion, or the
// "microsandbox" microVM runtime (which has no service-tier container but is
// tailable). Drift caught: a log scope added to internal/logs with no real service
// behind it, so `ai logs --service <x>` would advertise a source that nothing ever
// writes. Fix: remove the orphan scope, or back it with a real service/companion.
func TestNoOrphanLogScopes(test *testing.T) {
	known := map[string]bool{
		"microsandbox": true, // the microVM runtime — tailable, but not a service-tier container
	}
	for _, spec := range desiredServices() {
		known[spec.Name] = true
	}
	for _, companion := range odysseusCompanions() {
		known[companion] = true
	}
	for _, name := range logs.Services() {
		if !known[name] {
			test.Errorf("log scope %q has no backing service — it is not a desiredService, an Odysseus companion, or the microsandbox runtime; remove it from internal/logs or add the service", name)
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
		"ollama":     {ollamaContainer},
		"presidio":   {presidioAnalyzerContainer, presidioAnonymizerContainer},
		"litellm":    {litellmContainer, litellmDBContainer},
		"headroom":   {headroomContainer},
		"proxy":      {proxyContainer},
		"open-webui": {openWebUIContainer},
		"odysseus":   {odysseusContainer, chromadbContainer, searxngContainer, ntfyContainer},
		"dns":        {dnsContainer},
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
