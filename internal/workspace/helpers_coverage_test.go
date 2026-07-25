package workspace

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/sysinfo"
)

// TestParseDiskMiB covers the disk-size parse: a plain GB number becomes MiB, and
// blank/unparsable/zero falls back to the platform default (microVMDisk).
func TestParseDiskMiB(test *testing.T) {
	if got := parseDiskMiB("20"); got != 20*1024 {
		test.Errorf("parseDiskMiB(20) = %d, want %d", got, 20*1024)
	}
	for _, blankish := range []string{"", "  ", "abc", "0"} {
		if got := parseDiskMiB(blankish); got != microVMDisk {
			test.Errorf("parseDiskMiB(%q) = %d, want default %d", blankish, got, microVMDisk)
		}
	}
}

// TestClampWorkspaceMemoryMiB covers the OOM-safety clamp: an over-host request is
// capped at the usable ceiling (reserving host+service-tier+hypervisor headroom),
// while a request at/under that ceiling passes through unchanged.
func TestClampWorkspaceMemoryMiB(test *testing.T) {
	hostMiB, ok := sysinfo.MemoryMiB()
	if !ok {
		test.Skip("host memory not detectable on this platform")
	}
	usable := config.UsableHostMemoryMiB(hostMiB)

	// A request far above host RAM is clamped to the usable ceiling.
	over := hostMiB + (1 << 20) // + ~1 TiB worth of MiB — unambiguously above host
	if got := clampWorkspaceMemoryMiB(over); got != usable {
		test.Fatalf("clampWorkspaceMemoryMiB(%d) = %d, want usable ceiling %d", over, got, usable)
	}

	// The usable ceiling itself is not "over host" and passes through unchanged.
	if got := clampWorkspaceMemoryMiB(usable); got != usable {
		test.Fatalf("clampWorkspaceMemoryMiB(usable=%d) = %d, want %d unchanged", usable, got, usable)
	}

	// A small request (the boot minimum, always <= usable) passes through unchanged.
	if got := clampWorkspaceMemoryMiB(config.MinWorkspaceMemoryMiB); got != config.MinWorkspaceMemoryMiB {
		test.Fatalf("clampWorkspaceMemoryMiB(min=%d) = %d, want %d unchanged",
			config.MinWorkspaceMemoryMiB, got, config.MinWorkspaceMemoryMiB)
	}
}

// TestEnsureRelSymlinkCreatesFresh covers the create branch: no link exists yet, so
// a fresh symlink is created pointing at the (possibly relative/dangling) target.
func TestEnsureRelSymlinkCreatesFresh(test *testing.T) {
	dir := test.TempDir()
	link := filepath.Join(dir, "skills")
	target := "../.ai-platform/skills"

	if err := ensureRelSymlink(link, target); err != nil {
		test.Fatalf("ensureRelSymlink create: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		test.Fatalf("expected a symlink at %s: %v", link, err)
	}
	if got != target {
		test.Fatalf("symlink target = %q, want %q", got, target)
	}
}

// TestEnsureRelSymlinkIdempotent covers the idempotent branch: a correct existing
// symlink is left in place (no error, target unchanged).
func TestEnsureRelSymlinkIdempotent(test *testing.T) {
	dir := test.TempDir()
	link := filepath.Join(dir, "skills")
	target := "../.ai-platform/skills"

	if err := ensureRelSymlink(link, target); err != nil {
		test.Fatalf("first ensureRelSymlink: %v", err)
	}
	if err := ensureRelSymlink(link, target); err != nil {
		test.Fatalf("second (idempotent) ensureRelSymlink: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		test.Fatalf("expected a symlink at %s: %v", link, err)
	}
	if got != target {
		test.Fatalf("symlink target = %q, want %q unchanged", got, target)
	}
}

// TestEnsureRelSymlinkReplacesWrong covers the replace branch: an existing symlink
// pointing at the WRONG target is removed and recreated with the correct target.
func TestEnsureRelSymlinkReplacesWrong(test *testing.T) {
	dir := test.TempDir()
	link := filepath.Join(dir, "skills")
	target := "../.ai-platform/skills"

	if err := os.Symlink("../wrong/place", link); err != nil {
		test.Fatalf("seed wrong symlink: %v", err)
	}
	if err := ensureRelSymlink(link, target); err != nil {
		test.Fatalf("ensureRelSymlink replace: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		test.Fatalf("expected a symlink at %s: %v", link, err)
	}
	if got != target {
		test.Fatalf("symlink target = %q, want it replaced with %q", got, target)
	}
}

// TestEnsureRelSymlinkLeavesRealDir covers the do-not-clobber branch: a real
// (non-symlink) directory at the link path is left untouched.
func TestEnsureRelSymlinkLeavesRealDir(test *testing.T) {
	dir := test.TempDir()
	realDir := filepath.Join(dir, "skills")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		test.Fatalf("seed real dir: %v", err)
	}

	if err := ensureRelSymlink(realDir, "../.ai-platform/skills"); err != nil {
		test.Fatalf("ensureRelSymlink over real dir: %v", err)
	}
	info, err := os.Lstat(realDir)
	if err != nil {
		test.Fatalf("lstat real dir: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		test.Fatal("real dir must not be replaced by a symlink")
	}
	if !info.IsDir() {
		test.Fatal("real dir must remain a directory")
	}
}

// TestAgentCLINames asserts the launchable agent-CLI set is sorted, contains every
// known CLI, and excludes "pi" (removed as a launchable session CLI).
func TestAgentCLINames(test *testing.T) {
	names := AgentCLINames()
	if !sort.StringsAreSorted(names) {
		test.Fatalf("AgentCLINames() not sorted: %v", names)
	}

	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}

	for _, want := range []string{"opencode", "omp", "claude-code", "codex", "gemini", "hermes"} {
		if !present[want] {
			test.Fatalf("AgentCLINames() missing %q: %v", want, names)
		}
	}

	if present["pi"] {
		test.Fatalf("AgentCLINames() should not contain the removed \"pi\": %v", names)
	}
}
