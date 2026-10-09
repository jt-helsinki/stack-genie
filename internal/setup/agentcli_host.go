package setup

// agentcli_host.go installs agent CLIs directly onto the HOST (not into a
// per-workspace microVM image — there is no image anymore) so they can be run
// under `nono run --profile ... -- <cli>`, wired to the existing LiteLLM
// gateway. Install commands are each CLI's own official installer, verified
// against their current docs (not guessed):
//
//   - claude-code: https://code.claude.com/docs/en/setup — curl -fsSL https://claude.ai/install.sh | bash
//   - codex:       https://learn.chatgpt.com/docs/codex/cli — curl -fsSL https://chatgpt.com/codex/install.sh | sh
//   - opencode:    https://opencode.ai/docs/ — curl -fsSL https://opencode.ai/install | bash
//
// Each is Detect-gated (binary already on PATH is left untouched) and
// STRICTLY best-effort, matching ensureNonoInstalled/ensureOmlxInstalled.

import "os/exec"

// HostAgentCLIs are the agent CLIs this platform can install directly onto the
// host (as opposed to a per-workspace image, which no longer exists). Keys
// match the binary name on PATH.
const (
	HostAgentClaudeCode = "claude-code"
	HostAgentCodex      = "codex"
	HostAgentOpenCode   = "opencode"
)

// SupportedHostAgentCLIs lists the host-installable agent CLIs, in the order
// offered by `ai setup`'s picker.
func SupportedHostAgentCLIs() []string {
	return []string{HostAgentClaudeCode, HostAgentCodex, HostAgentOpenCode}
}

// hostAgentCLI describes one installable host agent CLI.
type hostAgentCLI struct {
	// binary is the command name Detect checks for on PATH.
	binary string
	// install performs the real install.
	install func() error
}

// hostAgentCLIsFn is the injectable seam over the hostAgentCLI registry, so
// tests exercise EnsureHostAgentCLIsInstalled without real network installs.
var hostAgentCLIsFn = func() map[string]hostAgentCLI {
	return map[string]hostAgentCLI{
		HostAgentClaudeCode: {binary: "claude", install: func() error { return curlPipeShell("https://claude.ai/install.sh") }},
		HostAgentCodex:      {binary: "codex", install: func() error { return curlPipeShell("https://chatgpt.com/codex/install.sh") }},
		HostAgentOpenCode:   {binary: "opencode", install: func() error { return curlPipeShell("https://opencode.ai/install") }},
	}
}

// agentCLILookPathFn is the injectable seam for the PATH presence check, so
// tests exercise EnsureHostAgentCLIsInstalled without depending on what's
// actually installed on the machine running the test.
var agentCLILookPathFn = exec.LookPath

// EnsureHostAgentCLIsInstalled installs each selected agent CLI directly onto
// the host when its binary is not already on PATH. Unknown keys are skipped
// silently (callers validate against SupportedHostAgentCLIs upstream). STRICTLY
// best-effort: a failed install is reported via progress and swallowed — it
// NEVER fails `ai setup`.
func EnsureHostAgentCLIsInstalled(selected []string, progress func(string)) {
	registry := hostAgentCLIsFn()
	for _, key := range selected {
		cli, known := registry[key]
		if !known {
			continue
		}
		if _, err := agentCLILookPathFn(cli.binary); err == nil {
			progress("  • " + key + " present on PATH")
			continue
		}
		progress("  • installing " + key + " on the host…")
		if err := cli.install(); err != nil {
			progress("  • " + key + " install skipped (" + err.Error() + ")")
			continue
		}
		if _, err := agentCLILookPathFn(cli.binary); err == nil {
			progress("  • " + key + " installed")
			continue
		}
		progress("  • " + key + " install ran but the binary was not detected on PATH — check `" + cli.binary + " --version`")
	}
}
