// Package create performs the deterministic, in-process work of creating a workspace
// from an already-built project.Spec: validate + cap resources against the host,
// scaffold the tracked .ai-platform/ files, seed context-optimization defaults, and
// pull the chosen Graphify model (best-effort). It is shared by `ai create` (the CLI)
// and the `ai ui` in-TUI create wizard so both paths behave identically without a
// subprocess. It deliberately does NOT import internal/cli — internal/cli imports
// internal/tui which imports this package, so importing cli here would cycle.
package create

import (
	"errors"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/sysinfo"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
)

// Result is what a successful create produces, for the caller to render (the CLI's
// --json envelope, or the TUI's hub refresh).
type Result struct {
	Name       string
	Root       string
	OS         string
	Tools      []string
	Stacks     []string
	Apps       []string
	ConfigYAML string
}

// Execute performs create for an ALREADY-RESOLVED spec (spec.Root must be an absolute
// path; the caller owns location resolution / attach / dry-run). It validates + caps
// resources against the host, scaffolds the project's tracked .ai-platform/ files,
// seeds the context-optimization defaults (+ Caveman skill), and pulls the chosen
// Graphify model (best-effort → warnings). Errors carry the right exit code
// (*output.Error) where it matters.
// Progress is a create-flow progress event: a human Step label plus, when the step is a
// model download, live byte counters (Completed/Total; 0 when not a download). Execute
// calls report (nil-safe) as it advances, so the CLI can render a progress bar + the TUI
// can stream a live log of what's happening.
type Progress struct {
	Step      string
	Completed int64
	Total     int64
}

// Execute runs the deterministic create work. report (may be nil) receives Progress
// events for each phase + the Graphify-model pull, so callers can show progress.
func Execute(spec project.Spec, now string, report func(Progress)) (Result, []string, error) {
	if report == nil {
		report = func(Progress) {}
	}
	// Reject an EXPLICIT over-host CPU/memory request, then resolve an UNSET value to
	// the platform default capped at the host — so the persisted config is never
	// larger than the machine.
	if err := ValidateResourcesWithinHost(spec.CPUs, spec.Memory); err != nil {
		return Result{}, nil, err
	}
	if err := ValidateDisk(spec.Disk); err != nil {
		return Result{}, nil, err
	}
	spec.CPUs, spec.Memory = CappedDefaultResources(spec.CPUs, spec.Memory)

	if err := project.EnsureCreatable(spec.Name, spec.Root); err != nil {
		return Result{}, nil, mapProjectErr(err)
	}
	// Create the location if it does not exist (Scaffold also MkdirAll's, but be
	// explicit so a brand-new path is clearly the workspace root).
	if err := os.MkdirAll(spec.Root, 0o755); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "create location %s: %s", spec.Root, err)
	}
	report(Progress{Step: "scaffolding project (Dockerfile, config, skills)"})
	if _, err := project.Scaffold(spec, now); err != nil {
		return Result{}, nil, mapProjectErr(err)
	}
	// Seed context-optimization defaults so the project config is self-describing. The
	// Caveman level is recorded here; the Caveman skill itself is installed by the real
	// toolkit at workspace start (workspace.registerCaveman), not scaffolded (arch §9).
	report(Progress{Step: "seeding context-optimization defaults"})
	if err := contextopt.SetStrategy(spec.Root, contextopt.DefaultStrategy); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "seed context strategy: %s", err)
	}
	if err := contextopt.SetCavemanLevel(spec.Root, contextopt.DefaultCavemanLevel); err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "seed caveman level: %s", err)
	}
	configYAML, err := os.ReadFile(config.ProjectPath(spec.Root))
	if err != nil {
		return Result{}, nil, output.Errorf(output.ExitRuntimeFailure, "read config.yaml: %s", err)
	}
	warnings := pullGraphifyModelIfAbsent(spec.GraphifyModel, report)
	warnings = append(warnings, oauthWarnings(spec)...)
	report(Progress{Step: "workspace scaffolded — start it to build the image + boot the microVM"})
	return Result{
		Name:       spec.Name,
		Root:       spec.Root,
		OS:         spec.OS,
		Tools:      spec.AgentCLIs,
		Stacks:     spec.Stacks,
		Apps:       spec.Apps,
		ConfigYAML: string(configYAML),
	}, warnings, nil
}

// oauthWarnings returns a security warning for each agent that runs in OAuth/plan mode:
// that mode routes the agent DIRECTLY to its provider, bypassing the gateway — so the tool
// firewall, secret masking, and content-level egress audit do NOT apply to it. Covers the
// OAuth-capable CLIs set to oauth AND the forced-oauth CLIs (copilot — always oauth, since
// it cannot route through the gateway at all). Selected agents only (Scaffold filters the
// persisted set the same way).
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

// ValidateResourcesWithinHost rejects a CPU/memory request that exceeds what the host
// can back (exit 2). Memory is capped BELOW host RAM (config.UsableHostMemoryMiB) so a
// microVM is never given the whole machine.
func ValidateResourcesWithinHost(cpus int, memory string) error {
	if err := config.ValidateCPUs(cpus); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if hostCPUs := sysinfo.CPUs(); cpus > hostCPUs {
		return output.Errorf(output.ExitInvalidInput, "--cpus %d exceeds the host's %d logical CPUs", cpus, hostCPUs)
	}
	if err := config.ValidateMemory(memory); err != nil {
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	}
	if memory != "" {
		if requested, err := config.ParseMemoryMiB(memory); err == nil {
			if hostMiB, ok := sysinfo.MemoryMiB(); ok {
				if usable := config.UsableHostMemoryMiB(hostMiB); requested > usable {
					return output.Errorf(output.ExitInvalidInput,
						"memory %s (%d MiB) exceeds the usable %d MiB — the platform reserves headroom for the host, service tier, and hypervisor (host has %d MiB; a microVM given all host RAM cannot boot)",
						memory, requested, usable, hostMiB)
				}
			}
		}
	}
	return nil
}

// ValidateDisk validates the workspace disk size ("<GB>", e.g. "20"). Blank is allowed
// (the platform default applies). The upper is sparse, so it is not host-capped like
// memory; only a malformed/non-positive value is rejected (exit 2).
func ValidateDisk(disk string) error {
	if strings.TrimSpace(disk) == "" {
		return nil
	}
	mib, err := config.ParseMemoryMiB(disk)
	if err != nil || mib <= 0 {
		return output.Errorf(output.ExitInvalidInput, "invalid disk size %q — use a plain number of GB (e.g. 20)", disk)
	}
	return nil
}

// CappedDefaultResources resolves an UNSET cpu/memory to the platform default and caps
// it at the host — so a host smaller than the default never yields an over-host config.
// Explicit over-host values are rejected earlier by ValidateResourcesWithinHost.
func CappedDefaultResources(cpus int, memory string) (int, string) {
	hostMiB, ok := sysinfo.MemoryMiB()
	return cappedResources(cpus, memory, sysinfo.CPUs(), hostMiB, ok)
}

// cappedResources is the host-agnostic core (host values injected so it is testable).
func cappedResources(cpus int, memory string, hostCPUs int, hostMiB uint64, hostMiBKnown bool) (int, string) {
	if cpus <= 0 {
		cpus = config.Default().Workspace.CPULimit
		if hostCPUs > 0 && cpus > hostCPUs {
			cpus = hostCPUs
		}
	}
	if memory == "" {
		memory = config.Default().Workspace.MemoryLimit
		if hostMiBKnown {
			usable := config.UsableHostMemoryMiB(hostMiB)
			if defaultMiB, err := config.ParseMemoryMiB(memory); err == nil && defaultMiB > usable {
				memory = fmt.Sprintf("%dM", usable)
			}
		}
	}
	return cpus, memory
}

// HostCPUs returns the host's logical CPU count (for the create wizard's vCPU hint).
func HostCPUs() int { return sysinfo.CPUs() }

// HostMemoryGB returns the host's total RAM in whole GB (0 if undeterminable).
func HostMemoryGB() int {
	if mib, ok := sysinfo.MemoryMiB(); ok {
		return int(mib / 1024)
	}
	return 0
}

// UsableHostMemoryGB is the largest workspace memory (whole GB) the platform allocates
// — host RAM minus the reserve for the host OS + service tier + hypervisor.
func UsableHostMemoryGB() int {
	if mib, ok := sysinfo.MemoryMiB(); ok {
		return int(config.UsableHostMemoryMiB(mib) / 1024)
	}
	return 0
}

// hfClient constructs the Hugging Face model-store client used to download the Graphify
// model. A package var so tests inject a fake; production wires hf.RealClient.
var hfClient = hf.RealClient

// modelRegistrar registers a freshly-pulled vLLM model in the gateway so it gains a
// stable id and shows in the live catalogue.
type modelRegistrar interface {
	RegisterVLLMModel(alias, model, apiBase string, supportsTools bool) error
}

// newRegistrar builds the gateway registrar. A package var so tests inject a fake;
// production binds a LiteLLM KeyManager over the real container prober.
var newRegistrar = func() modelRegistrar { return litellm.NewKeyManager(runtime.RealProber()) }

// graphifyVLLMServer is the host-side vLLM server manager slice used to serve the
// Graphify model: it starts/locates a per-model `vllm serve` endpoint. Production binds
// *vllm.Manager; tests a fake.
type graphifyVLLMServer interface {
	EnsureServedWithOptions(alias, model string, opts vllm.ServeOptions) (port int, endpoint string, err error)
}

// vLLM seams — package vars so tests inject fakes without touching the host.
var (
	vllmDetectFn       = vllm.Detect
	vllmPullFn         = vllm.Pull
	vllmManagerFactory = func() graphifyVLLMServer {
		return vllm.NewManager(vllm.Config{Runner: vllm.RealRunner{}, Probe: vllm.RealHealthProbe()})
	}
)

// graphifyAlias derives the gateway alias for a Graphify vLLM repo id: the id's last
// path segment (e.g. "mlx-community/Qwen2.5-7B-Instruct-4bit" → "Qwen2.5-7B-Instruct-4bit").
func graphifyAlias(repo string) string {
	if slash := strings.LastIndexByte(repo, '/'); slash >= 0 && slash < len(repo)-1 {
		return repo[slash+1:]
	}
	return repo
}

// pullGraphifyModelIfAbsent downloads the configured Graphify vLLM model (a curated
// Hugging Face repo id) into the vLLM store, starts its per-model server, and registers
// it in the gateway as vllm/<alias>. BEST-EFFORT: any failure is returned as a warning,
// never an error — the model can be pulled later with `ai models pull`. When vLLM is not
// installed it returns a warning pointing at `ai models install-vllm`.
func pullGraphifyModelIfAbsent(repo string, report func(Progress)) []string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil
	}
	if installed, _ := vllmDetectFn(); !installed {
		return []string{fmt.Sprintf("vLLM is not installed — the Graphify model %q was not pulled; install it with `ai models install-vllm`, then `ai models pull %s`", repo, repo)}
	}
	alias := graphifyAlias(repo)
	step := "pulling Graphify model " + repo
	report(Progress{Step: step})
	if err := vllmPullFn(repo); err != nil {
		return []string{fmt.Sprintf("could not prepare Graphify model %q: %s — pull it later with `ai models pull %s`", repo, err, repo)}
	}
	if err := hfClient().Download(repo, io.Discard); err != nil {
		return []string{fmt.Sprintf("could not download Graphify model %q: %s — pull it later with `ai models pull %s`", repo, err, repo)}
	}
	// Pass the curated tool-call/reasoning parsers (if confirmed for this repo) so
	// opencode's tool_choice="auto" requests work AND a thinking model's
	// <think>...</think> block does not leak raw into Graphify's structured output
	// — same as `ai models pull` (see hf.CuratedModel.ToolCallParser/ReasoningParser).
	port, _, err := vllmManagerFactory().EnsureServedWithOptions(alias, repo, vllm.ServeOptions{
		ToolCallParser:  hf.ToolCallParserFor(goruntime.GOOS, repo),
		ReasoningParser: hf.ReasoningParserFor(goruntime.GOOS, repo),
	})
	if err != nil {
		return []string{fmt.Sprintf("could not serve Graphify model %q: %s — pull it later with `ai models pull %s`", repo, err, repo)}
	}
	report(Progress{Step: "registering Graphify model " + repo + " with the gateway"})
	_ = config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: alias, Model: repo, Runtime: config.RuntimeVLLM,
		Endpoint: vllm.Endpoint(port), Status: "registered",
	})
	// vLLM tool support is model-dependent and not probed here; unknown defaults to capable.
	_ = newRegistrar().RegisterVLLMModel(alias, repo, vllm.ContainerEndpoint(port), true)
	return nil
}
