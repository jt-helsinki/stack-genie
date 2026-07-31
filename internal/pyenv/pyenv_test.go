package pyenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapExec captures the lookPath/runCommand seams and restores them on cleanup.
func swapExec(test *testing.T) {
	test.Helper()
	originalLook, originalRun := lookPath, runCommand
	test.Cleanup(func() { lookPath, runCommand = originalLook, originalRun })
}

func TestDirAndBinPaths(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	dir, err := Dir()
	if err != nil {
		test.Fatalf("Dir: %v", err)
	}
	if want := filepath.Join(home, ".ai-platform", "venv"); dir != want {
		test.Errorf("Dir = %q, want %q", dir, want)
	}
	python, err := PythonPath()
	if err != nil {
		test.Fatalf("PythonPath: %v", err)
	}
	if want := filepath.Join(dir, "bin", "python"); python != want {
		test.Errorf("PythonPath = %q, want %q", python, want)
	}
	if _, err := BinPath(""); err == nil {
		test.Error("BinPath(\"\") must error")
	}
}

func TestExists(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	present, err := Exists()
	if err != nil {
		test.Fatalf("Exists (absent): %v", err)
	}
	if present {
		test.Error("Exists must be false before the venv is created")
	}

	python, _ := PythonPath()
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		test.Fatal(err)
	}
	present, err = Exists()
	if err != nil {
		test.Fatalf("Exists (present): %v", err)
	}
	if !present {
		test.Error("Exists must be true once <venv>/bin/python exists")
	}
}

// Ensure prefers uv and requests the pinned interpreter.
func TestEnsurePrefersUV(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)

	var ran [][]string
	lookPath = func(name string) (string, error) {
		if name == "uv" {
			return "/usr/bin/uv", nil
		}
		return "", errors.New("not found")
	}
	runCommand = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{name}, args...))
		return nil, nil
	}

	created, err := Ensure()
	if err != nil {
		test.Fatalf("Ensure: %v", err)
	}
	if !created {
		test.Error("Ensure must report created=true on first creation")
	}
	if len(ran) != 1 || ran[0][0] != "uv" {
		test.Fatalf("expected a single `uv` invocation, got %v", ran)
	}
	joined := strings.Join(ran[0], " ")
	if !strings.Contains(joined, "venv") || !strings.Contains(joined, preferredPython) {
		test.Errorf("uv invocation must create a venv with the pinned python: %q", joined)
	}
}

// Ensure falls back to `python3 -m venv` when uv is absent.
func TestEnsureFallsBackToPython3(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)

	var ran [][]string
	lookPath = func(name string) (string, error) {
		if name == "python3" {
			return "/usr/bin/python3", nil
		}
		return "", errors.New("not found")
	}
	runCommand = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{name}, args...))
		return nil, nil
	}

	created, err := Ensure()
	if err != nil {
		test.Fatalf("Ensure: %v", err)
	}
	if !created {
		test.Error("Ensure must report created=true")
	}
	if len(ran) != 1 || !strings.HasSuffix(ran[0][0], "python3") {
		test.Fatalf("expected a python3 fallback invocation, got %v", ran)
	}
	joined := strings.Join(ran[0], " ")
	if !strings.Contains(joined, "-m venv") {
		test.Errorf("python3 fallback must be `-m venv`: %q", joined)
	}
}

// Ensure errors when neither uv nor python3 is available.
func TestEnsureNoToolchain(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	runCommand = func(string, ...string) ([]byte, error) { return nil, errors.New("should not run") }

	if _, err := Ensure(); err == nil {
		test.Fatal("Ensure must error when neither uv nor python3 is present")
	}
}

// Ensure is idempotent — a second call with the venv already present is a no-op.
func TestEnsureIdempotent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)
	python, _ := PythonPath()
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		test.Fatal(err)
	}
	runCommand = func(string, ...string) ([]byte, error) {
		test.Fatal("Ensure must not run any command when the venv already exists")
		return nil, nil
	}
	lookPath = func(string) (string, error) { return "", errors.New("unused") }

	created, err := Ensure()
	if err != nil {
		test.Fatalf("Ensure (idempotent): %v", err)
	}
	if created {
		test.Error("Ensure must report created=false when the venv already exists")
	}
}

// PipInstall requires the venv to exist first.
func TestPipInstallRequiresVenv(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)
	if err := PipInstall("vllm"); err == nil {
		test.Error("PipInstall must error before the venv is created")
	}
	if err := PipInstall(); err == nil {
		test.Error("PipInstall with no specs must error")
	}
}

// PipInstall prefers uv with an explicit --python pointing at the venv interpreter.
func TestPipInstallPrefersUV(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)
	python, _ := PythonPath()
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		test.Fatal(err)
	}

	var ran [][]string
	lookPath = func(name string) (string, error) {
		if name == "uv" {
			return "/usr/bin/uv", nil
		}
		return "", errors.New("not found")
	}
	runCommand = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{name}, args...))
		return nil, nil
	}
	if err := PipInstall("vllm"); err != nil {
		test.Fatalf("PipInstall: %v", err)
	}
	if len(ran) != 1 {
		test.Fatalf("expected one install invocation, got %v", ran)
	}
	joined := strings.Join(ran[0], " ")
	if !strings.HasPrefix(joined, "uv pip install --python ") || !strings.Contains(joined, python) || !strings.HasSuffix(joined, "vllm") {
		test.Errorf("uv pip install must target the venv python: %q", joined)
	}
}

// PipInstall falls back to the venv's own pip when uv is absent.
func TestPipInstallFallsBackToVenvPip(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapExec(test)
	python, _ := PythonPath()
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		test.Fatal(err)
	}

	var ran [][]string
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	runCommand = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{name}, args...))
		return nil, nil
	}
	if err := PipInstall("vllm"); err != nil {
		test.Fatalf("PipInstall: %v", err)
	}
	pip, _ := BinPath("pip")
	if len(ran) != 1 || ran[0][0] != pip {
		test.Fatalf("expected the venv pip, got %v (want %s)", ran, pip)
	}
}
