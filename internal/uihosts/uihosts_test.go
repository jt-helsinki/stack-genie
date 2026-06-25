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

// Names returns ALL UI subdomains regardless of which optional services are
// enabled — the /etc/hosts entries are stable so enabling open-webui/odysseus
// later needs no further privileged edit.
func TestNamesAlwaysAllSubdomains(t *testing.T) {
	names := Names("aip.example.com")
	want := []string{"litellm.aip.example.com", "chat.aip.example.com", "odysseus.aip.example.com"}
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
	if len(entries[0].Names) != 3 {
		t.Fatalf("expected 3 names (all UI subdomains), got %v", entries[0].Names)
	}
}

func TestURLsUseGatewayPort(t *testing.T) {
	urls := URLs("aip.local")
	if len(urls) != 3 {
		t.Fatalf("expected all 3 UI URLs, got %d", len(urls))
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
	// ALL UI subdomains appear (stable hosts entries regardless of enabled set).
	for _, name := range []string{"litellm.aip.local", "chat.aip.local", "odysseus.aip.local"} {
		if !strings.Contains(message, name) {
			t.Fatalf("manual message should list %q:\n%s", name, message)
		}
	}
	if !strings.Contains(message, "http://litellm.aip.local:18787") {
		t.Fatalf("manual message should list the URLs:\n%s", message)
	}
}

func TestServerGuidanceMentionsWildcardAndTLS(t *testing.T) {
	guidance := ServerGuidance("aip.example.com")
	if !strings.Contains(guidance, "*.aip.example.com") {
		t.Fatalf("server guidance should mention the wildcard DNS record:\n%s", guidance)
	}
	if !strings.Contains(guidance, "litellm.aip.example.com") || !strings.Contains(guidance, "chat.aip.example.com") {
		t.Fatalf("server guidance should list the per-host names:\n%s", guidance)
	}
	if !strings.Contains(strings.ToLower(guidance), "tls") {
		t.Fatalf("server guidance should mention TLS:\n%s", guidance)
	}
	if !strings.Contains(guidance, "no /etc/hosts editing") {
		t.Fatalf("server guidance should state /etc/hosts is not edited:\n%s", guidance)
	}
}
