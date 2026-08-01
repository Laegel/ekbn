package tui

import "github.com/charmbracelet/lipgloss"

var (
	robotStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("212")).
			Align(lipgloss.Center).
			Padding(1, 0)

	buttonStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("235")).
			Background(lipgloss.Color("212")).
			Padding(0, 3).
			Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Padding(1, 0, 0, 0)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	okStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("120")).
		Bold(true)

	viewportBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("240")).
				Padding(0, 1)
)
