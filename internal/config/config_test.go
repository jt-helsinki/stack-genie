package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMergeProjectOverGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	gp, _ := GlobalPath()
	writeFile(t, gp, `
runtime: docker
context:
  max_tokens: 64000
  strategy: balanced
`)
	projectRoot := filepath.Join(home, "projects", "app")
	writeFile(t, ProjectPath(projectRoot), `
os: alma
context:
  strategy: aggressive
`)

	cfg, err := Load(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime != "docker" { // from global, untouched by project
		t.Errorf("runtime = %q, want docker", cfg.Runtime)
	}
	if cfg.OS != "alma" { // only in project
		t.Errorf("os = %q, want alma", cfg.OS)
	}
	if cfg.Context.MaxTokens != 64000 { // global survives a nested partial override
		t.Errorf("max_tokens = %d, want 64000", cfg.Context.MaxTokens)
	}
	if cfg.Context.Strategy != "aggressive" { // project wins on the overlapping key
		t.Errorf("strategy = %q, want aggressive", cfg.Context.Strategy)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gp, _ := GlobalPath()
	writeFile(t, gp, "runtime: docker\nbogus: true\n")
	if _, err := Load(""); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

func TestMissingLayersOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("missing layers should not error: %v", err)
	}
	if cfg.Runtime != "" {
		t.Fatalf("expected empty config, got %+v", cfg)
	}
}
