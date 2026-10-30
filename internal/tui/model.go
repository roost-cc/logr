package tui

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/roost-cc/logr/internal/config"
	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/filter"
	"github.com/roost-cc/logr/internal/queryclient"
)

// ---- Messages ----

type rowsMsg struct{ rows []db.Row }
type tailMsg struct{ rows []db.Row }
type errMsg struct{ err error }
type exportDoneMsg struct{ path string }
type sourcesMsg struct{ sources []string }

// ---- Column definitions ----

type column struct {
	key     string
	label   string
	width   int
	visible bool
}

var defaultColumns = []column{
	{"log_time", "TIME", 14, true},
	{"source", "SOURCE", 12, true},
	{"level", "LEVEL", 7, true},
	{"message", "MESSAGE", 0, true},
}

// ---- Focus / overlay ----

type panel int

const (
	panelLogs panel = iota
	panelFilter
	panelSources
	panelColumns
	panelDetail
	panelCount // sentinel
)

type overlayMode int

const (
	overlayNone overlayMode = iota
	overlayExport
	overlayStats
	overlayHelp
)

const leftColWidth = 26 // total width of left column including its border

// ---- Model ----

type Model struct {
	qc     *queryclient.Client
	ctx    context.Context
	cancel context.CancelFunc
	width  int
	height int

	// Data
	rows   []db.Row
	maxID  int64
	marks  map[int64]bool
	cursor int

	// Filter panel
	filterInput textinput.Model
	filterExpr  string
	filterErr   string
	filterWhere string
	filterArgs  []any

	// Sources panel
	allSources   []string
	sourceCursor int

	// Columns panel
	columns   []column
	colCursor int
	sortCol   string
	sortDesc  bool

	// Focus & overlay
	focus   panel
	overlay overlayMode

	// Detail pane
	detail     viewport.Model
	showDetail bool

	// Stats
	stats *statsData

	// Status
	statusMsg string
	tail      bool
}

func NewModel(qc *queryclient.Client, cfg config.ClientConfig) Model {
	fi := textinput.New()
	fi.Placeholder = `source=storage  |  level=error  |  message CONTAINS "fail"  |  log_time > 1h`
	fi.CharLimit = 512

	cols := append([]column{}, defaultColumns...)
	if len(cfg.Columns) > 0 {
		for i, c := range cols {
			if v, ok := cfg.Columns[c.key]; ok {
				cols[i].visible = v
			}
		}
	}

	m := Model{
		qc:          qc,
		ctx:         nil, // set below
		cancel:      nil,
		marks:       make(map[int64]bool),
		columns:     cols,
		tail:        true,
		filterInput: fi,
		detail:      viewport.New(viewport.WithWidth(80), viewport.WithHeight(10)),
		focus:       panelLogs,
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())

	if cfg.Filter != "" {
		// Best-effort: ignore parse errors here (SetFilter called after)
		if res, err := filter.Parse(cfg.Filter, 1); err == nil {
			m.filterExpr = cfg.Filter
			m.filterWhere = res.SQL
			m.filterArgs = res.Args
			fi.SetValue(cfg.Filter)
		}
	}

	return m
}

// FilterExpr returns the current filter expression (for config persistence).
func (m Model) FilterExpr() string { return m.filterExpr }

// ColumnState returns the current column visibility map (for config persistence).
func (m Model) ColumnState() map[string]bool {
	state := make(map[string]bool, len(m.columns))
	for _, c := range m.columns {
		state[c.key] = c.visible
	}
	return state
}

func (m *Model) SetFilter(expr string) error {
	if expr == "" {
		m.filterExpr = ""
		m.filterWhere = ""
		m.filterArgs = nil
		m.filterInput.SetValue("")
		return nil
	}
	res, err := filter.Parse(expr, 1)
	if err != nil {
		return err
	}
	m.filterExpr = expr
	m.filterWhere = res.SQL
	m.filterArgs = res.Args
	m.filterInput.SetValue(expr)
	m.filterInput.CursorEnd()
	return nil
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchCmd(false),
		m.loadSourcesCmd(),
		m.tailTickCmd(),
	)
}

// ---- Commands ----

func (m Model) buildWhere() (string, []any) {
	return m.filterWhere, m.filterArgs
}

func (m Model) fetchCmd(tailMode bool) tea.Cmd {
	where, args := m.buildWhere()
	return func() tea.Msg {
		opts := db.QueryOpts{
			Where:    where,
			Args:     args,
			Limit:    500,
			SortCol:  m.sortCol,
			SortDesc: m.sortDesc,
		}
		if tailMode && m.maxID > 0 {
			opts.After = m.maxID
		}
		rows, err := m.qc.Query(m.ctx, opts)
		if err != nil {
			return errMsg{err}
		}
		if tailMode {
			return tailMsg{rows}
		}
		return rowsMsg{rows}
	}
}

func (m Model) loadSourcesCmd() tea.Cmd {
	return func() tea.Msg {
		srcs, err := m.qc.Sources(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return sourcesMsg{srcs}
	}
}

func (m Model) tailTickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return t })
}

func (m Model) exportCmd() tea.Cmd {
	rows := m.exportRows()
	return func() tea.Msg {
		path := fmt.Sprintf("logr-export-%s.jsonl", time.Now().Format("20060102-150405"))
		f, err := os.Create(path)
		if err != nil {
			return errMsg{err}
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		for _, r := range rows {
			out := map[string]any{
				"id": r.ID, "ingested_at": r.IngestedAt,
				"source": r.Source, "level": r.Level, "message": r.Message,
				"data": r.Data,
			}
			if r.LogTime != nil {
				out["log_time"] = r.LogTime
			}
			enc.Encode(out) //nolint:errcheck
		}
		return exportDoneMsg{path}
	}
}

func (m Model) exportCSVCmd() tea.Cmd {
	rows := m.exportRows()
	cols := m.visibleColumns()
	return func() tea.Msg {
		path := fmt.Sprintf("logr-export-%s.csv", time.Now().Format("20060102-150405"))
		f, err := os.Create(path)
		if err != nil {
			return errMsg{err}
		}
		defer f.Close()
		w := csv.NewWriter(f)
		headers := make([]string, len(cols))
		for i, c := range cols {
			headers[i] = c.label
		}
		w.Write(headers) //nolint:errcheck
		for _, r := range rows {
			rec := make([]string, len(cols))
			for i, c := range cols {
				rec[i] = rowField(r, c.key)
			}
			w.Write(rec) //nolint:errcheck
		}
		w.Flush()
		return exportDoneMsg{path}
	}
}

func (m Model) exportRows() []db.Row {
	if len(m.marks) > 0 {
		var out []db.Row
		for _, r := range m.rows {
			if m.marks[r.ID] {
				out = append(out, r)
			}
		}
		return out
	}
	return m.rows
}

// ---- Update ----

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.filterInput.SetWidth(max(m.width-leftColWidth-30, 20))
		m.detail.SetWidth(m.width - leftColWidth - 2)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseClickMsg:
		return m.handleClick(msg)

	case tea.MouseWheelMsg:
		return m.handleWheel(msg)

	case time.Time:
		if m.tail {
			return m, tea.Batch(m.fetchCmd(true), m.loadSourcesCmd(), m.tailTickCmd())
		}
		return m, m.tailTickCmd()

	case rowsMsg:
		m.rows = msg.rows
		m.updateMaxID()
		if m.tail && len(m.rows) > 0 {
			m.cursor = 0
		}
		m.updateDetail()
		return m, nil

	case tailMsg:
		if len(msg.rows) > 0 {
			m.rows = append(msg.rows, m.rows...)
			for _, r := range msg.rows {
				if r.ID > m.maxID {
					m.maxID = r.ID
				}
			}
			if len(m.rows) > 2000 {
				m.rows = m.rows[:2000]
			}
			if m.tail {
				m.cursor = 0
			}
			m.updateDetail()
		}
		return m, nil

	case sourcesMsg:
		// Merge: add any sources not already known, preserving disabled state.
		known := make(map[string]bool, len(m.allSources))
		for _, s := range m.allSources {
			known[s] = true
		}
		for _, s := range msg.sources {
			if !known[s] {
				m.allSources = append(m.allSources, s)
			}
		}
		return m, nil

	case errMsg:
		m.statusMsg = "error: " + msg.err.Error()
		return m, nil

	case exportDoneMsg:
		m.statusMsg = "exported → " + msg.path
		m.overlay = overlayNone
		return m, nil

	case statsMsg:
		m.stats = &msg.data
		return m, nil
	}

	// Route textinput updates when filter is focused
	if m.focus == panelFilter {
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Global keys available in any mode (except q when typing in filter)
	switch msg.String() {
	case "ctrl+c":
		m.cancel()
		return m, tea.Quit
	case "q":
		if m.focus != panelFilter {
			m.cancel()
			return m, tea.Quit
		}
	case "?":
		if m.overlay != overlayHelp {
			m.overlay = overlayHelp
			return m, nil
		}
	}

	// Overlay key handling
	switch m.overlay {
	case overlayHelp:
		m.overlay = overlayNone
		return m, nil

	case overlayStats:
		switch msg.String() {
		case "esc", "q", "s":
			m.overlay = overlayNone
		case "r":
			m.stats = nil
			return m, m.statsCmd()
		}
		return m, nil

	case overlayExport:
		switch msg.String() {
		case "j", "n":
			return m, m.exportCmd()
		case "c":
			return m, m.exportCSVCmd()
		case "esc", "q":
			m.overlay = overlayNone
		}
		return m, nil
	}

	// Panel switching
	switch msg.String() {
	case "tab":
		m.focus = (m.focus + 1) % panelCount
		if m.focus == panelDetail && !m.showDetail {
			m.focus = (m.focus + 1) % panelCount
		}
		if m.focus == panelFilter {
			m.filterInput.Focus()
			return m, textinput.Blink
		}
		m.filterInput.Blur()
		return m, nil
	case "shift+tab":
		m.focus = (m.focus + panelCount - 1) % panelCount
		if m.focus == panelDetail && !m.showDetail {
			m.focus = (m.focus + panelCount - 1) % panelCount
		}
		if m.focus == panelFilter {
			m.filterInput.Focus()
			return m, textinput.Blink
		}
		m.filterInput.Blur()
		return m, nil
	}

	// Panel-specific key handling
	switch m.focus {

	case panelFilter:
		switch msg.String() {
		case "enter":
			expr := strings.TrimSpace(m.filterInput.Value())
			if expr == "" {
				m.filterWhere = ""
				m.filterArgs = nil
				m.filterErr = ""
			} else {
				res, err := filter.Parse(expr, 1)
				if err != nil {
					m.filterErr = err.Error()
					return m, nil
				}
				m.filterWhere = res.SQL
				m.filterArgs = res.Args
				m.filterErr = ""
			}
			m.filterExpr = expr
			m.focus = panelLogs
			m.filterInput.Blur()
			m.cursor = 0
			return m, m.fetchCmd(false)
		case "esc":
			m.filterInput.SetValue(m.filterExpr)
			m.filterInput.CursorEnd()
			m.filterErr = ""
			m.focus = panelLogs
			m.filterInput.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd

	case panelSources:
		switch msg.String() {
		case "j", "down":
			if m.sourceCursor < len(m.allSources)-1 {
				m.sourceCursor++
			}
		case "k", "up":
			if m.sourceCursor > 0 {
				m.sourceCursor--
			}
		case "space", "enter":
			return m.toggleSourceAt(m.sourceCursor)
		case "esc":
			m.focus = panelLogs
		}
		return m, nil

	case panelColumns:
		switch msg.String() {
		case "j", "down":
			if m.colCursor < len(m.columns)-1 {
				m.colCursor++
			}
		case "k", "up":
			if m.colCursor > 0 {
				m.colCursor--
			}
		case "space", "enter":
			m.columns[m.colCursor].visible = !m.columns[m.colCursor].visible
		case "l", "right":
			col := m.columns[m.colCursor].key
			if m.sortCol == col && !m.sortDesc {
				m.sortCol, m.sortDesc = "", false // toggle off
			} else {
				m.sortCol, m.sortDesc = col, false // ASC
			}
			m.cursor = 0
			return m, m.fetchCmd(false)
		case "h", "left":
			col := m.columns[m.colCursor].key
			if m.sortCol == col && m.sortDesc {
				m.sortCol, m.sortDesc = "", false // toggle off
			} else {
				m.sortCol, m.sortDesc = col, true // DESC
			}
			m.cursor = 0
			return m, m.fetchCmd(false)
		case "esc":
			m.focus = panelLogs
		}
		return m, nil

	case panelDetail:
		switch msg.String() {
		case "esc", "q":
			m.focus = panelLogs
		case "enter", "space":
			m.showDetail = false
			m.focus = panelLogs
		default:
			var cmd tea.Cmd
			m.detail, cmd = m.detail.Update(msg)
			return m, cmd
		}
		return m, nil

	default: // panelLogs
		switch msg.String() {
		case "j", "down":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
				m.tail = false
				m.updateDetail()
			}
		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
				m.tail = false
				m.updateDetail()
			}
		case "pgdown":
			page := m.visibleLogRows()
			m.cursor += page
			if m.cursor >= len(m.rows) {
				m.cursor = len(m.rows) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.tail = false
			m.updateDetail()
		case "pgup":
			page := m.visibleLogRows()
			m.cursor -= page
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.updateDetail()
		case "home":
			m.cursor = 0
			m.tail = true
			m.updateDetail()
		case "end":
			if len(m.rows) > 0 {
				m.cursor = len(m.rows) - 1
			}
			m.tail = false
			m.updateDetail()
		case "g":
			m.cursor = len(m.rows) - 1
			m.updateDetail()
		case "G":
			m.cursor = 0
			m.tail = true
			m.updateDetail()
		case "t":
			m.tail = !m.tail
			if m.tail {
				m.cursor = 0
			}
		case "f":
			m.focus = panelFilter
			m.filterInput.Focus()
			return m, textinput.Blink
		case "/":
			m.focus = panelFilter
			m.filterInput.SetValue("message CONTAINS ")
			m.filterInput.CursorEnd()
			m.filterInput.Focus()
			return m, textinput.Blink
		case "s":
			m.overlay = overlayStats
			m.stats = nil
			return m, m.statsCmd()
		case "e":
			m.overlay = overlayExport
		case "m":
			if row := m.currentRow(); row != nil {
				if m.marks[row.ID] {
					delete(m.marks, row.ID)
				} else {
					m.marks[row.ID] = true
				}
			}
		case "M":
			m.marks = make(map[int64]bool)
		case "enter", "space":
			m.showDetail = !m.showDetail
			m.updateDetail()
			if m.showDetail {
				m.detail.GotoTop()
				m.focus = panelDetail
			}
			// Don't forward the toggle key: the viewport binds space to page down.
			return m, nil
		case "r":
			m.tail = false
			return m, tea.Batch(m.fetchCmd(false), m.loadSourcesCmd())
		}

		if m.showDetail {
			var cmd tea.Cmd
			m.detail, cmd = m.detail.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m *Model) updateMaxID() {
	for _, r := range m.rows {
		if r.ID > m.maxID {
			m.maxID = r.ID
		}
	}
}

func (m *Model) currentRow() *db.Row {
	if m.cursor < len(m.rows) {
		return &m.rows[m.cursor]
	}
	return nil
}

func (m Model) handleClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	if mouse.Button == tea.MouseLeft {
		p := m.panelAt(mouse.X, mouse.Y)
		prevFocus := m.focus
		m.focus = p
		if p == panelFilter {
			m.filterInput.Focus()
			return m, textinput.Blink
		}
		m.filterInput.Blur()

		switch p {
		case panelLogs:
			row := m.logRowAt(mouse.Y)
			if row >= 0 {
				if prevFocus == panelLogs && row == m.cursor {
					// Already selected: toggle detail (same as Enter)
					m.showDetail = !m.showDetail
					if m.showDetail {
						m.focus = panelDetail
						m.updateDetail()
						m.detail.GotoTop()
					}
				} else {
					m.cursor = row
					m.tail = false
					m.updateDetail()
				}
			}
		case panelSources:
			idx := m.sourceItemAt(mouse.Y)
			if idx >= 0 {
				if prevFocus == panelSources && idx == m.sourceCursor {
					// Already selected: toggle (same as Space)
					return m.toggleSourceAt(idx)
				}
				m.sourceCursor = idx
			}
		case panelColumns:
			idx := m.colItemAt(mouse.Y)
			if idx >= 0 {
				if prevFocus == panelColumns && idx == m.colCursor {
					// Already selected: toggle visibility (same as Space)
					m.columns[idx].visible = !m.columns[idx].visible
				} else {
					m.colCursor = idx
				}
			}
		}
	}
	return m, nil
}

func (m Model) handleWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	switch mouse.Button {
	case tea.MouseWheelUp:
		return m.scrollPanel(m.panelAt(mouse.X, mouse.Y), -3)
	case tea.MouseWheelDown:
		return m.scrollPanel(m.panelAt(mouse.X, mouse.Y), 3)
	}
	return m, nil
}

func (m Model) scrollPanel(p panel, delta int) (tea.Model, tea.Cmd) {
	switch p {
	case panelLogs:
		m.cursor += delta
		if m.cursor < 0 {
			m.cursor = 0
		}
		if m.cursor >= len(m.rows) {
			m.cursor = len(m.rows) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		if delta > 0 {
			m.tail = false
		}
		m.updateDetail()
	case panelSources:
		m.sourceCursor += delta
		if m.sourceCursor < 0 {
			m.sourceCursor = 0
		}
		if m.sourceCursor >= len(m.allSources) {
			m.sourceCursor = len(m.allSources) - 1
		}
	case panelColumns:
		m.colCursor += delta
		if m.colCursor < 0 {
			m.colCursor = 0
		}
		if m.colCursor >= len(m.columns) {
			m.colCursor = len(m.columns) - 1
		}
	case panelDetail:
		// SetYOffset clamps to [0, max offset].
		m.detail.SetYOffset(m.detail.YOffset() + delta)
	}
	return m, nil
}

// logRowAt converts a screen Y coordinate to an absolute row index in m.rows,
// or -1 if the coordinate is outside the log content area.
func (m Model) logRowAt(y int) int {
	// y=0: top bar, y=1: log panel top border, y=2: header, y=3+: rows
	rowInView := y - 3
	if rowInView < 0 {
		return -1
	}
	bodyH := m.height - 2
	if bodyH < 4 {
		bodyH = 4
	}
	logsH := bodyH
	if m.showDetail {
		logsH = bodyH - 12
	}
	tableH := logsH - 3 // (innerH=logsH-2) - 1 header
	if tableH < 1 {
		tableH = 1
	}
	if rowInView >= tableH {
		return -1
	}
	start, _ := m.visibleWindow(tableH)
	idx := start + rowInView
	if idx >= 0 && idx < len(m.rows) {
		return idx
	}
	return -1
}

// sourceItemAt converts a screen Y coordinate to a source list index, or -1.
func (m Model) sourceItemAt(y int) int {
	// y=1: sources top border, y=2: item 0, y=3: item 1, ...
	idx := y - 2
	if idx >= 0 && idx < len(m.allSources) {
		return idx
	}
	return -1
}

// colItemAt converts a screen Y coordinate to a column list index, or -1.
func (m Model) colItemAt(y int) int {
	bodyH := m.height - 2
	if bodyH < 4 {
		bodyH = 4
	}
	sourcesH := bodyH * 45 / 100
	if sourcesH < 4 {
		sourcesH = 4
	}
	if sourcesH > bodyH-4 {
		sourcesH = bodyH - 4
	}
	// columns panel top border at y=sourcesH+1, content starts at y=sourcesH+2
	idx := y - (sourcesH + 2)
	if idx >= 0 && idx < len(m.columns) {
		return idx
	}
	return -1
}

// toggleSourceAt toggles the source at idx in the filter and refetches.
func (m Model) toggleSourceAt(idx int) (Model, tea.Cmd) {
	if idx >= len(m.allSources) {
		return m, nil
	}
	src := m.allSources[idx]
	currentSources, rest := filter.ExtractSources(m.filterExpr)
	enabled := make(map[string]bool, len(m.allSources))
	if len(currentSources) == 0 {
		for _, s := range m.allSources {
			enabled[s] = true
		}
	} else {
		for _, s := range currentSources {
			enabled[s] = true
		}
	}
	if enabled[src] {
		delete(enabled, src)
	} else {
		enabled[src] = true
	}
	var newSources []string
	if len(enabled) < len(m.allSources) {
		for _, s := range m.allSources {
			if enabled[s] {
				newSources = append(newSources, s)
			}
		}
	}
	newExpr := filter.BuildExpr(newSources, rest)
	_ = m.SetFilter(newExpr)
	m.cursor = 0
	return m, m.fetchCmd(false)
}

// panelAt returns which panel contains screen coordinate (x, y).
func (m Model) panelAt(x, y int) panel {
	if m.height == 0 {
		return m.focus
	}
	bodyH := m.height - 2
	if bodyH < 4 {
		bodyH = 4
	}
	if y <= 0 {
		return panelFilter // top bar holds the filter input
	}
	if y >= m.height-1 {
		return m.focus
	}
	if x < leftColWidth {
		sourcesH := bodyH * 45 / 100
		if sourcesH < 4 {
			sourcesH = 4
		}
		if sourcesH > bodyH-4 {
			sourcesH = bodyH - 4
		}
		if y <= sourcesH {
			return panelSources
		}
		return panelColumns
	}
	if m.showDetail {
		detailH := 12
		logsH := bodyH - detailH
		if y > logsH {
			return panelDetail
		}
	}
	return panelLogs
}

// visibleLogRows returns the number of log rows currently visible.
func (m Model) visibleLogRows() int {
	bodyH := m.height - 2
	if bodyH < 4 {
		bodyH = 4
	}
	innerH := bodyH - 2
	if m.showDetail {
		innerH -= 12
	}
	tableH := innerH - 1
	if tableH < 1 {
		tableH = 1
	}
	return tableH
}

// visibleWindow returns the start/end row indices for the current view.
func (m Model) visibleWindow(maxRows int) (start, end int) {
	total := len(m.rows)
	if total == 0 {
		return 0, 0
	}
	end = total
	if total <= maxRows {
		return 0, end
	}
	start = m.cursor - maxRows/2
	if start < 0 {
		start = 0
	}
	end = start + maxRows
	if end > total {
		end = total
		start = end - maxRows
		if start < 0 {
			start = 0
		}
	}
	return
}

func (m *Model) updateDetail() {
	if !m.showDetail {
		return
	}
	r := m.currentRow()
	if r == nil {
		m.detail.SetContent("")
		return
	}
	out := map[string]any{
		"id": r.ID, "ingested_at": r.IngestedAt.Format(time.RFC3339),
		"source": r.Source, "level": r.Level, "message": r.Message,
		"data": r.Data,
	}
	if r.LogTime != nil {
		out["log_time"] = r.LogTime.Format(time.RFC3339Nano)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	m.detail.SetContent(string(b))
}

func (m Model) visibleColumns() []column {
	var out []column
	for _, c := range m.columns {
		if c.visible {
			out = append(out, c)
		}
	}
	return out
}

// ---- View ----

// View wraps render in a tea.View that requests the alt screen and cell-motion
// mouse events on every frame, including "loading…" and the overlays.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) render() string {
	if m.width == 0 {
		return "loading…"
	}

	// Full-screen overlays
	switch m.overlay {
	case overlayHelp:
		return m.viewHelp()
	case overlayStats:
		return m.viewStats()
	}

	bodyH := m.height - 2 // top bar + status bar
	if bodyH < 4 {
		bodyH = 4
	}

	leftInnerW := leftColWidth - 2
	rightTotalW := m.width - leftColWidth
	if rightTotalW < 20 {
		rightTotalW = 20
	}
	rightInnerW := rightTotalW - 2

	// Left column: Sources + Columns stacked
	sourcesH := bodyH * 45 / 100
	if sourcesH < 4 {
		sourcesH = 4
	}
	if sourcesH > bodyH-4 {
		sourcesH = bodyH - 4
	}
	columnsH := bodyH - sourcesH

	sourcesPanel := m.viewSourcesPanel(leftInnerW, sourcesH-2)
	columnsPanel := m.viewColumnsPanel(leftInnerW, columnsH-2)
	leftCol := lipgloss.JoinVertical(lipgloss.Left, sourcesPanel, columnsPanel)

	// Right column: Logs + optional Detail
	var rightCol string
	if m.showDetail {
		detailH := 12
		logsH := bodyH - detailH
		if logsH < 4 {
			logsH = 4
		}
		logsPanel := m.viewLogsPanel(rightInnerW, logsH-2)
		detailPanel := m.viewDetailPanel(rightInnerW, detailH-2)
		rightCol = lipgloss.JoinVertical(lipgloss.Left, logsPanel, detailPanel)
	} else {
		rightCol = m.viewLogsPanel(rightInnerW, bodyH-2)
	}

	middle := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, rightCol)

	// Export overlay
	if m.overlay == overlayExport {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			m.viewExportPrompt(), lipgloss.WithWhitespaceChars(" "))
	}

	top := m.viewTopBar()
	status := m.viewStatusBar()
	return lipgloss.JoinVertical(lipgloss.Left, top, middle, status)
}

// panelBorder returns a bordered style. Active panels get a blue border.
func panelBorder(active bool) lipgloss.Style {
	s := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	if active {
		return s.BorderForeground(colorBlue)
	}
	return s.BorderForeground(colorSurface1)
}

// titledBorderPanel builds a manually-framed panel with the title embedded in
// the top border line. Rendering each border segment separately (before and
// after the styled title) prevents the title's ANSI reset from killing the
// border color for the trailing dashes.
func titledBorderPanel(active bool, innerW, innerH int, styledTitle string, contentLines []string) string {
	borderColor := colorSurface1
	if active {
		borderColor = colorBlue
	}
	bs := lipgloss.NewStyle().Foreground(borderColor)
	lb := bs.Render("│")
	rb := bs.Render("│")

	titleLen := 2 + lipgloss.Width(styledTitle) + 1 // "─ " + title + " "
	topDashes := innerW - titleLen
	if topDashes < 0 {
		topDashes = 0
	}
	top := bs.Render("╭─ ") + styledTitle + bs.Render(" "+strings.Repeat("─", topDashes)+"╮")
	bot := bs.Render("╰" + strings.Repeat("─", innerW) + "╯")
	pad := lipgloss.NewStyle().Width(innerW)

	lines := make([]string, 0, innerH+2)
	lines = append(lines, top)
	for i := 0; i < innerH; i++ {
		content := ""
		if i < len(contentLines) {
			content = contentLines[i]
		}
		lines = append(lines, lb+pad.Render(content)+rb)
	}
	lines = append(lines, bot)
	return strings.Join(lines, "\n")
}

func (m Model) viewTopBar() string {
	tail := styleTailOn.Render("● TAIL")
	if !m.tail {
		tail = styleTailOff.Render("○ TAIL")
	}

	logo := lipgloss.NewStyle().Bold(true).Foreground(colorMauve).Render("logr")

	filterPrefix := styleFilterBar.Render("filter: ")
	var filterContent string
	if m.focus == panelFilter {
		filterContent = filterPrefix + m.filterInput.View()
		if m.filterErr != "" {
			filterContent += " " + styleLevel["ERROR"].Render("✗ "+m.filterErr)
		}
	} else if m.filterExpr != "" {
		filterContent = filterPrefix + styleBlue(m.filterExpr)
	} else {
		filterContent = filterPrefix + styleHelp.Render("(press f or Tab)")
	}

	help := styleHelp.Render("[?]")
	right := tail + "  " + help
	// left: logo + gap + filter + gap + right
	rightW := lipgloss.Width(right)
	logoW := lipgloss.Width(logo)
	filterW := m.width - logoW - rightW - 4
	if filterW < 10 {
		filterW = 10
	}
	filterTrunc := truncateLine(filterContent, filterW)
	gap1 := strings.Repeat(" ", 2)
	gap2W := m.width - logoW - lipgloss.Width(filterTrunc) - rightW - 4
	if gap2W < 1 {
		gap2W = 1
	}
	gap2 := strings.Repeat(" ", gap2W)

	bg := lipgloss.NewStyle().Background(colorMantle).Foreground(colorText)
	return bg.Width(m.width).Render(logo + gap1 + filterTrunc + gap2 + right)
}

func truncateLine(s string, maxW int) string {
	w := lipgloss.Width(s)
	if w <= maxW {
		return s
	}
	// crude: strip until it fits (ANSI-aware width)
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > maxW {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

func (m Model) viewSourcesPanel(innerW, innerH int) string {
	active := m.focus == panelSources
	titleText := styleHelp.Render("Sources")
	if active {
		titleText = styleKey.Render("Sources")
	}

	var contentLines []string

	activeSources, _ := filter.ExtractSources(m.filterExpr)
	enabledSet := make(map[string]bool, len(m.allSources))
	if len(activeSources) == 0 {
		for _, s := range m.allSources {
			enabledSet[s] = true
		}
	} else {
		for _, s := range activeSources {
			enabledSet[s] = true
		}
	}

	for i, src := range m.allSources {
		enabled := enabledSet[src]
		check := styleHelp.Render("[ ]")
		if enabled {
			check = styleGreen("[✓]")
		}
		label := truncate(src, innerW-5)
		line := check + " " + label
		if active && i == m.sourceCursor {
			line = styleSelected.Width(innerW).Render(line)
		}
		contentLines = append(contentLines, line)
	}

	if len(m.allSources) == 0 {
		contentLines = append(contentLines, styleHelp.Render("  no sources yet"))
	}

	return titledBorderPanel(active, innerW, innerH, titleText, contentLines)
}

func (m Model) viewColumnsPanel(innerW, innerH int) string {
	active := m.focus == panelColumns
	titleText := styleHelp.Render("Columns")
	if active {
		titleText = styleKey.Render("Columns")
	}

	var contentLines []string

	for i, c := range m.columns {
		check := styleHelp.Render("[ ]")
		if c.visible {
			check = styleGreen("[✓]")
		}
		label := c.label
		if c.key == m.sortCol {
			if m.sortDesc {
				label += " ▼"
			} else {
				label += " ▲"
			}
		}
		line := check + " " + label
		if active && i == m.colCursor {
			line = styleSelected.Width(innerW).Render(line)
		}
		contentLines = append(contentLines, line)
	}

	return titledBorderPanel(active, innerW, innerH, titleText, contentLines)
}

func (m Model) viewLogsPanel(innerW, innerH int) string {
	active := m.focus == panelLogs
	cols := m.visibleColumns()
	widths := m.columnWidths(cols, innerW)

	var headerParts []string
	for i, c := range cols {
		label := c.label
		if c.key == m.sortCol {
			if m.sortDesc {
				label += " ▼"
			} else {
				label += " ▲"
			}
		}
		headerParts = append(headerParts, styleHeader.Width(widths[i]).Render(label))
	}
	header := strings.Join(headerParts, " ")

	tableH := innerH - 1
	if tableH < 1 {
		tableH = 1
	}

	start, end := m.visibleWindow(tableH)
	rowLines := m.tableLines(cols, widths, start, end, innerW)
	sbLines := logScrollbar(len(m.rows), tableH, start, tableH, active)

	for len(rowLines) < tableH {
		rowLines = append(rowLines, "")
	}

	// Build frame manually so the scrollbar replaces the right border.
	borderColor := colorSurface1
	if active {
		borderColor = colorBlue
	}
	bs := lipgloss.NewStyle().Foreground(borderColor)
	lb := bs.Render("│")
	top := bs.Render("╭" + strings.Repeat("─", innerW) + "╮")
	bot := bs.Render("╰" + strings.Repeat("─", innerW) + "╯")

	pad := lipgloss.NewStyle().Width(innerW)
	lines := make([]string, 0, innerH+2)
	lines = append(lines, top)
	lines = append(lines, lb+pad.Render(header)+bs.Render("│"))
	for i := 0; i < tableH; i++ {
		content := ""
		if i < len(rowLines) {
			content = rowLines[i]
		}
		rb := bs.Render("│")
		if i < len(sbLines) {
			rb = sbLines[i]
		}
		lines = append(lines, lb+pad.Render(content)+rb)
	}
	lines = append(lines, bot)
	return strings.Join(lines, "\n")
}

// tableLines renders visible log rows as individual strings (without joining).
func (m Model) tableLines(cols []column, widths []int, start, end, contentW int) []string {
	if len(m.rows) == 0 {
		return []string{styleHelp.Render("  no logs")}
	}
	var lines []string
	for i := start; i < end; i++ {
		r := m.rows[i]
		marked := m.marks[r.ID]
		selected := i == m.cursor

		var cells []string
		for j, c := range cols {
			val := truncate(rowField(r, c.key), widths[j])
			styled := renderCell(val, r.Level, c.key, selected)
			cells = append(cells, lipgloss.NewStyle().Width(widths[j]).Render(styled))
		}
		markStr := " "
		if marked {
			markStr = styleMark.Render("*")
		}
		line := strings.Join(cells, " ") + markStr
		if selected {
			line = styleSelected.Width(contentW).Render(line)
		}
		lines = append(lines, line)
	}
	return lines
}

// logScrollbar returns one character per row for the right border / scrollbar.
// The track character matches the panel border colour; thumb is a distinct accent.
func logScrollbar(total, visible, start, height int, active bool) []string {
	borderColor := colorSurface1
	if active {
		borderColor = colorBlue
	}
	track := lipgloss.NewStyle().Foreground(borderColor).Render("│")
	lines := make([]string, height)
	if total <= visible {
		for i := range lines {
			lines[i] = track
		}
		return lines
	}
	thumbH := visible * height / total
	if thumbH < 1 {
		thumbH = 1
	}
	maxStart := total - visible
	if maxStart < 1 {
		maxStart = 1
	}
	thumbTop := start * (height - thumbH) / maxStart
	thumb := lipgloss.NewStyle().Foreground(colorOverlay1).Render("▐")
	for i := range lines {
		if i >= thumbTop && i < thumbTop+thumbH {
			lines[i] = thumb
		} else {
			lines[i] = track
		}
	}
	return lines
}

func (m Model) viewDetailPanel(innerW, innerH int) string {
	active := m.focus == panelDetail
	m.detail.SetWidth(innerW)
	m.detail.SetHeight(innerH)

	borderColor := colorSurface0
	if active {
		borderColor = colorBlue
	}
	bs := lipgloss.NewStyle().Foreground(borderColor)
	lb := bs.Render("│")
	// Title embedded in top border to avoid consuming a viewport line.
	titleText := styleHeader.Render("Detail")
	titleLen := 2 + lipgloss.Width(titleText) + 1 // "─ " + title + " "
	topDashes := innerW - titleLen
	if topDashes < 0 {
		topDashes = 0
	}
	top := bs.Render("╭─ ") + titleText + bs.Render(" "+strings.Repeat("─", topDashes)+"╮")
	bot := bs.Render("╰" + strings.Repeat("─", innerW) + "╯")
	pad := lipgloss.NewStyle().Width(innerW)

	// Viewport lines + scrollbar
	viewLines := strings.Split(m.detail.View(), "\n")
	for len(viewLines) < m.detail.Height() {
		viewLines = append(viewLines, "")
	}
	sbLines := logScrollbar(m.detail.TotalLineCount(), m.detail.Height(), m.detail.YOffset(), m.detail.Height(), active)

	lines := make([]string, 0, innerH+2)
	lines = append(lines, top)
	for i := 0; i < m.detail.Height(); i++ {
		content := ""
		if i < len(viewLines) {
			content = viewLines[i]
		}
		rb := bs.Render("│")
		if i < len(sbLines) {
			rb = sbLines[i]
		}
		lines = append(lines, lb+pad.Render(content)+rb)
	}
	lines = append(lines, bot)
	return strings.Join(lines, "\n")
}

func (m Model) viewStatusBar() string {
	marked := len(m.marks)
	msg := fmt.Sprintf(" rows:%d  marked:%d", len(m.rows), marked)
	if m.statusMsg != "" {
		msg += "  " + m.statusMsg
	}
	keys := styleKey.Render("[Tab]") + styleHelp.Render("panel") +
		"  " + styleKey.Render("[f]") + styleHelp.Render("ilter") +
		"  " + styleKey.Render("[t]") + styleHelp.Render("ail") +
		"  " + styleKey.Render("[s]") + styleHelp.Render("tats") +
		"  " + styleKey.Render("[m]") + styleHelp.Render("ark") +
		"  " + styleKey.Render("[e]") + styleHelp.Render("xport") +
		"  " + styleKey.Render("[Enter]") + styleHelp.Render("detail") +
		"  " + styleKey.Render("[q]") + styleHelp.Render("uit")
	rightW := lipgloss.Width(keys) + 1
	leftW := m.width - rightW
	if leftW < 0 {
		leftW = 0
	}
	return styleStatusBar.Width(leftW).Render(msg) + keys
}

func (m Model) viewExportPrompt() string {
	marked := len(m.marks)
	scope := fmt.Sprintf("all %d rows", len(m.rows))
	if marked > 0 {
		scope = fmt.Sprintf("%d marked rows", marked)
	}
	return styleBorder.Padding(1, 3).Render(
		styleHeader.Render("Export "+scope) + "\n\n" +
			styleKey.Render("[j]") + "  JSONL (one record per line)\n" +
			styleKey.Render("[c]") + "  CSV\n" +
			styleKey.Render("[Esc]") + " cancel",
	)
}

func (m Model) viewHelp() string {
	type binding struct{ key, desc string }
	bindings := []binding{
		{"Tab / Shift+Tab", "cycle panel focus (Filter → Sources → Columns → Logs)"},
		{"j / ↓", "next row / next item in focused panel"},
		{"k / ↑", "previous row / previous item"},
		{"Space / Enter", "toggle source or column  |  toggle detail pane (Logs)"},
		{"g", "go to oldest log"},
		{"G", "go to newest (re-enable tail)"},
		{"t", "toggle tail mode"},
		{"f", "jump to Filter panel"},
		{"/", "quick search (message CONTAINS)"},
		{"s", "stats / aggregation panel"},
		{"m", "mark / unmark row"},
		{"M", "clear all marks"},
		{"e", "export"},
		{"r", "refresh logs + sources"},
		{"q / Ctrl+C", "quit"},
		{"?", "this help"},
	}
	var b strings.Builder
	b.WriteString(styleHeader.Render("logr — keyboard shortcuts") + "\n\n")
	b.WriteString(styleHelp.Render("Filter DSL examples:") + "\n")
	for _, ex := range []string{
		"  source=storage",
		"  level=error",
		"  source IN (storage, account)",
		"  level!=info AND source=synapse",
		`  message CONTAINS "routing"`,
		`  NOT (message CONTAINS "health")`,
		`  NOT (source IN (account, residence))`,
		`  message MATCHES "^GET /api"`,
		`  NOT (message MATCHES* "debug")`,
		"  log_time > 1h",
		"  data.status=500",
	} {
		b.WriteString(styleHelp.Render(ex) + "\n")
	}
	b.WriteString("\n" + styleHelp.Render("Keys:") + "\n")
	for _, bnd := range bindings {
		b.WriteString(fmt.Sprintf("  %-28s %s\n", styleKey.Render(bnd.key), styleHelp.Render(bnd.desc)))
	}
	b.WriteString("\n" + styleHelp.Render("Press any key to close"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		styleBorder.Padding(1, 3).Render(b.String()))
}

// ---- Helpers ----

// joinLines joins lines, padding/truncating to exactly h lines.
func joinLines(lines []string, h int) string {
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m Model) columnWidths(cols []column, innerW int) []int {
	total := innerW - len(cols) - 1 // separators + mark char
	fixed := 0
	flexCount := 0
	for _, c := range cols {
		if c.width > 0 {
			fixed += c.width
		} else {
			flexCount++
		}
	}
	flex := 0
	if flexCount > 0 {
		flex = (total - fixed) / flexCount
		if flex < 20 {
			flex = 20
		}
	}
	out := make([]int, len(cols))
	for i, c := range cols {
		if c.width > 0 {
			out[i] = c.width
		} else {
			out[i] = flex
		}
	}
	return out
}

func rowField(r db.Row, key string) string {
	switch key {
	case "log_time":
		if r.LogTime != nil {
			return r.LogTime.Format("15:04:05.000")
		}
		return r.IngestedAt.Format("15:04:05.000")
	case "ingested_at":
		return r.IngestedAt.Format("15:04:05.000")
	case "source":
		return r.Source
	case "level":
		return r.Level
	case "message":
		return r.Message
	}
	if strings.HasPrefix(key, "data.") {
		k := key[5:]
		if v, ok := r.Data[k]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func renderCell(val, level, key string, selected bool) string {
	if selected {
		return val
	}
	switch key {
	case "log_time", "ingested_at":
		return styleTime.Render(val)
	case "source":
		return styleSource.Render(val)
	case "level":
		return levelStyle(level).Render(val)
	case "message":
		return styleMsg.Render(val)
	}
	return val
}

func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:width]
	}
	return s[:width-1] + "…"
}

func styleBlue(s string) string {
	return lipgloss.NewStyle().Foreground(colorBlue).Render(s)
}

func styleGreen(s string) string {
	return lipgloss.NewStyle().Foreground(colorGreen).Render(s)
}
