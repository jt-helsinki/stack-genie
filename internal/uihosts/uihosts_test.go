package uihosts

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

func TestGatewayPortMatchesRuntime(t *testing.T) {
	if GatewayPort() != runtime.DefaultGatewayPort {
		t.Fatalf("gateway port %d must match runtime.DefaultGatewayPort %d",
			GatewayPort(), runtime.DefaultGatewayPort)
	}
}

// Names returns the host UI subdomains. The only remaining host UI vhost is
// litellm (Open WebUI is now a per-workspace in-VM app and Odysseus was
// removed), so the /etc/hosts entry carries just litellm.<domain>.
func TestNamesAlwaysAllSubdomains(t *testing.T) {
	names := Names("aip.example.com")
	want := []string{"litellm.aip.example.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for index, name := range want {
		if names[index] != name {
			t.Fatalf("name[%d]=%q want %q (full: %v)", index, names[index], name, names)
		}
	}
}

func TestEntriesSingleLoopbackEntry(t *testing.T) {
	entries := Entries("aip.local")
	if len(entries) != 1 {
		t.Fatalf("expected one entry carrying all names, got %d", len(entries))
	}
	if entries[0].IP != "127.0.0.1" {
		t.Fatalf("UI subdomains must resolve to loopback, got %q", entries[0].IP)
	}
	if len(entries[0].Names) != 1 {
		t.Fatalf("expected 1 name (the only UI subdomain), got %v", entries[0].Names)
	}
}

func TestURLsUseGatewayPort(t *testing.T) {
	urls := URLs("aip.local")
	if len(urls) != 1 {
		t.Fatalf("expected the single UI URL, got %d", len(urls))
	}
	for _, url := range urls {
		if !strings.HasSuffix(url.URL, ":18787") {
			t.Fatalf("URL must use the gateway port 18787: %q", url.URL)
		}
		if !strings.HasPrefix(url.URL, "http://"+url.Host+":") {
			t.Fatalf("URL %q should be built from host %q", url.URL, url.Host)
		}
	}
}

func TestManualMessageContainsBlockAndURLs(t *testing.T) {
	message := ManualMessage("aip.local")
	if !strings.Contains(message, "/etc/hosts") {
		t.Fatalf("manual message should mention /etc/hosts:\n%s", message)
	}
	if !strings.Contains(message, "127.0.0.1") {
		t.Fatalf("manual message should contain the rendered block:\n%s", message)
	}
	// The only host UI subdomain appears.
	for _, name := range []string{"litellm.aip.local"} {
		if !strings.Contains(message, name) {
			t.Fatalf("manual message should list %q:\n%s", name, message)
		}
	}
	if !strings.Contains(message, "http://litellm.aip.local:18787") {
		t.Fatalf("manual message should list the URLs:\n%s", message)
	}
}

func TestServerCredentialsGuideListsURLsAndPasswordHints(t *testing.T) {
	guide := ServerCredentialsGuide("aip.example.com")
	// The only host UI's reachable URL appears (LiteLLM admin UI).
	for _, url := range []string{
		"http://litellm.aip.example.com:18787/ui",
	} {
		if !strings.Contains(guide, url) {
			t.Fatalf("credentials guide should list %q:\n%s", url, guide)
		}
	}
	// LiteLLM: how to rotate the admin password.
	if !strings.Contains(guide, "ai litellm password") {
		t.Fatalf("credentials guide should mention `ai litellm password`:\n%s", guide)
	}
	if !strings.HasSuffix(guide, "\n") {
		t.Fatalf("credentials guide should end with a trailing newline:\n%q", guide)
	}
}

func TestServerGuidanceMentionsWildcardAndTLS(t *testing.T) {
	guidance := ServerGuidance("aip.example.com")
	if !strings.Contains(guidance, "*.aip.example.com") {
		t.Fatalf("server guidance should mention the wildcard DNS record:\n%s", guidance)
	}
	if !strings.Contains(guidance, "litellm.aip.example.com") {
		t.Fatalf("server guidance should list the per-host names:\n%s", guidance)
	}
	if !strings.Contains(strings.ToLower(guidance), "tls") {
		t.Fatalf("server guidance should mention TLS:\n%s", guidance)
	}
	if !strings.Contains(guidance, "no /etc/hosts editing") {
		t.Fatalf("server guidance should state /etc/hosts is not edited:\n%s", guidance)
	}
}
