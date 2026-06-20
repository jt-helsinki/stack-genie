// Package console resolves the admin-console URLs of the host services and opens
// them in the user's browser, so a user can see what their setup is doing
// (LiteLLM usage/keys, the ClawPatrol firewall dashboard, …). The set of
// consoles is a small built-in registry of verified defaults; services without a
// web console are listed explicitly so the CLI can tell "no console" apart from
// "unknown service".
package console

import (
	"os/exec"
	"sort"
)

// consoles maps a host service to its admin-console URL. An empty value means the
// service is known but has no web console (e.g. Ollama is an API on :11434).
var consoles = map[string]string{
	"litellm":      "http://localhost:4000/ui", // LiteLLM admin UI (keys, usage, logs)
	"clawpatrol":   "http://127.0.0.1:8123",    // ClawPatrol firewall dashboard
	"ollama":       "",                         // HTTP API on :11434, no console UI
	"microsandbox": "",                         // microVM runtime, no console
	// Headroom is not a host service — it runs per-project in the workspace.
}

// Known reports whether name is a recognized host service.
func Known(name string) bool {
	_, ok := consoles[name]
	return ok
}

// URL returns the admin-console URL for a service and whether it has one.
func URL(name string) (string, bool) {
	url, ok := consoles[name]
	return url, ok && url != ""
}

// WithConsoles returns the services that have an admin console, sorted by name.
func WithConsoles() []NamedURL {
	var named []NamedURL
	for name, url := range consoles {
		if url != "" {
			named = append(named, NamedURL{Name: name, URL: url})
		}
	}
	sort.Slice(named, func(left, right int) bool { return named[left].Name < named[right].Name })
	return named
}

// NamedURL pairs a service with its console URL (for listings / JSON output).
type NamedURL struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Opener opens a URL in the user's browser. Injectable so the CLI is testable.
type Opener interface {
	Open(url string) error
}

// RealOpener returns an Opener that shells out to the platform's URL handler.
func RealOpener(goos string) Opener { return osOpener{goos: goos} }

type osOpener struct{ goos string }

func (opener osOpener) Open(url string) error {
	name, args := opener.command(url)
	return exec.Command(name, args...).Start()
}

func (opener osOpener) command(url string) (string, []string) {
	switch opener.goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "cmd", []string{"/c", "start", "", url}
	default:
		return "xdg-open", []string{url}
	}
}
