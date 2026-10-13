package setup

// multica_server.go runs the self-hosted Multica server (https://multica.ai) — the
// CONTAINER half of the Multica service: its own pgvector Postgres, the Go backend,
// and the Next.js web UI. Like Open WebUI it is a CORE service (always installed, no
// setup picker), INTERNAL-ONLY, and fronted by the proxy at multica.<domain>:18787
// (the backend's /ws goes straight to it — see proxyCaddyfile). The host-side
// `multica` CLI + daemon that actually runs agent CLIs is the other half
// (multica_host.go); it is NOT a container.
//
// hardware bring-up: the three-container boot, first-account signup (the verification
// code is printed in the backend log while no email provider is configured), and the
// browser WebSocket through the proxy are exercised only on a provisioned host.

import (
	"crypto/rand"
	"encoding/hex"
	"os"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

const (
	multicaWebContainer     = "aip-multica"
	multicaBackendContainer = "aip-multica-backend"
	multicaDBContainer      = "aip-multica-db"
	multicaDBVolume         = "multica-db"
	multicaUploadsVolume    = "aip-multica-uploads"
	multicaDBUser           = "multica"
	multicaDBName           = "multica"
	// multicaDatabaseURL carries no secret: the DB is internal-only on aip-net with
	// trust auth, mirroring litellmDatabaseURL.
	multicaDatabaseURL = "postgres://" + multicaDBUser + "@" + multicaDBContainer + ":5432/" + multicaDBName + "?sslmode=disable"
	// multicaJWTCredential is the OS-keychain entry holding the backend's JWT signing
	// secret. It must stay stable across relaunches (a changed secret logs every user
	// out), so it is generated once and reloaded — never written to platform disk.
	multicaJWTCredential = "MULTICA_JWT_SECRET"
)

// multicaPublicURL is the origin browsers and the host daemon reach Multica at: the
// multica.<domain> proxy vhost. The backend needs it as FRONTEND_ORIGIN (CORS and the
// WebSocket Origin check — its default allowlist is localhost only).
func multicaPublicURL(domain string) string {
	return "http://multica." + domain + ":" + proxyHostPort
}

// ensureMulticaJWTSecret exports JWT_SECRET into this process's environment (for the
// backend's `-e JWT_SECRET` passthrough) from the OS keychain, generating and storing
// one on first use. Unlike the optional host backends this is NOT best-effort: a
// backend started without a secret refuses to boot, so a generation failure errors.
func ensureMulticaJWTSecret() error {
	if value, err := findCredentialFn(multicaJWTCredential); err == nil && value != "" {
		return os.Setenv("JWT_SECRET", value)
	}
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return err
	}
	secret := hex.EncodeToString(buffer)
	if err := storeCredentialFn(multicaJWTCredential, secret); err != nil {
		return err
	}
	return os.Setenv("JWT_SECRET", secret)
}

// ensureMultica brings up the three Multica containers in dependency order (DB →
// backend → web), each recreated only when not already running on its current image.
// Idempotent. The proxy must be reconciled AFTER this (it resolves the multica
// upstreams at config-load).
func ensureMultica(prober runtime.Prober, containerRuntime, domain string) error {
	dbDir, err := systemVolumeDir(multicaDBVolume, 0o700)
	if err != nil {
		return err
	}
	if err := ensureMulticaJWTSecret(); err != nil {
		return serviceStartError("Multica (JWT secret)")
	}
	// The secret only needs to be in the environment for the `docker run -e JWT_SECRET`
	// passthrough below. Drop it afterwards so nothing else this process spawns (notably
	// the Multica daemon, and through it every agent) inherits it.
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()
	publicURL := multicaPublicURL(domain)
	containers := []struct {
		name string
		args []string
	}{
		{multicaDBContainer, []string{
			"-e", "POSTGRES_USER=" + multicaDBUser,
			"-e", "POSTGRES_DB=" + multicaDBName,
			"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
			// pg17: the data dir is the pre-18 /var/lib/postgresql/data (unlike litellm-db's pg18).
			"-v", dbDir + ":/var/lib/postgresql/data",
			containerImage("multica-db"),
		}},
		{multicaBackendContainer, []string{
			"-v", multicaUploadsVolume + ":/app/data/uploads",
			"-e", "DATABASE_URL=" + multicaDatabaseURL,
			"-e", "PORT=8080",
			"-e", "JWT_SECRET", // passthrough (ensureMulticaJWTSecret) — value stays out of argv
			"-e", "APP_ENV=production",
			"-e", "FRONTEND_ORIGIN=" + publicURL,
			"-e", "CORS_ALLOWED_ORIGINS=" + publicURL,
			"-e", "MULTICA_APP_URL=" + publicURL,
			"-e", "DO_NOT_TRACK=1", // internal service: no usage telemetry
			containerImage("multica-backend"),
		}},
		{multicaWebContainer, []string{
			"-e", "HOSTNAME=0.0.0.0",
			"-e", "REMOTE_API_URL=http://" + multicaBackendContainer + ":8080",
			containerImage("multica"),
		}},
	}
	images := map[string]string{
		multicaDBContainer:      containerImage("multica-db"),
		multicaBackendContainer: containerImage("multica-backend"),
		multicaWebContainer:     containerImage("multica"),
	}
	for _, container := range containers {
		if containerOnCurrentImage(prober, containerRuntime, container.name, images[container.name]) {
			continue
		}
		_, _ = prober.Run(containerRuntime, "rm", "-f", container.name)
		args := append([]string{"run", "-d", "--name", container.name, "--network", platformNetwork}, container.args...)
		if _, err := prober.Run(containerRuntime, args...); err != nil {
			return serviceStartError("Multica")
		}
	}
	return nil
}

// stopMultica stops all three Multica containers (web first, DB last), skipping any
// that are not running.
func stopMultica(prober runtime.Prober, containerRuntime string) error {
	for _, name := range []string{multicaWebContainer, multicaBackendContainer, multicaDBContainer} {
		if !containerRunning(prober, containerRuntime, name) {
			continue
		}
		if _, err := prober.Run(containerRuntime, "stop", name); err != nil {
			return err
		}
	}
	return nil
}

// multicaContainersUp reports whether all three Multica containers are running — the
// service's readiness signal (the backend retries its own DB connect for minutes, so
// "running" is the honest cheap probe, matching headroom/open-webui).
func multicaContainersUp(prober runtime.Prober, containerRuntime string) bool {
	for _, name := range []string{multicaWebContainer, multicaBackendContainer, multicaDBContainer} {
		if !containerRunning(prober, containerRuntime, name) {
			return false
		}
	}
	return true
}
