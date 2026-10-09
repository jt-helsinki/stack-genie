package uninstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/hostsfile"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/state"
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

	if report.StoppedContainers != 2 {
		test.Errorf("StoppedContainers = %d, want 2", report.StoppedContainers)
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
	// docker stop ran BEFORE docker rm -f, both with both ids — a graceful stop
	// (letting e.g. postgres checkpoint cleanly) precedes the force-remove.
	stopIndex := indexOfLine(prober.ran, "docker stop abc123 def456")
	rmIndex := indexOfLine(prober.ran, "docker rm -f abc123 def456")
	if stopIndex < 0 {
		test.Errorf("expected `docker stop abc123 def456`, ran: %v", prober.ran)
	}
	if rmIndex < 0 {
		test.Errorf("expected `docker rm -f abc123 def456`, ran: %v", prober.ran)
	}
	if stopIndex >= 0 && rmIndex >= 0 && stopIndex > rmIndex {
		test.Errorf("stop must run BEFORE rm -f, ran: %v", prober.ran)
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

// seedProjectIndex registers a project at path in the global index (same shape
// internal/project.Scaffold produces), so stopWorkspaces can match a session's
// Workdir against a REGISTERED project root.
func seedProjectIndex(test *testing.T, name, path string) {
	test.Helper()
	index, err := state.LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if index.Projects == nil {
		index.Projects = map[string]state.ProjectIndexEntry{}
	}
	index.Projects[name] = state.ProjectIndexEntry{Path: path}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
}

// TestStopWorkspaces: uninstall stops every RUNNING nono session whose working
// directory matches a REGISTERED project (skipping a session belonging to a
// directory the platform never registered), preserves DATA (it only stops a
// session — never deletes a project's files), and records a "data preserved"
// line.
func TestStopWorkspaces(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	projectRoot := filepath.Join(home, "myproject")
	seedProjectIndex(test, "myproject", projectRoot)

	runner := &nono.FakeRunner{Sessions: []nono.Session{
		{SessionID: "sess-1", Workdir: projectRoot, Status: "running"},
		{SessionID: "sess-2", Workdir: "/some/unregistered/dir", Status: "running"},
	}}
	original := nonoRunnerFn
	defer func() { nonoRunnerFn = original }()
	nonoRunnerFn = func() nono.Runner { return runner }

	var lines []string
	stopped := stopWorkspaces(func(line string) { lines = append(lines, line) })

	if stopped != 1 {
		test.Errorf("stopped = %d, want 1 (only the registered project's session)", stopped)
	}
	if len(runner.StopCalls) != 1 || runner.StopCalls[0].SessionID != "sess-1" {
		test.Errorf("expected exactly one Stop call for sess-1, got: %+v", runner.StopCalls)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "data preserved") {
		test.Errorf("expected a 'data preserved' record line, got: %v", lines)
	}

	// No running sessions at all → nothing to stop, no line.
	nonoRunnerFn = func() nono.Runner { return &nono.FakeRunner{} }
	if got := stopWorkspaces(func(string) {}); got != 0 {
		test.Errorf("with no sessions, stopWorkspaces must stop nothing, got %d", got)
	}

	// nono unreachable (e.g. not installed) → best-effort no-op, never fails.
	nonoRunnerFn = func() nono.Runner { return &nono.FakeRunner{ListSessionsErr: errors.New("nono unreachable")} }
	if got := stopWorkspaces(func(string) {}); got != 0 {
		test.Errorf("with nono unreachable, stopWorkspaces must stop nothing, got %d", got)
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

// withFakeNonoOnPath puts an executable named "nono" on PATH for the duration
// of the test and returns its resolved path — ExternalDeps() resolves nono's
// removal target via a real exec.LookPath, not a fixed HOME-relative location
// (nono's own installer, unlike the old platform-pinned msb binary, does not
// publish one fixed install path).
func withFakeNonoOnPath(test *testing.T) string {
	test.Helper()
	dir := test.TempDir()
	path := filepath.Join(dir, "nono")
	mustWrite(test, path, "#!/bin/sh\n")
	if err := os.Chmod(path, 0o755); err != nil {
		test.Fatal(err)
	}
	test.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestExternalDepsPresenceAndRemoval(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("PATH", "") // start with nothing resolvable

	prober := &fakeProber{present: map[string]bool{}} // nothing on PATH, per fakeProber

	// Absent with nothing on PATH.
	deps := ExternalDeps()
	if len(deps) != 1 || deps[0].Binary != "nono" {
		test.Fatalf("ExternalDeps() = %+v, want exactly one nono entry", deps)
	}
	if len(deps[0].Targets) != 0 {
		test.Errorf("nono dep should have no targets when not on PATH, got %v", deps[0].Targets)
	}

	// Put a fake nono binary on PATH — ExternalDeps() should target its exact
	// resolved location.
	nonoPath := withFakeNonoOnPath(test)
	nonoDep := ExternalDeps()[0]
	if len(nonoDep.Targets) != 1 || nonoDep.Targets[0] != nonoPath {
		test.Errorf("nono dep targets = %v, want [%q]", nonoDep.Targets, nonoPath)
	}

	// Removing nono via Run clears its resolved target.
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })
	report, err := Run(Options{RemoveDeps: []ExternalDep{nonoDep}}, prober, nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(report.RemovedDeps) != 1 || report.RemovedDeps[0] != nonoDep.Name {
		test.Errorf("RemovedDeps = %v, want [%q]", report.RemovedDeps, nonoDep.Name)
	}
	if _, statErr := os.Stat(nonoPath); !os.IsNotExist(statErr) {
		test.Error("the resolved nono binary should be removed")
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

// indexOfLine returns the position of want in lines, or -1 if absent — used to
// assert relative ordering between two recorded commands.
func indexOfLine(lines []string, want string) int {
	for index, line := range lines {
		if line == want {
			return index
		}
	}
	return -1
}

// TestRunStopsOmlxAndKeepsModels: a full plain uninstall stops the host-native omlx
// servers (omlx is the sole local runtime now) and NEVER removes the host model store
// (volumes/models is expensive to refetch — only --purge removes it).
func TestRunStopsOmlxAndKeepsModels(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	modelBlob := filepath.Join(home, ".ai-platform", "volumes", "models", "omlx", "weights.safetensors")
	mustWrite(test, modelBlob, "x")

	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	report, err := Run(Options{}, prober, nil) // plain uninstall, no purge
	if err != nil {
		test.Fatalf("Run: %v", err)
	}
	if report.RemovedOmlx {
		test.Error("RemovedOmlx = true, want false on a plain uninstall (runtime kept)")
	}
	if !containsLine(prober.ran, "pkill -f omlx serve") {
		test.Errorf("expected a omlx server stop attempt, ran: %v", prober.ran)
	}
	// The downloaded host model store must survive a plain (non-purge) uninstall.
	if _, statErr := os.Stat(modelBlob); os.IsNotExist(statErr) {
		test.Error("host model store (volumes/models) must be kept on a plain uninstall")
	}
}

// TestRunRemoveRuntimesStopsAndRemoves: opting into runtime removal (the TTY
// default-yes / no --keep-runtimes) attempts removal of the host-native omlx runtime,
// records it in the Report, and STILL keeps the downloaded model store (only --purge
// removes that).
func TestRunRemoveRuntimesStopsAndRemoves(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	omlxBlob := filepath.Join(home, ".ai-platform", "volumes", "models", "omlx", "weights.safetensors")
	mustWrite(test, omlxBlob, "x")

	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	report, err := Run(Options{RemoveRuntimes: true}, prober, nil) // no purge
	if err != nil {
		test.Fatalf("Run: %v", err)
	}
	if !report.RemovedOmlx {
		test.Error("RemovedOmlx = false, want true when RemoveRuntimes is set")
	}
	if !containsLine(prober.ran, "pkill -f omlx serve") {
		test.Errorf("expected a omlx server stop attempt, ran: %v", prober.ran)
	}
	// Removing the RUNTIME must never delete the downloaded MODELS on a plain uninstall.
	if _, statErr := os.Stat(omlxBlob); os.IsNotExist(statErr) {
		test.Error("omlx model store must be kept when removing the runtime on a plain uninstall")
	}
}

// TestRunKeepRuntimesSkipsRemoval: without RemoveRuntimes (the --keep-runtimes /
// prompt-no path) the omlx runtime is not removed, though its running servers are
// still stopped (the platform is being torn down).
func TestRunKeepRuntimesSkipsRemoval(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	report, err := Run(Options{}, prober, nil) // RemoveRuntimes defaults false
	if err != nil {
		test.Fatalf("Run: %v", err)
	}
	if report.RemovedOmlx {
		test.Errorf("the omlx runtime must be kept when RemoveRuntimes is false, got removed=%t", report.RemovedOmlx)
	}
}

// TestRunPurgeWithRuntimesRemovesModelStore: --purge RemoveAll's ~/.ai-platform
// (models included) even alongside runtime removal.
func TestRunPurgeWithRuntimesRemovesModelStore(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("ZDOTDIR", "")
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion}); err != nil {
		test.Fatalf("persist runtime: %v", err)
	}
	modelBlob := filepath.Join(home, ".ai-platform", "volumes", "models", "blobs", "sha256-abc")
	mustWrite(test, modelBlob, "x")

	prober := &fakeProber{present: map[string]bool{}}
	withHostsSeams(test, filepath.Join(home, "etc-hosts"), func(string, []byte) error { return nil })

	report, err := Run(Options{Purge: true, RemoveRuntimes: true}, prober, nil)
	if err != nil {
		test.Fatalf("Run: %v", err)
	}
	if !report.Purged {
		test.Error("Purged = false, want true")
	}
	if _, statErr := os.Stat(modelBlob); !os.IsNotExist(statErr) {
		test.Error("--purge must RemoveAll ~/.ai-platform including the downloaded models")
	}
}
