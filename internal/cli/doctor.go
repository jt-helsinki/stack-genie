package cli

import (
	"context"
	"fmt"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/doctor"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/uihosts"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

const doctorLiveProbeTimeout = 4 * time.Second

const (
	doctorWorkspaceMicroVMCheck = "workspace microVM"
	doctorWorkspaceLogsCheck    = "workspace logs"
	doctorWorkspaceExecLabel    = "workspace exec"
)

// newDoctorCmd builds the consolidated `ai doctor [name]` (CLI §10.1, §12.1):
// dependency + service + (when inside/naming a workspace) runtime health checks
// with repair suggestions. It always runs to completion and exits 0 with a
// report; per-check `status` (and the top-level `ok`) convey health. The
// optional [name] (or being inside a workspace directory) adds the workspace
// runtime section; otherwise that section is omitted.
func newDoctorCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "doctor [name]",
		Short:             "Check platform dependencies, services, and workspace runtime health",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(_ *cobra.Command, args []string) error {
			deps := doctor.Deps{
				GOOS:     goruntime.GOOS,
				GOARCH:   goruntime.GOARCH,
				Prober:   runtime.RealProber(),
				Services: doctorServices(),
				Domain:   doctorDomain(),
			}
			// Add the workspace-runtime section only when a name is given OR the
			// cwd resolves to a workspace; otherwise omit it (not an error).
			if workspaceRuntime := resolveDoctorWorkspace(firstArg(args)); workspaceRuntime != nil {
				deps.Workspace = workspaceRuntime
			}
			report := doctor.Run(deps)
			*exit = emitter.Success("doctor", report)
			return nil
		},
	}
}

// doctorServices fetches the full managed-service list (the same backend as
// `ai services status`) and maps it into the doctor layer's Service view. The mapping lives here so the doctor
// package never imports internal/setup (which imports internal/doctor — an
// import cycle). A status-load failure yields an empty list: doctor still
// reports the platform-dependency checks rather than failing.
func doctorServices() []doctor.Service {
	var services []doctor.Service
	if statuses, err := setup.ServicesStatus(setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)); err == nil {
		services = mapDoctorServices(statuses, goruntime.GOOS)
	}
	// vLLM is a host-native OPTIONAL local runtime (not an aip-* container), so it is
	// surfaced independently of the service-tier status load. It is Optional, so a
	// not-installed vLLM never turns `ai doctor` unhealthy.
	installed, _ := vllmDetectFn()
	services = append(services, vllmDoctorService(goruntime.GOOS, installed))
	return services
}

// vllmDoctorService builds the OPTIONAL host-native vLLM line for `ai doctor`. When
// installed it reports the platform weight format; when absent it folds the per-OS
// install guidance into the state so the note is actionable. It is pure so it is
// unit-testable without probing the host.
func vllmDoctorService(goos string, installed bool) doctor.Service {
	service := doctor.Service{Name: "vllm", Optional: true}
	if installed {
		service.Healthy = true
		service.State = "installed"
		service.Detail = "optional host-native runtime (`ai models pull --runtime vllm`)"
		return service
	}
	service.State = "not installed — optional: " + strings.Join(vllmInstallGuidanceFn(goos), "; ")
	service.Detail = "optional; only needed for `ai models pull --runtime vllm`"
	return service
}

// mapDoctorServices maps the setup service statuses into the doctor layer's Service
// view, dropping the microVM runtime line. The host-native vLLM backend already
// carries its own actionable Detail (from setup.vllmStatus). Pure so it is
// unit-testable without a live service tier.
func mapDoctorServices(statuses []setup.ServiceStatus, _ string) []doctor.Service {
	services := make([]doctor.Service, 0, len(statuses))
	for _, status := range statuses {
		// The microVM runtime (Mode "runtime") is covered by the platform
		// virtualization/microsandbox checks and the workspace section, so it is
		// not duplicated as a service line.
		if status.Mode == "runtime" {
			continue
		}
		services = append(services, doctor.Service{
			Name:     status.Name,
			State:    status.State,
			Healthy:  status.Healthy,
			Optional: status.Optional,
			Detail:   status.Detail,
		})
	}
	return services
}

// doctorDomain builds the DOMAIN section for `ai doctor`: the resolved platform
// base domain, the UI subdomain URLs (litellm.<domain>:18787), and
// the resolution status — standalone reports whether the /etc/hosts block is
// present + up to date; a server reports the DNS/TLS reminder. Returns nil (omit
// the section) when there is no runtime.yaml yet (a fresh, un-setup host).
func doctorDomain() *doctor.DomainInfo {
	info, err := runtime.Load()
	if err != nil || info == nil {
		return nil
	}
	domain := info.ResolveDomain()

	urls := make([]doctor.DomainURL, 0)
	for _, url := range uihosts.URLs(domain) {
		urls = append(urls, doctor.DomainURL{Service: url.Service, Host: url.Host, URL: url.URL})
	}
	domainInfo := &doctor.DomainInfo{Domain: domain, Role: info.Role, URLs: urls}

	if uihosts.ManageHostsForRole(info.Role) {
		domainInfo.Standalone = true
		present, upToDate, statusErr := uihosts.HostsStatus(uihosts.DefaultHostsPath, domain)
		if statusErr == nil {
			domainInfo.HostsPresent = present
			domainInfo.HostsUpToDate = upToDate
		}
	} else if info.Role == runtime.RoleServer {
		domainInfo.ServerReminder = "create DNS records (*." + domain +
			" or per-host litellm." + domain +
			") → this server's IP, and terminate a TLS cert at nginx. " +
			"Server mode is network-exposed: the LiteLLM admin UI requires a password " +
			"(set at `ai setup`, or `ai litellm password`)"
		domainInfo.ServerCredentials = uihosts.ServerCredentialsGuide(domain)
	}
	return domainInfo
}

// resolveDoctorWorkspace builds the per-workspace runtime section for `ai doctor`
// when an explicit name is given OR the cwd resolves to a workspace. It returns
// nil (omit the section) when no name is given and the cwd is not inside a
// workspace — that is not a failure. Runtime/verification shortfalls are folded
// into the returned struct as errors so doctor renders them as Checks (exit 0
// with a full report) rather than aborting (the old `ai workspace doctor` exited
// 3/4 here).
func resolveDoctorWorkspace(explicit string) *doctor.WorkspaceRuntime {
	name := explicit
	if name == "" {
		// No explicit name: only add the section if the cwd is inside a workspace.
		resolved, found, err := currentProjectName()
		if err != nil || !found {
			return nil
		}
		name = resolved
	}
	workspaceRuntime := &doctor.WorkspaceRuntime{Project: name}
	info, err := runtime.Detect(goruntime.GOOS, goruntime.GOARCH, runtime.RealProber(), nowRFC3339())
	if err != nil {
		// Missing container runtime or Microsandbox — surfaced as an error Check.
		workspaceRuntime.RuntimeErr = err
		return workspaceRuntime
	}
	workspaceRuntime.Rootless = info.Rootless
	workspaceRuntime.Virtualization = info.Microsandbox.Virtualization
	workspaceRuntime.Available = info.Microsandbox.Available
	if verifyErr := runtime.Verify(info); verifyErr != nil {
		workspaceRuntime.VerifyErr = verifyErr
	}
	workspaceRuntime.LiveChecks = doctorWorkspaceLiveChecks(name, workspace.RealManager(goruntime.GOOS, nowRFC3339).Sandbox)
	return workspaceRuntime
}

func doctorWorkspaceLiveChecks(project string, sandbox workspace.Sandbox) []doctor.Check {
	if sandbox == nil {
		return nil
	}
	vmName := workspace.Name(project)
	checks := make([]doctor.Check, 0, 3)
	ctx, cancel := context.WithTimeout(context.Background(), doctorLiveProbeTimeout)
	running, err := sandbox.IsRunning(ctx, vmName)
	cancel()
	if err != nil {
		return []doctor.Check{{
			Name: doctorWorkspaceMicroVMCheck, Status: doctor.StatusError,
			Detail:     "liveness probe failed: " + err.Error(),
			Suggestion: "run `ai restart " + project + "` if the workspace should be running",
		}}
	}
	if !running {
		return []doctor.Check{{
			Name: doctorWorkspaceMicroVMCheck, Status: doctor.StatusError,
			Detail:     "not running",
			Suggestion: "run `ai start " + project + "`",
		}}
	}
	checks = append(checks, doctor.Check{Name: doctorWorkspaceMicroVMCheck, Status: doctor.StatusOK, Detail: "running"})
	checks = append(checks, doctorWorkspaceLogCheck(project, vmName, sandbox))
	checks = append(checks, doctorWorkspaceExecCheck(project, vmName, sandbox))
	return checks
}

func doctorWorkspaceLogCheck(project, vmName string, sandbox workspace.Sandbox) doctor.Check {
	ctx, cancel := context.WithTimeout(context.Background(), doctorLiveProbeTimeout)
	logTail, err := sandbox.LogTailContext(ctx, vmName, 20)
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil || timedOut {
		detail := errorDetail("log tail", err, timedOut)
		return doctor.Check{
			Name: doctorWorkspaceLogsCheck, Status: doctor.StatusError,
			Detail:     detail,
			Suggestion: "run `msb logs " + vmName + " --tail 200` for details, then `ai restart " + project + "` if needed",
		}
	}
	detail := "readable"
	if strings.TrimSpace(logTail) == "" {
		detail = "readable; no output captured"
	}
	return doctor.Check{Name: doctorWorkspaceLogsCheck, Status: doctor.StatusOK, Detail: detail}
}

func doctorWorkspaceExecCheck(project, vmName string, sandbox workspace.Sandbox) doctor.Check {
	ctx, cancel := context.WithTimeout(context.Background(), doctorLiveProbeTimeout)
	result, err := sandbox.ExecContext(ctx, vmName, []string{"true"})
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil || timedOut {
		return doctor.Check{
			Name: doctorWorkspaceExecLabel, Status: doctor.StatusError,
			Detail:     errorDetail("msb exec", err, timedOut),
			Suggestion: "workspace logs may still be available; run `ai restart " + project + "` to recover the exec channel",
		}
	}
	if result.ExitCode != 0 {
		return doctor.Check{
			Name: doctorWorkspaceExecLabel, Status: doctor.StatusError,
			Detail:     fmt.Sprintf("probe exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr)),
			Suggestion: "run `ai restart " + project + "`",
		}
	}
	return doctor.Check{Name: doctorWorkspaceExecLabel, Status: doctor.StatusOK, Detail: "responsive"}
}

func errorDetail(label string, err error, timedOut bool) string {
	if timedOut {
		return label + " timed out"
	}
	if err != nil {
		return err.Error()
	}
	return label + " failed"
}
