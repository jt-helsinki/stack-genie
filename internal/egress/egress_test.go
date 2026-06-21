package egress

import (
	"errors"
	"testing"
)

func TestEgressLifecycle(test *testing.T) {
	root := test.TempDir()

	// Default (no config yet) resolves to deny.
	if network, _ := Get(root); network.ResolvedEgress() != "deny" {
		test.Errorf("default egress = %q, want deny", network.ResolvedEgress())
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
