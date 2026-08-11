package hf

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCuratedModelsPerOS(t *testing.T) {
	darwin := CuratedModels("darwin")
	if len(darwin) == 0 {
		t.Fatal("expected a non-empty darwin curated list")
	}
	for _, model := range darwin {
		if model.Repo == "" || model.Name == "" {
			t.Fatalf("darwin curated model missing name/repo: %+v", model)
		}
		if got := model.Repo[:len("mlx-community/")]; got != "mlx-community/" {
			t.Fatalf("darwin curated repo %q is not mlx-community/*", model.Repo)
		}
	}
	linux := CuratedModels("linux")
	if len(linux) == 0 {
		t.Fatal("expected a non-empty linux curated list")
	}
	for _, model := range linux {
		if len(model.Repo) >= len("mlx-community/") && model.Repo[:len("mlx-community/")] == "mlx-community/" {
			t.Fatalf("linux curated repo %q must not be mlx-community/*", model.Repo)
		}
	}
	// The returned slice is a copy — mutating it must not affect the next call.
	darwin[0].Name = "mutated"
	if CuratedModels("darwin")[0].Name == "mutated" {
		t.Fatal("CuratedModels leaked its backing array")
	}
}

func TestStoreDirMirrorsVLLM(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := StoreDir()
	if err != nil {
		t.Fatalf("StoreDir: %v", err)
	}
	want := filepath.Join(home, ".ai-platform", "volumes", "models", "vllm")
	if dir != want {
		t.Fatalf("StoreDir = %q, want %q", dir, want)
	}
}

func TestBinaryPathFallsBackToPATH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No venv hf present → the bare "hf" name for a PATH lookup.
	if got := BinaryPath(); got != "hf" {
		t.Fatalf("BinaryPath with no venv = %q, want %q", got, "hf")
	}
}

func TestDetectUsesLookPath(t *testing.T) {
	original := lookPath
	t.Cleanup(func() { lookPath = original })

	lookPath = func(string) (string, error) { return "/usr/bin/hf", nil }
	if !Detect() {
		t.Fatal("Detect should be true when the binary resolves")
	}
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if Detect() {
		t.Fatal("Detect should be false when the binary does not resolve")
	}
}

func TestInstallUsesPyenvSeams(t *testing.T) {
	origEnsure, origPip := pyenvEnsureFn, pyenvPipInstallFn
	t.Cleanup(func() { pyenvEnsureFn, pyenvPipInstallFn = origEnsure, origPip })

	ensured := false
	var installed []string
	pyenvEnsureFn = func() (bool, error) { ensured = true; return true, nil }
	pyenvPipInstallFn = func(specs ...string) error { installed = specs; return nil }

	if err := Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !ensured {
		t.Fatal("Install must ensure the venv")
	}
	if len(installed) != 2 || installed[0] != "-U" || installed[1] != pipInstallPackage {
		t.Fatalf("Install pip specs = %v, want [-U %s]", installed, pipInstallPackage)
	}
}

func TestInstallPropagatesEnsureError(t *testing.T) {
	origEnsure, origPip := pyenvEnsureFn, pyenvPipInstallFn
	t.Cleanup(func() { pyenvEnsureFn, pyenvPipInstallFn = origEnsure, origPip })

	wantErr := errors.New("no python")
	pyenvEnsureFn = func() (bool, error) { return false, wantErr }
	pyenvPipInstallFn = func(...string) error { t.Fatal("pip must not run when ensure fails"); return nil }

	if err := Install(); !errors.Is(err, wantErr) {
		t.Fatalf("Install err = %v, want %v", err, wantErr)
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	fake := &Fake{
		Cached:        []CachedModel{{Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit"}},
		DownloadLines: []string{"Fetching 1 files", "done"},
	}
	var lines []string
	if err := fake.Download("a/b", func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if fake.DownloadedRepo != "a/b" || len(fake.DownloadedAll) != 1 {
		t.Fatalf("Download not recorded: %+v", fake)
	}
	if len(lines) != 2 {
		t.Fatalf("progress lines = %v, want 2", lines)
	}
	cached, err := fake.CacheList()
	if err != nil || len(cached) != 1 {
		t.Fatalf("CacheList = %v, %v", cached, err)
	}
	if err := fake.CacheRemove("a/b"); err != nil {
		t.Fatalf("CacheRemove: %v", err)
	}
	if fake.RemovedRepo != "a/b" {
		t.Fatalf("CacheRemove not recorded: %+v", fake)
	}
}

// The Fake records the token passed to Login and surfaces its configured error, and
// Whoami returns the canned user / not-logged-in error — so the CLI is testable without
// a real `hf` (mirroring how Download/CacheList are faked).
func TestFakeAuthRecordsCalls(t *testing.T) {
	fake := &Fake{WhoamiUser: "alice"}
	if err := fake.Login("hf_token_123"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if fake.LoginToken != "hf_token_123" {
		t.Fatalf("Login token = %q, want the passed token", fake.LoginToken)
	}
	user, err := fake.Whoami()
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if user != "alice" {
		t.Fatalf("Whoami user = %q, want alice", user)
	}
	if err := fake.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if fake.LogoutCalls != 1 {
		t.Fatalf("LogoutCalls = %d, want 1", fake.LogoutCalls)
	}

	// Configured errors are surfaced: a login failure and a not-logged-in whoami.
	loginErr := errors.New("bad token")
	notLoggedIn := errors.New("Not logged in")
	failing := &Fake{LoginErr: loginErr, WhoamiErr: notLoggedIn}
	if err := failing.Login("x"); !errors.Is(err, loginErr) {
		t.Fatalf("Login err = %v, want %v", err, loginErr)
	}
	if _, err := failing.Whoami(); !errors.Is(err, notLoggedIn) {
		t.Fatalf("Whoami err = %v, want %v", err, notLoggedIn)
	}
}
