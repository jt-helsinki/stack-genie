// Package create performs the deterministic, in-process work of creating a workspace
// from an already-built project.Spec: scaffold the tracked .ai-platform/ files and
// seed context-optimization defaults. It is shared by `ai create` (the CLI) and the
// `ai ui` in-TUI create wizard so both paths behave identically without a subprocess.
// It deliberately does NOT import internal/cli — internal/cli imports internal/tui
// which imports this package, so importing cli here would cycle.
package create

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/sysinfo"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// Result is what a successful create produces, for the caller to render (the CLI's
// --json envelope, or the TUI's hub refresh).
type Result struct {
	Name       string
	Root       string
	Tools      []string
	ConfigYAML string
}

// Progress is a create-flow progress event: a human Step label. Execute calls report
// (nil-safe) as it advances, so the CLI can render a step log and the TUI can stream
// a live log of what's happening.
type Progress struct {
	Step string
}

// Execute runs the deterministic create work for an ALREADY-RESOLVED spec (spec.Root
// must be an absolute path; the caller owns location resolution / attach / dry-run):
// it scaffolds the project's tracked .ai-platform/ files and seeds the
// context-optimization defaults (+ Caveman level). There is no image to build and no
// resources to validate — nono sandboxes the host directly, so a workspace is ready
// to use the moment it's scaffolded (`ai agent <cli>`/`ai shell` generate its nono
// profile on first launch). Errors carry the right exit code (*output.Error) where it
// matters.
// prepareWorkspace is the tool-install step, a var so tests do not run real installers.
var prepareWorkspace = workspace.PrepareProject

func Execute(spec project.Spec, now string, report func(Progress)) (Result, []string, error) {
	if report == nil {
		report = func(Progress) {}
	}
	if err := project.EnsureCreatable(spec.Name, spec.Root); err != nil {
		return Result{}, nil, mapProjectErr(err)
	}
	// Create the location if it does not exist (Scaffold also MkdirAll's, but be
	// explicit so a brand-new path is clearly the workspace root).
	if err := os.MkdirAll(spec.Root, 0o755); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "create location %s: %s", spec.Root, err)
	}
	report(Progress{Step: "scaffolding workspace (config, skills)"})
	if _, err := project.Scaffold(spec, now); err != nil {
		return Result{}, nil, mapProjectErr(err)
	}
	// Seed context-optimization defaults so the project config is self-describing. The
	// Caveman level is recorded here; the Caveman skill itself is installed by the
	// tool-install step below, not scaffolded.
	report(Progress{Step: "seeding context-optimization defaults"})
	if err := contextopt.SetStrategy(spec.Root, contextopt.DefaultStrategy); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "seed context strategy: %s", err)
	}
	if err := contextopt.SetCavemanLevel(spec.Root, contextopt.DefaultCavemanLevel); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "seed caveman level: %s", err)
	}
	// Install the selected AI tools now, at creation, rather than on first launch (slow steps
	// run detached; logs land in <project>/.ai-platform/run).
	report(Progress{Step: "installing AI tools"})
	if err := prepareWorkspace(spec.Root, func(step string) { report(Progress{Step: step}) }); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "set up workspace tools: %s", err)
	}
	configYAML, err := os.ReadFile(config.ProjectPath(spec.Root))
	if err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "read config.yaml: %s", err)
	}
	warnings := oauthWarnings(spec)
	report(Progress{Step: "workspace registered — run `ai agent <cli>` or `ai shell` to start a sandboxed session"})
	return Result{
		Name:       spec.Name,
		Root:       spec.Root,
		Tools:      spec.AgentCLIs,
		ConfigYAML: string(configYAML),
	}, warnings, nil
}

// oauthWarnings returns a security warning for each agent that runs in OAuth/plan mode:
// that mode routes the agent DIRECTLY to its provider, bypassing the gateway — so the tool
// firewall, secret masking, and content-level egress audit do NOT apply to it. Covers the
// OAuth-capable CLIs set to oauth AND any forced-oauth CLI (always oauth, since it cannot
// route through the gateway at all). Selected agents only (Scaffold filters the persisted
// set the same way).
func oauthWarnings(spec project.Spec) []string {
	var warnings []string
	for _, cli := range config.OAuthCapableCLIs() {
		if !slices.Contains(spec.AgentCLIs, cli) {
			continue
		}
		if spec.AuthModes[cli] == "oauth" {
			warnings = append(warnings, fmt.Sprintf(
				"OAuth/plan mode routes %s DIRECTLY to the provider, bypassing the gateway — the tool firewall, secret masking, and content-level egress audit do NOT apply to it (see docs/deferred/oauth-agent-firewall-via-gateway.md)", cli))
		}
	}
	for _, cli := range config.ForcedOAuthCLIs() {
		if !slices.Contains(spec.AgentCLIs, cli) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s authenticates natively to its provider and talks DIRECTLY to it — it cannot route through the gateway, so the tool firewall, secret masking, and content-level egress audit do NOT apply to it (see docs/deferred/oauth-agent-firewall-via-gateway.md)", cli))
	}
	return warnings
}

// mapProjectErr maps a project error to a platform exit code (invalid input for the
// name/exists/unknown sentinels, else a runtime failure). An already-typed
// *output.Error passes through.
func mapProjectErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	switch {
	case errors.Is(err, project.ErrInvalidName),
		errors.Is(err, project.ErrAlreadyExists),
		errors.Is(err, project.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// HostCPUs returns the host's logical CPU count (unused now that the resource-sizing
// wizard steps are gone; kept for callers that still reference it).
func HostCPUs() int { return sysinfo.CPUs() }
