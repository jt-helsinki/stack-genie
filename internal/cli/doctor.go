package cli

import (
	goruntime "runtime"

	"github.com/jt-helsinki/ideal-robot/internal/doctor"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/uihosts"
	"github.com/spf13/cobra"
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
			if workspace := resolveDoctorWorkspace(firstArg(args)); workspace != nil {
				deps.Workspace = workspace
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
	statuses, err := setup.ServicesStatus(setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339))
	if err != nil {
		return nil
	}
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
// base domain, the portless UI subdomain URLs (litellm.<domain>), and
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
	workspace := &doctor.WorkspaceRuntime{Project: name}
	info, err := runtime.Detect(goruntime.GOOS, goruntime.GOARCH, runtime.RealProber(), nowRFC3339())
	if err != nil {
		// Missing container runtime or Microsandbox — surfaced as an error Check.
		workspace.RuntimeErr = err
		return workspace
	}
	workspace.Rootless = info.Rootless
	workspace.Virtualization = info.Microsandbox.Virtualization
	workspace.Available = info.Microsandbox.Available
	if verifyErr := runtime.Verify(info); verifyErr != nil {
		workspace.VerifyErr = verifyErr
	}
	return workspace
}
