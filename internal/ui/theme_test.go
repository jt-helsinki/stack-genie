package ui

import "testing"

func TestApplyKnownThemeSwitchesActive(test *testing.T) {
	test.Cleanup(func() { _ = Apply(DefaultTheme) }) // don't leak into other tests

	if err := Apply("dracula"); err != nil {
		test.Fatalf("Apply(dracula): %v", err)
	}
	if CurrentTheme() != "dracula" {
		test.Fatalf("CurrentTheme = %q, want dracula", CurrentTheme())
	}
	if HuhTheme() == nil {
		test.Fatal("HuhTheme returned nil after Apply")
	}
}

func TestApplyUnknownThemeErrorsAndKeepsActive(test *testing.T) {
	test.Cleanup(func() { _ = Apply(DefaultTheme) })

	_ = Apply(DefaultTheme)
	if err := Apply("nope"); err == nil {
		test.Fatal("Apply(unknown) must return an error")
	}
	if CurrentTheme() != DefaultTheme {
		test.Fatalf("Apply(unknown) changed the active theme to %q", CurrentTheme())
	}
}

// Every registered theme — built-in or custom — must apply cleanly and yield a
// non-nil huh form theme and an accent (so a broken custom palette is caught).
func TestEveryRegisteredThemeApplies(test *testing.T) {
	test.Cleanup(func() { _ = Apply(DefaultTheme) })

	for _, name := range ThemeNames() {
		if err := Apply(name); err != nil {
			test.Errorf("Apply(%q): %v", name, err)
			continue
		}
		if HuhTheme() == nil {
			test.Errorf("theme %q: HuhTheme() is nil", name)
		}
		if Accent() == "" {
			test.Errorf("theme %q: Accent() is empty", name)
		}
		_ = TableStyles() // must not panic for any theme
	}
}

func TestThemeRegistryHasDefault(test *testing.T) {
	if !IsTheme(DefaultTheme) {
		test.Fatalf("%q must be a registered theme", DefaultTheme)
	}
	if len(ThemeNames()) == 0 {
		test.Fatal("no themes registered")
	}
	// ThemeNames must be sorted (stable menu/help ordering).
	names := ThemeNames()
	for index := 1; index < len(names); index++ {
		if names[index-1] > names[index] {
			test.Fatalf("ThemeNames not sorted: %v", names)
		}
	}
}

func TestLoadSaveThemeRoundTrip(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	// No file yet → the default.
	if got := LoadThemeName(); got != DefaultTheme {
		test.Fatalf("LoadThemeName (absent) = %q, want %q", got, DefaultTheme)
	}
	if err := SaveThemeName("catppuccin"); err != nil {
		test.Fatalf("SaveThemeName: %v", err)
	}
	if got := LoadThemeName(); got != "catppuccin" {
		test.Fatalf("LoadThemeName after save = %q, want catppuccin", got)
	}
}

func TestLoadThemeNameFallsBackOnUnknownSaved(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	// A saved-but-unknown theme (e.g. a removed theme) must not break the CLI.
	if err := SaveThemeName("bogus"); err != nil {
		test.Fatalf("SaveThemeName: %v", err)
	}
	if got := LoadThemeName(); got != DefaultTheme {
		test.Fatalf("unknown saved theme should fall back to %q, got %q", DefaultTheme, got)
	}
}
