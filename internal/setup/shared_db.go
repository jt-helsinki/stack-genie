package setup

// shared_db.go gives a service its OWN database (and role) inside the existing LiteLLM
// Postgres (aip-litellm-db) instead of a DB container of its own. Langflow and Multica both
// use it. Tradeoffs the user accepted: that Postgres uses trust auth on the private network,
// so a separate role is hygiene, not a boundary (a compromised service could reach the
// LiteLLM database); and `ai services stop|restart litellm` stops the DB and takes these
// services' storage down with it.

import (
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// ensureLiteLLMPostgresDatabase makes sure database name (owned by role) exists in the LiteLLM
// Postgres — starting that Postgres first if needed (ensureLiteLLMDB is idempotent) — and that
// each extension exists inside it. name, role and extensions are platform constants, never user
// input, so they are interpolated into SQL directly. An existing database is left untouched; the
// extensions are re-applied every call (CREATE EXTENSION IF NOT EXISTS), which also repairs a
// database an interrupted earlier run left without them. Admin runs as the LiteLLM DB's own
// superuser over the container's local socket.
func ensureLiteLLMPostgresDatabase(prober runtime.Prober, containerRuntime, name, role string, extensions ...string) error {
	if err := ensureLiteLLMDB(prober, containerRuntime); err != nil {
		return err
	}
	psql := func(database, query string) ([]byte, error) {
		return prober.Run(containerRuntime, "exec", litellmDBContainer, "psql",
			"-U", litellmDBUser, "-d", database, "-tA", "-c", query)
	}
	if out, err := psql(litellmDBName, "SELECT 1 FROM pg_database WHERE datname = '"+name+"'"); err != nil || strings.TrimSpace(string(out)) != "1" {
		// The role may already exist from an interrupted earlier run; only the database must succeed.
		_, _ = psql(litellmDBName, "CREATE ROLE "+role+" LOGIN")
		if _, err := psql(litellmDBName, "CREATE DATABASE "+name+" OWNER "+role); err != nil {
			return serviceStartError(name + " database")
		}
	}
	for _, extension := range extensions {
		if _, err := psql(name, "CREATE EXTENSION IF NOT EXISTS "+extension); err != nil {
			return serviceStartError(name + " database extension " + extension)
		}
	}
	return nil
}
