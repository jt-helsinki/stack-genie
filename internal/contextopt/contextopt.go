// Package contextopt backs the context-optimization commands (CLI §9, arch §8–10).
// Both components are per-project and live IN the workspace, not on the host:
// Headroom (input compression) runs as a local proxy wrapping the agent CLI, and
// Caveman (output compression) is an agent skill. This package owns the
// per-project configuration — the Headroom strategy and the Caveman level — and
// seeds the in-workspace artifacts (the Caveman skill today; Headroom is
// installed in the workspace image and reads the strategy from project config).
// Live Headroom token metrics require the running proxy in the workspace.
package contextopt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/jt-helsinki/ideal-robot/internal/config"
)

// Strategies and CavemanLevels are the valid choices (config repo-layout §12.4).
var (
	Strategies    = []string{"conservative", "balanced", "aggressive"}
	CavemanLevels = []string{"lite", "full", "ultra", "wenyan"}
)

// DefaultCavemanLevel and DefaultStrategy are seeded into a new project so its
// config is self-describing (matches the global defaults in config.Default).
const (
	DefaultCavemanLevel = "full"
	DefaultStrategy     = "balanced"
)

var (
	ErrInvalidStrategy     = errors.New("invalid context strategy")
	ErrInvalidCavemanLevel = errors.New("invalid caveman level")
)

// SetStrategy sets the Headroom input-compression strategy on a project.
func SetStrategy(projectRoot, strategy string) error {
	if !slices.Contains(Strategies, strategy) {
		return fmt.Errorf("%w: %q (one of %v)", ErrInvalidStrategy, strategy, Strategies)
	}
	projectConfig, err := config.LoadProjectConfig(projectRoot)
	if err != nil {
		return err
	}
	projectConfig.Context.Strategy = strategy
	return config.WriteProject(projectRoot, projectConfig)
}

// SetCavemanLevel sets the Caveman output-compression level on a project and
// (re)installs the Caveman skill at that level.
func SetCavemanLevel(projectRoot, level string) error {
	if !slices.Contains(CavemanLevels, level) {
		return fmt.Errorf("%w: %q (one of %v)", ErrInvalidCavemanLevel, level, CavemanLevels)
	}
	projectConfig, err := config.LoadProjectConfig(projectRoot)
	if err != nil {
		return err
	}
	projectConfig.Context.CavemanLevel = level
	if err := config.WriteProject(projectRoot, projectConfig); err != nil {
		return err
	}
	return InstallCavemanSkill(projectRoot, level)
}

// cavemanSkillPath is the project's Caveman skill file (arch §9, repo-layout §2.2).
func cavemanSkillPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".ai-platform", "skills", "caveman", "SKILL.md")
}

// InstallCavemanSkill seeds (or refreshes) the per-project Caveman skill at the
// given level. It is git-tracked and loaded by the in-workspace agent (arch §9).
func InstallCavemanSkill(projectRoot, level string) error {
	if level == "" {
		level = DefaultCavemanLevel
	}
	path := cavemanSkillPath(projectRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(cavemanSkill(level)), 0o644)
}

func cavemanSkill(level string) string {
	return fmt.Sprintf(`# Caveman — output compression

level: %s

This platform-seeded skill steers the in-workspace agent toward terse output to
reduce output tokens (Caveman: https://github.com/JuliusBrussee/caveman). It is
the output-side complement to Headroom (input compression).

Levels: lite | full | ultra | wenyan. Change with:

    ai context caveman <project> <level>
`, level)
}

// HeadroomMetrics are live input-compression metrics from the running proxy.
type HeadroomMetrics struct {
	TokensSaved int    `json:"tokens_saved"`
	Strategy    string `json:"strategy"`
}

// Status is the result of `ai context status` (CLI §9.1).
type Status struct {
	Strategy         string           `json:"strategy"`
	CavemanLevel     string           `json:"caveman_level"`
	CavemanInstalled bool             `json:"caveman_installed"`
	Headroom         *HeadroomMetrics `json:"headroom"` // nil until the proxy is running (hardware)
}

// GetStatus reports the configured strategy + Caveman level and whether the
// Caveman skill is installed. Live Headroom metrics need the running proxy.
func GetStatus(projectRoot string) (Status, error) {
	projectConfig, err := config.LoadProjectConfig(projectRoot)
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Strategy:     projectConfig.Context.Strategy,
		CavemanLevel: projectConfig.Context.CavemanLevel,
	}
	if _, err := os.Stat(cavemanSkillPath(projectRoot)); err == nil {
		status.CavemanInstalled = true
	}
	return status, nil
}
