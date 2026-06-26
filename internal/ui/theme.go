package ui

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// This file makes the platform's look-and-feel selectable. A theme bundles a huh
// form-theme constructor with the accent colour used for headings, so picking a
// theme restyles BOTH the interactive prompts/forms and the headings together.
// The semantic status colours (success/warn/failure/muted) stay fixed across
// themes — green/amber/red/grey should read the same everywhere. The choice is
// persisted per host in ~/.ai-platform/config/ui.yaml and applied once at startup
// (see Execute), so every command's prompts and stepper match.

// DefaultTheme is applied when none is configured (or a saved name is unknown).
const DefaultTheme = "default"

type themeDef struct {
	huh    func() *huh.Theme
	accent lipgloss.Color
}

// themes is the registry of selectable named themes (wrapping huh's built-ins
// with an accent for headings).
var themes = map[string]themeDef{
	// The default CLI look is bright pink + bright blue (BluePink), replacing huh's
	// purple ThemeCharm which read poorly. The heading accent is the bright pink.
	"default":    {ThemeFactory(BluePink), lipgloss.Color("#FF66FF")},
	"blue-pink":  {ThemeFactory(BluePink), lipgloss.Color("#FF66FF")},
	"dracula":    {huh.ThemeDracula, lipgloss.Color("212")},    // pink
	"catppuccin": {huh.ThemeCatppuccin, lipgloss.Color("183")}, // mauve
	"base16":     {huh.ThemeBase16, lipgloss.Color("45")},      // cyan
	"monochrome": {huh.ThemeBase, lipgloss.Color("252")},       // near-white, minimal colour
	// Custom themes.
	"orange": {
		huh:    ThemeFactory(OrangeBlue),
		accent: lipgloss.Color("#F97316"),
	},
	"orange-blue": {
		huh:    ThemeFactory(OrangeBlue),
		accent: lipgloss.Color("#F97316"),
	},
	"synthwave": {
		huh:    ThemeFactory(Synthwave),
		accent: lipgloss.Color("#FF2BD6"),
	},
	"cyberpunk": {
		huh:    ThemeFactory(RetroCyberpunk),
		accent: lipgloss.Color("#FE53BB"),
	},
	"tokyo-night": {
		huh:    ThemeFactory(TokyoNight),
		accent: lipgloss.Color("#7DCFFF"),
	},
	"vaporwave": {
		huh:    ThemeFactory(Vaporwave),
		accent: lipgloss.Color("#FF71CE"),
	},
	"tron": {
		huh:    ThemeFactory(Tron),
		accent: lipgloss.Color("#00FFFF"),
	},
	"nord": {
		huh:    ThemeFactory(Nord),
		accent: lipgloss.Color("#88C0D0"),
	},
	"gruvbox": {
		huh:    ThemeFactory(Gruvbox),
		accent: lipgloss.Color("#FE8019"),
	},
	"onedark": {
		huh:    ThemeFactory(OneDark),
		accent: lipgloss.Color("#61AFEF"),
	},
}

// active / activeName track the currently applied theme (the default until Apply
// changes it). They are mutated once at startup before any rendering, so no
// synchronisation is needed.
var (
	active     = themes[DefaultTheme]
	activeName = DefaultTheme
)

// ThemeNames returns the selectable theme names, sorted.
func ThemeNames() []string {
	names := make([]string, 0, len(themes))
	for name := range themes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsTheme reports whether name is a known theme.
func IsTheme(name string) bool {
	_, ok := themes[name]
	return ok
}

// CurrentTheme returns the active theme's name.
func CurrentTheme() string { return activeName }

// Apply switches the active theme: subsequent HuhTheme() calls use its form theme
// and Heading is restyled to its accent. An unknown name returns an error and
// leaves the active theme unchanged.
func Apply(name string) error {
	def, ok := themes[name]
	if !ok {
		return fmt.Errorf("unknown theme %q (one of %v)", name, ThemeNames())
	}
	active = def
	activeName = name
	Heading = lipgloss.NewStyle().Bold(true).Foreground(def.accent)
	return nil
}

// uiPrefs is the persisted UI preferences file (~/.ai-platform/config/ui.yaml).
type uiPrefs struct {
	Theme string `yaml:"theme,omitempty"`
}

func uiPrefsPath() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ui.yaml"), nil
}

// LoadThemeName returns the persisted theme name, falling back to DefaultTheme
// when nothing is saved, the file is absent/unreadable, or the saved name is no
// longer a known theme.
func LoadThemeName() string {
	path, err := uiPrefsPath()
	if err != nil {
		return DefaultTheme
	}
	var prefs uiPrefs
	if err := conffile.Read(path, &prefs); err != nil {
		return DefaultTheme
	}
	if !IsTheme(prefs.Theme) {
		return DefaultTheme
	}
	return prefs.Theme
}

// SaveThemeName persists the chosen theme name (the caller validates it first via
// IsTheme / Apply).
func SaveThemeName(name string) error {
	path, err := uiPrefsPath()
	if err != nil {
		return err
	}
	return conffile.WriteAtomic(path, uiPrefs{Theme: name})
}
