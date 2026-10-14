package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Multica's browser WebSocket cannot ride the web UI's Next.js rewrites, so its vhost
// must send /ws (and /ws/*, not /ws*) straight to the backend with response buffering
// off, and everything else to the web UI.
func TestProxyCaddyfileRoutesMulticaWebSocketToBackend(test *testing.T) {
	rendered := proxyCaddyfile("aip.local")
	for _, want := range []string{
		"multica.aip.local:80 {",
		"@ws path /ws /ws/*",
		"reverse_proxy http://aip-multica-backend:8080 {",
		"flush_interval -1",
		"reverse_proxy http://aip-multica:3000",
	} {
		if !strings.Contains(rendered, want) {
			test.Errorf("Caddyfile missing %q:\n%s", want, rendered)
		}
	}
	// Only the multica vhost gets a /ws route — other UIs keep the single reverse_proxy.
	if strings.Count(rendered, "@ws path") != 1 {
		test.Errorf("expected exactly one /ws route:\n%s", rendered)
	}
}

func stubMulticaCredentials(test *testing.T, stored map[string]string) {
	test.Helper()
	origStore, origFind := storeCredentialFn, findCredentialFn
	test.Cleanup(func() { storeCredentialFn, findCredentialFn = origStore, origFind })
	findCredentialFn = func(name string) (string, error) {
		if value, ok := stored[name]; ok {
			return value, nil
		}
		return "", errors.New("not found")
	}
	storeCredentialFn = func(name, value string) error {
		stored[name] = value
		return nil
	}
	_ = os.Unsetenv("JWT_SECRET")
	test.Cleanup(func() { _ = os.Unsetenv("JWT_SECRET") })
}

// The JWT secret must be generated once and then reused: regenerating it on every
// relaunch would log every Multica user out.
func TestEnsureMulticaJWTSecretIsStable(test *testing.T) {
	stored := map[string]string{}
	stubMulticaCredentials(test, stored)

	if err := ensureMulticaJWTSecret(); err != nil {
		test.Fatal(err)
	}
	first := os.Getenv("JWT_SECRET")
	if len(first) != 64 || stored[multicaJWTCredential] != first {
		test.Fatalf("expected a 64-char secret stored in the keychain, got %q (stored %q)", first, stored[multicaJWTCredential])
	}
	_ = os.Unsetenv("JWT_SECRET")
	if err := ensureMulticaJWTSecret(); err != nil {
		test.Fatal(err)
	}
	if got := os.Getenv("JWT_SECRET"); got != first {
		test.Errorf("secret changed across relaunch: %q → %q", first, got)
	}
}

// The server uses the EXISTING Postgres (its own database + the two extensions it needs),
// comes up backend → web, internal-only, with the JWT secret passed by NAME only and uploads on
// a bind mount under ~/.ai-platform/volumes.
func TestEnsureMulticaLaunchesTwoInternalContainersOnSharedPostgres(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	stubMulticaCredentials(test, map[string]string{multicaJWTCredential: "s3cret-value"})

	prober := &databaseProber{}
	if err := ensureMultica(prober, "docker", "aip.local"); err != nil {
		test.Fatal(err)
	}
	var launched []string
	for _, call := range prober.calls {
		if len(call) >= 5 && call[1] == "run" {
			launched = append(launched, call[4]) // docker run -d --name <name>
		}
	}
	// The first run is the shared LiteLLM Postgres being ensured up (ensureLiteLLMDB).
	want := []string{litellmDBContainer, multicaBackendContainer, multicaWebContainer}
	if strings.Join(launched, ",") != strings.Join(want, ",") {
		test.Fatalf("launch order = %v, want %v", launched, want)
	}
	queries := strings.Join(psqlCalls(prober), " | ")
	for _, wantQuery := range []string{
		"litellm: CREATE DATABASE multica OWNER multica",
		"multica: CREATE EXTENSION IF NOT EXISTS pgcrypto",
		"multica: CREATE EXTENSION IF NOT EXISTS pg_trgm",
	} {
		if !strings.Contains(queries, wantQuery) {
			test.Errorf("missing %q in %s", wantQuery, queries)
		}
	}
	uploads := filepath.Join(home, ".ai-platform", "volumes", multicaUploadsVolume)
	for _, call := range prober.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "s3cret-value") {
			test.Errorf("JWT secret value leaked into argv: %s", joined)
		}
		if len(call) >= 2 && call[1] == "run" && call[4] != litellmDBContainer &&
			(strings.Contains(joined, " -p ") || strings.Contains(joined, "--publish")) {
			test.Errorf("multica containers must be internal-only: %s", joined)
		}
		if len(call) >= 5 && call[1] == "run" && call[4] == multicaBackendContainer {
			for _, wantArg := range []string{
				"-e JWT_SECRET ",
				"DATABASE_URL=postgres://multica@aip-litellm-db:5432/multica?sslmode=disable",
				"-v " + uploads + ":/app/data/uploads",
				"LOCAL_UPLOAD_DIR=/app/data/uploads",
				"FRONTEND_ORIGIN=http://multica.aip.local:18787",
				"CORS_ALLOWED_ORIGINS=http://multica.aip.local:18787",
				"COOKIE_DOMAIN= ",
			} {
				if !strings.Contains(joined+" ", wantArg) {
					test.Errorf("backend run args missing %q: %s", wantArg, joined)
				}
			}
		}
		if len(call) >= 5 && call[1] == "run" && call[4] == multicaWebContainer {
			for _, wantArg := range []string{"REMOTE_API_URL=http://aip-multica-backend:8080", "NEXT_PUBLIC_API_URL= ", "NEXT_PUBLIC_WS_URL= "} {
				if !strings.Contains(joined+" ", wantArg) {
					test.Errorf("web run args missing %q: %s", wantArg, joined)
				}
			}
		}
	}
	if os.Getenv("JWT_SECRET") != "" {
		test.Error("JWT_SECRET left in the process environment after launch (inherited by the daemon)")
	}
	if info, err := os.Stat(uploads); err != nil || !info.IsDir() {
		test.Errorf("uploads bind-mount dir not created: %v", err)
	}
}

// Compose rejects a service mounting an undeclared named volume, so every named volume the
// service tier mounts must be declared at the top level of the debug artifact.
func TestComposeDeclaresNamedVolumes(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	content, err := ServicesComposeYAML("127.0.0.1")
	if err != nil {
		test.Fatal(err)
	}
	for _, volume := range []string{langflowVolume} {
		if !strings.Contains(string(content), "\nvolumes:\n") || !strings.Contains(string(content), volume+": {}") {
			test.Errorf("named volume %q not declared at the top level:\n%s", volume, content)
		}
	}
}
