package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// Sentinel errors for the real impls. Missing tools → exit 3 (mapped by the
// CLI's mapWorkspaceErr). Runtime failures are plain errors → exit 4.
var (
	ErrContainerRuntimeMissing = errors.New("no container runtime (docker or podman) found")
	ErrMsbMissing              = errors.New("microsandbox (msb) is not installed")
	// ErrPending marks any genuinely-unimplemented seam. None remain in the
	// workspace build + microVM lifecycle; retained so other code/tests
	// referencing the symbol still compile.
	ErrPending = errors.New("workspace operation is not yet wired")
)

// realBuilder builds the workspace OCI image with the detected container
// runtime (Docker or Podman, Slice 6 — never hardcoded), then loads it into
// Microsandbox. msb cannot read the container runtime's local image store, so
// the image is exported to a tar and fed to `msb load` (verified, msb 0.5.7).
type realBuilder struct{ prober runtime.Prober }

func (builder realBuilder) Build(projectRoot, imageRef string) error {
	containerRuntime, err := runtime.ContainerRuntimeName(builder.prober)
	if err != nil {
		return ErrContainerRuntimeMissing
	}
	if _, err := builder.prober.LookPath("msb"); err != nil {
		return ErrMsbMissing
	}

	// 1. Build the image from <projectRoot>/.ai-platform/Dockerfile.
	dockerfile := filepath.Join(projectRoot, ".ai-platform", "Dockerfile")
	buildArgs := containerRuntime.BuildArgs(imageRef, dockerfile, projectRoot)
	if err := runStreaming(containerRuntime.Name, buildArgs...); err != nil {
		return fmt.Errorf("%s build %s: %w", containerRuntime.Name, imageRef, err)
	}

	// 2. Export the freshly-built image to a temp tar. msb cannot read the
	//    runtime's local image store, so it is loaded from an image tar.
	tarFile, err := os.CreateTemp("", "aip-image-*.tar")
	if err != nil {
		return fmt.Errorf("create image tar: %w", err)
	}
	tarPath := tarFile.Name()
	_ = tarFile.Close()
	defer func() { _ = os.Remove(tarPath) }()

	if err := runStreaming(containerRuntime.Name, "save", "-o", tarPath, imageRef); err != nil {
		return fmt.Errorf("%s save %s: %w", containerRuntime.Name, imageRef, err)
	}

	// 3. Load the image tar into Microsandbox.
	if err := runStreaming("msb", "load", "--input", tarPath); err != nil {
		return fmt.Errorf("msb load %s: %w", imageRef, err)
	}
	return nil
}

// realSandbox drives Microsandbox microVMs via `msb`.
type realSandbox struct{ prober runtime.Prober }

func (sandbox realSandbox) ensureInstalled() error {
	if _, err := sandbox.prober.LookPath("msb"); err != nil {
		return ErrMsbMissing
	}
	return nil
}

// Create creates AND boots a detached microVM from imageRef, mounting the host
// project source at /workspace and the persistent overlay (arch §26) at
// /persist so writes outside the project mount survive stop/start and destroy
// recreation. The egress policy is applied via netArgs. `--replace` makes
// recreation idempotent.
func (sandbox realSandbox) Create(name, imageRef, projectMount, overlayPath string, netArgs []string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	args := []string{
		"create", imageRef,
		"--name", name,
		"--memory", "1G",
		"--volume", projectMount + ":/workspace",
		"--volume", overlayPath + ":/persist",
		"--workdir", "/workspace",
		"--replace",
	}
	args = append(args, netArgs...)
	if err := runStreaming("msb", args...); err != nil {
		return fmt.Errorf("msb create %s: %w", name, err)
	}
	return nil
}

func (sandbox realSandbox) Start(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	if err := runStreaming("msb", "start", name); err != nil {
		return fmt.Errorf("msb start %s: %w", name, err)
	}
	return nil
}

func (sandbox realSandbox) Stop(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	if err := runStreaming("msb", "stop", "-f", name); err != nil {
		return fmt.Errorf("msb stop %s: %w", name, err)
	}
	return nil
}

func (sandbox realSandbox) Destroy(name string) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	// `-f` stops the microVM first if running, then removes it.
	if err := runStreaming("msb", "remove", "-f", name); err != nil {
		return fmt.Errorf("msb remove %s: %w", name, err)
	}
	return nil
}

// Exec runs argv inside the running microVM. The inner command's exit code is
// faithfully propagated by msb to its own process exit; a non-zero inner exit
// is data carried in ExecResult, NOT a Go error. Only infrastructure failures
// (microVM down, msb missing) are returned as errors (§4.5).
func (sandbox realSandbox) Exec(name string, argv []string) (ExecResult, error) {
	if err := sandbox.ensureInstalled(); err != nil {
		return ExecResult{}, err
	}
	args := append([]string{"exec", name, "--"}, argv...)
	command := exec.Command("msb", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	result := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		result.ExitCode = 0
		return result, nil
	}
	// A non-zero inner exit surfaces as *exec.ExitError; that is the command's
	// own exit code (data), not a platform failure.
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	// Anything else (couldn't launch msb, signal, etc.) is an infra failure.
	return ExecResult{}, fmt.Errorf("msb exec %s: %w", name, err)
}

// WriteFile writes content to guestPath inside the running microVM as the
// `workspace` user (the image's home owner), creating parent directories. It
// pipes the content to `cat` over stdin so no file payload appears in argv. A
// missing msb → exit 3; any write/exec failure → exit 4 (plain error mapped by
// the CLI's mapWorkspaceErr).
func (sandbox realSandbox) WriteFile(name, guestPath string, content []byte) error {
	if err := sandbox.ensureInstalled(); err != nil {
		return err
	}
	guestDir := path.Dir(guestPath)
	// Single shell so the mkdir and the redirected cat share one exec; content
	// arrives on stdin (kept out of argv).
	shellScript := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(guestDir), shellQuote(guestPath))
	command := exec.Command("msb", "exec", name, "-u", "workspace", "--", "sh", "-c", shellScript)
	command.Stdin = bytes.NewReader(content)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("msb exec %s write %s: %w", name, guestPath, err)
	}
	return nil
}

// shellQuote single-quotes a path for safe interpolation into the `sh -c`
// script (the guest paths are platform-controlled, but quoting keeps it robust).
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// runStreaming runs name with args, streaming stdout/stderr to the parent
// process so build/boot progress is visible.
func runStreaming(name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// RealManager builds a Manager wired to the actual host (used by the CLI).
func RealManager(goos string, now func() string) Manager {
	prober := runtime.RealProber()
	return Manager{
		Builder: realBuilder{prober: prober},
		Sandbox: realSandbox{prober: prober},
		Keys:    litellm.NewKeyManager(prober),
		Now:     now,
		GOOS:    goos,
	}
}
