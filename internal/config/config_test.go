package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(test *testing.T, path, content string) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

func TestMergeProjectOverGlobal(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	gp, _ := GlobalPath()
	writeFile(test, gp, `
runtime: docker
context:
  max_tokens: 64000
  strategy: balanced
`)
	projectRoot := filepath.Join(home, "projects", "app")
	writeFile(test, ProjectPath(projectRoot), `
os: alma
context:
  strategy: aggressive
`)

	cfg, err := Load(projectRoot)
	if err != nil {
		test.Fatal(err)
	}
	if cfg.Runtime != "docker" { // from global, untouched by project
		test.Errorf("runtime = %q, want docker", cfg.Runtime)
	}
	if cfg.OS != "alma" { // only in project
		test.Errorf("os = %q, want alma", cfg.OS)
	}
	if cfg.Context.MaxTokens != 64000 { // global survives a nested partial override
		test.Errorf("max_tokens = %d, want 64000", cfg.Context.MaxTokens)
	}
	if cfg.Context.Strategy != "aggressive" { // project wins on the overlapping key
		test.Errorf("strategy = %q, want aggressive", cfg.Context.Strategy)
	}
}

func TestUnknownFieldRejected(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	gp, _ := GlobalPath()
	writeFile(test, gp, "runtime: docker\nbogus: true\n")
	if _, err := Load(""); err == nil {
		test.Fatal("expected unknown-field rejection")
	}
}

func TestMissingLayersOK(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	cfg, err := Load("")
	if err != nil {
		test.Fatalf("missing layers should not error: %v", err)
	}
	if cfg.Runtime != "" {
		test.Fatalf("expected empty config, got %+v", cfg)
	}
}

func TestNetworkValidate(test *testing.T) {
	valid := NetworkConfig{
		EgressProxy:       "clawpatrol",
		AllowHostServices: []HostService{{Host: "gateway", Port: 5432}},
		PublishPorts:      []PortMapping{{Guest: 3000, Host: 3000}},
	}
	if err := valid.Validate(); err != nil {
		test.Fatalf("valid network rejected: %v", err)
	}

	badPort := NetworkConfig{AllowHostServices: []HostService{{Port: 70000}}}
	if err := badPort.Validate(); err == nil {
		test.Fatal("expected out-of-range port to be rejected")
	}

	dupHost := NetworkConfig{PublishPorts: []PortMapping{{Guest: 3000, Host: 8080}, {Guest: 3001, Host: 8080}}}
	if err := dupHost.Validate(); err == nil {
		test.Fatal("expected duplicate host port to be rejected")
	}
}

func TestResolveHostServices(test *testing.T) {
	network := NetworkConfig{AllowHostServices: []HostService{
		{Host: "gateway", Port: 5432},  // gateway token → resolved address
		{Port: 6379},                   // empty host → resolved address
		{Host: "10.0.0.5", Port: 9092}, // explicit host preserved
	}}
	resolved := network.ResolveHostServices("192.0.2.1")
	want := []string{"192.0.2.1:5432", "192.0.2.1:6379", "10.0.0.5:9092"}
	if len(resolved) != len(want) {
		test.Fatalf("resolved = %v, want %v", resolved, want)
	}
	for index := range want {
		if resolved[index] != want[index] {
			test.Errorf("resolved[%d] = %q, want %q", index, resolved[index], want[index])
		}
	}
}
