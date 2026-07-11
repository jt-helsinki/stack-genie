package contextopt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestSetStrategy(test *testing.T) {
	root := test.TempDir()
	if err := SetStrategy(root, "aggressive"); err != nil {
		test.Fatal(err)
	}
	loaded, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if loaded.Context.Strategy != "aggressive" {
		test.Fatalf("strategy = %q", loaded.Context.Strategy)
	}
}

func TestSetStrategyInvalid(test *testing.T) {
	if err := SetStrategy(test.TempDir(), "turbo"); !errors.Is(err, ErrInvalidStrategy) {
		test.Fatalf("want ErrInvalidStrategy, got %v", err)
	}
}

func TestHeadroomParams(test *testing.T) {
	cases := map[string]struct{ keepTurns, bufferTokens int }{
		"conservative": {8, 12000},
		"balanced":     {5, 8000},
		"aggressive":   {2, 4000},
		"unknown":      {5, 8000}, // falls back to balanced
	}
	for strategy, want := range cases {
		keepTurns, bufferTokens := HeadroomParams(strategy)
		if keepTurns != want.keepTurns || bufferTokens != want.bufferTokens {
			test.Errorf("%s -> (%d,%d), want (%d,%d)", strategy, keepTurns, bufferTokens, want.keepTurns, want.bufferTokens)
		}
	}
	// More aggressive strategies must compress more (fewer kept turns, smaller buffer).
	conservativeTurns, conservativeBuf := HeadroomParams("conservative")
	aggressiveTurns, aggressiveBuf := HeadroomParams("aggressive")
	if aggressiveTurns >= conservativeTurns || aggressiveBuf >= conservativeBuf {
		test.Errorf("aggressive should compress more than conservative")
	}
}

// SetCavemanLevel records the level in config. It no longer writes a skill file —
// the real Caveman toolkit installs the skill at workspace start.
func TestSetCavemanLevelSetsConfig(test *testing.T) {
	root := test.TempDir()
	if err := SetCavemanLevel(root, "ultra"); err != nil {
		test.Fatal(err)
	}
	loaded, err := config.LoadProjectConfig(root)
	if err != nil {
		test.Fatal(err)
	}
	if loaded.Context.CavemanLevel != "ultra" {
		test.Fatalf("caveman level = %q", loaded.Context.CavemanLevel)
	}
	// The stub SKILL.md is no longer seeded by SetCavemanLevel.
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "skills", "caveman", "SKILL.md")); err == nil {
		test.Fatal("SetCavemanLevel should not write a skill file (installed at workspace start)")
	}
}

func TestSetCavemanLevelInvalid(test *testing.T) {
	if err := SetCavemanLevel(test.TempDir(), "mega"); !errors.Is(err, ErrInvalidCavemanLevel) {
		test.Fatalf("want ErrInvalidCavemanLevel, got %v", err)
	}
}

func TestStatusReflectsConfigAndSkill(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := test.TempDir()
	before, err := GetStatus(root)
	if err != nil {
		test.Fatal(err)
	}
	if before.CavemanInstalled {
		test.Fatal("caveman should not be installed initially")
	}

	if err := SetStrategy(root, "balanced"); err != nil {
		test.Fatal(err)
	}
	if err := SetCavemanLevel(root, "lite"); err != nil {
		test.Fatal(err)
	}

	afterConfig, err := GetStatus(root)
	if err != nil {
		test.Fatal(err)
	}
	// Config is set, but the skill is still absent until it is installed at start.
	if afterConfig.Strategy != "balanced" || afterConfig.CavemanLevel != "lite" || afterConfig.CavemanInstalled {
		test.Fatalf("status after config: %+v", afterConfig)
	}

	// Simulate the workspace-start installer landing the caveman skill in the pool.
	skillPath := filepath.Join(root, ".ai-platform", "skills", "caveman", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("---\nname: caveman\n---\n"), 0o644); err != nil {
		test.Fatal(err)
	}

	after, err := GetStatus(root)
	if err != nil {
		test.Fatal(err)
	}
	if !after.CavemanInstalled {
		test.Fatalf("caveman should be installed once the skill file exists: %+v", after)
	}
	if after.Headroom != nil {
		test.Fatal("Headroom metrics need the running proxy; expected nil host-side")
	}
}

func TestStatusReturnsMergedConfigWithProjectPriority(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	root := test.TempDir()
	writeGlobalConfig(test, `
context:
  strategy: aggressive
  caveman_level: ultra
`)
	if err := config.WriteProject(root, &config.Config{Context: config.ContextConfig{Strategy: "conservative"}}); err != nil {
		test.Fatal(err)
	}

	status, err := GetStatus(root)
	if err != nil {
		test.Fatal(err)
	}
	if status.Strategy != "conservative" {
		test.Fatalf("project strategy should override global strategy, got %q", status.Strategy)
	}
	if status.CavemanLevel != "ultra" {
		test.Fatalf("global caveman level should survive when project omits it, got %q", status.CavemanLevel)
	}
}

// Status.Human() renders a labeled two-column table of the strategy, the
// Caveman level, and whether the Caveman skill is installed.
func TestStatusHuman(test *testing.T) {
	rendered := Status{Strategy: "balanced", CavemanLevel: "lite", CavemanInstalled: true}.Human()
	for _, want := range []string{"SETTING", "VALUE", "Strategy", "balanced", "Caveman level", "lite", "Caveman installed", "true"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("context status table missing %q:\n%s", want, rendered)
		}
	}
}
