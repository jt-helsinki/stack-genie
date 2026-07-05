package egress

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
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
	// ships open so in-VM nerdctl can pull images; it is re-lockable per project.
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

	// Allow: add, idempotent, host="" → gateway, bad port rejected.
	if err := Allow(root, "kafka-1", 9092); err != nil {
		test.Fatal(err)
	}
	if err := Allow(root, "kafka-1", 9092); err != nil { // idempotent
		test.Fatal(err)
	}
	if err := Allow(root, "", 5432); err != nil { // → gateway:5432
		test.Fatal(err)
	}
	if err := Allow(root, "x", 70000); !errors.Is(err, ErrInvalidPort) {
		test.Fatalf("out-of-range port should be ErrInvalidPort, got %v", err)
	}

	network, _ := Get(root)
	if network.Egress != "public" {
		test.Errorf("egress = %q, want public", network.Egress)
	}
	if len(network.AllowHostServices) != 2 {
		test.Fatalf("want 2 allow rules, got %+v", network.AllowHostServices)
	}
	var sawGateway bool
	for _, service := range network.AllowHostServices {
		if service.Host == "gateway" && service.Port == 5432 {
			sawGateway = true
		}
	}
	if !sawGateway {
		test.Errorf("empty host should default to gateway: %+v", network.AllowHostServices)
	}

	// Deny removes one.
	if err := Deny(root, "kafka-1", 9092); err != nil {
		test.Fatal(err)
	}
	if network, _ := Get(root); len(network.AllowHostServices) != 1 {
		test.Errorf("after deny want 1 rule, got %+v", network.AllowHostServices)
	}

	// Publish replaces by host port; unpublish removes.
	if err := Publish(root, 3000, 3000); err != nil {
		test.Fatal(err)
	}
	if err := Publish(root, 3001, 3000); err != nil { // same host port → replace
		test.Fatal(err)
	}
	network, _ = Get(root)
	if len(network.PublishPorts) != 1 || network.PublishPorts[0].Guest != 3001 {
		test.Fatalf("publish should replace by host port: %+v", network.PublishPorts)
	}
	if err := Unpublish(root, 3000); err != nil {
		test.Fatal(err)
	}
	if network, _ := Get(root); len(network.PublishPorts) != 0 {
		test.Errorf("after unpublish want 0, got %+v", network.PublishPorts)
	}
}

func TestGetReturnsMergedConfigWithProjectPriority(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := test.TempDir()
	writeGlobalConfig(test, `
network:
  egress: deny
  allow_host_services:
    - host: global-db
      port: 5432
  publish_ports:
    - guest: 8080
      host: 18080
`)
	if err := config.WriteProject(root, &config.Config{Network: config.NetworkConfig{
		Egress:            "public",
		AllowHostServices: []config.HostService{{Host: "project-db", Port: 15432}},
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
	if len(network.AllowHostServices) != 1 || network.AllowHostServices[0].Host != "project-db" {
		test.Fatalf("project allow-list should override global allow-list, got %+v", network.AllowHostServices)
	}
	if len(network.PublishPorts) != 1 || network.PublishPorts[0].Host != 18080 {
		test.Fatalf("global publish ports should survive when project omits them, got %+v", network.PublishPorts)
	}
}

func TestValidateHost(test *testing.T) {
	good := []string{
		"gateway",
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

func TestSplitPortPair(test *testing.T) {
	cases := []struct {
		input     string
		wantGuest int
		wantHost  int
		wantErr   bool
	}{
		// Valid "guest:host".
		{"3000:8080", 3000, 8080, false},
		// Surrounding whitespace is trimmed.
		{" 3000:8080 ", 3000, 8080, false},
		// Missing colon.
		{"3000", 0, 0, true},
		// Too many parts.
		{"1:2:3", 0, 0, true},
		// Non-numeric guest port.
		{"x:8080", 0, 0, true},
		// Non-numeric host port.
		{"3000:y", 0, 0, true},
	}
	for _, testCase := range cases {
		guest, host, err := SplitPortPair(testCase.input)
		if testCase.wantErr {
			if err == nil {
				test.Errorf("SplitPortPair(%q) = (%d,%d,nil), want error", testCase.input, guest, host)
			}
			continue
		}
		if err != nil {
			test.Errorf("SplitPortPair(%q) unexpected error: %v", testCase.input, err)
			continue
		}
		if guest != testCase.wantGuest || host != testCase.wantHost {
			test.Errorf("SplitPortPair(%q) = (%d,%d), want (%d,%d)",
				testCase.input, guest, host, testCase.wantGuest, testCase.wantHost)
		}
	}
}

func TestAllowRejectsBadHost(test *testing.T) {
	root := test.TempDir()
	if err := Allow(root, "http://api.github.com", 443); !errors.Is(err, ErrInvalidHost) {
		test.Fatalf("Allow with scheme should be ErrInvalidHost, got %v", err)
	}
	// A valid domain with default-ish HTTPS port is accepted.
	if err := Allow(root, "api.github.com", 443); err != nil {
		test.Fatalf("Allow domain: %v", err)
	}
	// A suffix wildcard is accepted.
	if err := Allow(root, "*.npmjs.org", 443); err != nil {
		test.Fatalf("Allow wildcard: %v", err)
	}
}
