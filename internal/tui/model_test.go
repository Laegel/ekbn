package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ekbn/internal/specify"
)

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+f":
		return tea.KeyMsg{Type: tea.KeyCtrlF}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func stubRunSpecify(t *testing.T, lines []string, err error) RunSpecifyFunc {
	t.Helper()
	return func(ctx context.Context, opts specify.Options, out io.Writer) error {
		for _, l := range lines {
			io.WriteString(out, l+"\n")
		}
		return err
	}
}

func TestIdleToInput(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	if m.screen != screenIdle {
		t.Fatalf("initial screen = %v, want screenIdle", m.screen)
	}

	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)
	if m.screen != screenInput {
		t.Errorf("screen after enter = %v, want screenInput", m.screen)
	}
}

func TestIdleQuit(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	_, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("expected a tea.Quit command")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("cmd() = %T, want tea.QuitMsg", msg)
	}
}

func TestTypingIntoTextarea(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)

	for _, r := range "hello" {
		updated, _ = m.Update(keyMsg(string(r)))
		m = updated.(Model)
	}
	if got := m.textarea.Value(); got != "hello" {
		t.Errorf("textarea.Value() = %q, want %q", got, "hello")
	}
}

func TestCtrlFTogglesFilePicker(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)

	updated, _ = m.Update(keyMsg("ctrl+f"))
	m = updated.(Model)
	if !m.usingPicker {
		t.Fatal("ctrl+f should switch to the file picker")
	}

	updated, _ = m.Update(keyMsg("esc"))
	m = updated.(Model)
	if m.usingPicker {
		t.Error("esc should switch back to typing")
	}
}

func TestSubmitEmptyTextDoesNothing(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)

	updated, _ = m.Update(keyMsg("ctrl+s"))
	m = updated.(Model)
	if m.screen != screenInput {
		t.Errorf("screen = %v, want screenInput (empty submit should be a no-op)", m.screen)
	}
}

func TestSubmitRunsAndStreamsOutput(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, []string{"line one", "line two"}, nil))
	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)

	for _, r := range "a spec" {
		updated, _ = m.Update(keyMsg(string(r)))
		m = updated.(Model)
	}

	updated, cmd := m.Update(keyMsg("ctrl+s"))
	m = updated.(Model)
	if m.screen != screenRunning {
		t.Fatalf("screen after submit = %v, want screenRunning", m.screen)
	}
	if cmd == nil {
		t.Fatal("submit should return a command that starts the run")
	}

	// Drive the async handshake synchronously: startSpecifyCmd's returned
	// tea.Cmd blocks until the goroutine reports back, which is instant here
	// since the stub run is synchronous and fast.
	startedMsg := cmd()
	updated, cmd = m.Update(startedMsg)
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected a command waiting on the output channel")
	}

	var sawDone bool
	for i := 0; i < 10 && !sawDone; i++ {
		msg := cmd()
		updated, cmd = m.Update(msg)
		m = updated.(Model)
		if _, ok := msg.(specifyDoneMsg); ok {
			sawDone = true
		}
		if cmd == nil {
			break
		}
	}

	if !sawDone {
		t.Fatal("did not observe a specifyDoneMsg")
	}
	if m.screen != screenDone {
		t.Errorf("screen = %v, want screenDone", m.screen)
	}
	if m.lastErr != nil {
		t.Errorf("lastErr = %v, want nil", m.lastErr)
	}
	if !strings.Contains(m.output.String(), "line one") || !strings.Contains(m.output.String(), "line two") {
		t.Errorf("output = %q, want both lines", m.output.String())
	}
}

func TestSubmitFailureShowsError(t *testing.T) {
	wantErr := errors.New("boom")
	m := New(context.Background(), stubRunSpecify(t, nil, wantErr))
	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)
	for _, r := range "x" {
		updated, _ = m.Update(keyMsg(string(r)))
		m = updated.(Model)
	}
	_, cmd := m.Update(keyMsg("ctrl+s"))

	startedMsg := cmd()
	updated, cmd = m.Update(startedMsg)
	m = updated.(Model)

	for i := 0; i < 10; i++ {
		msg := cmd()
		updated, cmd = m.Update(msg)
		m = updated.(Model)
		if _, ok := msg.(specifyDoneMsg); ok {
			break
		}
	}

	if m.screen != screenDone {
		t.Fatalf("screen = %v, want screenDone", m.screen)
	}
	if m.lastErr == nil || m.lastErr.Error() != "boom" {
		t.Errorf("lastErr = %v, want boom", m.lastErr)
	}
}

func TestDoneEnterReturnsToInput(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	m.screen = screenDone
	m.lastErr = nil

	updated, _ := m.Update(keyMsg("enter"))
	m = updated.(Model)
	if m.screen != screenInput {
		t.Errorf("screen after enter on done = %v, want screenInput", m.screen)
	}
}

func TestDoneEscReturnsToIdle(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	m.screen = screenDone

	updated, _ := m.Update(keyMsg("esc"))
	m = updated.(Model)
	if m.screen != screenIdle {
		t.Errorf("screen after esc on done = %v, want screenIdle", m.screen)
	}
}

func TestRunningCtrlCCancelsAndReturnsToInput(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	m.screen = screenRunning
	cancelled := false
	m.cancelRun = func() { cancelled = true }

	updated, _ := m.Update(keyMsg("ctrl+c"))
	m = updated.(Model)
	if !cancelled {
		t.Error("ctrl+c while running should cancel the in-flight run")
	}
	if m.screen != screenInput {
		t.Errorf("screen after cancel = %v, want screenInput", m.screen)
	}
}

func TestViewsRenderKeyContent(t *testing.T) {
	m := New(context.Background(), stubRunSpecify(t, nil, nil))
	m.width, m.height = 80, 24

	if !strings.Contains(m.View(), "specify a spec") {
		t.Error("idle view should show the button label")
	}

	m.screen = screenInput
	if !strings.Contains(m.View(), "Specify a spec") {
		t.Error("input view should show its title")
	}

	m.screen = screenDone
	m.output.WriteString("hello")
	m.viewport.SetContent("hello")
	if !strings.Contains(m.View(), "done") {
		t.Error("done view should show a success indicator when lastErr is nil")
	}
}
