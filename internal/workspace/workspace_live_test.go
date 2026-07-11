package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// TestRealMicroVM exercises the REAL Builder + Sandbox against the host's
// container runtime and Microsandbox (`msb`): build a tiny image, boot a
// microVM, exec into it (asserting stdout + faithful exit-code propagation),
// then stop and destroy it. It is gated behind AIP_HARDWARE_TESTS because it
// requires Docker/Podman + msb and an Apple-Silicon (or Linux/KVM) host.
//
// Run with: AIP_HARDWARE_TESTS=1 go test ./internal/workspace/ -run TestRealMicroVM -v
func TestRealMicroVM(test *testing.T) {
	if os.Getenv("AIP_HARDWARE_TESTS") == "" {
		test.Skip("set AIP_HARDWARE_TESTS=1 to run the live microVM test (needs docker/podman + msb)")
	}

	prober := runtime.RealProber()
	builder := realBuilder{prober: prober}
	sandbox := realSandbox{prober: prober}

	// (a) A tiny project with FROM alpine.
	projectRoot := test.TempDir()
	dockerfileDir := filepath.Join(projectRoot, ".ai-platform")
	if err := os.MkdirAll(dockerfileDir, 0o755); err != nil {
		test.Fatal(err)
	}
	dockerfile := "FROM alpine:3.20\nCMD [\"sleep\", \"infinity\"]\n"
	if err := os.WriteFile(filepath.Join(dockerfileDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		test.Fatal(err)
	}

	const name = "aip-live-test"
	imageRef := name + ":latest"
	overlayPath := test.TempDir()

	// Best-effort cleanup of any leftover sandbox/image regardless of outcome.
	test.Cleanup(func() {
		_ = sandbox.Destroy(name)
		if containerRuntime, err := runtime.ContainerRuntimeName(prober); err == nil {
			_ = runStreaming(containerRuntime.Name, "rmi", "-f", imageRef)
		}
	})

	// (b) Build -> Create -> Exec asserting stdout + exit 0.
	if err := builder.Build(projectRoot, imageRef); err != nil {
		test.Fatalf("Build: %v", err)
	}
	if err := sandbox.Create(name, imageRef, projectRoot, overlayPath, VMResources{}, nil); err != nil {
		test.Fatalf("Create: %v", err)
	}

	resultOK, err := sandbox.Exec(name, []string{"sh", "-c", "echo hello-from-vm; exit 0"})
	if err != nil {
		test.Fatalf("Exec(ok): %v", err)
	}
	if resultOK.ExitCode != 0 {
		test.Fatalf("Exec(ok) exit = %d, want 0 (stderr: %q)", resultOK.ExitCode, resultOK.Stderr)
	}
	if !strings.Contains(resultOK.Stdout, "hello-from-vm") {
		test.Fatalf("Exec(ok) stdout = %q, want it to contain hello-from-vm", resultOK.Stdout)
	}

	// (c) A non-zero inner exit is carried in ExecResult, not a Go error.
	resultFail, err := sandbox.Exec(name, []string{"sh", "-c", "exit 7"})
	if err != nil {
		test.Fatalf("Exec(fail) returned a platform error for a non-zero inner exit: %v", err)
	}
	if resultFail.ExitCode != 7 {
		test.Fatalf("Exec(fail) exit = %d, want 7", resultFail.ExitCode)
	}

	// (d) Stop + Destroy.
	if err := sandbox.Stop(name); err != nil {
		test.Fatalf("Stop: %v", err)
	}
	if err := sandbox.Destroy(name); err != nil {
		test.Fatalf("Destroy: %v", err)
	}
}
