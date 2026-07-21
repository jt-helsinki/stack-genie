package ui

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// minRGBDistance is the minimum Euclidean RGB distance required between a theme's
// accent (primary) and secondary colours when BOTH are hex. The TUI uses the pair
// as a foreground/background combination for tabs, so they must be visibly
// distinct — same-hue near-duplicates (e.g. the old #F97316/#FF9933) must fail.
const minRGBDistance = 100.0

// parseHex converts a "#RRGGBB" lipgloss colour to its RGB components. It reports
// ok=false for any value that is not a 7-character hex string (e.g. ANSI-256
// codes used by the built-in themes).
func parseHex(value string) (red, green, blue int, ok bool) {
	if len(value) != 7 || !strings.HasPrefix(value, "#") {
		return 0, 0, 0, false
	}
	redValue, errRed := strconv.ParseInt(value[1:3], 16, 0)
	greenValue, errGreen := strconv.ParseInt(value[3:5], 16, 0)
	blueValue, errBlue := strconv.ParseInt(value[5:7], 16, 0)
	if errRed != nil || errGreen != nil || errBlue != nil {
		return 0, 0, 0, false
	}
	return int(redValue), int(greenValue), int(blueValue), true
}

func TestThemeAccentSecondaryContrast(t *testing.T) {
	for name, def := range themes {
		accent := string(def.accent)
		secondary := string(def.secondary)

		if accent == secondary {
			t.Errorf("theme %q: accent and secondary are identical (%q); the tab fg/bg pair must differ", name, accent)
			continue
		}

		accentRed, accentGreen, accentBlue, accentOk := parseHex(accent)
		secondaryRed, secondaryGreen, secondaryBlue, secondaryOk := parseHex(secondary)
		if !accentOk || !secondaryOk {
			// ANSI-256 (non-hex) built-in codes — the != check above covers them.
			continue
		}

		deltaRed := float64(accentRed - secondaryRed)
		deltaGreen := float64(accentGreen - secondaryGreen)
		deltaBlue := float64(accentBlue - secondaryBlue)
		distance := math.Sqrt(deltaRed*deltaRed + deltaGreen*deltaGreen + deltaBlue*deltaBlue)
		if distance < minRGBDistance {
			t.Errorf("theme %q: accent %s and secondary %s are too close (RGB distance %.1f < %.0f); pick a contrasting hue",
				name, accent, secondary, distance, minRGBDistance)
		}
	}
}
