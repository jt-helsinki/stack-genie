// Package contextopt backs the context-optimization commands (CLI §9, arch §8–10).
// Caveman (output compression) is a per-project agent skill that lives IN the
// workspace. Headroom (input compression) runs as a shared host container in
// front of LiteLLM (arch §15); per-project tuning survives because the project's
// strategy maps to the two per-request knobs Headroom honors (HeadroomParams),
// baked into the agent's request body at workspace start. This package owns the
// per-project configuration — the Headroom strategy and the Caveman level — and
// seeds the in-workspace Caveman skill. Live Headroom token metrics require the
// running host proxy.
package contextopt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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

// HeadroomParams maps a project's compression strategy to the two per-request
// knobs the host Headroom proxy honors: keep_turns (recent turns kept verbatim)
// and output_buffer_tokens (output token reservation). Higher values compress
// less. These ride in the agent's request body (extra_body) so per-project
// control survives the shared host Headroom (arch §8–10, §15). An unknown
// strategy falls back to balanced (Headroom's documented defaults).
func HeadroomParams(strategy string) (keepTurns, outputBufferTokens int) {
	switch strategy {
	case "conservative":
		return 8, 12000
	case "aggressive":
		return 2, 4000
	default: // balanced
		return 5, 8000
	}
}

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
	// The Agent Skills standard (claude/opencode/pi all read it) requires YAML
	// frontmatter with `name` and `description` — pi rejects a skill missing the
	// description. Keep the body's `level:` line so the level is visible + testable.
	return fmt.Sprintf(`---
name: caveman
description: Output-compression guidance (level %s) — steer responses toward terse output to cut output tokens; the output-side complement to Headroom input compression.
---

# Caveman — output compression

level: %s

This platform-seeded skill steers the in-workspace agent toward terse output to
reduce output tokens (Caveman: https://github.com/JuliusBrussee/caveman). It is
the output-side complement to Headroom (input compression).

Levels: lite | full | ultra | wenyan. Change with:

    ai context caveman <project> <level>
`, level, level)
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

// Human renders the context-optimization status as a two-column table of
// labeled settings — the Headroom strategy, the Caveman level, and whether the
// Caveman skill is installed — for non-JSON output.
func (status Status) Human() string {
	installed := ui.Muted.Render("false")
	if status.CavemanInstalled {
		installed = ui.Success.Render("true")
	}
	rows := [][]string{
		{ui.Label.Render("Strategy"), styleSetting(status.Strategy)},
		{ui.Label.Render("Caveman level"), styleSetting(status.CavemanLevel)},
		{ui.Label.Render("Caveman installed"), installed},
	}
	return ui.Table([]string{"SETTING", "VALUE"}, rows)
}

// styleSetting renders a setting value as a data token (or a muted "(unset)" when
// blank), keeping the orUnset wording.
func styleSetting(value string) string {
	if value == "" {
		return ui.Muted.Render(orUnset(value))
	}
	return ui.Value.Render(orUnset(value))
}

// orUnset renders "(unset)" for an empty setting so blank values read clearly.
func orUnset(value string) string {
	if value == "" {
		return "(unset)"
	}
	return value
}

// GetStatus reports the configured strategy + Caveman level and whether the
// Caveman skill is installed. Live Headroom metrics need the running proxy.
func GetStatus(projectRoot string) (Status, error) {
	projectConfig, err := config.Load(projectRoot)
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
