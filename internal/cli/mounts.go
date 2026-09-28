package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// mountsResult is the typed payload of `ai mounts list`: the workspace's isolated dirs
// (config.yaml workspace.isolated_dirs, guest-private named volumes) and shared mounts
// (config.yaml workspace.shared_mounts, extra host↔guest bind mounts).
type mountsResult struct {
	Project      string               `json:"project"`
	IsolatedDirs []string             `json:"isolated_dirs"`
	SharedMounts []config.SharedMount `json:"shared_mounts"`
}

// Human renders the isolated dirs and shared mounts as two tables.
func (result mountsResult) Human() string {
	var builder strings.Builder
	if len(result.IsolatedDirs) == 0 {
		builder.WriteString(ui.Muted.Render("No isolated dirs.") + "\n")
	} else {
		rows := make([][]string, 0, len(result.IsolatedDirs))
		for _, dir := range result.IsolatedDirs {
			rows = append(rows, []string{ui.Value.Render(dir)})
		}
		builder.WriteString(ui.Table([]string{"ISOLATED DIR (guest-private volume, not on host)"}, rows) + "\n")
	}
	if len(result.SharedMounts) == 0 {
		builder.WriteString(ui.Muted.Render("No shared mounts."))
	} else {
		rows := make([][]string, 0, len(result.SharedMounts))
		for _, mount := range result.SharedMounts {
			rows = append(rows, []string{ui.Value.Render(mount.GuestPath), ui.Value.Render(mount.HostPath)})
		}
		builder.WriteString(ui.Table([]string{"GUEST PATH", "HOST PATH (shared both ways)"}, rows))
	}
	return builder.String()
}

// mountActionResult is the typed payload of the mutating `ai mounts` subcommands.
type mountActionResult struct {
	Project  string `json:"project"`
	Kind     string `json:"kind"` // "isolated" or "shared"
	Dir      string `json:"dir"`
	HostPath string `json:"host_path,omitempty"`
	Action   string `json:"action"`
}

// Human renders a one-line confirmation, always noting the restart requirement (the
// mount set is applied at microVM create).
func (result mountActionResult) Human() string {
	label := "isolated dir"
	target := result.Dir
	if result.Kind == "shared" {
		label = "shared mount"
		target = result.Dir + " ↔ " + result.HostPath
	}
	line := fmt.Sprintf("%s %s %s in workspace %s", result.Action, label, ui.Value.Render(target), ui.Value.Render(strconv.Quote(result.Project)))
	line += "\nRestart the workspace to apply the mount change: " + ui.Primary.Render("ai restart "+result.Project)
	return line
}

// newMountsCmd builds `ai mounts <list|add|remove> [dir] [name]` — manage the
// workspace's extra mounts beyond the base project bind mount: ISOLATED dirs
// (config.yaml workspace.isolated_dirs — a guest-relative subpath excluded from the
// host mount and backed by a private, workspace-scoped named volume instead) and
// SHARED mounts (config.yaml workspace.shared_mounts — an extra host directory
// bind-mounted at an arbitrary absolute guest path, visible both ways). See
// internal/workspace/workspace_sdk.go extraMounts for how both are applied at microVM
// create. A relative <dir> is an isolated dir; an ABSOLUTE <dir> together with --host
// is a shared mount — that leading "/" is what disambiguates add/remove between the
// two kinds. The optional trailing [name] resolves the workspace like the other verbs
// (explicit → --project → cwd).
func newMountsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mounts <list|add|remove> [dir] [name]",
		Short: "Manage isolated dirs and shared host mounts beyond the workspace's project mount",
		Long: "Manage two kinds of extra mount beyond the base project bind mount:\n\n" +
			"  isolated dir   ai mounts add node_modules\n" +
			"                 A guest-relative subdirectory (e.g. node_modules, target)\n" +
			"                 excluded from the host project bind mount and backed by a\n" +
			"                 private, workspace-scoped volume instead: writes made in the\n" +
			"                 sandbox never touch the host directory, and whatever the host\n" +
			"                 has there stays untouched and invisible to the guest. Lets\n" +
			"                 host-built and sandbox-built binaries coexist without collision.\n\n" +
			"  shared mount   ai mounts add /home/workspace/shared --host ~/Downloads\n" +
			"                 An absolute guest path bind-mounted from an arbitrary HOST\n" +
			"                 directory (created if missing), visible and writable on BOTH\n" +
			"                 sides.\n\n" +
			"The mount set is applied at microVM create/recreate, so add/remove require\n" +
			"(and offer) a restart to take effect. [workspace] defaults to the current\n" +
			"directory's workspace.",
		Args: cobra.MinimumNArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return []string{"list", "add", "remove"}, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]
			if action == "list" {
				name, err := resolveProjectName(cmd, secondArg(args))
				if err != nil {
					*exit = emitter.Failure("mounts.list", err)
					return nil
				}
				runMountsList(emitter, exit, name)
				return nil
			}
			if action != "add" && action != "remove" {
				*exit = emitter.Failure("mounts", output.Errorf(output.ExitInvalidInput,
					"unknown mounts action %q (one of: list, add, remove)", action))
				return nil
			}
			if len(args) < 2 {
				*exit = emitter.Failure("mounts."+action, output.Errorf(output.ExitInvalidInput,
					"usage: ai mounts %s <dir> [--host <host-path>] [name]", action))
				return nil
			}
			dir := args[1]
			hostPath, _ := cmd.Flags().GetString("host")
			name, err := resolveProjectName(cmd, thirdArg(args))
			if err != nil {
				*exit = emitter.Failure("mounts."+action, err)
				return nil
			}
			runMountsAction(emitter, exit, action, dir, hostPath, name)
			return nil
		},
	}
	cmd.Flags().String("host", "", "host directory for a SHARED mount (with an absolute <dir>); omit for an isolated dir")
	return cmd
}

// runMountsList lists the workspace's isolated dirs and shared mounts.
func runMountsList(emitter *output.Emitter, exit *int, name string) {
	root, err := resolveProjectRoot(name)
	if err != nil {
		*exit = emitter.Failure("mounts.list", mapContextErr(err))
		return
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		*exit = emitter.Failure("mounts.list", output.Errorf(output.ExitRuntimeFailure, "read workspace config: %s", err))
		return
	}
	*exit = emitter.Success("mounts.list", mountsResult{
		Project:      name,
		IsolatedDirs: projectConfig.Workspace.IsolatedDirs,
		SharedMounts: projectConfig.Workspace.SharedMounts,
	})
}

// runMountsAction adds or removes an isolated dir or shared mount — an absolute dir is
// a shared mount (requires --host on add), a relative dir is an isolated dir (rejects
// --host) — and, on a running workspace, offers to restart so the change takes effect
// immediately.
func runMountsAction(emitter *output.Emitter, exit *int, action, dir, hostPath, name string) {
	command := "mounts." + action
	isShared := strings.HasPrefix(dir, "/")
	if isShared && hostPath == "" && action == "add" {
		*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput,
			"absolute dir %q needs --host <host-path> (a shared mount) — pass a guest-relative dir for an isolated dir instead", dir))
		return
	}
	if !isShared && hostPath != "" {
		*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput,
			"--host is only valid with an absolute dir (a shared mount) — %q is a guest-relative isolated dir", dir))
		return
	}

	root, err := resolveProjectRoot(name)
	if err != nil {
		*exit = emitter.Failure(command, mapContextErr(err))
		return
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		*exit = emitter.Failure(command, output.Errorf(output.ExitRuntimeFailure, "read workspace config: %s", err))
		return
	}

	if isShared {
		if !runSharedMountAction(emitter, exit, command, action, dir, hostPath, projectConfig) {
			return
		}
	} else {
		if !runIsolatedDirAction(emitter, exit, command, action, dir, projectConfig) {
			return
		}
	}

	if err := config.WriteProject(root, projectConfig); err != nil {
		*exit = emitter.Failure(command, output.Errorf(output.ExitRuntimeFailure, "write workspace config: %s", err))
		return
	}
	kind := "isolated"
	if isShared {
		kind = "shared"
	}
	*exit = emitter.Success(command, mountActionResult{Project: name, Kind: kind, Dir: dir, HostPath: hostPath, Action: action})
	applyWorkspaceRestart(emitter, root)
}

// runIsolatedDirAction applies an add/remove to projectConfig.Workspace.IsolatedDirs in
// place. Returns false (having already reported failure) if validation or the action
// itself fails.
func runIsolatedDirAction(emitter *output.Emitter, exit *int, command, action, dir string, projectConfig *config.Config) bool {
	if action == "add" {
		if err := create.ValidateIsolatedDirs([]string{dir}); err != nil {
			*exit = emitter.Failure(command, err)
			return false
		}
	}
	dirs := projectConfig.Workspace.IsolatedDirs
	switch action {
	case "add":
		if slices.Contains(dirs, dir) {
			*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput, "isolated dir %q already present", dir))
			return false
		}
		projectConfig.Workspace.IsolatedDirs = append(dirs, dir)
	case "remove":
		index := slices.Index(dirs, dir)
		if index < 0 {
			*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput, "isolated dir %q not present", dir))
			return false
		}
		projectConfig.Workspace.IsolatedDirs = slices.Delete(slices.Clone(dirs), index, index+1)
	}
	return true
}

// runSharedMountAction applies an add/remove to projectConfig.Workspace.SharedMounts in
// place, keyed by guest path. Returns false (having already reported failure) if
// validation or the action itself fails.
func runSharedMountAction(emitter *output.Emitter, exit *int, command, action, guestPath, hostPath string, projectConfig *config.Config) bool {
	mounts := projectConfig.Workspace.SharedMounts
	index := slices.IndexFunc(mounts, func(mount config.SharedMount) bool { return mount.GuestPath == guestPath })
	switch action {
	case "add":
		if index >= 0 {
			*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput, "shared mount %q already present", guestPath))
			return false
		}
		candidate := append(slices.Clone(mounts), config.SharedMount{GuestPath: guestPath, HostPath: hostPath})
		if err := create.ValidateSharedMounts(candidate); err != nil {
			*exit = emitter.Failure(command, err)
			return false
		}
		projectConfig.Workspace.SharedMounts = candidate
	case "remove":
		if index < 0 {
			*exit = emitter.Failure(command, output.Errorf(output.ExitInvalidInput, "shared mount %q not present", guestPath))
			return false
		}
		projectConfig.Workspace.SharedMounts = slices.Delete(slices.Clone(mounts), index, index+1)
	}
	return true
}
