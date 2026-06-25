package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/envfile"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// runLiteLLMPassword drives `ai litellm password` RunE with a captured emitter,
// returning the exit code it set.
func runLiteLLMPassword(test *testing.T, json bool) int {
	test.Helper()
	emitter := &output.Emitter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, JSON: json}
	exit := output.ExitOK
	cmd := newLiteLLMPasswordCmd(emitter, &exit)
	if err := cmd.RunE(cmd, nil); err != nil {
		test.Fatalf("RunE returned error (should set exit + return nil): %v", err)
	}
	return exit
}

// TestLiteLLMPasswordNoSetup: with no runtime.yaml the command exits 3
// (platform not set up) — deterministic, no container touched.
func TestLiteLLMPasswordNoSetup(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runLiteLLMPassword(test, true); exit != output.ExitMissingDep {
		test.Errorf("no setup exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

// TestLiteLLMPasswordClientRole: a client host has no local LiteLLM → exit 3
// before any container probe.
func TestLiteLLMPasswordClientRole(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleClient}); err != nil {
		test.Fatalf("persist: %v", err)
	}
	if exit := runLiteLLMPassword(test, true); exit != output.ExitMissingDep {
		test.Errorf("client role exit = %d, want %d", exit, output.ExitMissingDep)
	}
}

// TestLiteLLMPasswordResultHuman renders the secured-UI confirmation.
func TestLiteLLMPasswordResultHuman(test *testing.T) {
	result := litellmPasswordResult{Secured: true, LoginAs: "admin", URL: "http://litellm.aip.local:18787/ui"}
	human := result.Human()
	if !strings.Contains(human, "admin") || !strings.Contains(human, result.URL) {
		test.Errorf("Human() missing fields: %q", human)
	}
}

// TestLiteLLMPasswordRequiredAtSetup pins the role decision used by `ai setup`:
// only the server role must collect a password.
func TestLiteLLMPasswordRequiredAtSetup(test *testing.T) {
	if !liteLLMPasswordRequiredAtSetup(runtime.RoleServer) {
		test.Error("server must require a LiteLLM password at setup")
	}
	for _, role := range []string{runtime.RoleStandalone, runtime.RoleClient, ""} {
		if liteLLMPasswordRequiredAtSetup(role) {
			test.Errorf("role %q must NOT require a password at setup", role)
		}
	}
}

// TestOfferPersistNonInteractiveFallback: on a non-TTY the persist offer does
// NOT write the env file (it prints the manual export block instead).
func TestOfferPersistNonInteractiveFallback(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	err := &bytes.Buffer{}
	emitter := &output.Emitter{Out: &bytes.Buffer{}, Err: err}
	offerPersistLiteLLMSecrets(emitter, false, "pw", "sk-key")

	path, _ := envfile.Path()
	if _, statErr := os.Stat(path); statErr == nil {
		test.Errorf("non-interactive offer must NOT write %s", path)
	}
	if !strings.Contains(err.String(), "export UI_PASSWORD") {
		test.Errorf("non-interactive offer must print the manual export block: %q", err.String())
	}
}

// TestPersistOfferWritesViaEnvfile confirms the YES-branch write the offer
// performs round-trips through envfile (the offer's interactive branch can't be
// driven without a TTY, so we exercise the same envfile.Write call it makes).
func TestPersistOfferWritesViaEnvfile(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := envfile.Write(map[string]string{"UI_PASSWORD": "pw", "LITELLM_MASTER_KEY": "sk-key"}); err != nil {
		test.Fatalf("Write: %v", err)
	}
	_ = os.Unsetenv("UI_PASSWORD")
	_ = os.Unsetenv("LITELLM_MASTER_KEY")
	if err := envfile.Load(); err != nil {
		test.Fatalf("Load: %v", err)
	}
	if os.Getenv("UI_PASSWORD") != "pw" || os.Getenv("LITELLM_MASTER_KEY") != "sk-key" {
		test.Errorf("persisted secrets did not round-trip: %q / %q",
			os.Getenv("UI_PASSWORD"), os.Getenv("LITELLM_MASTER_KEY"))
	}
}
