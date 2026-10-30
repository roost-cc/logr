package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roost-cc/logr/internal/config"
	"github.com/roost-cc/logr/internal/db"
)

// newTestModel builds a sized model holding one row. Commands are never run,
// so the nil query client is never touched.
func newTestModel(t *testing.T) Model {
	t.Helper()
	m := NewModel(nil, config.ClientConfig{})
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return update(t, m, rowsMsg{rows: []db.Row{{
		ID:         1,
		IngestedAt: time.Unix(0, 0),
		Source:     "storage",
		Level:      "INFO",
		Message:    "hello",
	}}})
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

func keyPress(text string) tea.KeyPressMsg {
	if text == " " {
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	r := []rune(text)[0]
	return tea.KeyPressMsg{Code: r, Text: text}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestSpaceOpensDetailFromLogs(t *testing.T) {
	m := newTestModel(t)
	if m.focus != panelLogs || m.showDetail {
		t.Fatalf("precondition: focus=%v showDetail=%v", m.focus, m.showDetail)
	}
	m = update(t, m, keyPress(" "))
	if !m.showDetail {
		t.Fatal("space on the logs panel did not open the detail pane")
	}
	if m.focus != panelDetail {
		t.Fatalf("focus = %v, want panelDetail", m.focus)
	}
	m = update(t, m, keyPress(" "))
	if m.showDetail {
		t.Fatal("space on the detail pane did not close it")
	}
}

func TestDetailOpensAtTop(t *testing.T) {
	m := newTestModel(t)
	data := map[string]any{}
	for i := range 40 {
		data[string(rune('a'+i%26))+strings.Repeat("x", i)] = i
	}
	m.rows[0].Data = data
	m = update(t, m, keyPress(" "))
	m.detail.SetYOffset(15) // scrolled, as a previous record may have left it
	m = update(t, m, keyPress(" "))
	m = update(t, m, keyPress(" "))
	if got := m.detail.YOffset(); got != 0 {
		t.Fatalf("detail YOffset after reopening = %d, want 0", got)
	}
}

func TestSpaceTogglesColumn(t *testing.T) {
	m := newTestModel(t)
	m.focus = panelColumns
	before := m.columns[m.colCursor].visible
	m = update(t, m, keyPress(" "))
	if m.columns[m.colCursor].visible == before {
		t.Fatal("space on the columns panel did not toggle the column")
	}
}

func TestQuitOutsideFilter(t *testing.T) {
	m := newTestModel(t)
	_, cmd := m.Update(keyPress("q"))
	if !isQuit(cmd) {
		t.Fatal("q on the logs panel did not quit")
	}
}

func TestQTypedIntoFocusedFilter(t *testing.T) {
	m := newTestModel(t)
	m.focus = panelFilter
	m.filterInput.Focus()
	next, cmd := m.Update(keyPress("q"))
	if isQuit(cmd) {
		t.Fatal("q quit while the filter was focused")
	}
	m = next.(Model)
	if got := m.filterInput.Value(); !strings.HasSuffix(got, "q") {
		t.Fatalf("filter value = %q, want it to end with q", got)
	}
	m = update(t, m, keyPress(" "))
	if got := m.filterInput.Value(); got != "q " {
		t.Fatalf("filter value = %q, want %q", got, "q ")
	}
}

func TestQuickSearchPutsCursorAtEnd(t *testing.T) {
	m := newTestModel(t)
	m.filterInput.SetValue("source=pipe") // leaves the cursor mid-way in the next value
	m = update(t, m, keyPress("/"))
	m = update(t, m, keyPress("x"))
	if got, want := m.filterInput.Value(), "message CONTAINS x"; got != want {
		t.Fatalf("filter value = %q, want %q", got, want)
	}
}

func TestViewRequestsAltScreenAndMouse(t *testing.T) {
	check := func(name string, m Model) {
		t.Helper()
		v := m.View()
		if !v.AltScreen {
			t.Errorf("%s: AltScreen = false", name)
		}
		if v.MouseMode != tea.MouseModeCellMotion {
			t.Errorf("%s: MouseMode = %v, want MouseModeCellMotion", name, v.MouseMode)
		}
	}
	unsized := NewModel(nil, config.ClientConfig{})
	if unsized.width != 0 {
		t.Fatalf("precondition: width = %d", unsized.width)
	}
	check("unsized", unsized)
	sized := newTestModel(t)
	check("sized", sized)
	sized.overlay = overlayHelp
	check("help overlay", sized)
}

func TestClickFocusesPanels(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, tea.MouseClickMsg{X: leftColWidth + 10, Y: 0, Button: tea.MouseLeft})
	if m.focus != panelFilter {
		t.Fatalf("click on the filter bar: focus = %v, want panelFilter", m.focus)
	}
	if !m.filterInput.Focused() {
		t.Fatal("click on the filter bar did not focus the input")
	}
	m = update(t, m, tea.MouseClickMsg{X: 2, Y: 3, Button: tea.MouseLeft})
	if m.focus != panelSources {
		t.Fatalf("click on the sources panel: focus = %v, want panelSources", m.focus)
	}
	if m.filterInput.Focused() {
		t.Fatal("filter input still focused after clicking another panel")
	}
}

func TestWheelScrollsLogs(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, tea.MouseWheelMsg{X: leftColWidth + 10, Y: 5, Button: tea.MouseWheelDown})
	if m.tail {
		t.Fatal("wheel down on the logs panel did not leave tail mode")
	}
}
