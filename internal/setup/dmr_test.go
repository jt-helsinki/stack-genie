package setup

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
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

func TestReconcileDMREnabledDefaultFalse(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// No runtime.yaml at all → disabled default.
	if reconcileDMREnabled() {
		test.Error("with no runtime.yaml, DMR must default to disabled")
	}
	// Persisted-but-unset → still disabled (the zero value).
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleStandalone}); err != nil {
		test.Fatal(err)
	}
	if reconcileDMREnabled() {
		test.Error("an unset DockerModelRunnerEnabled must resolve to disabled")
	}
}

func TestReconcileDMREnabledRoundtrip(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{
		SchemaVersion:            runtime.SchemaVersion,
		Role:                     runtime.RoleStandalone,
		DockerModelRunnerEnabled: true,
	}); err != nil {
		test.Fatal(err)
	}
	loaded, err := runtime.Load()
	if err != nil || loaded == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if !loaded.DMREnabled() {
		test.Error("persisted DockerModelRunnerEnabled=true should round-trip as enabled")
	}
	if !reconcileDMREnabled() {
		test.Error("reconcileDMREnabled should reflect the persisted enabled flag")
	}
}

// TestStatusForDMRDisabledOmitted: with DMR disabled (the default), statusFor lists
// no docker-model-runner line at all — it is irrelevant unless a model routes to it.
func TestStatusForDMRDisabledOmitted(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{prober: fakeProber{}}
	statuses, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name == dmrServiceName {
			test.Errorf("DMR must be omitted when disabled, got %+v", status)
		}
	}
}

// TestStatusForDMREnabledVisible: with DMR enabled, statusFor surfaces a host-mode
// docker-model-runner line whose state comes from the HTTP probe (not a container).
func TestStatusForDMREnabledVisible(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{
		SchemaVersion:            runtime.SchemaVersion,
		Role:                     runtime.RoleStandalone,
		DockerModelRunnerEnabled: true,
	}); err != nil {
		test.Fatal(err)
	}
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
		test.Fatal("DMR must be listed when enabled")
	}
	if found.Mode != runtime.OllamaModeHost {
		test.Errorf("DMR Mode = %q, want host", found.Mode)
	}
	if found.State != "running" || !found.Healthy {
		test.Errorf("reachable DMR should be running/healthy, got state=%q healthy=%v", found.State, found.Healthy)
	}
	if found.Address == "" {
		test.Error("DMR should surface a host-reachable address")
	}

	// Unreachable → listed but stopped/unhealthy.
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
