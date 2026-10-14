package setup

import (
	"strings"
	"testing"
)

// databaseProber answers psql so the "does the database exist" probe is controllable and
// every exec is recorded.
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

// psqlCalls returns each psql exec as "<database>: <query>".
func psqlCalls(prober *databaseProber) []string {
	var calls []string
	for _, call := range prober.calls {
		if len(call) > 3 && call[1] == "exec" && call[3] == "psql" {
			calls = append(calls, call[7]+": "+call[len(call)-1]) // call[7] is the -d database
		}
	}
	return calls
}

// A service gets its OWN database in the existing LiteLLM Postgres — created once, never
// touched when it already exists (a relaunch can't clobber data) — and its extensions are
// applied INSIDE that database every time.
func TestEnsureLiteLLMPostgresDatabaseCreatesOnceAndIsIdempotent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fresh := &databaseProber{}
	if err := ensureLiteLLMPostgresDatabase(fresh, "docker", "svc", "svc", "pgcrypto", "pg_trgm"); err != nil {
		test.Fatal(err)
	}
	calls := strings.Join(psqlCalls(fresh), " | ")
	for _, want := range []string{
		"litellm: CREATE ROLE svc LOGIN",
		"litellm: CREATE DATABASE svc OWNER svc",
		"svc: CREATE EXTENSION IF NOT EXISTS pgcrypto",
		"svc: CREATE EXTENSION IF NOT EXISTS pg_trgm",
	} {
		if !strings.Contains(calls, want) {
			test.Errorf("missing %q in %s", want, calls)
		}
	}

	existing := &databaseProber{exists: true}
	if err := ensureLiteLLMPostgresDatabase(existing, "docker", "svc", "svc", "pgcrypto"); err != nil {
		test.Fatal(err)
	}
	for _, call := range psqlCalls(existing) {
		if strings.Contains(call, "CREATE DATABASE") || strings.Contains(call, "CREATE ROLE") {
			test.Errorf("existing database must not be recreated: %s", call)
		}
	}
	if !strings.Contains(strings.Join(psqlCalls(existing), "|"), "svc: CREATE EXTENSION IF NOT EXISTS pgcrypto") {
		test.Error("extensions must still be (re)applied to an existing database")
	}
}
