package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
)

// This file installs/registers the opt-in AI tools (config.yaml
// context.*_enabled: caveman, graphify, code-review-graph, codebase-memory-mcp,
// openviking, tokensave, agentmemory, ponytail, agent-skills, serena) on the HOST, once
// per project, best-effort. Under Microsandbox each of these ran INSIDE a
// project's own microVM via a staged script + `msb exec` — a filesystem and
// process-tree boundary nono does not provide (nono sandboxes a single host
// process, it does not virtualize $HOME or give each project its own OS). So a
// tool's HOME-scoped state (a `gemini extensions install`, hermes's
// ~/.hermes venv, OpenViking's one `openviking-server` on a fixed port) is now
// SHARED MACHINE-WIDE across every project that enables it, not isolated per
// workspace as it was under Microsandbox — a direct, deliberate scope change
// from moving off per-project VMs, not an oversight. Project-scoped tool state
// (anything written into <project>/.ai-platform or the project directory
// itself — caveman/graphify/code-review-graph/codebase-memory-mcp/tokensave/
// ponytail's skill file/agent-skills) remains correctly per-project, since the
// project directory IS the host directory, not a bind-mounted copy.
//
// Each registration is gated by its own install marker under
// <project>/.ai-platform, so re-running it on every Agent/Shell launch (there
// is no separate "start" step anymore — see Manager.Agent) is cheap after the
// first run. A slow install (network fetch, git clone, codebase build) is
// launched DETACHED (its own process group, released immediately) so it never
// blocks an agent/shell launch; a fast config-only step runs as a bounded
// blocking exec instead. Every step is best-effort: a failure is logged to
// stderr and retried on the next launch, never fails the launch itself.

// toolTeardownTimeout bounds every best-effort teardown exec below — all are
// fast (removing files, possibly invoking an already-installed tool's own
// uninstall verb), so a generous but bounded ceiling is enough.
const toolTeardownTimeout = 30 * time.Second

// graphifyInstallTimeout bounds the blocking git-init + per-CLI
// `graphify install`/`graphify uninstall` step (config-only — no build).
const graphifyInstallTimeout = 60 * time.Second

// codebaseMemoryInstallTimeout bounds codebase-memory-mcp's config-only
// `install` (auto-detects installed agent CLIs, writes their MCP config).
const codebaseMemoryInstallTimeout = 60 * time.Second

// sharedResourceKinds are the shared pools under <project>/.ai-platform/ that
// hold ONE copy of a skill/agent/prompt, symlinked into every CLI that wants
// it (linkSharedResources).
var sharedResourceKinds = []string{"agents", "skills", "prompts", "projects"}

// sharedResourceLink maps one shared-pool kind to each agent CLI's REAL
// per-project directory for that kind. A CLI absent from a kind's map has no
// concept of it and is skipped (codex/gemini have no skills/agents; gemini
// only has commands).
type sharedResourceLink struct {
	pool   string // .ai-platform/<pool>
	perCLI map[string]string
}

var sharedResourceLinks = []sharedResourceLink{
	{pool: "skills", perCLI: map[string]string{
		"opencode":    ".opencode/skills",
		"claude-code": ".claude/skills",
		"omp":         ".omp/skills",
		"hermes":      ".hermes/skills",
	}},
	{pool: "agents", perCLI: map[string]string{
		"opencode":    ".opencode/agents",
		"claude-code": ".claude/agents",
		"omp":         ".omp/agents",
	}},
	{pool: "prompts", perCLI: map[string]string{
		"opencode":    ".opencode/commands",
		"claude-code": ".claude/commands",
		"gemini":      ".gemini/commands",
		"omp":         ".omp/commands",
	}},
}

// linkSharedResources ensures the shared .ai-platform pools exist and
// symlinks each into every installed CLI's real per-project dir (relative
// symlinks). Idempotent: a correct symlink is left as-is, a wrong one is
// replaced, and a real (non-symlink) dir/file the user created is left
// untouched.
func linkSharedResources(root string, tools []string) error {
	for _, kind := range sharedResourceKinds {
		if err := os.MkdirAll(filepath.Join(root, ".ai-platform", kind), 0o755); err != nil {
			return err
		}
	}
	installed := make(map[string]bool, len(tools))
	for _, tool := range tools {
		installed[tool] = true
	}
	for _, link := range sharedResourceLinks {
		for cli, relDir := range link.perCLI {
			if !installed[cli] {
				continue
			}
			linkPath := filepath.Join(root, relDir)
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				return err
			}
			if err := ensureRelSymlink(linkPath, filepath.Join("..", ".ai-platform", link.pool)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureRelSymlink makes linkPath a symlink to target (relative), idempotently.
func ensureRelSymlink(linkPath, target string) error {
	if current, err := os.Readlink(linkPath); err == nil {
		if current == target {
			return nil
		}
		if err := os.Remove(linkPath); err != nil {
			return err
		}
	} else if _, statErr := os.Lstat(linkPath); statErr == nil {
		return nil // a real dir/file — do not clobber user content
	}
	return os.Symlink(target, linkPath)
}

// warnf prints a best-effort-failure warning to stderr, consistently prefixed.
func warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}

// shQuote single-quotes value for safe interpolation into a shell script.
func shQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// toolPool returns <root>/.ai-platform.
func toolPool(root string) string { return filepath.Join(root, ".ai-platform") }

// runDetachedScript writes script to <root>/.ai-platform/run/<tool>-install.sh
// and launches it as its own, detached process group (stdout+stderr captured
// to <root>/.ai-platform/run/<tool>-install.log), returning as soon as it has
// started — mirroring internal/omlx's detached host-process pattern. The
// script owns its own internal `timeout` bound and install marker, so a
// killed/incomplete run simply retries on the next launch.
func runDetachedScript(root, tool, script string) error {
	return runDetachedScriptFn(root, tool, script)
}

// runDetachedScriptFn is the injectable seam over the real detached-launch
// implementation (mirrors the project's established *Fn-variable pattern,
// e.g. internal/workspace/profile.go's pullPackFn) so unit tests can stage a
// tool's script and inspect it without actually running real network
// commands (git clone, curl, npx, …) in the background — a real launch
// outlives the test and races t.TempDir()'s cleanup.
var runDetachedScriptFn = realRunDetachedScript

func realRunDetachedScript(root, tool, script string) error {
	runDir := filepath.Join(toolPool(root), "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	scriptPath := filepath.Join(runDir, tool+"-install.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(runDir, tool+"-install.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	command := exec.Command("bash", scriptPath)
	command.Dir = root
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// runBlockingScript runs script synchronously (bounded by timeout) with root
// as its working directory. Used for fast, config-only tool steps the caller
// needs completed before returning.
func runBlockingScript(root, script string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", script)
	command.Dir = root
	return command.Run()
}

// registerAITools runs every opt-in AI tool's register/teardown step for
// project root, best-effort. Called once per Manager.Agent/Shell launch —
// cheap after the first run, since each step is gated by its own install
// marker. Must link the shared resource pool FIRST: several tools (caveman,
// ponytail, agent-skills) write into it and need the per-CLI symlinks to
// already exist for those files to reach opencode/claude-code/omp/gemini.
func (manager Manager) registerAITools(root string, projectConfig *config.Config) {
	if projectConfig == nil {
		return
	}
	if err := os.MkdirAll(toolPool(root), 0o755); err != nil {
		warnf("could not create .ai-platform: %v", err)
		return
	}
	if err := linkSharedResources(root, projectConfig.Agent.Tools); err != nil {
		warnf("could not link shared agent resources: %v", err)
	}
	manager.registerGraphify(root, projectConfig)
	manager.registerCaveman(root, projectConfig)
	manager.registerCodeReviewGraph(root, projectConfig)
	manager.registerCodebaseMemory(root, projectConfig)
	manager.registerOpenViking(root, projectConfig)
	manager.registerTokenSave(root, projectConfig)
	manager.registerAgentMemory(root, projectConfig)
	manager.registerPonytail(root, projectConfig)
	manager.registerAgentSkills(root, projectConfig)
	manager.registerSerena(root, projectConfig)
}

// --- Graphify -----------------------------------------------------------

// graphifyPlatformFlag maps a selected agent CLI to Graphify's own
// `install/uninstall --project --platform <token>` value. claude-code uses the
// default platform (empty token); omp/hermes are absent — they get Graphify's
// skill only via the shared resource pool (omp is not a Graphify platform at
// all; hermes has no graphify platform integration).
var graphifyPlatformFlag = map[string]string{
	"opencode":    "opencode",
	"claude-code": "",
	"codex":       "codex",
	"gemini":      "gemini",
}

// registerGraphify ALWAYS git-inits the project (independent of Graphify —
// every workspace should be git-backed) then, when enabled, installs Graphify
// with each selected agent CLI and installs its git hook; when disabled, runs
// Graphify's own uninstall for each previously-registered CLI. Detection of
// "already a repo" uses `git rev-parse --is-inside-work-tree`, not a bare
// `[ -d .git ]` test, since a valid repo can be a `.git` directory OR a `.git`
// file (a gitlink); a DANGLING gitlink file is removed first so `git init`
// produces a real standalone repo instead of following a dead pointer. Runs as
// a bounded BLOCKING exec (config-only, no network) so the caller can rely on
// the git-init having happened before returning.
func (manager Manager) registerGraphify(root string, projectConfig *config.Config) {
	graphifyOn := projectConfig.Context.GraphifyEnabledOrDefault()
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".graphify-installed")
	const gitPresent = "command -v git >/dev/null 2>&1"
	const isRepo = "git rev-parse --is-inside-work-tree >/dev/null 2>&1"
	const graphifyPresent = "command -v graphify >/dev/null 2>&1"
	clauses := []string{
		"if " + gitPresent + " && ! " + isRepo + "; then [ -f .git ] && rm -f .git; git init >/dev/null 2>&1; fi",
	}
	if graphifyOn {
		var installs []string
		for _, cli := range projectConfig.Agent.Tools {
			platform, known := graphifyPlatformFlag[cli]
			if !known {
				continue
			}
			install := "graphify install --project"
			if platform != "" {
				install += " --platform " + platform
			}
			installs = append(installs, install)
		}
		if len(installs) > 0 {
			clauses = append(clauses, "if "+graphifyPresent+" && [ ! -f "+shQuote(installMarker)+
				" ]; then "+strings.Join(installs, " && ")+" && touch "+shQuote(installMarker)+"; fi")
		}
		clauses = append(clauses, "if "+graphifyPresent+" && "+gitPresent+" && "+isRepo+"; then graphify hook install; fi")
	} else {
		var uninstalls []string
		for _, cli := range projectConfig.Agent.Tools {
			platform, known := graphifyPlatformFlag[cli]
			if !known {
				continue
			}
			uninstall := "graphify uninstall --project"
			if platform != "" {
				uninstall += " --platform " + platform
			}
			uninstalls = append(uninstalls, uninstall)
		}
		if len(uninstalls) > 0 {
			clauses = append(clauses, "if "+graphifyPresent+" && [ -f "+shQuote(installMarker)+
				" ]; then "+strings.Join(uninstalls, "; ")+"; fi")
		}
		clauses = append(clauses, "rm -rf "+shQuote(filepath.Join(root, "graphify-out"))+" "+shQuote(installMarker))
	}
	script := strings.Join(clauses, "; ")
	if err := runBlockingScript(root, script, graphifyInstallTimeout); err != nil {
		warnf("graphify registration failed (will retry next launch): %v", err)
	}
	if !graphifyOn {
		manager.stripGraphifyAgentsSection(root)
		return
	}
	manager.setupGraphifyMCP(root)
}

// graphifyTeardownScript surgically strips the "## graphify" section
// Graphify's own `graphify install --project` appends to the shared project
// AGENTS.md. `graphify uninstall --project` (run above when enabled→disabled)
// is the preferred path, but needs the `graphify` binary on PATH, which may
// already be gone by the time a disable is applied. This fallback needs no
// binary.
const graphifyTeardownScript = `import os, re

agents_path = os.path.join(os.getcwd(), "AGENTS.md")
if os.path.isfile(agents_path):
    with open(agents_path) as handle:
        content = handle.read()
    stripped = re.sub(r"(?m)^## graphify\n.*?(?=\n## |\Z)", "", content, count=1, flags=re.S)
    if stripped != content:
        with open(agents_path, "w") as handle:
            handle.write(stripped)
`

func (manager Manager) stripGraphifyAgentsSection(root string) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTeardownTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-c", graphifyTeardownScript)
	command.Dir = root
	_ = command.Run() // best-effort: python3 absent or nothing to strip is not an error
}

// setupGraphifyMCP installs graphifyy[mcp] into the project venv and builds
// the graph, DETACHED and best-effort. The MCP server (agentcfg.GraphifyMCPArgs)
// runs `<venv>/bin/python -m graphify.serve graphify-out/graph.json`, so the
// package must be importable from that venv. `graphify update .` is AST-only
// (no API cost); Graphify's own git hook keeps the graph fresh after.
func (manager Manager) setupGraphifyMCP(root string) {
	venvPython := filepath.Join(root, ".venv-msb", "bin", "python")
	script := "export PATH=\"$HOME/.local/bin:$PATH\"\n" +
		"[ -x " + shQuote(venvPython) + " ] || exit 0\n" +
		"uv pip install --python " + shQuote(venvPython) + " --quiet \"graphifyy[mcp]\"\n" +
		"command -v graphify >/dev/null 2>&1 && graphify update . || true\n"
	if err := runDetachedScript(root, "graphify-mcp", script); err != nil {
		warnf("graphify MCP setup could not be launched (will retry next launch): %v", err)
	}
}

// --- Caveman --------------------------------------------------------------

// cavemanOnlyAgent maps a selected agent CLI to Caveman's own
// (https://github.com/JuliusBrussee/caveman) `install.sh --only <agent>`
// token. omp is absent — Caveman has no omp token, so it receives the skill
// via the shared pool instead.
var cavemanOnlyAgent = map[string]string{
	"claude-code": "claude",
	"gemini":      "gemini",
	"opencode":    "opencode",
	"codex":       "codex",
}

// registerCaveman installs the Caveman output-compression toolkit, best-effort.
// It runs Caveman's OWN installer so each detected CLI gets its CLI-native
// integration (skills, agents, commands, the opencode plugin, claude hooks +
// statusline, the gemini extension), then mirrors the skill/agent/command dirs
// opencode received into the shared <project>/.ai-platform pool so omp (which
// reads skills ONLY from that pool) picks them up too. DETACHED — the install
// clones a repo and runs a node installer, a multi-second-to-minute network
// operation. ONCE-guarded by a marker under .ai-platform so a relaunch neither
// re-runs it nor clobbers user edits.
func (manager Manager) registerCaveman(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".caveman-installed")
	if !projectConfig.Context.CavemanEnabledOrDefault() {
		manager.teardownCaveman(root)
		return
	}
	only := make([]string, 0, len(projectConfig.Agent.Tools))
	for _, cli := range projectConfig.Agent.Tools {
		// gemini is skipped: its `extensions install` step prompts for
		// workspace-trust on the controlling TTY and ignores --non-interactive,
		// which BLOCKS FOREVER under a TTY-less background exec (verified
		// against the Microsandbox-era install). gemini still receives
		// Caveman's commands via the shared pool, same as omp.
		if cli == "gemini" {
			continue
		}
		if agent, ok := cavemanOnlyAgent[cli]; ok {
			only = append(only, "--only "+agent)
		}
	}
	if len(only) == 0 {
		return
	}
	// cloneDir is created at RUN TIME via mktemp -d (private, 0700, unpredictable
	// name) rather than a fixed shared-/tmp path — a predictable clone dir is a
	// local symlink/TOCTOU target (another local user could pre-stage a symlink
	// at a guessable name before the `rm -rf` + git clone runs).
	clone := `cloneDir=$(mktemp -d "${TMPDIR:-/tmp}/stackgenie-caveman-src.XXXXXX") && ` +
		"git clone --depth 1 https://github.com/JuliusBrussee/caveman \"$cloneDir\""
	installCmd := "cd \"$cloneDir\" && timeout --kill-after=30s 600s node bin/install.js --non-interactive --with-hooks " +
		strings.Join(only, " ")
	mirror := `og="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"; ` +
		`for pair in "skills:skills" "agents:agents" "commands:prompts"; do ` +
		`src="$og/${pair%%:*}"; dst=` + shQuote(pool) + `"/${pair##*:}"; ` +
		`[ -d "$src" ] || continue; mkdir -p "$dst"; cp -a "$src/." "$dst/" 2>/dev/null || true; done`
	skillMarkerFile := filepath.Join(pool, "skills", "caveman", "SKILL.md")
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"{ " + clone + " && " + installCmd + "; }; installed=$?; rm -rf \"$cloneDir\"\n" +
		"[ \"$installed\" -eq 0 ] && { " + mirror + "; } && [ -f " + shQuote(skillMarkerFile) +
		" ] && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "caveman", script); err != nil {
		warnf("caveman install could not be launched (will retry next launch): %v", err)
	}
}

// teardownCaveman removes Caveman's shared-pool skill/agent/command files when
// the tool is disabled. GAP: Caveman ships no confirmed uninstall command, so
// its CLI-native artifacts (claude-code hooks + statusline, the opencode
// plugin file, the gemini extension) are NOT reverted here — only what the
// platform mirrored into the shared pool is removed. Not gated on the install
// marker: a partial/failed install never touches it, and the removals below
// are idempotent no-ops when nothing was installed.
func (manager Manager) teardownCaveman(root string) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".caveman-installed")
	script := "rm -rf " + shQuote(filepath.Join(pool, "skills", "caveman")) + " " +
		shQuote(filepath.Join(pool, "agents", "caveman")) + " " + shQuote(filepath.Join(pool, "prompts", "caveman")) + "; " +
		"rm -f " + shQuote(installMarker)
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("caveman teardown could not complete (will retry next launch): %v", err)
	}
}

// --- code-review-graph ------------------------------------------------------

// codeReviewGraphPlatformFlag maps a selected agent CLI to code-review-graph's
// (https://code-review-graph.com) `install --platform <token>` value, used for
// CLIs whose config the platform does not itself own. codex/omp/hermes are
// absent — they get code-review-graph's MCP server via managed-config
// injection instead (agentcfg.EnabledMCPServers).
var codeReviewGraphPlatformFlag = map[string]string{
	"claude-code": "claude-code",
	"gemini":      "gemini-cli",
	"opencode":    "opencode",
}

// registerCodeReviewGraph installs code-review-graph and registers it as an
// MCP server with each selected, supported agent CLI, then builds + visualizes
// the codebase graph. DETACHED — `build` parses the whole codebase, which can
// take minutes on a large repo. No API key is used (local embeddings by
// default). ONCE-guarded by a marker under .ai-platform.
func (manager Manager) registerCodeReviewGraph(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".code-review-graph-installed")
	if !projectConfig.Context.CodeReviewGraphEnabledOrDefault() {
		manager.teardownCodeReviewGraph(root)
		return
	}
	installs := make([]string, 0, len(projectConfig.Agent.Tools))
	for _, cli := range projectConfig.Agent.Tools {
		platform, known := codeReviewGraphPlatformFlag[cli]
		if !known {
			continue
		}
		installs = append(installs, "code-review-graph install --platform "+platform)
	}
	if len(installs) == 0 {
		warnf("code-review-graph is enabled but no supported CLI (opencode, claude-code, or gemini) is selected — skipping install.")
		return
	}
	steps := append(installs, "code-review-graph build", "code-review-graph visualize")
	inner := strings.Join(steps, " && ")
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"command -v code-review-graph >/dev/null 2>&1 || exit 0\n" +
		"timeout --kill-after=30s 600s bash -c " + shQuote(inner) + "; installed=$?\n" +
		"[ \"$installed\" -eq 0 ] && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "code-review-graph", script); err != nil {
		warnf("code-review-graph install could not be launched (will retry next launch): %v", err)
	}
}

// codeReviewGraphTeardownScript surgically removes code-review-graph's MCP
// entry from opencode's project configs and its delimited section from
// AGENTS.md, without touching unrelated content in either file.
const codeReviewGraphTeardownScript = `import json, os, re

project = os.getcwd()

def strip_mcp_entry(path):
    if not os.path.isfile(path):
        return
    try:
        with open(path) as handle:
            data = json.load(handle)
    except (json.JSONDecodeError, OSError):
        return
    mcp = data.get("mcp")
    if not isinstance(mcp, dict) or "code-review-graph" not in mcp:
        return
    del mcp["code-review-graph"]
    if not mcp:
        data.pop("mcp", None)
    if not data or set(data.keys()) <= {"$schema"}:
        os.remove(path)
        return
    with open(path, "w") as handle:
        json.dump(data, handle, indent=2)
        handle.write("\n")

for relative in (os.path.join(project, "opencode.jsonc"), os.path.join(project, ".opencode", "opencode.jsonc")):
    strip_mcp_entry(relative)

agents_path = os.path.join(project, "AGENTS.md")
if os.path.isfile(agents_path):
    with open(agents_path) as handle:
        content = handle.read()
    stripped = content
    stripped = re.sub(
        r"\n?<!-- code-review-graph MCP tools -->.*?<!-- /code-review-graph MCP tools -->\n?",
        "\n", stripped, flags=re.S)
    stripped = re.sub(
        r"\n?## MCP Tools: code-review-graph.*?<!-- /code-review-graph MCP tools -->\n?",
        "\n", stripped, flags=re.S)
    if stripped != content:
        with open(agents_path, "w") as handle:
            handle.write(stripped)
`

// teardownCodeReviewGraph removes the built visualization, the install
// marker, and (via codeReviewGraphTeardownScript) the MCP entries/AGENTS.md
// section it wrote, when the tool is disabled. GAP: code-review-graph ships no
// confirmed uninstall command, so the MCP entries it wrote for
// claude-code/gemini are NOT reverted — they go stale but harmless once the
// binary is gone. Not gated on the install marker (see teardownCaveman).
func (manager Manager) teardownCodeReviewGraph(root string) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".code-review-graph-installed")
	ctx, cancel := context.WithTimeout(context.Background(), toolTeardownTimeout)
	defer cancel()
	cleanup := exec.CommandContext(ctx, "python3", "-c", codeReviewGraphTeardownScript)
	cleanup.Dir = root
	_ = cleanup.Run()
	script := "rm -rf " + shQuote(filepath.Join(root, ".code-review-graph")) + "; rm -f " + shQuote(installMarker)
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("code-review-graph teardown could not complete (will retry next launch): %v", err)
	}
}

// --- codebase-memory-mcp -----------------------------------------------------

// codebaseMemoryClients are the install targets codebase-memory-mcp is asked to configure.
// codex is deliberately absent: its installer fails on a ~/.codex/config.toml carrying
// nono's managed block (reproduced with v0.11.0), which aborted the whole install on every
// launch — and codex gets this server from agentcfg.EnabledMCPServers instead.
const codebaseMemoryClients = "claude,opencode,gemini,hermes,omp"

// registerCodebaseMemory registers codebase-memory-mcp
// (https://github.com/DeusData/codebase-memory-mcp) as an MCP server with each
// installed agent CLI via its auto-detecting `install` (no per-platform flag).
// Config-only (no network, no build), so it runs as a bounded BLOCKING exec.
// ONCE-guarded by a marker under .ai-platform.
func (manager Manager) registerCodebaseMemory(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".codebase-memory-installed")
	if !projectConfig.Context.CodebaseMemoryEnabledOrDefault() {
		manager.teardownCodebaseMemory(root)
		return
	}
	script := "[ -f " + shQuote(installMarker) + " ] && exit 0; " +
		"command -v codebase-memory-mcp >/dev/null 2>&1 || exit 0; " +
		"timeout --kill-after=15s 45s codebase-memory-mcp install --clients=" + codebaseMemoryClients + " && touch " + shQuote(installMarker)
	if err := runBlockingScript(root, script, codebaseMemoryInstallTimeout); err != nil {
		warnf("codebase-memory-mcp registration could not complete (will retry next launch): %v", err)
	}
}

// teardownCodebaseMemory removes the install marker when the tool is
// disabled. GAP: codebase-memory-mcp's install.sh has no uninstall mode, so
// the per-CLI MCP entries it auto-detected and wrote cannot be cleanly
// reverted — they go stale but harmless once the binary is gone.
func (manager Manager) teardownCodebaseMemory(root string) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".codebase-memory-installed")
	if err := os.Remove(installMarker); err != nil && !os.IsNotExist(err) {
		warnf("codebase-memory-mcp teardown could not complete (will retry next launch): %v", err)
	}
}

// --- OpenViking --------------------------------------------------------------

// openVikingPluginFlag maps a selected agent CLI to OpenViking's shared
// memory-plugin installer `--harness` value (the install.sh used by CLIs
// without a native plugin marketplace).
var openVikingPluginFlag = map[string]string{
	"opencode": "opencode",
	"omp":      "pi",
}

// registerOpenViking installs OpenViking (https://docs.openviking.ai) and
// registers it with each selected supported agent CLI. Unlike every other
// tool here, OpenViking runs a PERSISTENT SERVER on a FIXED port (127.0.0.1:
// 1933) — under Microsandbox this was safely per-workspace (one VM, one
// server); on the host it is necessarily a SINGLE SHARED server + data store
// for every project on this machine that enables it (pgrep-guarded, so a
// second project's launch just reuses the already-running one rather than
// conflicting). This is a deliberate, named scope reduction from the
// Microsandbox model, not a bug: a true per-project server would need a
// per-project port + data directory this platform does not yet allocate.
// DETACHED — clones no repo, but starts a server and waits on it. ONCE-guarded
// by a marker under .ai-platform.
func (manager Manager) registerOpenViking(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".openviking-installed")
	if !projectConfig.Context.OpenVikingEnabledOrDefault() {
		manager.teardownOpenViking(root)
		return
	}
	var registrations []string
	for _, cli := range projectConfig.Agent.Tools {
		switch cli {
		case "claude-code":
			registrations = append(registrations,
				"claude plugin marketplace add https://raw.githubusercontent.com/volcengine/OpenViking/main/.claude-plugin/marketplace.json",
				"claude plugin install openviking-memory@openviking")
		case "codex":
			registrations = append(registrations,
				"codex plugin marketplace add volcengine/OpenViking",
				"codex plugin add openviking-memory@openviking")
		default:
			if harness, ok := openVikingPluginFlag[cli]; ok {
				registrations = append(registrations,
					"bash <(curl -fsSL https://raw.githubusercontent.com/volcengine/OpenViking/main/examples/memory-plugin-shared/install.sh) --harness "+harness)
			}
		}
	}
	if len(registrations) == 0 {
		warnf("OpenViking is enabled but no supported CLI (opencode, claude-code, codex, or omp) is selected — skipping install.")
		return
	}
	// The server's own config must exist before it will start at all. Omitting
	// the `embedding` section falls back to its built-in local dense model — no
	// provider/API key needed to start (only semantic-search quality is
	// affected, verified against the Microsandbox-era install).
	ovHome := filepath.Join(os.Getenv("HOME"), ".openviking")
	ovConf := fmt.Sprintf(`{
  "storage": {"workspace": %q},
  "server": {"host": "127.0.0.1", "port": 1933}
}
`, filepath.Join(ovHome, "data"))
	if err := os.MkdirAll(ovHome, 0o755); err != nil {
		warnf("OpenViking config could not be staged (will retry next launch): %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(ovHome, "ov.conf"), []byte(ovConf), 0o644); err != nil {
		warnf("OpenViking config could not be staged (will retry next launch): %v", err)
		return
	}
	steps := []string{
		"mkdir -p " + shQuote(filepath.Join(ovHome, "data")),
		"pgrep -f openviking-server >/dev/null 2>&1 || " +
			"(setsid openviking-server --config " + shQuote(filepath.Join(ovHome, "ov.conf")) +
			" >" + shQuote(filepath.Join(ovHome, "server.log")) + " 2>&1 & sleep 3)",
		"ov language en",
		"ov config add custom --name workspace --url http://127.0.0.1:1933 --activate -o json",
	}
	steps = append(steps, registrations...)
	inner := strings.Join(steps, " && ")
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"command -v openviking-server >/dev/null 2>&1 || exit 0\n" +
		"timeout --kill-after=30s 300s bash -c " + shQuote(inner) + "; installed=$?\n" +
		"[ \"$installed\" -eq 0 ] && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "openviking", script); err != nil {
		warnf("OpenViking install could not be launched (will retry next launch): %v", err)
	}
}

// teardownOpenViking stops the shared OpenViking server and removes its data
// when the tool is disabled. Because the server is shared machine-wide (see
// registerOpenViking), this will also stop it for any OTHER project that
// still has OpenViking enabled — an accepted consequence of the shared-server
// design; that project's next launch simply restarts it. Not gated on the
// install marker (see teardownCaveman).
func (manager Manager) teardownOpenViking(root string) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".openviking-installed")
	ovHome := filepath.Join(os.Getenv("HOME"), ".openviking")
	script := "pkill -f openviking-server >/dev/null 2>&1; rm -rf " + shQuote(ovHome) + "; rm -f " + shQuote(installMarker)
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("OpenViking teardown could not complete (will retry next launch): %v", err)
	}
}

// --- tokensave ----------------------------------------------------------------

// tokenSaveAgentFlag maps an installed agent CLI to tokensave's own
// (https://github.com/aovestdipaperino/tokensave) `install --agent` value.
// codex/hermes/omp are absent — their configs are platform-rewritten wholesale
// each launch, so a native install would be clobbered; they get tokensave's
// MCP server via managed-config injection instead.
var tokenSaveAgentFlag = map[string]string{
	"claude-code": "claude",
	"opencode":    "opencode",
	"gemini":      "gemini",
}

// registerTokenSave builds the project's semantic index (`tokensave init
// --no-git-hook` — avoiding Graphify's own git hooks) and registers tokensave
// with each installed, natively-supported agent CLI. DETACHED — looping
// `install` across several CLIs can take a while. No API key is used. ONCE-
// guarded by a marker under .ai-platform.
func (manager Manager) registerTokenSave(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".tokensave-installed")
	if !projectConfig.Context.TokenSaveEnabledOrDefault() {
		manager.teardownTokenSave(root, projectConfig)
		return
	}
	installs := []string{"tokensave init --no-git-hook"}
	for _, cli := range projectConfig.Agent.Tools {
		if agent, known := tokenSaveAgentFlag[cli]; known {
			installs = append(installs, "tokensave install --agent "+agent+" --local")
		}
	}
	inner := strings.Join(installs, " && ")
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"command -v tokensave >/dev/null 2>&1 || exit 0\n" +
		"timeout --kill-after=30s 300s bash -c " + shQuote(inner) + " && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "tokensave", script); err != nil {
		warnf("tokensave install could not be launched (will retry next launch): %v", err)
	}
}

// teardownTokenSave reverts each previously-registered CLI's tokensave
// registration and removes the project-local index, when the tool is
// disabled. Not gated on the install marker (see teardownCaveman).
func (manager Manager) teardownTokenSave(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".tokensave-installed")
	var uninstalls []string
	for _, cli := range projectConfig.Agent.Tools {
		if agent, known := tokenSaveAgentFlag[cli]; known {
			uninstalls = append(uninstalls, "tokensave uninstall --agent "+agent+" --local || true")
		}
	}
	script := "rm -rf " + shQuote(filepath.Join(root, ".tokensave")) + "; rm -f " + shQuote(installMarker)
	if len(uninstalls) > 0 {
		script = "command -v tokensave >/dev/null 2>&1 && { " + strings.Join(uninstalls, "; ") + "; }; " + script
	}
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("tokensave teardown could not complete (will retry next launch): %v", err)
	}
}

// --- agentmemory ---------------------------------------------------------

// registerAgentMemory connects agentmemory
// (https://github.com/rohitg00/agentmemory) with claude-code (`agentmemory
// connect claude-code --with-hooks`) and/or gemini (`gemini mcp add
// agentmemory npx -y @agentmemory/mcp --scope user`), whichever are selected.
// codex/hermes get it via MCP injection instead; opencode gets neither native
// nor injected agentmemory — its native integration needs a nonstandard "mcp"
// JSON shape plus a copied plugin file too fragile to script reliably, a
// genuine gap, not an oversight. DETACHED — npx may fetch @agentmemory/mcp
// over the network on first run. ONCE-guarded by a marker under .ai-platform.
func (manager Manager) registerAgentMemory(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".agentmemory-installed")
	if !projectConfig.Context.AgentMemoryEnabledOrDefault() {
		manager.teardownAgentMemory(root, projectConfig)
		return
	}
	var installs []string
	if slices.Contains(projectConfig.Agent.Tools, "claude-code") {
		installs = append(installs, "agentmemory connect claude-code --with-hooks")
	}
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		installs = append(installs, "gemini mcp add agentmemory npx -y @agentmemory/mcp --scope user")
	}
	if len(installs) == 0 {
		return
	}
	inner := strings.Join(installs, " && ")
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"command -v agentmemory >/dev/null 2>&1 || exit 0\n" +
		"timeout --kill-after=30s 120s bash -c " + shQuote(inner) + " && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "agentmemory", script); err != nil {
		warnf("agentmemory install could not be launched (will retry next launch): %v", err)
	}
}

// teardownAgentMemory best-effort reverts claude-code/gemini's registrations
// when the tool is disabled. `agentmemory disconnect`/`gemini mcp remove` are
// unconfirmed symmetric counterparts to the connect/add verbs, attempted
// best-effort. Not gated on the install marker (see teardownCaveman).
func (manager Manager) teardownAgentMemory(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".agentmemory-installed")
	var uninstalls []string
	if slices.Contains(projectConfig.Agent.Tools, "claude-code") {
		uninstalls = append(uninstalls, "agentmemory disconnect claude-code || true")
	}
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		uninstalls = append(uninstalls, "gemini mcp remove agentmemory || true")
	}
	script := "rm -f " + shQuote(installMarker)
	if len(uninstalls) > 0 {
		script = "command -v agentmemory >/dev/null 2>&1 && { " + strings.Join(uninstalls, "; ") + "; }; " + script
	}
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("agentmemory teardown could not complete (will retry next launch): %v", err)
	}
}

// --- serena ------------------------------------------------------------------

// serenaVenvPython is the project Python environment Serena installs into — the same
// <project>/.venv-msb that Graphify's MCP server and agentconfig.enabledMCPServers use.
func serenaVenvPython(root string) string {
	return filepath.Join(root, ".venv-msb", "bin", "python")
}

// registerSerena installs Serena (https://github.com/oraios/serena, PyPI `serena-agent`)
// into the PROJECT's Python environment, creating <project>/.venv-msb with uv first when
// nothing has (no other step creates it since the microVM went away), then registers it with
// claude-code (`claude mcp add`, local scope — private to this machine, never the checked-in
// .mcp.json) and gemini (`gemini mcp add`, project scope) when selected. codex/hermes/omp get
// it through MCP injection (agentcfg.EnabledMCPServers) instead; opencode gets neither native
// nor injected Serena — the same gap as agentmemory (its MCP shape is nonstandard).
// DETACHED (a pip resolve plus language-server downloads) and ONCE-guarded by a marker; it
// needs uv and exits quietly — retrying next launch — when uv isn't on PATH.
func (manager Manager) registerSerena(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".serena-installed")
	if !projectConfig.Context.SerenaEnabledOrDefault() {
		manager.teardownSerena(root, projectConfig)
		return
	}
	venvPython := serenaVenvPython(root)
	serena := agentcfg.SerenaBinary(venvPython)
	var registrations []string
	if slices.Contains(projectConfig.Agent.Tools, "claude-code") {
		registrations = append(registrations, "claude mcp get serena >/dev/null 2>&1 || claude mcp add --scope local serena -- "+
			shQuote(serena)+" "+strings.Join(agentcfg.SerenaMCPArgs("claude-code"), " "))
	}
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		registrations = append(registrations, "gemini mcp add --scope project serena "+shQuote(serena)+" "+
			strings.Join(agentcfg.SerenaMCPArgs("ide-assistant"), " "))
	}
	script := "#!/usr/bin/env bash\n" +
		"export PATH=\"$HOME/.local/bin:$PATH\"\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"command -v uv >/dev/null 2>&1 || exit 0\n" +
		"[ -x " + shQuote(venvPython) + " ] || uv venv " + shQuote(filepath.Join(root, ".venv-msb")) + " || exit 0\n" +
		"uv pip install --python " + shQuote(venvPython) + " --quiet serena-agent || exit 0\n"
	for _, registration := range registrations {
		script += "{ " + registration + "; } || exit 0\n"
	}
	script += "touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "serena", script); err != nil {
		warnf("serena install could not be launched (will retry next launch): %v", err)
	}
}

// teardownSerena best-effort removes the claude-code/gemini registrations when the tool is
// disabled and clears the install marker. The serena-agent package stays in .venv-msb
// (harmless; removed with the project's artifacts on `ai delete`). Not gated on the marker
// (see teardownCaveman).
func (manager Manager) teardownSerena(root string, projectConfig *config.Config) {
	installMarker := filepath.Join(toolPool(root), ".serena-installed")
	var uninstalls []string
	if slices.Contains(projectConfig.Agent.Tools, "claude-code") {
		uninstalls = append(uninstalls, "claude mcp remove --scope local serena || true")
	}
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		uninstalls = append(uninstalls, "gemini mcp remove --scope project serena || true")
	}
	script := "rm -f " + shQuote(installMarker)
	if len(uninstalls) > 0 {
		script = strings.Join(uninstalls, "; ") + "; " + script
	}
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("serena teardown could not complete (will retry next launch): %v", err)
	}
}

// --- ponytail -------------------------------------------------------------

// registerPonytail drops the Ponytail skill
// (https://github.com/DietrichGebert/ponytail — nags the agent to check
// YAGNI/stdlib/existing code before writing anything new) into the shared
// skill pool, reaching every skill-capable CLI through linkSharedResources'
// symlinks, and best-effort installs gemini's native extension when gemini is
// selected. codex/hermes get no Ponytail integration — a genuine gap, no
// viable non-interactive install path exists for either. DETACHED — fetches
// the skill file (and the gemini extension) over the network. ONCE-guarded.
func (manager Manager) registerPonytail(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".ponytail-installed")
	if !projectConfig.Context.PonytailEnabledOrDefault() {
		manager.teardownPonytail(root, projectConfig)
		return
	}
	skillDir := filepath.Join(pool, "skills", "ponytail")
	inner := "mkdir -p " + shQuote(skillDir) + " && curl -fsSL " +
		"https://raw.githubusercontent.com/DietrichGebert/ponytail/main/skills/ponytail/SKILL.md " +
		"-o " + shQuote(filepath.Join(skillDir, "SKILL.md"))
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		inner += " && { command -v gemini >/dev/null 2>&1 && " +
			"gemini extensions install https://github.com/DietrichGebert/ponytail || true; }"
	}
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"timeout --kill-after=15s 60s bash -c " + shQuote(inner) + " && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "ponytail", script); err != nil {
		warnf("ponytail install could not be launched (will retry next launch): %v", err)
	}
}

// teardownPonytail removes the shared-pool skill file when the tool is
// disabled and best-effort reverts the gemini extension (`gemini extensions
// uninstall` — unconfirmed, attempted best-effort). Not gated on the install
// marker (see teardownCaveman).
func (manager Manager) teardownPonytail(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".ponytail-installed")
	inner := "rm -rf " + shQuote(filepath.Join(pool, "skills", "ponytail"))
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		inner += "; command -v gemini >/dev/null 2>&1 && gemini extensions uninstall ponytail || true"
	}
	script := "bash -c " + shQuote(inner) + "; rm -f " + shQuote(installMarker)
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("ponytail teardown could not complete (will retry next launch): %v", err)
	}
}

// --- agent-skills ---------------------------------------------------------

// registerAgentSkills clones addyosmani/agent-skills
// (https://github.com/addyosmani/agent-skills — a curated library of
// senior-engineer skills) and mirrors its skills/agents/references dirs into
// the shared resource pool, reaching every skill-capable CLI through
// linkSharedResources' symlinks, and best-effort installs gemini's native
// `gemini skills install` when gemini is selected. codex/hermes get no
// agent-skills integration — a genuine gap, no viable non-interactive path
// for either. The exact set of copied names is recorded in two manifest files
// so teardown can remove precisely what was added. DETACHED — clones a small
// repo over the network. ONCE-guarded.
func (manager Manager) registerAgentSkills(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".agent-skills-installed")
	skillsManifest := filepath.Join(pool, ".agent-skills-manifest-skills")
	agentsManifest := filepath.Join(pool, ".agent-skills-manifest-agents")
	if !projectConfig.Context.AgentSkillsEnabledOrDefault() {
		manager.teardownAgentSkills(root, projectConfig)
		return
	}
	// cloneDir is created at RUN TIME via mktemp -d (private, 0700,
	// unpredictable name) — see registerCaveman's identical rationale.
	inner := `cloneDir=$(mktemp -d "${TMPDIR:-/tmp}/stackgenie-agent-skills-src.XXXXXX") && ` +
		"git clone --depth 1 https://github.com/addyosmani/agent-skills.git \"$cloneDir\" && " +
		"mkdir -p " + shQuote(filepath.Join(pool, "skills")) + " " + shQuote(filepath.Join(pool, "agents")) + " " +
		shQuote(filepath.Join(pool, "references")) + " && " +
		"cp -a \"$cloneDir/skills/.\" " + shQuote(filepath.Join(pool, "skills")) + "/ && " +
		"cp -a \"$cloneDir/agents/.\" " + shQuote(filepath.Join(pool, "agents")) + "/ && " +
		"cp -a \"$cloneDir/references/.\" " + shQuote(filepath.Join(pool, "references")) + "/ && " +
		"(cd \"$cloneDir/skills\" && ls -1) > " + shQuote(skillsManifest) + " && " +
		"(cd \"$cloneDir/agents\" && ls -1) > " + shQuote(agentsManifest) + " && " +
		"rm -rf \"$cloneDir\""
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		inner += " && { command -v gemini >/dev/null 2>&1 && " +
			"gemini skills install https://github.com/addyosmani/agent-skills.git --path skills || true; }"
	}
	script := "#!/usr/bin/env bash\n" +
		"[ -f " + shQuote(installMarker) + " ] && exit 0\n" +
		"command -v git >/dev/null 2>&1 || exit 0\n" +
		"timeout --kill-after=15s 120s bash -c " + shQuote(inner) + " && touch " + shQuote(installMarker) + "\n"
	if err := runDetachedScript(root, "agent-skills", script); err != nil {
		warnf("agent-skills install could not be launched (will retry next launch): %v", err)
	}
}

// teardownAgentSkills removes exactly the shared-pool files recorded in the
// install's manifests, plus the references dir, when the tool is disabled.
// Also best-effort attempts `gemini skills uninstall` (unconfirmed symmetric
// counterpart). Not gated on the install marker (see teardownCaveman).
func (manager Manager) teardownAgentSkills(root string, projectConfig *config.Config) {
	pool := toolPool(root)
	installMarker := filepath.Join(pool, ".agent-skills-installed")
	skillsManifest := filepath.Join(pool, ".agent-skills-manifest-skills")
	agentsManifest := filepath.Join(pool, ".agent-skills-manifest-agents")
	inner := "if [ -f " + shQuote(skillsManifest) + " ]; then while IFS= read -r entry; do " +
		"[ -n \"$entry\" ] && rm -rf " + shQuote(filepath.Join(pool, "skills")) + "/\"$entry\"; done < " + shQuote(skillsManifest) + "; fi; " +
		"if [ -f " + shQuote(agentsManifest) + " ]; then while IFS= read -r entry; do " +
		"[ -n \"$entry\" ] && rm -f " + shQuote(filepath.Join(pool, "agents")) + "/\"$entry\"; done < " + shQuote(agentsManifest) + "; fi; " +
		"rm -rf " + shQuote(filepath.Join(pool, "references"))
	if slices.Contains(projectConfig.Agent.Tools, "gemini") {
		inner += "; command -v gemini >/dev/null 2>&1 && gemini skills uninstall agent-skills || true"
	}
	script := "bash -c " + shQuote(inner) + "; rm -f " + shQuote(installMarker) + " " + shQuote(skillsManifest) + " " + shQuote(agentsManifest)
	if err := runBlockingScript(root, script, toolTeardownTimeout); err != nil {
		warnf("agent-skills teardown could not complete (will retry next launch): %v", err)
	}
}
