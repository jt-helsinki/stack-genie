package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/uihosts"
)

// parseOptionalFlag turns the --optional CSV (the non-interactive setup contract)
// into a validated service list: "none" disables all, a CSV selects (trimmed),
// and any unknown name is exit 2.
func TestParseOptionalFlag(test *testing.T) {
	valid := []struct {
		in   string
		want []string
	}{
		{"none", []string{}},
		{"NONE", []string{}},
		{"open-webui", []string{"open-webui"}},
		{"open-webui,odysseus", []string{"open-webui", "odysseus"}},
		{"  open-webui , odysseus  ", []string{"open-webui", "odysseus"}},
		{"open-webui,,", []string{"open-webui"}},
	}
	for _, testCase := range valid {
		got, err := parseOptionalFlag(testCase.in)
		if err != nil {
			test.Errorf("parseOptionalFlag(%q) unexpected error: %v", testCase.in, err)
			continue
		}
		if !slices.Equal(got, testCase.want) {
			test.Errorf("parseOptionalFlag(%q) = %v, want %v", testCase.in, got, testCase.want)
		}
	}

	for _, bad := range []string{"bogus", "open-webui,bogus", "llm-guard"} {
		_, err := parseOptionalFlag(bad)
		var platformErr *output.Error
		if !errors.As(err, &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Errorf("parseOptionalFlag(%q) = %v, want exit %d", bad, err, output.ExitInvalidInput)
		}
	}
}

// optionalServicesFromFlag is the non-interactive (--json / no-TTY) resolution:
// an empty flag leaves the set unspecified (set=false, keep persisted/default);
// any value makes an explicit choice (set=true), with "none" meaning empty.
func TestOptionalServicesFromFlag(test *testing.T) {
	if set, services, err := optionalServicesFromFlag(""); set || services != nil || err != nil {
		test.Errorf("empty flag = (set=%v, %v, %v), want (false, nil, nil) — unspecified", set, services, err)
	}
	if set, services, err := optionalServicesFromFlag("none"); !set || len(services) != 0 || err != nil {
		test.Errorf("\"none\" = (set=%v, %v, %v), want (true, [], nil)", set, services, err)
	}
	set, services, err := optionalServicesFromFlag("open-webui")
	if !set || err != nil || !slices.Equal(services, []string{"open-webui"}) {
		test.Errorf("\"open-webui\" = (set=%v, %v, %v), want (true, [open-webui], nil)", set, services, err)
	}
	if _, _, err := optionalServicesFromFlag("bogus"); err == nil {
		test.Error("unknown --optional name must error (exit 2)")
	}
}

// The server-hostname seed defaults to localhost when nothing is persisted, and
// reuses the configured domain when set.
func TestDefaultServerDomain(test *testing.T) {
	if got := defaultServerDomain(nil); got != "localhost" {
		test.Errorf("no persisted info: got %q, want localhost", got)
	}
	if got := defaultServerDomain(&runtime.Info{}); got != "localhost" {
		test.Errorf("empty persisted domain: got %q, want localhost", got)
	}
	if got := defaultServerDomain(&runtime.Info{Domain: "aip.example.com"}); got != "aip.example.com" {
		test.Errorf("persisted domain: got %q, want aip.example.com", got)
	}
}

// The server hostname must pass the SHARED `ai domain` hostname validator
// (validateDomain) — including the localhost default (a bare label is valid).
func TestServerHostnameUsesSharedDomainValidator(test *testing.T) {
	if err := validateDomain("localhost"); err != nil {
		test.Fatalf("localhost (the server default) must validate: %v", err)
	}
	if err := validateDomain("aip.example.com"); err != nil {
		test.Fatalf("a real hostname must validate: %v", err)
	}
	if err := validateDomain("http://aip.example.com"); err == nil {
		test.Fatal("a URL must be rejected by the domain validator")
	}
}

// Under --json / no TTY only the server role carries a hostname; standalone/client
// leave it empty so Run's load-modify-save preserves or defaults the domain.
func TestNonInteractiveServerDomain(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if got := nonInteractiveServerDomain(runtime.RoleStandalone); got != "" {
		test.Errorf("standalone: got %q, want empty", got)
	}
	if got := nonInteractiveServerDomain(runtime.RoleClient); got != "" {
		test.Errorf("client: got %q, want empty", got)
	}
	// Server with no persisted runtime.yaml → empty (Run defaults it).
	if got := nonInteractiveServerDomain(runtime.RoleServer); got != "" {
		test.Errorf("server, no persisted domain: got %q, want empty", got)
	}
	// Server reuses a persisted domain.
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Domain: "kept.example.com"}); err != nil {
		test.Fatal(err)
	}
	if got := nonInteractiveServerDomain(runtime.RoleServer); got != "kept.example.com" {
		test.Errorf("server, persisted domain: got %q, want kept.example.com", got)
	}
}

// Server mode produces the per-UI credential guide (the post-setup admin-access
// instructions): each UI's URL plus how to set/rotate its password.
func TestServerModeProducesCredentialsGuide(test *testing.T) {
	guide := uihosts.ServerCredentialsGuide("aip.example.com")
	if !strings.Contains(guide, "ai litellm password") {
		test.Fatalf("server credentials guide must mention `ai litellm password`:\n%s", guide)
	}
	for _, url := range []string{
		"http://litellm.aip.example.com:18787/ui",
		"http://chat.aip.example.com:18787",
		"http://odysseus.aip.example.com:18787",
	} {
		if !strings.Contains(guide, url) {
			test.Fatalf("server credentials guide must list %q:\n%s", url, guide)
		}
	}
}
