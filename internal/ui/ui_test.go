package ui

import (
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

func TestEnabledOffForMachineModes(test *testing.T) {
	// --json and --plain must always disable the TUI (protecting the stdout
	// JSON contract and honoring the plain-output opt-out), regardless of TTY.
	if Enabled(&output.Emitter{JSON: true}) {
		test.Error("TUI must be disabled under --json")
	}
	if Enabled(&output.Emitter{Plain: true}) {
		test.Error("TUI must be disabled under --plain")
	}
}

func TestHuhThemeNonNil(test *testing.T) {
	if HuhTheme() == nil {
		test.Fatal("HuhTheme() returned nil")
	}
}
