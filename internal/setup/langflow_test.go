package setup

import (
	"os"
	"strings"
	"testing"
)

func langflowRun(test *testing.T, requireAuth bool, stored map[string]string) (launch string, report []string) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	stubMulticaCredentials(test, stored)
	test.Cleanup(func() { _ = os.Unsetenv(langflowPassword) })
	prober := &recordingProber{}
	if err := ensureLangflow(prober, "docker", requireAuth, func(line string) { report = append(report, line) }); err != nil {
		test.Fatal(err)
	}
	// The first `run` is the LiteLLM DB (ensureLiteLLMDB); Langflow's is the one naming it.
	for _, call := range prober.calls {
		if len(call) > 4 && call[1] == "run" && call[4] == langflowContainer {
			return strings.Join(call, " "), report
		}
	}
	test.Fatal("no langflow run recorded")
	return "", nil
}

// Standalone/client run open (auto-login); the container is internal-only on a named volume.
func TestEnsureLangflowOpenMode(test *testing.T) {
	launch, report := langflowRun(test, false, map[string]string{})
	for _, want := range []string{"--name aip-langflow", "-v aip-langflow-data:/app/langflow", "LANGFLOW_AUTO_LOGIN=true",
		"LANGFLOW_DATABASE_URL=postgresql://langflow@aip-litellm-db:5432/langflow"} {
		if !strings.Contains(launch, want) {
			test.Errorf("launch missing %q: %s", want, launch)
		}
	}
	if strings.Contains(launch, " -p ") || strings.Contains(launch, "SUPERUSER") || len(report) != 0 {
		test.Errorf("open mode must publish nothing, set no superuser, report nothing: %s %v", launch, report)
	}
}

// Server role requires a login: password generated once, shown once, passed by NAME only,
// and not left in the environment for the Multica daemon to inherit.
func TestEnsureLangflowAuthMode(test *testing.T) {
	stored := map[string]string{}
	launch, report := langflowRun(test, true, stored)
	password := stored[langflowPassword]
	if len(password) != 32 {
		test.Fatalf("expected a generated 32-char password in the keychain, got %q", password)
	}
	for _, want := range []string{"LANGFLOW_AUTO_LOGIN=false", "LANGFLOW_SUPERUSER=admin", "-e LANGFLOW_SUPERUSER_PASSWORD "} {
		if !strings.Contains(launch+" ", want) {
			test.Errorf("launch missing %q: %s", want, launch)
		}
	}
	if strings.Contains(launch, password) {
		test.Errorf("password value leaked into argv: %s", launch)
	}
	if !strings.Contains(strings.Join(report, "\n"), password) {
		test.Errorf("generated password was not shown to the user once: %v", report)
	}
	if os.Getenv(langflowPassword) != "" {
		test.Error("password left in the process environment after launch")
	}
	// Second run reuses the stored password and does not print it again.
	_, again := langflowRun(test, true, stored)
	if stored[langflowPassword] != password || len(again) != 0 {
		test.Errorf("password regenerated or re-shown on relaunch: %v", again)
	}
}
