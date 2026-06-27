package ui

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

type Palette struct {
	Primary   lipgloss.TerminalColor
	Secondary lipgloss.TerminalColor
	Success   lipgloss.TerminalColor
	Error     lipgloss.TerminalColor
	Text      lipgloss.TerminalColor
	Muted     lipgloss.TerminalColor
	ButtonFg  lipgloss.TerminalColor
}

func ThemeFactory(palette Palette) func() *huh.Theme {
	return func() *huh.Theme {
		return ThemeFromPalette(palette)
	}
}

func ThemeFromPalette(palette Palette) *huh.Theme {
	theme := huh.ThemeBase()

	theme.Focused.Base =
		theme.Focused.Base.BorderForeground(palette.Primary)

	theme.Focused.Card = theme.Focused.Base

	theme.Focused.Title =
		theme.Focused.Title.
			Foreground(palette.Primary).
			Bold(true)

	theme.Focused.NoteTitle =
		theme.Focused.NoteTitle.
			Foreground(palette.Secondary).
			Bold(true)

	theme.Focused.Directory =
		theme.Focused.Directory.
			Foreground(palette.Primary)

	theme.Focused.Description =
		theme.Focused.Description.
			Foreground(palette.Muted)

	theme.Focused.ErrorIndicator =
		theme.Focused.ErrorIndicator.
			Foreground(palette.Error)

	theme.Focused.ErrorMessage =
		theme.Focused.ErrorMessage.
			Foreground(palette.Error)

	theme.Focused.SelectSelector =
		theme.Focused.SelectSelector.
			Foreground(palette.Secondary)

	theme.Focused.NextIndicator =
		theme.Focused.NextIndicator.
			Foreground(palette.Secondary)

	theme.Focused.PrevIndicator =
		theme.Focused.PrevIndicator.
			Foreground(palette.Secondary)

	theme.Focused.MultiSelectSelector =
		theme.Focused.MultiSelectSelector.
			Foreground(palette.Secondary)

	theme.Focused.Option =
		theme.Focused.Option.
			Foreground(palette.Text)

	theme.Focused.SelectedOption =
		theme.Focused.SelectedOption.
			Foreground(palette.Success)

	theme.Focused.SelectedPrefix =
		lipgloss.NewStyle().
			Foreground(palette.Success).
			SetString("◆ ")

	theme.Focused.UnselectedPrefix =
		lipgloss.NewStyle().
			Foreground(palette.Muted).
			SetString("◇ ")

	theme.Focused.FocusedButton =
		theme.Focused.FocusedButton.
			Foreground(palette.ButtonFg).
			Background(palette.Secondary).
			Bold(true)

	theme.Focused.Next = theme.Focused.FocusedButton

	theme.Focused.TextInput.Cursor =
		theme.Focused.TextInput.Cursor.
			Foreground(palette.Primary)

	theme.Focused.TextInput.Prompt =
		theme.Focused.TextInput.Prompt.
			Foreground(palette.Secondary)

	theme.Blurred = theme.Focused
	theme.Blurred.Base =
		theme.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())

	theme.Blurred.Card = theme.Blurred.Base

	theme.Group.Title = theme.Focused.Title
	theme.Group.Description = theme.Focused.Description

	return theme
}

// BluePink is the default CLI palette (per the maintainer's spec): bright pink +
// bright blue, replacing huh's purple ThemeCharm which read poorly. Errors are
// bright red; the bright-orange warning colour is the fixed colorWarn in ui.go.
var BluePink = Palette{
	Primary:   lipgloss.Color("#FF66FF"), // bright pink  (R255 G102 B255)
	Secondary: lipgloss.Color("#33FFFF"), // bright blue  (R51  G255 B255)
	Success:   lipgloss.Color("#33FF99"), // readable green for the selected option
	Error:     lipgloss.Color("#FF3333"), // bright red   (R255 G51  B51)
	Text:      lipgloss.Color("#E5E7EB"),
	Muted:     lipgloss.Color("#9CA3AF"),
	ButtonFg:  lipgloss.Color("#000000"),
}

var OrangeBlue = Palette{
	Primary:   lipgloss.Color("#F97316"), // orange
	Secondary: lipgloss.Color("#33FFFF"), // bright blue
	Success:   lipgloss.Color("#00FF00"),
	Error:     lipgloss.Color("#FF0000"),
	Text:      lipgloss.Color("#CCFFFF"),
	Muted:     lipgloss.Color("#6B7280"),
	ButtonFg:  lipgloss.Color("#FFFFFF"),
}

// OrangeBlueVivid is OrangeBlue with a brighter, more-saturated azure blue
// secondary (less cyan), so the orange-blue theme reads distinct from orange.
var OrangeBlueVivid = Palette{
	Primary:   lipgloss.Color("#F97316"), // orange
	Secondary: lipgloss.Color("#2D9CFF"), // bright azure blue
	Success:   lipgloss.Color("#00FF00"),
	Error:     lipgloss.Color("#FF0000"),
	Text:      lipgloss.Color("#CCFFFF"),
	Muted:     lipgloss.Color("#6B7280"),
	ButtonFg:  lipgloss.Color("#FFFFFF"),
}

var RetroCyberpunk = Palette{
	Primary:   lipgloss.Color("#08F7FE"),
	Secondary: lipgloss.Color("#FE53BB"),
	Success:   lipgloss.Color("#09FBD3"),
	Error:     lipgloss.Color("#FF5555"),
	Text:      lipgloss.Color("#E2E8F0"),
	Muted:     lipgloss.Color("#64748B"),
	ButtonFg:  lipgloss.Color("#000000"),
}

var Synthwave = Palette{
	Primary:   lipgloss.Color("#00D9FF"),
	Secondary: lipgloss.Color("#FF2BD6"),
	Success:   lipgloss.Color("#00FF88"),
	Error:     lipgloss.Color("#FF4D6D"),
	Text:      lipgloss.Color("#E5E7EB"),
	Muted:     lipgloss.Color("#8B8BA7"),
	ButtonFg:  lipgloss.Color("#FFFFFF"),
}

var TokyoNight = Palette{
	Primary:   lipgloss.Color("#7DCFFF"),
	Secondary: lipgloss.Color("#FF9E64"), // orange
	Success:   lipgloss.Color("#9ECE6A"),
	Error:     lipgloss.Color("#FF757F"),
	Text:      lipgloss.Color("#C0CAF5"),
	Muted:     lipgloss.Color("#565F89"),
	ButtonFg:  lipgloss.Color("#1A1B26"),
}

var Vaporwave = Palette{
	Primary:   lipgloss.Color("#5EEAD4"),
	Secondary: lipgloss.Color("#FF71CE"),
	Success:   lipgloss.Color("#7CFFCB"),
	Error:     lipgloss.Color("#FF6B81"),
	Text:      lipgloss.Color("#F3F4F6"),
	Muted:     lipgloss.Color("#A78BFA"),
	ButtonFg:  lipgloss.Color("#111827"),
}

var Tron = Palette{
	Primary:   lipgloss.Color("#00FFFF"),
	Secondary: lipgloss.Color("#FF00FF"),
	Success:   lipgloss.Color("#00FFAA"),
	Error:     lipgloss.Color("#FF5555"),
	Text:      lipgloss.Color("#E5E7EB"),
	Muted:     lipgloss.Color("#94A3B8"),
	ButtonFg:  lipgloss.Color("#000000"),
}

var Nord = Palette{
	Primary:   lipgloss.Color("#88C0D0"), // frost blue
	Secondary: lipgloss.Color("#D08770"), // aurora orange
	Success:   lipgloss.Color("#A3BE8C"),
	Error:     lipgloss.Color("#BF616A"),
	Text:      lipgloss.Color("#ECEFF4"),
	Muted:     lipgloss.Color("#4C566A"),
	ButtonFg:  lipgloss.Color("#2E3440"),
}

var Gruvbox = Palette{
	Primary:   lipgloss.Color("#FE8019"), // orange
	Secondary: lipgloss.Color("#458588"), // blue
	Success:   lipgloss.Color("#B8BB26"),
	Error:     lipgloss.Color("#FB4934"),
	Text:      lipgloss.Color("#EBDBB2"),
	Muted:     lipgloss.Color("#928374"),
	ButtonFg:  lipgloss.Color("#282828"),
}

var OneDark = Palette{
	Primary:   lipgloss.Color("#61AFEF"), // blue
	Secondary: lipgloss.Color("#E06C75"), // red
	Success:   lipgloss.Color("#98C379"),
	Error:     lipgloss.Color("#E06C75"),
	Text:      lipgloss.Color("#ABB2BF"),
	Muted:     lipgloss.Color("#5C6370"),
	ButtonFg:  lipgloss.Color("#282C34"),
}
