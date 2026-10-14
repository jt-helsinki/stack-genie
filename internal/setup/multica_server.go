package setup

// multica_server.go runs the self-hosted Multica server (https://multica.ai) — the
// CONTAINER half of the Multica service: the Go backend and the Next.js web UI, wired per
// SELF_HOSTING_ADVANCED.md's same-origin layout. Like Open WebUI it is a CORE service (always
// installed, no setup picker), INTERNAL-ONLY, and fronted by the proxy at
// multica.<domain>:18787 (the backend's /ws goes straight to it — see proxyCaddyfile).
//
// Storage: Multica's own database (and role) lives in the existing LiteLLM Postgres
// (shared_db.go). Its docs state it does NOT use pgvector — the bundled pgvector/pgvector:pg17
// image is only a historical name — and needs just the pgcrypto and pg_trgm extensions, which
// the stock image ships. Uploads are a bind mount under ~/.ai-platform/volumes, removed with
// the rest of the platform's data. The docs call for PostgreSQL 17; aip-litellm-db is 18.4,
// which is untested against Multica's migrations (hardware bring-up).
//
// The host-side `multica` CLI + daemon that actually runs agent CLIs is the other half
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
	// multicaUploadsVolume is the per-name subdir under ~/.ai-platform/volumes holding
	// attachments, bind-mounted at multicaUploadsDir. LOCAL_UPLOAD_DIR is set to that absolute
	// path explicitly: the docs warn a relative default silently orphans uploads when the
	// launch directory changes.
	multicaUploadsVolume = "multica-uploads"
	multicaUploadsDir    = "/app/data/uploads"
	multicaDBUser        = "multica"
	multicaDBName        = "multica"
	// multicaDatabaseURL carries no secret: the shared Postgres is internal-only on aip-net with
	// trust auth, mirroring litellmDatabaseURL.
	multicaDatabaseURL = "postgres://" + multicaDBUser + "@" + litellmDBContainer + ":5432/" + multicaDBName + "?sslmode=disable"
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

// ensureMultica brings up the two Multica containers in dependency order (backend → web),
// each recreated only when not already running on its current image, after making sure its
// database exists in the shared Postgres. Idempotent. The proxy must be reconciled AFTER this
// (it resolves the multica upstreams at config-load).
func ensureMultica(prober runtime.Prober, containerRuntime, domain string) error {
	if err := ensureLiteLLMPostgresDatabase(prober, containerRuntime, multicaDBName, multicaDBUser, "pgcrypto", "pg_trgm"); err != nil {
		return err
	}
	uploadsDir, err := systemVolumeDir(multicaUploadsVolume, 0o755)
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
		name  string
		image string
		args  []string
	}{
		{multicaBackendContainer, containerImage("multica-backend"), []string{
			"-v", uploadsDir + ":" + multicaUploadsDir,
			"-e", "LOCAL_UPLOAD_DIR=" + multicaUploadsDir,
			"-e", "DATABASE_URL=" + multicaDatabaseURL,
			"-e", "PORT=8080",
			"-e", "JWT_SECRET", // passthrough (ensureMulticaJWTSecret) — value stays out of argv
			"-e", "APP_ENV=production",
			// Same-origin layout (SELF_HOSTING_ADVANCED.md): the browser reaches everything on
			// one origin, so cookies stay host-only (COOKIE_DOMAIN empty) and one value covers
			// both HTTP CORS and the WebSocket Origin check.
			"-e", "FRONTEND_ORIGIN=" + publicURL,
			"-e", "CORS_ALLOWED_ORIGINS=" + publicURL,
			"-e", "COOKIE_DOMAIN=",
			"-e", "MULTICA_APP_URL=" + publicURL,
			"-e", "DO_NOT_TRACK=1", // internal service: no usage telemetry
		}},
		{multicaWebContainer, containerImage("multica"), []string{
			"-e", "HOSTNAME=0.0.0.0",
			// Next.js rewrites proxy /v1, /api, /auth and /uploads here; only /ws bypasses
			// them, which the proxy vhost routes straight to the backend.
			"-e", "REMOTE_API_URL=http://" + multicaBackendContainer + ":8080",
			"-e", "NEXT_PUBLIC_API_URL=", // empty: the client uses relative paths on the page origin
			"-e", "NEXT_PUBLIC_WS_URL=",
		}},
	}
	for _, container := range containers {
		if containerOnCurrentImage(prober, containerRuntime, container.name, container.image) {
			continue
		}
		_, _ = prober.Run(containerRuntime, "rm", "-f", container.name)
		args := append([]string{"run", "-d", "--name", container.name, "--network", platformNetwork}, container.args...)
		args = append(args, container.image)
		if _, err := prober.Run(containerRuntime, args...); err != nil {
			return serviceStartError("Multica")
		}
	}
	return nil
}

// stopMultica stops the Multica containers (web first), skipping any that are not running.
// The shared Postgres is LiteLLM's and is left to it.
func stopMultica(prober runtime.Prober, containerRuntime string) error {
	for _, name := range []string{multicaWebContainer, multicaBackendContainer} {
		if !containerRunning(prober, containerRuntime, name) {
			continue
		}
		if _, err := prober.Run(containerRuntime, "stop", name); err != nil {
			return err
		}
	}
	return nil
}

// multicaContainersUp reports whether both Multica containers are running — the
// service's readiness signal (the backend retries its own DB connect for minutes, so
// "running" is the honest cheap probe, matching headroom/open-webui).
func multicaContainersUp(prober runtime.Prober, containerRuntime string) bool {
	for _, name := range []string{multicaWebContainer, multicaBackendContainer} {
		if !containerRunning(prober, containerRuntime, name) {
			return false
		}
	}
	return true
}
