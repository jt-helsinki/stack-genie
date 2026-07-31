// Package pyenv manages the platform-managed, HOST-SIDE Python virtual environment
// at ~/.ai-platform/venv (paths.VenvDir). It is the single home for platform-wide
// Python tooling that runs on the host — most importantly the vLLM local-inference
// backend — as opposed to a workspace's in-VM `.venv-msb`.
//
// The venv is created at `ai setup` (best-effort, never fatal) via `uv venv` when uv
// is on PATH (uv can fetch a suitable CPython), else `python3 -m venv`. Once it
// exists, the platform resolves its Python tools from it (e.g. vllm resolves
// <venv>/bin/vllm before falling back to PATH), so the platform "uses that
// environment to run its Python code".
//
// All host mutation (venv creation, pip installs) is a documented, self-contained
// action under ~/.ai-platform — removed by `ai uninstall --purge`. The exec seams
// (lookPath / runCommand) are injectable so the package is unit-tested without a real
// Python toolchain.
package pyenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// preferredPython is the interpreter version requested from uv when it creates the
// venv. vLLM on Apple Silicon requires Python 3.11+; uv fetches a matching CPython
// when the host lacks one. The `python3 -m venv` fallback uses whatever `python3`
// resolves to (which must itself be 3.11+ for a subsequent vLLM install).
const preferredPython = "3.11"

// lookPath locates a host binary (uv / python3). Injectable seam for tests.
var lookPath = exec.LookPath

// runCommand runs a command to completion, returning its combined output on failure
// so the caller can surface why a venv/pip step failed. Injectable seam for tests.
var runCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Dir returns the platform venv directory (~/.ai-platform/venv).
func Dir() (string, error) {
	return paths.VenvDir()
}

// binSubdir is the venv's executables subdirectory. The platform targets macOS and
// Linux only (POSIX venv layout — `bin`, not Windows `Scripts`).
const binSubdir = "bin"

// BinPath returns the absolute path to a tool inside the platform venv
// (<venv>/bin/<tool>), WITHOUT checking that it exists — callers stat it (or run it)
// themselves. An empty tool name is an error.
func BinPath(tool string) (string, error) {
	if tool == "" {
		return "", fmt.Errorf("pyenv: BinPath requires a non-empty tool name")
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, binSubdir, tool), nil
}

// PythonPath returns the platform venv's python interpreter (<venv>/bin/python).
func PythonPath() (string, error) {
	return BinPath("python")
}

// Exists reports whether the platform venv is initialized — i.e. its python
// interpreter is present. A missing venv (or unresolvable HOME) is (false, err=nil)
// unless resolving the path itself failed.
func Exists() (bool, error) {
	pythonPath, err := PythonPath()
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(pythonPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	}
	return true, nil
}

// Ensure creates the platform venv if it does not already exist, returning whether it
// was created this call (false = already present). It is idempotent. Creation prefers
// `uv venv --python <ver> <dir>` (uv fetches a suitable CPython) and falls back to
// `python3 -m venv <dir>`. If neither uv nor python3 is available it returns an error
// — the caller (`ai setup`) treats that as a non-fatal warning, since the venv is only
// needed by opt-in Python backends.
func Ensure() (created bool, err error) {
	exists, err := Exists()
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	dir, err := Dir()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return false, fmt.Errorf("pyenv: prepare parent of %s: %w", dir, err)
	}

	// Prefer uv (fast, and it can provision Python 3.11+ even when the host lacks it).
	// If uv fails (e.g. offline, cannot fetch the interpreter) fall through to the
	// stdlib venv rather than failing outright.
	if _, uvErr := lookPath("uv"); uvErr == nil {
		if _, runErr := runCommand("uv", "venv", "--python", preferredPython, dir); runErr == nil {
			return true, nil
		}
	}

	pythonBin, pyErr := lookPath("python3")
	if pyErr != nil {
		return false, fmt.Errorf(
			"pyenv: cannot create the platform venv at %s — neither `uv` nor `python3` is on PATH "+
				"(install uv from https://astral.sh/uv, or a Python 3.11+ from python.org)", dir)
	}
	if out, runErr := runCommand(pythonBin, "-m", "venv", dir); runErr != nil {
		return false, fmt.Errorf("pyenv: `%s -m venv %s` failed: %w: %s", pythonBin, dir, runErr, string(out))
	}
	return true, nil
}

// PipInstall installs (or upgrades) one or more pip requirement specs into the
// platform venv, e.g. PipInstall("vllm") or PipInstall("--upgrade", "pip"). It
// requires the venv to already exist (call Ensure first). It prefers
// `uv pip install --python <venv-python> <args…>` when uv is present (much faster),
// else the venv's own `<venv>/bin/pip install <args…>`. The combined output is folded
// into any error so a failed install is actionable.
//
// hardware bring-up: a real vLLM install is a large, OS-specific network download
// (on Apple Silicon the Metal plugin), so callers invoke this deliberately — the
// platform does not pip-install heavy runtimes automatically at `ai setup`.
func PipInstall(specs ...string) error {
	if len(specs) == 0 {
		return fmt.Errorf("pyenv: PipInstall requires at least one requirement spec")
	}
	exists, err := Exists()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("pyenv: platform venv not created yet — run `ai setup` first")
	}
	pythonPath, err := PythonPath()
	if err != nil {
		return err
	}

	if _, uvErr := lookPath("uv"); uvErr == nil {
		args := append([]string{"pip", "install", "--python", pythonPath}, specs...)
		if out, runErr := runCommand("uv", args...); runErr != nil {
			return fmt.Errorf("pyenv: `uv pip install` failed: %w: %s", runErr, string(out))
		}
		return nil
	}

	pipPath, err := BinPath("pip")
	if err != nil {
		return err
	}
	args := append([]string{"install"}, specs...)
	if out, runErr := runCommand(pipPath, args...); runErr != nil {
		return fmt.Errorf("pyenv: `%s install` failed: %w: %s", pipPath, runErr, string(out))
	}
	return nil
}
