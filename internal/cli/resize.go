package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// resizeResult is the typed payload for `ai resize`.
type resizeResult struct {
	Project string `json:"project"`
	CPUs    int    `json:"cpus,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Disk    string `json:"disk,omitempty"`
}

// Human renders the applied resource limits.
func (result resizeResult) Human() string {
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "%s workspace %s resources updated\n",
		ui.Success.Render(ui.IconOK), ui.Value.Render(result.Project))
	_, _ = fmt.Fprintf(&builder, "  vCPUs:  %s\n", ui.Value.Render(orDash(strconv.Itoa(result.CPUs))))
	_, _ = fmt.Fprintf(&builder, "  memory: %s GB\n", ui.Value.Render(orDash(result.Memory)))
	_, _ = fmt.Fprintf(&builder, "  disk:   %s GB\n", ui.Value.Render(orDash(result.Disk)))
	_, _ = fmt.Fprintf(&builder, "  %s\n",
		ui.Muted.Render("Applied to the microVM on the next start (restart to apply now)."))
	return builder.String()
}

// newResizeCmd builds `ai resize [workspace]` — change a workspace's disk (writable
// rootfs), memory, and/or vCPUs after creation. It edits the project config.yaml and,
// on a running workspace, offers to restart so the change takes effect immediately
// (msb rebuilds the rootfs from the image with the new sizes on every start).
func newResizeCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resize [workspace]",
		Short: "Change a workspace's disk, memory, and/or vCPUs",
		Long: "Change a workspace's disk (writable rootfs / in-VM container image store),\n" +
			"memory, and/or vCPUs after creation. Pass at least one of --disk/--memory/--cpus.\n" +
			"The change is written to the workspace config and applied to the microVM on the\n" +
			"next start; on a running workspace you are offered a restart to apply it now.\n" +
			"[workspace] defaults to the current directory's workspace.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.resize", err)
				return nil
			}
			diskChanged := cmd.Flags().Changed("disk")
			memChanged := cmd.Flags().Changed("memory")
			cpusChanged := cmd.Flags().Changed("cpus")
			if !diskChanged && !memChanged && !cpusChanged {
				*exit = emitter.Failure("workspace.resize", output.Errorf(output.ExitInvalidInput,
					"nothing to change — pass at least one of --disk, --memory, or --cpus"))
				return nil
			}
			disk, _ := cmd.Flags().GetString("disk")
			memory, _ := cmd.Flags().GetString("memory")
			cpus, _ := cmd.Flags().GetInt("cpus")

			// Validate before writing anything (host-cap cpus/memory; sanity-check disk).
			if memChanged || cpusChanged {
				checkCPUs, checkMem := 0, ""
				if cpusChanged {
					checkCPUs = cpus
				}
				if memChanged {
					checkMem = memory
				}
				if err := create.ValidateResourcesWithinHost(checkCPUs, checkMem); err != nil {
					*exit = emitter.Failure("workspace.resize", err)
					return nil
				}
			}
			if diskChanged {
				if err := create.ValidateDisk(disk); err != nil {
					*exit = emitter.Failure("workspace.resize", err)
					return nil
				}
			}

			root, err := resolveProjectRoot(name)
			if err != nil {
				*exit = emitter.Failure("workspace.resize", mapContextErr(err))
				return nil
			}
			projectConfig, err := config.LoadProjectConfig(root)
			if err != nil {
				*exit = emitter.Failure("workspace.resize", output.Errorf(output.ExitRuntimeFailure, "read workspace config: %s", err))
				return nil
			}
			if cpusChanged {
				projectConfig.Workspace.CPULimit = cpus
			}
			if memChanged {
				projectConfig.Workspace.MemoryLimit = strings.TrimSpace(memory)
			}
			if diskChanged {
				projectConfig.Workspace.DiskLimit = strings.TrimSpace(disk)
			}
			if err := config.WriteProject(root, projectConfig); err != nil {
				*exit = emitter.Failure("workspace.resize", output.Errorf(output.ExitRuntimeFailure, "write workspace config: %s", err))
				return nil
			}

			*exit = emitter.Success("workspace.resize", resizeResult{
				Project: name,
				CPUs:    projectConfig.Workspace.CPULimit,
				Memory:  projectConfig.Workspace.MemoryLimit,
				Disk:    projectConfig.Workspace.DiskLimit,
			})
			// The sizes are applied at microVM (re)create, so a running workspace MUST be
			// restarted for the resize to take effect — do it unconditionally.
			applyWorkspaceRestart(emitter, root)
			return nil
		},
	}
	cmd.Flags().String("disk", "", "workspace disk (writable rootfs) in GB, a plain number (sizes the in-VM container image store)")
	cmd.Flags().String("memory", "", "workspace memory in GB, a plain number (capped below host RAM)")
	cmd.Flags().Int("cpus", 0, "workspace vCPUs (capped at host logical CPUs)")
	_ = cmd.RegisterFlagCompletionFunc("disk", cobra.NoFileCompletions)
	_ = cmd.RegisterFlagCompletionFunc("memory", cobra.NoFileCompletions)
	return cmd
}
