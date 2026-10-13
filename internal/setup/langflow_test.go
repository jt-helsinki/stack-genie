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

// databaseProber answers psql so ensureLangflowDatabase's "does it exist" probe is controllable
// and every exec is recorded.
type databaseProber struct {
	recordingProber
	exists bool
}

func (prober *databaseProber) Run(name string, args ...string) ([]byte, error) {
	prober.calls = append(prober.calls, append([]string{name}, args...))
	if len(args) > 0 && args[0] == "exec" && strings.Contains(strings.Join(args, " "), "FROM pg_database") && prober.exists {
		return []byte("1\n"), nil
	}
	return nil, nil
}

func psqlQueries(prober *databaseProber) []string {
	var queries []string
	for _, call := range prober.calls {
		if len(call) > 3 && call[1] == "exec" && call[3] == "psql" {
			queries = append(queries, call[len(call)-1])
		}
	}
	return queries
}

// Langflow gets its OWN database in the existing LiteLLM Postgres — created once, never
// touched when it already exists (so a relaunch can't clobber flows).
func TestEnsureLangflowDatabaseCreatesOnceAndIsIdempotent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fresh := &databaseProber{}
	if err := ensureLangflowDatabase(fresh, "docker"); err != nil {
		test.Fatal(err)
	}
	queries := strings.Join(psqlQueries(fresh), " | ")
	for _, want := range []string{"CREATE ROLE langflow LOGIN", "CREATE DATABASE langflow OWNER langflow"} {
		if !strings.Contains(queries, want) {
			test.Errorf("missing %q in %s", want, queries)
		}
	}
	existing := &databaseProber{exists: true}
	if err := ensureLangflowDatabase(existing, "docker"); err != nil {
		test.Fatal(err)
	}
	for _, query := range psqlQueries(existing) {
		if strings.HasPrefix(query, "CREATE") {
			test.Errorf("existing database must not be recreated: %s", query)
		}
	}
}
