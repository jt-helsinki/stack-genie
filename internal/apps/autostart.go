package apps

import (
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
)

// The gateway env for the app containers is passed by SHELL VARIABLE REFERENCE, not
// value: the generated script sources the in-VM agent env file (which exports the
// scoped virtual key + gateway URL) and then references ${OPENAI_*}. So the scoped
// LiteLLM virtual key is NEVER written into the script (or, therefore, onto host
// disk). These are the exact env-var names AgentEnvScript exports.
const (
	// apiKeyRef / modelRef are shell-variable references handed to runArgs in place of
	// literal values, so the rendered `nerdctl run` line carries `-e
	// "OPENAI_API_KEY=${OPENAI_API_KEY}"` (a reference) rather than a key value. The
	// gateway BASE URL is not a secret and is baked LITERALLY (passed to
	// AppsAutostartScript) so the app is wired to the gateway even when no Graphify model
	// is configured (OPENAI_BASE_URL is only exported by the agent env when a model is set).
	apiKeyRef = "${OPENAI_API_KEY}"
	modelRef  = "${OPENAI_MODEL}"
)

// AppsAutostartScript returns the shell script that (re)starts every INSTALLED in-VM
// app container inside the workspace microVM, or "" when no apps are installed.
//
// It is meant to run DETACHED as ROOT (nerdctl needs root) at workspace start: it
// sources the in-VM agent env file (agentcfg.AgentEnvFileGuestPath), waits for the
// in-VM container runtime (containerd) to be ready, then for each installed app
// idempotently recreates its container (`nerdctl rm -f` then `nerdctl run -d`) with
// the manifest's published port, persistent data volume, optional /workspace mount,
// memory limit, and gateway-pointing env.
//
// SECURITY: the KEY is rendered as a SHELL-VARIABLE REFERENCE (${OPENAI_API_KEY}),
// never a value, so the workspace's scoped virtual key is never staged into the
// script — it is resolved at run time from the sourced agent env file (falling back
// to the always-exported AIP_GATEWAY_KEY). No key value ever hits disk. gatewayURL is
// NOT a secret and is baked literally so the app reaches the gateway even when no
// Graphify model is configured (the agent env only exports OPENAI_BASE_URL with a model).
func AppsAutostartScript(projectConfig *config.Config, gatewayURL string) string {
	if projectConfig == nil {
		return ""
	}
	var body []string
	for _, entry := range projectConfig.Apps {
		manifest, ok := Lookup(entry.Key)
		if !ok {
			continue
		}
		// The persistent data dir is a host bind source (/persist/apps/<key>) that nerdctl
		// auto-creates ROOT-owned 0755. Apps whose container runs as a NON-root user (e.g.
		// a non-root user) then cannot write it — the app can crash on startup (e.g. a
		// SQLite "unable to open database file" error). Create it
		// world-writable up front so any container uid can persist state (the /persist
		// overlay is a single-user per-workspace volume, so 0777 is acceptable).
		dataDir := guestAppDataRoot + "/" + manifest.Key
		body = append(body, "mkdir -p "+dataDir+" && chmod 0777 "+dataDir)
		// Idempotent recreate: drop any prior container of the same name, then run fresh.
		body = append(body, "nerdctl rm -f "+manifest.ContainerName()+" 2>/dev/null || true")
		// Reuse runArgs so the container knowledge (name, port, volume, /workspace mount,
		// memory, env keys) lives in ONE place. The KEY is the shell reference above (so
		// runArgs renders `-e KEY=${OPENAI_API_KEY}`); the gateway URL is baked literally.
		argv := runArgs(manifest, entry.Port, gatewayURL, apiKeyRef, modelRef)
		parts := make([]string, len(argv))
		for index, token := range argv {
			parts[index] = renderScriptArg(token)
		}
		body = append(body, strings.Join(parts, " "))
	}
	if len(body) == 0 {
		return ""
	}
	var buffer strings.Builder
	buffer.WriteString("#!/usr/bin/env bash\n")
	buffer.WriteString("# Managed by the AI Development Platform — auto-starts the installed in-VM app\n")
	buffer.WriteString("# containers at workspace start (detached, best-effort). Each container is\n")
	buffer.WriteString("# recreated idempotently. The scoped virtual key is NEVER written here: the\n")
	buffer.WriteString("# gateway env is sourced from the in-VM agent env file and referenced by shell\n")
	buffer.WriteString("# variable, so no key value is ever staged to a file.\n")
	buffer.WriteString("set -a\n")
	buffer.WriteString("[ -f " + agentcfg.AgentEnvFileGuestPath + " ] && . " + agentcfg.AgentEnvFileGuestPath + "\n")
	buffer.WriteString("set +a\n")
	// AIP_GATEWAY_KEY (the scoped key) is ALWAYS exported by the agent env file; OPENAI_*
	// only when a Graphify model is configured. Fall the key back to it so the apps route
	// through the gateway even when no Graphify model is set.
	buffer.WriteString(": \"${OPENAI_API_KEY:=${AIP_GATEWAY_KEY}}\"\n")
	// Wait (bounded) for containerd to serve requests before running; ensureContainerd
	// already brought it up at start, this is defensive against a slow first boot.
	buffer.WriteString("iters=0; while [ $iters -lt 60 ]; do nerdctl info >/dev/null 2>&1 && break; iters=$((iters+1)); sleep 1; done\n")
	for _, line := range body {
		buffer.WriteString(line + "\n")
	}
	return buffer.String()
}

// renderScriptArg renders one runArgs token for embedding in the shell script: a token
// carrying a shell-variable reference (${…}) is double-quoted so the shell EXPANDS it at
// run time (and any special chars in the value are safe); the other tokens (flags,
// container name, port map, paths, image) contain no whitespace and are left bare.
func renderScriptArg(token string) string {
	if strings.Contains(token, "${") {
		return "\"" + token + "\""
	}
	return token
}
