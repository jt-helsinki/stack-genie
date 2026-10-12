package cli

import (
	"fmt"
	goruntime "runtime"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/doctor"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/uihosts"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

const doctorWorkspaceSessionsCheck = "workspace sessions"

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
	// omlx is the host-native SOLE local-inference runtime (not an aip-* container),
	// so it is surfaced independently of the service-tier status load. It is still
	// Optional here (never turns `ai doctor` unhealthy) since `ai setup` installs it
	// best-effort — a fresh host between setup and its background install completing
	// should not read as broken.
	services = append(services, omlxDoctorService(omlxDetectFn()))
	return services
}

// omlxDoctorService builds the host-native omlx line for `ai doctor`. When absent it
// folds the install guidance into the state so the note is actionable. Pure so it is
// unit-testable without probing the host.
func omlxDoctorService(installed bool) doctor.Service {
	service := doctor.Service{Name: "omlx", Optional: true}
	if installed {
		service.Healthy = true
		service.State = "installed"
		service.Detail = "local-inference backend (`ai services console omlx` for its admin panel)"
		return service
	}
	service.State = "not installed: " + strings.Join(omlx.InstallGuidance(), "; ")
	service.Detail = "the sole local-inference backend — `ai setup` installs it automatically"
	return service
}

// mapDoctorServices maps the setup service statuses into the doctor layer's Service
// view, dropping the microVM runtime line. The host-native omlx backend already
// carries its own actionable Detail (from setup.omlxStatus). Pure so it is
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
			") → this server's IP, and terminate a TLS cert at the proxy. " +
			"Server mode is network-exposed: the LiteLLM admin UI requires a password " +
			"(set at `ai setup`, or `ai litellm password`)"
		domainInfo.ServerCredentials = uihosts.ServerCredentialsGuide(domain)
	}
	return domainInfo
}

// resolveDoctorWorkspace builds the per-workspace runtime section for `ai doctor`
// when an explicit name is given OR the cwd resolves to a workspace. It returns
// nil (omit the section) when no name is given and the cwd is not inside a
// workspace — that is not a failure. Unlike the Microsandbox microVM runtime
// this replaced, there is no container-runtime/rootless/virtualization posture
// to probe — nono sandboxes a host process directly — so this just reports
// whether nono is installed plus the workspace's live nono session count.
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
	return &doctor.WorkspaceRuntime{
		Project:       name,
		NonoInstalled: nono.Detect(runtime.RealProber()).Installed,
		LiveChecks:    doctorWorkspaceLiveChecks(name),
	}
}

// doctorWorkspaceLiveChecks probes the workspace's live nono sessions: how many
// are currently running, out of how many nono currently has recorded for it.
func doctorWorkspaceLiveChecks(project string) []doctor.Check {
	sessions, err := workspace.RealManager(nowRFC3339).ListSessions(project)
	if err != nil {
		return []doctor.Check{{
			Name: doctorWorkspaceSessionsCheck, Status: doctor.StatusError,
			Detail:     "listing sessions failed: " + err.Error(),
			Suggestion: "run `ai agent <cli> " + project + "` to start one",
		}}
	}
	running := 0
	for _, session := range sessions {
		if session.Status == "running" {
			running++
		}
	}
	if running == 0 {
		return []doctor.Check{{
			Name: doctorWorkspaceSessionsCheck, Status: doctor.StatusWarn,
			Detail:     fmt.Sprintf("0 running (%d recorded)", len(sessions)),
			Suggestion: "run `ai agent <cli> " + project + "` or `ai shell " + project + "`",
		}}
	}
	return []doctor.Check{{
		Name: doctorWorkspaceSessionsCheck, Status: doctor.StatusOK,
		Detail: fmt.Sprintf("%d running (%d recorded)", running, len(sessions)),
	}}
}
