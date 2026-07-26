package uninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/hostsfile"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/uihosts"
)

// withHostsSeams redirects the /etc/hosts removal seams to a temp file + a stub
// writer for the duration of the test, restoring them after.
func withHostsSeams(t *testing.T, path string, write func(string, []byte) error) {
	t.Helper()
	origPath, origWriter := hostsPath, hostsWriter
	hostsPath, hostsWriter = path, write
	t.Cleanup(func() { hostsPath, hostsWriter = origPath, origWriter })
}

func TestRemoveHostsBlockStandaloneRemoves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleStandalone}); err != nil {
		t.Fatalf("persist runtime: %v", err)
	}
	hostsFilePath := filepath.Join(t.TempDir(), "hosts")
	if _, err := hostsfile.Apply(hostsFilePath, uihosts.Entries("aip.local")); err != nil {
		t.Fatalf("seed hosts: %v", err)
	}
	wrote := false
	withHostsSeams(t, hostsFilePath, func(_ string, content []byte) error {
		wrote = true
		return os.WriteFile(hostsFilePath, content, 0o644)
	})
	if !removeHostsBlock(func(string) {}) {
		t.Fatal("standalone with a present block should remove it")
	}
	if !wrote {
		t.Fatal("expected the privileged write to be invoked")
	}
	data, _ := os.ReadFile(hostsFilePath)
	if strings.Contains(string(data), "litellm.aip.local") {
		t.Fatalf("block not removed:\n%s", data)
	}
}

func TestRemoveHostsBlockServerSkips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleServer}); err != nil {
		t.Fatalf("persist runtime: %v", err)
	}
	hostsFilePath := filepath.Join(t.TempDir(), "hosts")
	if _, err := hostsfile.Apply(hostsFilePath, uihosts.Entries("aip.local")); err != nil {
		t.Fatalf("seed hosts: %v", err)
	}
	withHostsSeams(t, hostsFilePath, func(string, []byte) error {
		t.Fatal("server must NOT touch /etc/hosts")
		return nil
	})
	if removeHostsBlock(func(string) {}) {
		t.Fatal("server role must not remove the hosts block")
	}
}

func TestRemoveHostsBlockNoBlockNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleStandalone}); err != nil {
		t.Fatalf("persist runtime: %v", err)
	}
	hostsFilePath := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsFilePath, []byte("127.0.0.1\tlocalhost\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	withHostsSeams(t, hostsFilePath, func(string, []byte) error {
		t.Fatal("must not write when no managed block is present")
		return nil
	})
	if removeHostsBlock(func(string) {}) {
		t.Fatal("no block present → nothing removed")
	}
}

func TestStripManagedRemovesOnlyOurLines(test *testing.T) {
	input := strings.Join([]string{
		"# my real config",
		"export EDITOR=vim",
		"",
		`export PATH="X:$PATH"  ` + pathMarker,
		"",
		completionMarker,
		`fpath=("$HOME/.zsh/completions" $fpath)`,
		"autoload -Uz compinit && compinit",
		`alias gs="git status"`,
		"",
	}, "\n")

	got := stripManaged(input)

	for _, keep := range []string{"# my real config", "export EDITOR=vim", `alias gs="git status"`} {
		if !strings.Contains(got, keep) {
			test.Errorf("stripped output dropped a user line %q:\n%s", keep, got)
		}
	}
	for _, gone := range []string{pathMarker, completionMarker, "fpath=(", "compinit"} {
		if strings.Contains(got, gone) {
			test.Errorf("stripped output still contains %q:\n%s", gone, got)
		}
	}
}

func TestStripManagedNoMarkersUnchanged(test *testing.T) {
	input := "export EDITOR=vim\nalias gs=\"git status\"\n"
	if got := stripManaged(input); got != input {
		test.Errorf("a file with no managed lines must be unchanged:\n%q", got)
	}
}

// fakeProber records the commands it is asked to run and returns canned output.
type fakeProber struct {
	present map[string]bool   // binaries on PATH
	output  map[string][]byte // keyed by "name arg0 arg1 ..."
	ran     []string          // every Run invocation, joined
}

func (prober *fakeProber) LookPath(file string) (string, error) {
	if prober.present[file] {
		return "/usr/bin/" + file, nil
	}
	return "", os.ErrNotExist
}

func (prober *fakeProber) Run(name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	prober.ran = append(prober.ran, key)
	return prober.output[key], nil
}

func (prober *fakeProber) Exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRunFullTeardown(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	// Keep XDG dirs under the temp HOME so completion-file paths are scoped.
	test.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	test.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	test.Setenv("ZDOTDIR", "")

	// Lay down a prior install: binary, platform state, a project, completion
	// files, and a .zshrc carrying both managed blocks plus user lines.
	binaryPath := filepath.Join(home, ".ai-platform", "bin", "ai")
	mustWrite(test, binaryPath, "binary")
	mustWrite(test, filepath.Join(home, ".ai-platform", "config", "config.yaml"), "os: alma")
	projectFile := filepath.Join(home, "projects", "keepme", "main.go")
	mustWrite(test, projectFile, "package main")
	completionFile := filepath.Join(home, ".zsh", "completions", "_ai")
	mustWrite(test, completionFile, "#compdef ai")
	mustWrite(test, filepath.Join(home, ".zshrc"), strings.Join([]string{
		"export EDITOR=vim",
		`export PATH="x"  ` + pathMarker,
		completionMarker,
		"fpath=(x $fpath)",
		"autoload -Uz compinit && compinit",
		`alias gs="git status"`,
		"",
	}, "\n"))

	prober := &fakeProber{
		present: map[string]bool{"docker": true},
		output:  map[string][]byte{"docker ps -aq --filter name=aip-": []byte("abc123\ndef456\n")},
	}
	// Isolate the /etc/hosts removal seam to a temp file so Run never reaches the
	// real privileged sudo writer on a developer machine that ran `ai setup`.
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	var steps []string
	report, err := Run(Options{Purge: true, BinaryPath: binaryPath}, prober, func(line string) {
		steps = append(steps, line)
	})
	if err != nil {
		test.Fatalf("Run: %v", err)
	}

	if report.RemovedContainers != 2 {
		test.Errorf("RemovedContainers = %d, want 2", report.RemovedContainers)
	}
	if report.RemovedBinary != binaryPath {
		test.Errorf("RemovedBinary = %q, want %q", report.RemovedBinary, binaryPath)
	}
	if !report.Purged {
		test.Error("Purged = false, want true")
	}
	// docker rm -f was issued with both ids.
	if !containsLine(prober.ran, "docker rm -f abc123 def456") {
		test.Errorf("expected `docker rm -f abc123 def456`, ran: %v", prober.ran)
	}
	// Binary, completion file, and platform state are gone; project survives.
	for _, gone := range []string{binaryPath, completionFile, filepath.Join(home, ".ai-platform")} {
		if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
			test.Errorf("expected %q removed, but it still exists", gone)
		}
	}
	if _, statErr := os.Stat(projectFile); statErr != nil {
		test.Errorf("project source must survive uninstall: %v", statErr)
	}
	// rc keeps the user's lines, drops ours.
	rc, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if !strings.Contains(string(rc), `alias gs="git status"`) || !strings.Contains(string(rc), "export EDITOR=vim") {
		test.Errorf("user rc lines were dropped:\n%s", rc)
	}
	if strings.Contains(string(rc), pathMarker) || strings.Contains(string(rc), "compinit") {
		test.Errorf("managed rc lines survived:\n%s", rc)
	}
	if len(steps) == 0 {
		test.Error("expected progress lines, got none")
	}
	// A transcript is written to ~/ai-uninstall.log and survives --purge.
	if report.LogPath != filepath.Join(home, LogName) {
		test.Errorf("LogPath = %q, want %q", report.LogPath, filepath.Join(home, LogName))
	}
	logBytes, logErr := os.ReadFile(report.LogPath)
	if logErr != nil {
		test.Fatalf("uninstall log not written: %v", logErr)
	}
	logText := string(logBytes)
	for _, want := range []string{"=== ai uninstall", "Removed platform containers", "container images via docker", "Purged ~/.ai-platform", "=== uninstall finished ==="} {
		if !strings.Contains(logText, want) {
			test.Errorf("uninstall log missing %q:\n%s", want, logText)
		}
	}
}

func TestRunIdempotentOnCleanHome(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })
	report, err := Run(Options{BinaryPath: filepath.Join(home, "nope", "ai")}, prober, nil)
	if err != nil {
		test.Fatalf("Run on clean home should not error: %v", err)
	}
	if report.RemovedContainers != 0 || report.RemovedBinary != "" || len(report.CleanedRC) != 0 {
		test.Errorf("expected an empty report on a clean home, got %+v", report)
	}
}

// TestPlan pins the --dry-run step list to the keep-models-by-default semantics: a
// plain uninstall removes ~/.ai-platform but keeps volumes/models; --purge removes
// everything. Both leave project directories untouched.
func TestPlan(test *testing.T) {
	plain := strings.Join(Plan(false), "\n")
	purged := strings.Join(Plan(true), "\n")

	if !strings.Contains(plain, "KEEP downloaded models") {
		test.Errorf("Plan(false) must keep the models:\n%s", plain)
	}
	if strings.Contains(plain, "including downloaded models") {
		test.Errorf("Plan(false) must not claim it removes the models:\n%s", plain)
	}
	if !strings.Contains(purged, "including downloaded models") {
		test.Errorf("Plan(true) must remove the models too:\n%s", purged)
	}
	for _, steps := range []string{plain, purged} {
		if !strings.Contains(steps, "leave your project directories untouched") {
			test.Errorf("every plan must promise project dirs are untouched:\n%s", steps)
		}
		// Both plans (plain AND purge) must announce image removal — images are gone
		// on every uninstall, not only under --purge.
		if !strings.Contains(steps, "remove all platform container images") {
			test.Errorf("every plan must announce container-image removal:\n%s", steps)
		}
	}
}

// TestRemoveImages: uninstall removes BOTH the pinned service-tier images (one
// `rmi -f <ref>` per versions.Default() entry) and every aip-* workspace image
// (one `rmi -f <ids…>` from the reference filter), counting them and recording a
// human line — run on a PLAIN uninstall (no --purge needed).
func TestRemoveImages(test *testing.T) {
	prober := &fakeProber{
		present: map[string]bool{"docker": true},
		output:  map[string][]byte{"docker images --filter reference=aip-* -q": []byte("img1\nimg2\n")},
	}
	var lines []string
	removed := removeImages(prober, func(line string) { lines = append(lines, line) })

	// (b) the aip-* workspace images were removed in one `rmi -f`.
	if !containsLine(prober.ran, "docker rmi -f img1 img2") {
		test.Errorf("expected `docker rmi -f img1 img2`, ran: %v", prober.ran)
	}
	// (a) each pinned service-tier ref got its own `rmi -f <ref>`. Assert the litellm
	// pin specifically — the service the user reported not updating.
	refs := serviceImageRefs()
	if len(refs) == 0 {
		test.Fatal("versions.Default() must pin at least one service image")
	}
	litellmRef := ""
	for _, ref := range refs {
		if strings.Contains(ref, "litellm") && !strings.Contains(ref, "litellm-db") {
			litellmRef = ref
		}
	}
	if litellmRef == "" {
		test.Fatal("versions.Default() must pin a litellm image")
	}
	if !containsLine(prober.ran, "docker rmi -f "+litellmRef) {
		test.Errorf("expected `docker rmi -f %s`, ran: %v", litellmRef, prober.ran)
	}
	// Count = every pinned service ref (the fake `rmi` always succeeds) + the 2
	// aip-* image ids.
	if want := len(refs) + 2; removed != want {
		test.Errorf("removed = %d, want %d", removed, want)
	}
	if strings.Join(lines, "\n") == "" || !strings.Contains(strings.Join(lines, "\n"), "container images via docker") {
		test.Errorf("expected a 'container images via docker' record line, got: %v", lines)
	}

	// No container runtime installed → nothing removed, no line.
	none := &fakeProber{present: map[string]bool{}}
	if got := removeImages(none, func(string) {}); got != 0 {
		test.Errorf("with no runtime, removeImages must remove nothing, got %d", got)
	}
}

// TestStopWorkspaces: uninstall stops running workspace microVMs (aip-* sandboxes
// from `msb list`) via `msb stop -f <name>`, skips non-platform VMs, preserves DATA
// (it only stops — never `msb delete`, never a filesystem removal), and records a
// "data preserved" line.
func TestStopWorkspaces(test *testing.T) {
	test.Setenv("HOME", test.TempDir()) // no pinned ~/.ai-platform/bin/msb → resolves bare "msb"
	listing := "NAME        STATE\naip-demo    running\nother-vm    running\n"
	prober := &fakeProber{
		present: map[string]bool{"msb": true},
		output:  map[string][]byte{"msb list": []byte(listing)},
	}
	var lines []string
	stopped := stopWorkspaces(prober, func(line string) { lines = append(lines, line) })

	if stopped != 1 {
		test.Errorf("stopped = %d, want 1 (only the aip-* sandbox)", stopped)
	}
	if !containsLine(prober.ran, "msb stop -f aip-demo") {
		test.Errorf("expected `msb stop -f aip-demo`, ran: %v", prober.ran)
	}
	// The non-platform VM must NOT be stopped.
	if containsLine(prober.ran, "msb stop -f other-vm") {
		test.Errorf("must not stop non-platform VMs, ran: %v", prober.ran)
	}
	// DATA-preserving: only `msb list` + `msb stop` were run — no delete/rm of any kind.
	for _, cmd := range prober.ran {
		if strings.Contains(cmd, "delete") || strings.Contains(cmd, "rm ") || strings.Contains(cmd, "remove") {
			test.Errorf("stopWorkspaces must not remove data, but ran: %q", cmd)
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), "data preserved") {
		test.Errorf("expected a 'data preserved' record line, got: %v", lines)
	}

	// No msb on PATH → nothing to stop, no line.
	none := &fakeProber{present: map[string]bool{}}
	if got := stopWorkspaces(none, func(string) {}); got != 0 {
		test.Errorf("with no msb, stopWorkspaces must stop nothing, got %d", got)
	}
}

func TestRunKeepsModelsWithoutPurge(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")

	// Lay down platform state: config, cache, credentials env file, a legacy
	// litellm-db volume, and the (expensive) downloaded model store.
	configFile := filepath.Join(home, ".ai-platform", "config", "config.yaml")
	cacheFile := filepath.Join(home, ".ai-platform", "cache", "catalog.yaml")
	envFile := filepath.Join(home, ".ai-platform", ".ai-platform.env")
	dbFile := filepath.Join(home, ".ai-platform", "volumes", "litellm-db", "PG_VERSION")
	modelBlob := filepath.Join(home, ".ai-platform", "volumes", "models", "blobs", "sha256-abc")
	for _, path := range []string{configFile, cacheFile, envFile, dbFile, modelBlob} {
		mustWrite(test, path, "x")
	}

	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	report, err := Run(Options{}, prober, nil) // plain uninstall, no purge
	if err != nil {
		test.Fatalf("Run: %v", err)
	}
	if !report.RemovedState {
		test.Error("RemovedState = false, want true")
	}
	if report.Purged {
		test.Error("Purged = true on a non-purge uninstall")
	}

	// Everything but the model store is gone.
	for _, gone := range []string{configFile, cacheFile, envFile, dbFile} {
		if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
			test.Errorf("expected %q removed on plain uninstall, but it still exists", gone)
		}
	}
	// The downloaded models survive for a reinstall.
	if _, statErr := os.Stat(modelBlob); statErr != nil {
		test.Errorf("downloaded models must survive a non-purge uninstall: %v", statErr)
	}
}

func TestExternalDepsPresenceAndRemoval(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("MSB_HOME", "")

	prober := &fakeProber{present: map[string]bool{}} // nothing on PATH

	// Use msb: all of its targets are HOME-relative, so presence is isolated to
	// the temp HOME.
	var msb ExternalDep
	for _, dep := range ExternalDeps() {
		if dep.Binary == "msb" {
			msb = dep
		}
	}

	// Absent on a clean HOME.
	if msb.Present(prober) {
		test.Error("msb should not be detected on a clean HOME")
	}

	// Install msb: ~/.microsandbox dir + a ~/.local/bin/msb symlink target.
	msbHome := filepath.Join(home, ".microsandbox")
	mustWrite(test, filepath.Join(msbHome, "bin", "msb"), "x")
	mustWrite(test, filepath.Join(home, ".local", "bin", "msb"), "x")
	if !msb.Present(prober) {
		test.Error("msb should be detected present (install dir exists)")
	}

	// Removing msb via Run clears its targets.
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })
	report, err := Run(Options{RemoveDeps: []ExternalDep{msb}}, prober, nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(report.RemovedDeps) != 1 || report.RemovedDeps[0] != msb.Name {
		test.Errorf("RemovedDeps = %v, want [%q]", report.RemovedDeps, msb.Name)
	}
	if _, statErr := os.Stat(msbHome); !os.IsNotExist(statErr) {
		test.Error("~/.microsandbox should be removed")
	}
	if _, statErr := os.Stat(filepath.Join(home, ".local", "bin", "msb")); !os.IsNotExist(statErr) {
		test.Error("~/.local/bin/msb symlink should be removed")
	}
}

func mustWrite(test *testing.T, path, content string) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// TestStopHostOllamaContainerModeNoop: in container mode (and with no runtime.yaml
// at all) uninstall attempts NO host-native Ollama teardown — the container-mode
// path is unchanged.
func TestStopHostOllamaContainerModeNoop(test *testing.T) {
	// (a) No runtime.yaml at all → defaults to container → no-op.
	test.Setenv("HOME", test.TempDir())
	prober := &fakeProber{present: map[string]bool{}}
	if stopHostOllama(prober, func(string) {}) {
		test.Error("no runtime.yaml should resolve to container mode (no host teardown)")
	}
	if containsLine(prober.ran, "pkill -f ollama serve") {
		test.Errorf("container mode must not run pkill, ran: %v", prober.ran)
	}

	// (b) Explicit container mode persisted → still a no-op.
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, OllamaMode: runtime.OllamaModeContainer}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	prober = &fakeProber{present: map[string]bool{}}
	if stopHostOllama(prober, func(string) {}) {
		test.Error("container mode should not attempt host Ollama teardown")
	}
	if len(prober.ran) != 0 {
		test.Errorf("container mode must run no commands, ran: %v", prober.ran)
	}
}

// TestStopHostOllamaHostMode: in host mode uninstall attempts the best-effort
// host-native `ollama serve` stop (the hardware bring-up pkill stub) and records it.
func TestStopHostOllamaHostMode(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, OllamaMode: runtime.OllamaModeHost}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	prober := &fakeProber{present: map[string]bool{}}
	var lines []string
	if !stopHostOllama(prober, func(line string) { lines = append(lines, line) }) {
		test.Fatal("host mode should attempt host-native Ollama teardown")
	}
	if !containsLine(prober.ran, "pkill -f ollama serve") {
		test.Errorf("expected `pkill -f ollama serve`, ran: %v", prober.ran)
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "host-native Ollama") {
		test.Errorf("expected a recorded host-Ollama teardown line, got: %v", lines)
	}
}

// TestRunHostOllamaStopsAndKeepsModels: a full plain uninstall in host mode stops
// the host-native Ollama process (reflected in Report.StoppedHostOllama) and NEVER
// removes the host model store (volumes/models is expensive to refetch — only
// --purge removes it).
func TestRunHostOllamaStopsAndKeepsModels(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, OllamaMode: runtime.OllamaModeHost}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	modelBlob := filepath.Join(home, ".ai-platform", "volumes", "models", "blobs", "sha256-abc")
	mustWrite(test, modelBlob, "x")

	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	report, err := Run(Options{}, prober, nil) // plain uninstall, no purge
	if err != nil {
		test.Fatalf("Run: %v", err)
	}
	if !report.StoppedHostOllama {
		test.Error("StoppedHostOllama = false, want true in host mode")
	}
	if !containsLine(prober.ran, "pkill -f ollama serve") {
		test.Errorf("expected host-Ollama stop attempt, ran: %v", prober.ran)
	}
	// The downloaded host model store must survive a plain (non-purge) uninstall.
	if _, statErr := os.Stat(modelBlob); os.IsNotExist(statErr) {
		test.Error("host model store (volumes/models) must be kept on a plain uninstall")
	}
}
