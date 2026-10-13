package setup

// langflow.go runs Langflow (https://langflow.org), the visual flow builder, as a CORE
// service like Open WebUI: always installed, ONE INTERNAL-ONLY container fronted by the
// proxy at langflow.<domain>:18787. It stores its flows in its OWN database (and role)
// inside the existing LiteLLM Postgres (aip-litellm-db) rather than SQLite or a DB
// container of its own. Tradeoffs the user accepted: that Postgres uses trust auth, so the
// separate role is hygiene, not a boundary (a compromised Langflow could reach the LiteLLM
// database); and `ai services stop|restart litellm` stops that DB, taking Langflow's
// storage down with it.
// Auth follows the role policy (runtime.RequireUIAuth): standalone/client run OPEN via
// Langflow's auto-login; server requires a login, with a superuser password generated
// once into the OS keychain and shown ONCE when generated.
//
// Model access is NOT pre-wired: Langflow has no env hook for an OpenAI-compatible base
// URL. In its Model Providers settings, point an OpenAI provider at http://aip-proxy/v1
// (the same gateway path every agent uses) with a LiteLLM key.
//
// hardware bring-up: first boot, the superuser login, and the UI through the proxy
// vhost are exercised only on a provisioned host.

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

const (
	langflowContainer = "aip-langflow"
	langflowVolume    = "aip-langflow-data"
	// langflowDataDir is Langflow's config dir inside the container (secret key, uploads);
	// the named volume mounts here so it survives recreation and image upgrades.
	langflowDataDir     = "/app/langflow"
	langflowDBName      = "langflow"
	langflowDBUser      = "langflow"
	langflowDatabaseURL = "postgresql://" + langflowDBUser + "@" + litellmDBContainer + ":5432/" + langflowDBName
	langflowUser        = "admin"
	langflowPassword    = "LANGFLOW_SUPERUSER_PASSWORD"
)

// ensureLangflowPassword exports the superuser password into this process's environment
// (for the `-e LANGFLOW_SUPERUSER_PASSWORD` passthrough) from the OS keychain,
// generating, storing and reporting it on first use.
func ensureLangflowPassword(report func(string)) error {
	if value, err := findCredentialFn(langflowPassword); err == nil && value != "" {
		return os.Setenv(langflowPassword, value)
	}
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return err
	}
	password := hex.EncodeToString(buffer)
	if err := storeCredentialFn(langflowPassword, password); err != nil {
		return err
	}
	report("      Langflow login — user: " + langflowUser + "  password: " + password + "  (shown once; kept in the OS keychain as " + langflowPassword + ")")
	return os.Setenv(langflowPassword, password)
}

// ensureLangflowDatabase makes sure Langflow's own database exists in the LiteLLM
// Postgres, starting that Postgres first if needed (ensureLiteLLMDB is idempotent).
// Idempotent: an existing database is left untouched. Admin runs as the LiteLLM DB's own
// superuser over the container's local socket.
func ensureLangflowDatabase(prober runtime.Prober, containerRuntime string) error {
	if err := ensureLiteLLMDB(prober, containerRuntime); err != nil {
		return err
	}
	psql := func(query string) ([]byte, error) {
		return prober.Run(containerRuntime, "exec", litellmDBContainer, "psql",
			"-U", litellmDBUser, "-d", litellmDBName, "-tA", "-c", query)
	}
	if out, err := psql("SELECT 1 FROM pg_database WHERE datname = '" + langflowDBName + "'"); err == nil && strings.TrimSpace(string(out)) == "1" {
		return nil
	}
	// The role may already exist from an interrupted earlier run; only the database must succeed.
	_, _ = psql("CREATE ROLE " + langflowDBUser + " LOGIN")
	if _, err := psql("CREATE DATABASE " + langflowDBName + " OWNER " + langflowDBUser); err != nil {
		return serviceStartError("Langflow database")
	}
	return nil
}

// ensureLangflow starts the Langflow container unless it is already running on the
// current image. Idempotent.
func ensureLangflow(prober runtime.Prober, containerRuntime string, requireAuth bool, report func(string)) error {
	if containerOnCurrentImage(prober, containerRuntime, langflowContainer, containerImage("langflow")) {
		return nil
	}
	if err := ensureLangflowDatabase(prober, containerRuntime); err != nil {
		return err
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", langflowContainer)
	args := []string{
		"run", "-d", "--name", langflowContainer,
		"--network", platformNetwork,
		"-v", langflowVolume + ":" + langflowDataDir,
		"-e", "LANGFLOW_CONFIG_DIR=" + langflowDataDir,
		"-e", "LANGFLOW_DATABASE_URL=" + langflowDatabaseURL,
		"-e", "DO_NOT_TRACK=true", // internal service: no usage telemetry
	}
	if requireAuth {
		if err := ensureLangflowPassword(report); err != nil {
			return serviceStartError("Langflow (superuser password)")
		}
		// Only needed for the passthrough below — drop it so the Multica daemon and the
		// agents it spawns never inherit it.
		defer func() { _ = os.Unsetenv(langflowPassword) }()
		args = append(args,
			"-e", "LANGFLOW_AUTO_LOGIN=false",
			"-e", "LANGFLOW_SUPERUSER="+langflowUser,
			"-e", langflowPassword, // passthrough — the value stays out of argv
		)
	} else {
		args = append(args, "-e", "LANGFLOW_AUTO_LOGIN=true")
	}
	args = append(args, containerImage("langflow"))
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("Langflow")
	}
	return nil
}
