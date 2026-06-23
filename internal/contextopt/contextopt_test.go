package contextopt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/config"
)

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

func TestSetCavemanLevelInstallsSkill(test *testing.T) {
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
	skill, err := os.ReadFile(filepath.Join(root, ".ai-platform", "skills", "caveman", "SKILL.md"))
	if err != nil {
		test.Fatalf("skill not installed: %v", err)
	}
	if !strings.Contains(string(skill), "level: ultra") {
		test.Fatalf("skill missing level:\n%s", skill)
	}
}

func TestSetCavemanLevelInvalid(test *testing.T) {
	if err := SetCavemanLevel(test.TempDir(), "mega"); !errors.Is(err, ErrInvalidCavemanLevel) {
		test.Fatalf("want ErrInvalidCavemanLevel, got %v", err)
	}
}

func TestStatusReflectsConfigAndSkill(test *testing.T) {
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

	after, err := GetStatus(root)
	if err != nil {
		test.Fatal(err)
	}
	if after.Strategy != "balanced" || after.CavemanLevel != "lite" || !after.CavemanInstalled {
		test.Fatalf("status: %+v", after)
	}
	if after.Headroom != nil {
		test.Fatal("Headroom metrics need the running proxy; expected nil host-side")
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
