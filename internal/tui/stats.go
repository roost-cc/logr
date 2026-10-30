package tui

import (
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/queryclient"
)

// statsData holds all aggregation results for the stats panel.
type statsData struct {
	total       int64
	bySource    []db.AggRow
	byLevel     []db.AggRow
	sourceLevel []db.AggRow
}

type statsMsg struct{ data statsData }

func (m Model) statsCmd() tea.Cmd {
	where, args := m.filterWhere, m.filterArgs
	return func() tea.Msg {
		res, err := m.qc.Stats(m.ctx, where, args)
		if err != nil {
			return errMsg{err}
		}
		return statsMsg{fromStatsResult(res)}
	}
}

func fromStatsResult(r *queryclient.StatsResult) statsData {
	return statsData{
		total:       r.Total,
		bySource:    r.BySource,
		byLevel:     r.ByLevel,
		sourceLevel: r.SourceLevel,
	}
}

func (m Model) viewStats() string {
	if m.stats == nil {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			styleHelp.Render("loading stats…"))
	}

	var b strings.Builder

	// Title
	filterDesc := "(no filter)"
	if m.filterExpr != "" {
		filterDesc = m.filterExpr
	}
	b.WriteString(styleHeader.Render("logr stats") +
		"  " + styleHelp.Render("filter: ") + styleBlue(filterDesc) +
		"  " + styleHelp.Render(fmt.Sprintf("total: %s", commaN(m.stats.total))) +
		"\n" + styleSeparator.Render(strings.Repeat("─", m.width)) + "\n\n")

	// Two-column layout: by source | by level
	halfW := m.width/2 - 2
	srcBlock := renderAggTable("BY SOURCE", m.stats.bySource, m.stats.total, halfW, colorSapphire)
	lvlBlock := renderAggTable("BY LEVEL", m.stats.byLevel, m.stats.total, halfW, colorMauve)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, srcBlock, "  ", lvlBlock))
	b.WriteString("\n\n")

	// Full-width: source × level
	b.WriteString(styleSeparator.Render(strings.Repeat("─", m.width)) + "\n")
	b.WriteString(renderCrossTable("SOURCE × LEVEL", m.stats.sourceLevel, m.stats.total, m.width))

	b.WriteString("\n\n" + styleHelp.Render("[Esc] / [s]  back to logs    [r]  refresh stats"))

	return b.String()
}

func renderAggTable(title string, rows []db.AggRow, total int64, width int, barColor color.Color) string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(title) + "\n")

	if len(rows) == 0 {
		b.WriteString(styleHelp.Render("  (no data)"))
		return b.String()
	}

	max := rows[0].Count
	barWidth := width - 28
	if barWidth < 4 {
		barWidth = 4
	}

	barStyle := lipgloss.NewStyle().Foreground(barColor)

	for _, r := range rows {
		label := truncate(r.Key, 14)
		pct := float64(0)
		if total > 0 {
			pct = float64(r.Count) / float64(total) * 100
		}
		filled := int(float64(r.Count) / float64(max) * float64(barWidth))
		if filled < 1 && r.Count > 0 {
			filled = 1
		}
		bar := barStyle.Render(strings.Repeat("█", filled)) +
			styleHelp.Render(strings.Repeat("░", barWidth-filled))
		b.WriteString(fmt.Sprintf("  %-14s %s %5d %4.0f%%\n",
			label, bar, r.Count, pct))
	}
	return b.String()
}

func renderCrossTable(title string, rows []db.AggRow, total int64, width int) string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(title) + "\n")

	if len(rows) == 0 {
		b.WriteString(styleHelp.Render("  (no data)"))
		return b.String()
	}

	max := rows[0].Count
	barWidth := width - 36
	if barWidth < 4 {
		barWidth = 4
	}

	for _, r := range rows {
		label := fmt.Sprintf("%-12s / %-7s", truncate(r.Key, 12), truncate(r.Key2, 7))
		filled := int(float64(r.Count) / float64(max) * float64(barWidth))
		if filled < 1 && r.Count > 0 {
			filled = 1
		}

		lvlS := levelStyle(r.Key2)
		bar := lvlS.Render(strings.Repeat("█", filled)) +
			styleHelp.Render(strings.Repeat("░", barWidth-filled))
		pct := float64(0)
		if total > 0 {
			pct = float64(r.Count) / float64(total) * 100
		}
		b.WriteString(fmt.Sprintf("  %s  %s  %5d  %4.0f%%\n",
			label, bar, r.Count, pct))
	}
	return b.String()
}

func commaN(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}
