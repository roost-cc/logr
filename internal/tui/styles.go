// Catppuccin Mocha palette — https://catppuccin.com/palette
package tui

import "charm.land/lipgloss/v2"

var (
	colorBase     = lipgloss.Color("#1e1e2e") // base
	colorMantle   = lipgloss.Color("#181825") // mantle
	colorCrust    = lipgloss.Color("#11111b") // crust
	colorSurface0 = lipgloss.Color("#313244") // surface0
	colorSurface1 = lipgloss.Color("#45475a") // surface1
	colorOverlay0 = lipgloss.Color("#6c7086") // overlay0
	colorOverlay1 = lipgloss.Color("#7f849c") // overlay1
	colorSubtext0 = lipgloss.Color("#a6adc8") // subtext0
	colorText     = lipgloss.Color("#cdd6f4") // text
	colorLavender = lipgloss.Color("#b4befe") // lavender
	colorBlue     = lipgloss.Color("#89b4fa") // blue
	colorSapphire = lipgloss.Color("#74c7ec") // sapphire
	colorSky      = lipgloss.Color("#89dceb") // sky
	colorTeal     = lipgloss.Color("#94e2d5") // teal
	colorGreen    = lipgloss.Color("#a6e3a1") // green
	colorYellow   = lipgloss.Color("#f9e2af") // yellow
	colorPeach    = lipgloss.Color("#fab387") // peach
	colorMaroon   = lipgloss.Color("#eba0ac") // maroon
	colorRed      = lipgloss.Color("#f38ba8") // red
	colorMauve    = lipgloss.Color("#cba6f7") // mauve
	colorPink     = lipgloss.Color("#f5c2e7") // pink
	colorFlamingo = lipgloss.Color("#f2cdcd") // flamingo

	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorLavender)

	styleSelected = lipgloss.NewStyle().
			Background(colorSurface1).
			Foreground(colorText)

	styleMark = lipgloss.NewStyle().
			Foreground(colorYellow).
			Bold(true)

	styleLevel = map[string]lipgloss.Style{
		"TRACE": lipgloss.NewStyle().Foreground(colorOverlay0),
		"DEBUG": lipgloss.NewStyle().Foreground(colorOverlay1),
		"INFO":  lipgloss.NewStyle().Foreground(colorGreen),
		"WARN":  lipgloss.NewStyle().Foreground(colorYellow),
		"ERROR": lipgloss.NewStyle().Foreground(colorRed).Bold(true),
		"FATAL": lipgloss.NewStyle().Foreground(colorRed).Bold(true).Reverse(true),
	}

	styleSource = lipgloss.NewStyle().Foreground(colorSapphire)
	styleTime   = lipgloss.NewStyle().Foreground(colorOverlay1)
	styleMsg    = lipgloss.NewStyle().Foreground(colorText)

	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSurface1).
			Background(colorBase)

	styleFilterBar = lipgloss.NewStyle().
			Foreground(colorBlue).
			Bold(true)

	styleStatusBar = lipgloss.NewStyle().
			Background(colorSurface0).
			Foreground(colorSubtext0).
			PaddingLeft(1).
			PaddingRight(1)

	styleTailOn = lipgloss.NewStyle().
			Foreground(colorGreen).
			Bold(true)

	styleTailOff = lipgloss.NewStyle().
			Foreground(colorOverlay0)

	styleKey = lipgloss.NewStyle().
			Foreground(colorMauve).
			Bold(true)

	styleHelp = lipgloss.NewStyle().
			Foreground(colorOverlay0)

	styleSeparator = lipgloss.NewStyle().
			Foreground(colorSurface1)
)

func levelStyle(level string) lipgloss.Style {
	if s, ok := styleLevel[level]; ok {
		return s
	}
	return styleMsg
}
