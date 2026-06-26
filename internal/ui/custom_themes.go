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

func ThemeFactory(p Palette) func() *huh.Theme {
	return func() *huh.Theme {
		return ThemeFromPalette(p)
	}
}

func ThemeFromPalette(p Palette) *huh.Theme {
	t := huh.ThemeBase()

	t.Focused.Base =
		t.Focused.Base.BorderForeground(p.Primary)

	t.Focused.Card = t.Focused.Base

	t.Focused.Title =
		t.Focused.Title.
			Foreground(p.Primary).
			Bold(true)

	t.Focused.NoteTitle =
		t.Focused.NoteTitle.
			Foreground(p.Secondary).
			Bold(true)

	t.Focused.Directory =
		t.Focused.Directory.
			Foreground(p.Primary)

	t.Focused.Description =
		t.Focused.Description.
			Foreground(p.Muted)

	t.Focused.ErrorIndicator =
		t.Focused.ErrorIndicator.
			Foreground(p.Error)

	t.Focused.ErrorMessage =
		t.Focused.ErrorMessage.
			Foreground(p.Error)

	t.Focused.SelectSelector =
		t.Focused.SelectSelector.
			Foreground(p.Secondary)

	t.Focused.NextIndicator =
		t.Focused.NextIndicator.
			Foreground(p.Secondary)

	t.Focused.PrevIndicator =
		t.Focused.PrevIndicator.
			Foreground(p.Secondary)

	t.Focused.MultiSelectSelector =
		t.Focused.MultiSelectSelector.
			Foreground(p.Secondary)

	t.Focused.Option =
		t.Focused.Option.
			Foreground(p.Text)

	t.Focused.SelectedOption =
		t.Focused.SelectedOption.
			Foreground(p.Success)

	t.Focused.SelectedPrefix =
		lipgloss.NewStyle().
			Foreground(p.Success).
			SetString("◆ ")

	t.Focused.UnselectedPrefix =
		lipgloss.NewStyle().
			Foreground(p.Muted).
			SetString("◇ ")

	t.Focused.FocusedButton =
		t.Focused.FocusedButton.
			Foreground(p.ButtonFg).
			Background(p.Secondary).
			Bold(true)

	t.Focused.Next = t.Focused.FocusedButton

	t.Focused.TextInput.Cursor =
		t.Focused.TextInput.Cursor.
			Foreground(p.Primary)

	t.Focused.TextInput.Prompt =
		t.Focused.TextInput.Prompt.
			Foreground(p.Secondary)

	t.Blurred = t.Focused
	t.Blurred.Base =
		t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())

	t.Blurred.Card = t.Blurred.Base

	t.Group.Title = t.Focused.Title
	t.Group.Description = t.Focused.Description

	return t
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
	Primary:   lipgloss.Color("#00FFFF"),
	Secondary: lipgloss.Color("#FF9933"),
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
	Secondary: lipgloss.Color("#F7768E"),
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
	Primary:   lipgloss.Color("#88C0D0"),
	Secondary: lipgloss.Color("#81A1C1"),
	Success:   lipgloss.Color("#A3BE8C"),
	Error:     lipgloss.Color("#BF616A"),
	Text:      lipgloss.Color("#ECEFF4"),
	Muted:     lipgloss.Color("#4C566A"),
	ButtonFg:  lipgloss.Color("#2E3440"),
}

var Gruvbox = Palette{
	Primary:   lipgloss.Color("#FE8019"),
	Secondary: lipgloss.Color("#FABD2F"),
	Success:   lipgloss.Color("#B8BB26"),
	Error:     lipgloss.Color("#FB4934"),
	Text:      lipgloss.Color("#EBDBB2"),
	Muted:     lipgloss.Color("#928374"),
	ButtonFg:  lipgloss.Color("#282828"),
}

var OneDark = Palette{
	Primary:   lipgloss.Color("#61AFEF"),
	Secondary: lipgloss.Color("#C678DD"),
	Success:   lipgloss.Color("#98C379"),
	Error:     lipgloss.Color("#E06C75"),
	Text:      lipgloss.Color("#ABB2BF"),
	Muted:     lipgloss.Color("#5C6370"),
	ButtonFg:  lipgloss.Color("#282C34"),
}
