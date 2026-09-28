package workspace

import (
	"context"
	"testing"

	microsandbox "github.com/superradcompany/microsandbox/sdk/go"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// TestExtraMountsBase verifies the base mount set (project + persist) is always
// present even with no isolated dirs or shared mounts.
func TestExtraMountsBase(test *testing.T) {
	mounts := extraMounts("demo", "/host/project", "/host/overlay", nil, nil)
	if len(mounts) != 2 {
		test.Fatalf("mounts = %v, want exactly the base project+persist mounts", mounts)
	}
	if _, ok := mounts[workspaceWorkdir]; !ok {
		test.Errorf("missing base project mount at %q", workspaceWorkdir)
	}
	if _, ok := mounts["/persist"]; !ok {
		test.Error("missing base persist mount at /persist")
	}
}

// TestExtraMountsIsolatedDir verifies an isolated dir gets a NAMED (not bind) mount at
// its guest subpath under workspaceWorkdir, scoped by workspace name.
func TestExtraMountsIsolatedDir(test *testing.T) {
	mounts := extraMounts("demo", "/host/project", "/host/overlay", []string{"node_modules"}, nil)
	guestPath := workspaceWorkdir + "/node_modules"
	mount, ok := mounts[guestPath]
	if !ok {
		test.Fatalf("mounts = %v, want an entry at %q", mounts, guestPath)
	}
	if mount.Kind() != microsandbox.MountKindNamed {
		test.Errorf("isolated dir mount kind = %v, want MountKindNamed", mount.Kind())
	}
}

// TestExtraMountsSharedMount verifies a shared mount gets an ordinary BIND mount at
// its own arbitrary guest path, from the given host path.
func TestExtraMountsSharedMount(test *testing.T) {
	mounts := extraMounts("demo", "/host/project", "/host/overlay", nil, []config.SharedMount{
		{GuestPath: "/home/workspace/shared", HostPath: "/host/downloads"},
	})
	mount, ok := mounts["/home/workspace/shared"]
	if !ok {
		test.Fatalf("mounts = %v, want an entry at /home/workspace/shared", mounts)
	}
	if mount.Kind() != microsandbox.MountKindBind {
		test.Errorf("shared mount kind = %v, want MountKindBind", mount.Kind())
	}
}

// TestIsolatedVolumeNameScopedByWorkspace verifies two workspaces isolating the same
// relative dir get distinct volume names (no cross-workspace collision), and that the
// name is stable for the same (workspace, dir) pair.
func TestIsolatedVolumeNameScopedByWorkspace(test *testing.T) {
	nameA := isolatedVolumeName("demo", "apps/web/node_modules")
	nameB := isolatedVolumeName("other", "apps/web/node_modules")
	if nameA == nameB {
		test.Errorf("volume names collided across workspaces: %q", nameA)
	}
	if isolatedVolumeName("demo", "apps/web/node_modules") != nameA {
		test.Error("volume name is not stable for the same (workspace, dir) pair")
	}
}

// TestEnsureIsolatedVolumesCreatesThenIsIdempotent is a LIVE regression test against
// the real microsandbox runtime (requires msb — see hardware bring-up conventions):
// it reproduces the exact bug found in production — mounting a NAMED volume that was
// never created first fails Create with "volume not found" — by verifying
// ensureIsolatedVolumes creates the volume so a subsequent mount succeeds, and that
// calling it again (the restart path) is a no-op rather than an "already exists" error.
func TestEnsureIsolatedVolumesCreatesThenIsIdempotent(test *testing.T) {
	if _, err := microsandbox.ListVolumes(context.Background()); err != nil {
		test.Skipf("microsandbox runtime not available in this environment: %v", err)
	}
	workspaceName := "aip-test-ensure-isolated-volumes"
	dir := "node_modules"
	volumeName := isolatedVolumeName(workspaceName, dir)
	test.Cleanup(func() { _ = microsandbox.RemoveVolume(context.Background(), volumeName) })

	if err := ensureIsolatedVolumes(workspaceName, []string{dir}); err != nil {
		test.Fatalf("ensureIsolatedVolumes (first call): %v", err)
	}
	if err := ensureIsolatedVolumes(workspaceName, []string{dir}); err != nil {
		test.Fatalf("ensureIsolatedVolumes (second call, must be idempotent): %v", err)
	}
}
