package ui

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/paths"
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
	huh       func() *huh.Theme
	accent    lipgloss.Color
	secondary lipgloss.Color
}

// themes is the registry of selectable named themes (wrapping huh's built-ins
// with an accent for headings).
var themes = map[string]themeDef{
	// The default CLI look is bright pink + bright blue (BluePink), replacing huh's
	// purple ThemeCharm which read poorly. The heading accent is the bright pink.
	"default":    {ThemeFactory(BluePink), lipgloss.Color("#FF66FF"), lipgloss.Color("#33FFFF")},
	"blue-pink":  {ThemeFactory(BluePink), lipgloss.Color("#FF2BD6"), lipgloss.Color("#33FFFF")},
	"dracula":    {huh.ThemeDracula, lipgloss.Color("#FF2BD6"), lipgloss.Color("#50FA7B")},    // pink + green
	"catppuccin": {huh.ThemeCatppuccin, lipgloss.Color("#CBA6F7"), lipgloss.Color("#FE53BB")}, // mauve + peach
	"base16":     {huh.ThemeBase16, lipgloss.Color("45"), lipgloss.Color("212")},              // cyan + pink
	"monochrome": {huh.ThemeBase, lipgloss.Color("252"), lipgloss.Color("238")},               // light grey + dark grey
	// Custom themes.
	"orange": {
		huh:       ThemeFactory(OrangeBlue),
		accent:    lipgloss.Color("#F97316"), // orange
		secondary: lipgloss.Color("#33FFFF"), // bright blue
	},
	"synthwave": {
		huh:       ThemeFactory(Synthwave),
		accent:    lipgloss.Color("#00D9FF"), // cyan
		secondary: lipgloss.Color("#FF2BD6"), // magenta
	},
	"cyberpunk": {
		huh:       ThemeFactory(RetroCyberpunk),
		accent:    lipgloss.Color("#08F7FE"), // cyan
		secondary: lipgloss.Color("#FE53BB"), // pink
	},
	"tokyo-night": {
		huh:       ThemeFactory(TokyoNight),
		accent:    lipgloss.Color("#4ABDFF"),  // cyan
		secondary: lipgloss.Color("#FF8F4Dq"), // orange
	},
	"vaporwave": {
		huh:       ThemeFactory(Vaporwave),
		accent:    lipgloss.Color("#5EEAD4"), // teal
		secondary: lipgloss.Color("#FF71CE"), // pink
	},
	"tron": {
		huh:       ThemeFactory(Tron),
		accent:    lipgloss.Color("#00FFFF"),
		secondary: lipgloss.Color("#FF00FF"),
	},
	"nord": {
		huh:       ThemeFactory(Nord),
		accent:    lipgloss.Color("#72D0EC"), // frost blue
		secondary: lipgloss.Color("#E86137"), // aurora orange
	},
	"gruvbox": {
		huh:       ThemeFactory(Gruvbox),
		accent:    lipgloss.Color("#FE8019"), // orange
		secondary: lipgloss.Color("#0000FF"), // blue
	},
	"onedark": {
		huh:       ThemeFactory(OneDark),
		accent:    lipgloss.Color("#61AFEF"), // blue
		secondary: lipgloss.Color("#E22330"), // red
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
	// Mouse is the `ai ui` mouse-capture setting (clickable tab bars). A pointer
	// distinguishes "never set" (absent → MouseDefault, capture ON) from an
	// explicit false (capture OFF, so native text selection needs no modifier).
	Mouse *bool `yaml:"mouse,omitempty"`
}

// MouseDefault is the mouse-capture setting applied when none is persisted:
// enabled, so the `ai ui` tab bars are clickable out of the box.
const MouseDefault = true

func uiPrefsPath() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ui.yaml"), nil
}

// loadPrefs best-effort reads the persisted UI preferences, returning the zero
// value when the file is absent or unreadable (callers apply their defaults).
func loadPrefs() uiPrefs {
	path, err := uiPrefsPath()
	if err != nil {
		return uiPrefs{}
	}
	var prefs uiPrefs
	if err := conffile.Read(path, &prefs); err != nil {
		return uiPrefs{}
	}
	return prefs
}

// savePrefs applies mutate to the current preferences and writes them back, so
// each setting's Save* preserves the others (theme vs mouse).
func savePrefs(mutate func(prefs *uiPrefs)) error {
	path, err := uiPrefsPath()
	if err != nil {
		return err
	}
	prefs := loadPrefs()
	mutate(&prefs)
	return conffile.WriteAtomic(path, prefs)
}

// LoadThemeName returns the persisted theme name, falling back to DefaultTheme
// when nothing is saved, the file is absent/unreadable, or the saved name is no
// longer a known theme.
func LoadThemeName() string {
	prefs := loadPrefs()
	if !IsTheme(prefs.Theme) {
		return DefaultTheme
	}
	return prefs.Theme
}

// SaveThemeName persists the chosen theme name (the caller validates it first via
// IsTheme / Apply), preserving the other saved preferences.
func SaveThemeName(name string) error {
	return savePrefs(func(prefs *uiPrefs) { prefs.Theme = name })
}

// LoadMouseEnabled returns the persisted `ai ui` mouse-capture setting (clickable
// tab bars), falling back to MouseDefault when nothing is saved or the file is
// absent/unreadable.
func LoadMouseEnabled() bool {
	prefs := loadPrefs()
	if prefs.Mouse == nil {
		return MouseDefault
	}
	return *prefs.Mouse
}

// SaveMouseEnabled persists the `ai ui` mouse-capture setting, preserving the
// other saved preferences (the theme).
func SaveMouseEnabled(enabled bool) error {
	return savePrefs(func(prefs *uiPrefs) { prefs.Mouse = &enabled })
}
