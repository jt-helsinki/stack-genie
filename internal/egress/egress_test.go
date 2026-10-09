package egress

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

func writeGlobalConfig(test *testing.T, content string) {
	test.Helper()
	path, err := config.GlobalPath()
	if err != nil {
		test.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

func TestEgressLifecycle(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := test.TempDir()

	// Default (no config yet) resolves to public (allow-outbound) — the workspace
	// starts open; it is re-lockable per project.
	if network, _ := Get(root); network.ResolvedEgress() != "public" {
		test.Errorf("default egress = %q, want public", network.ResolvedEgress())
	}

	// Mode: valid set, invalid rejected.
	if err := SetMode(root, "public"); err != nil {
		test.Fatal(err)
	}
	if err := SetMode(root, "bogus"); !errors.Is(err, ErrInvalidMode) {
		test.Fatalf("invalid mode should be ErrInvalidMode, got %v", err)
	}

	// Allow: add, idempotent, bad host rejected.
	if err := Allow(root, "api.github.com"); err != nil {
		test.Fatal(err)
	}
	if err := Allow(root, "api.github.com"); err != nil { // idempotent
		test.Fatal(err)
	}
	if err := Allow(root, "*.npmjs.org"); err != nil {
		test.Fatal(err)
	}
	if err := Allow(root, "http://bad"); !errors.Is(err, ErrInvalidHost) {
		test.Fatalf("bad host should be ErrInvalidHost, got %v", err)
	}

	network, _ := Get(root)
	if network.Egress != "public" {
		test.Errorf("egress = %q, want public", network.Egress)
	}
	if len(network.AllowDomain) != 2 {
		test.Fatalf("want 2 allow entries, got %+v", network.AllowDomain)
	}

	// Deny moves a host from allow to deny.
	if err := Deny(root, "api.github.com"); err != nil {
		test.Fatal(err)
	}
	network, _ = Get(root)
	if len(network.AllowDomain) != 1 || network.AllowDomain[0] != "*.npmjs.org" {
		test.Errorf("after deny, allow should keep only the untouched entry: %+v", network.AllowDomain)
	}
	if len(network.DenyDomain) != 1 || network.DenyDomain[0] != "api.github.com" {
		test.Errorf("after deny, deny should carry the moved entry: %+v", network.DenyDomain)
	}

	// Clear removes from both lists.
	if err := Clear(root, "api.github.com"); err != nil {
		test.Fatal(err)
	}
	network, _ = Get(root)
	if len(network.DenyDomain) != 0 {
		test.Errorf("after clear, deny should be empty: %+v", network.DenyDomain)
	}
}

func TestGetReturnsMergedConfigWithProjectPriority(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := test.TempDir()
	writeGlobalConfig(test, `
network:
  egress: deny
  allow_domain:
    - global.example.com
`)
	if err := config.WriteProject(root, &config.Config{Network: config.NetworkConfig{
		Egress:      "public",
		AllowDomain: []string{"project.example.com"},
	}}); err != nil {
		test.Fatal(err)
	}

	network, err := Get(root)
	if err != nil {
		test.Fatal(err)
	}
	if network.Egress != "public" {
		test.Fatalf("project egress should override global egress, got %q", network.Egress)
	}
	if len(network.AllowDomain) != 1 || network.AllowDomain[0] != "project.example.com" {
		test.Fatalf("project allow-list should override global allow-list, got %+v", network.AllowDomain)
	}
}

func TestValidateHost(test *testing.T) {
	good := []string{
		"10.0.0.5",
		"192.168.1.1",
		"db.internal",
		"api.github.com",
		"registry.npmjs.org",
		"localhost",
		"*.npmjs.org",
		"*.pkg.github.com",
	}
	for _, host := range good {
		if err := ValidateHost(host); err != nil {
			test.Errorf("ValidateHost(%q) = %v, want nil", host, err)
		}
	}

	bad := []string{
		"",
		" ",
		"has space",
		"with\ttab",
		"http://api.github.com",
		"https://example.com",
		"example.com/path",
		"*",
		"*.",
		"*.com",   // single suffix label
		"a*b.com", // mid-label wildcard
	}
	for _, host := range bad {
		if err := ValidateHost(host); !errors.Is(err, ErrInvalidHost) {
			test.Errorf("ValidateHost(%q) = %v, want ErrInvalidHost", host, err)
		}
	}
}

func TestAllowRejectsBadHost(test *testing.T) {
	root := test.TempDir()
	if err := Allow(root, "http://api.github.com"); !errors.Is(err, ErrInvalidHost) {
		test.Fatalf("Allow with scheme should be ErrInvalidHost, got %v", err)
	}
	// A valid domain is accepted.
	if err := Allow(root, "api.github.com"); err != nil {
		test.Fatalf("Allow domain: %v", err)
	}
	// A suffix wildcard is accepted.
	if err := Allow(root, "*.npmjs.org"); err != nil {
		test.Fatalf("Allow wildcard: %v", err)
	}
}
