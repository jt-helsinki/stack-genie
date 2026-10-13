package multica

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveName(test *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "arm64"}:  "multica_darwin_arm64.tar.gz",
		{"linux", "amd64"}:   "multica_linux_amd64.tar.gz",
		{"windows", "amd64"}: "",
		{"linux", "386"}:     "",
	}
	for platform, want := range cases {
		if got := archiveName(platform[0], platform[1]); got != want {
			test.Errorf("archiveName(%v) = %q, want %q", platform, got, want)
		}
	}
}

// The shim must hand the CLI name and every argument through untouched, and survive
// a path or CLI name containing a single quote.
func TestWriteShims(test *testing.T) {
	dir := filepath.Join(test.TempDir(), "shims")
	if err := WriteShims(dir, "/opt/it's/ai", map[string]string{"claude-code": "claude", "codex": "codex"}); err != nil {
		test.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "claude"))
	if err != nil {
		test.Fatal(err)
	}
	want := `exec '/opt/it'\''s/ai' multica-exec 'claude-code' -- "$@"`
	if !strings.Contains(string(content), want) {
		test.Errorf("shim = %q, want it to contain %q", content, want)
	}
	info, _ := os.Stat(filepath.Join(dir, "codex"))
	if info == nil || info.Mode()&0o111 == 0 {
		test.Error("codex shim missing or not executable")
	}
}

func TestPathWithout(test *testing.T) {
	list := strings.Join([]string{"/a", "/shims/", "/b", "/shims"}, string(os.PathListSeparator))
	got := PathWithout(list, "/shims")
	if want := strings.Join([]string{"/a", "/b"}, string(os.PathListSeparator)); got != want {
		test.Errorf("PathWithout = %q, want %q", got, want)
	}
}

func TestDaemonEnvPrependsShimDirOnce(test *testing.T) {
	test.Setenv("PATH", "/usr/bin")
	var pathEntries []string
	for _, entry := range DaemonEnv("/shims") {
		if strings.HasPrefix(entry, "PATH=") {
			pathEntries = append(pathEntries, entry)
		}
	}
	if len(pathEntries) != 1 || pathEntries[0] != "PATH=/shims"+string(os.PathListSeparator)+"/usr/bin" {
		test.Errorf("PATH entries = %v", pathEntries)
	}
}

func TestConfigureSetsServerAndAppURL(test *testing.T) {
	var calls []string
	run := func(_ []string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	if err := Configure(run, "http://multica.aip.local:18787"); err != nil {
		test.Fatal(err)
	}
	want := []string{"config set server_url http://multica.aip.local:18787", "config set app_url http://multica.aip.local:18787"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		test.Errorf("calls = %v, want %v", calls, want)
	}
	failing := func([]string, ...string) ([]byte, error) { return []byte("boom"), errors.New("exit 1") }
	if err := Configure(failing, "x"); err == nil {
		test.Error("expected Configure to surface a failing multica config set")
	}
}

func TestStartDaemonUsesShimPath(test *testing.T) {
	test.Setenv("PATH", "/usr/bin")
	var gotEnv []string
	run := func(env []string, _ ...string) ([]byte, error) { gotEnv = env; return nil, nil }
	if err := StartDaemon(run, "/shims"); err != nil {
		test.Fatal(err)
	}
	found := false
	for _, entry := range gotEnv {
		found = found || strings.HasPrefix(entry, "PATH=/shims")
	}
	if !found {
		test.Errorf("daemon not started with the shim PATH: %v", gotEnv)
	}
}

// Agents inherit the daemon's environment, so platform secrets must never reach it.
func TestDaemonEnvStripsPlatformSecrets(test *testing.T) {
	for _, name := range platformSecrets {
		test.Setenv(name, "leak-me")
	}
	test.Setenv("HOME_KEEP", "ok")
	kept := false
	for _, entry := range DaemonEnv("/shims") {
		if strings.Contains(entry, "leak-me") {
			test.Errorf("secret leaked into daemon env: %s", strings.SplitN(entry, "=", 2)[0])
		}
		kept = kept || entry == "HOME_KEEP=ok"
	}
	if !kept {
		test.Error("ordinary env vars must survive")
	}
}
