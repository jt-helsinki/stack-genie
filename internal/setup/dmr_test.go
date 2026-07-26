package setup

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeDMRHTTP swaps dmrHTTPGet for one that returns the given status (or a transport
// error when status == 0), restoring the original on cleanup. Mirrors fakeOllamaHTTP.
func fakeDMRHTTP(test *testing.T, status int) {
	test.Helper()
	original := dmrHTTPGet
	test.Cleanup(func() { dmrHTTPGet = original })
	dmrHTTPGet = func(string) (*http.Response, error) {
		if status == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
}

func TestDMRReachable(test *testing.T) {
	fakeDMRHTTP(test, http.StatusOK)
	if !dmrReachable() {
		test.Error("200 from the DMR endpoint should be reachable")
	}
	fakeDMRHTTP(test, http.StatusServiceUnavailable)
	if dmrReachable() {
		test.Error("a non-200 status must not count as reachable")
	}
	fakeDMRHTTP(test, 0) // transport error
	if dmrReachable() {
		test.Error("a transport error must not count as reachable")
	}
}

func TestEnsureDMRReachability(test *testing.T) {
	fakeDMRHTTP(test, http.StatusOK)
	if err := ensureDMR(); err != nil {
		test.Errorf("reachable DMR should pass, got %v", err)
	}

	fakeDMRHTTP(test, 0) // transport error
	err := ensureDMR()
	if err == nil {
		test.Fatal("unreachable DMR must return an actionable error")
	}
	if !strings.Contains(err.Error(), "12434") {
		test.Errorf("error should be actionable (mention the DMR port/enable steps): %v", err)
	}
}

// TestStatusForDMRAlwaysVisible: DMR is always available as an option, so statusFor
// always surfaces a host-mode docker-model-runner line whose state comes from the
// HTTP probe (not a container) — running when reachable, stopped when not.
func TestStatusForDMRAlwaysVisible(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeDMRHTTP(test, http.StatusOK)
	services := realServices{prober: fakeProber{}}
	statuses, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	var found *ServiceStatus
	for index := range statuses {
		if statuses[index].Name == dmrServiceName {
			found = &statuses[index]
			break
		}
	}
	if found == nil {
		test.Fatal("DMR must always be listed")
	}
	if found.Mode != "host" {
		test.Errorf("DMR Mode = %q, want host", found.Mode)
	}
	if found.State != "running" || !found.Healthy {
		test.Errorf("reachable DMR should be running/healthy, got state=%q healthy=%v", found.State, found.Healthy)
	}
	if found.Address == "" {
		test.Error("DMR should surface a host-reachable address")
	}

	// Unreachable → still listed but stopped/unhealthy.
	fakeDMRHTTP(test, 0)
	statuses, err = services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name != dmrServiceName {
			continue
		}
		if status.State != "stopped" || status.Healthy {
			test.Errorf("unreachable DMR should be stopped/unhealthy, got state=%q healthy=%v", status.State, status.Healthy)
		}
	}
}
