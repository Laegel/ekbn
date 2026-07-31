// Package tui is ekbn's terminal front-end: a small robot-and-button idle
// screen that leads into a spec input (typed/pasted text, or a file picked
// from storage) and, on submit, runs specify's decomposition in-process,
// streaming its output into a scrolling panel in the same screen.
package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/filepicker"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ekbn/internal/specify"
)

type screen int

const (
	screenIdle screen = iota
	screenInput
	screenRunning
	screenDone
)

// RunSpecifyFunc matches specify.Run's signature. Injected on Model so tests
// can stub it out without a real opencode binary.
type RunSpecifyFunc func(context.Context, specify.Options, io.Writer) error

// Model is the top-level Bubble Tea model for ekbn's TUI.
type Model struct {
	ctx context.Context

	screen        screen
	width, height int

	textarea    textarea.Model
	usingPicker bool
	filepicker  filepicker.Model

	viewport viewport.Model
	output   strings.Builder

	runSpecify RunSpecifyFunc
	cancelRun  context.CancelFunc
	outMsgs    chan tea.Msg
	lastErr    error

	quitting bool
}

// New builds the initial idle-screen model. runSpecify is normally
// specify.Run; tests inject a stub instead.
func New(ctx context.Context, runSpecify RunSpecifyFunc) Model {
	ta := textarea.New()
	ta.Placeholder = "Paste or type a spec here..."
	ta.ShowLineNumbers = false

	fp := filepicker.New()
	fp.AllowedTypes = []string{".md", ".txt"}
	fp.DirAllowed = false
	fp.FileAllowed = true

	vp := viewport.New(80, 20)

	return Model{
		ctx:        ctx,
		screen:     screenIdle,
		textarea:   ta,
		filepicker: fp,
		viewport:   vp,
		runSpecify: runSpecify,
	}
}

func (m Model) Init() tea.Cmd {
	return nil
}

// ---------------------------------------------------------------------------
// Messages relaying specify.Run's output from its own goroutine into the
// Bubble Tea Update loop, which must never be mutated from outside it.

type outputMsg []byte

type specifyStartedMsg struct {
	msgs   chan tea.Msg
	cancel context.CancelFunc
}

type specifyDoneMsg struct{ err error }

// chanWriter relays every Write into msgs as a tea.Msg, since specify.Run's
// output is produced on a goroutine outside Bubble Tea's own Update loop.
type chanWriter struct{ msgs chan<- tea.Msg }

func (w chanWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p)) // copy: the writer may reuse p after Write returns
	copy(b, p)
	w.msgs <- outputMsg(b)
	return len(p), nil
}

func waitForMsg(msgs <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-msgs }
}

// startSpecifyCmd launches runSpecify on its own goroutine and returns the
// tea.Cmd that reports back once it has started (with the channel Update
// then relays from).
func (m Model) startSpecifyCmd(opts specify.Options) tea.Cmd {
	runSpecify := m.runSpecify
	parent := m.ctx
	return func() tea.Msg {
		msgs := make(chan tea.Msg, 32)
		runCtx, cancel := context.WithCancel(parent)
		go func() {
			err := runSpecify(runCtx, opts, chanWriter{msgs})
			msgs <- specifyDoneMsg{err}
		}()
		return specifyStartedMsg{msgs: msgs, cancel: cancel}
	}
}

func (m Model) startRun(opts specify.Options) (Model, tea.Cmd) {
	m.output.Reset()
	m.viewport.SetContent("")
	m.screen = screenRunning
	return m, m.startSpecifyCmd(opts)
}

// ---------------------------------------------------------------------------

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = msg.Width - 2
		m.viewport.Height = msg.Height - 6
		m.textarea.SetWidth(msg.Width - 2)
		m.textarea.SetHeight(msg.Height - 8)
		var cmd tea.Cmd
		m.filepicker, cmd = m.filepicker.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		switch m.screen {
		case screenIdle:
			return m.updateIdle(msg)
		case screenInput:
			return m.updateInput(msg)
		case screenRunning:
			return m.updateRunning(msg)
		case screenDone:
			return m.updateDone(msg)
		}
		return m, nil

	case specifyStartedMsg:
		m.outMsgs = msg.msgs
		m.cancelRun = msg.cancel
		return m, waitForMsg(m.outMsgs)

	case outputMsg:
		m.output.Write(msg)
		m.viewport.SetContent(m.output.String())
		m.viewport.GotoBottom()
		return m, waitForMsg(m.outMsgs)

	case specifyDoneMsg:
		m.lastErr = msg.err
		m.screen = screenDone
		return m, nil
	}
	return m, nil
}

func (m Model) updateIdle(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		return m, tea.Quit
	case "enter", " ":
		m.screen = screenInput
		cmd := m.textarea.Focus()
		return m, cmd
	}
	return m, nil
}

func (m Model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.usingPicker {
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "ctrl+f", "esc":
			m.usingPicker = false
			return m, nil
		}
		var cmd tea.Cmd
		m.filepicker, cmd = m.filepicker.Update(msg)
		if didSelect, path := m.filepicker.DidSelectFile(msg); didSelect {
			return m.startRun(specify.Options{Files: []string{path}})
		}
		return m, cmd
	}

	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc":
		m.screen = screenIdle
		return m, nil
	case "ctrl+f":
		m.usingPicker = true
		return m, m.filepicker.Init()
	case "ctrl+s":
		text := strings.TrimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		return m.startRun(specify.Options{Text: m.textarea.Value()})
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m Model) updateRunning(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		if m.cancelRun != nil {
			m.cancelRun()
		}
		m.screen = screenInput
		return m, nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m Model) updateDone(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		return m, tea.Quit
	case "enter":
		m.output.Reset()
		m.screen = screenInput
		return m, nil
	case "esc":
		m.output.Reset()
		m.screen = screenIdle
		return m, nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// ---------------------------------------------------------------------------

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	switch m.screen {
	case screenIdle:
		return m.idleView()
	case screenInput:
		return m.inputView()
	case screenRunning:
		return m.runningView()
	case screenDone:
		return m.doneView()
	}
	return ""
}

func (m Model) idleView() string {
	button := buttonStyle.Render("specify a spec")
	return "\n" + robotStyle.Render(robotArt) + "\n\n" +
		lipgloss.PlaceHorizontal(m.width, lipgloss.Center, button) + "\n\n" +
		helpStyle.Render("enter/space: start   •   q: quit")
}

func (m Model) inputView() string {
	var body string
	var help string
	if m.usingPicker {
		body = m.filepicker.View()
		help = "enter: pick file   •   ctrl+f/esc: back to typing   •   ctrl+c: quit"
	} else {
		body = m.textarea.View()
		help = "ctrl+s: submit   •   ctrl+f: browse a file   •   esc: back   •   ctrl+c: quit"
	}
	return "\n" + titleStyle.Render("Specify a spec") + "\n\n" + body + "\n\n" + helpStyle.Render(help)
}

func (m Model) runningView() string {
	return "\n" + titleStyle.Render("Running specify...") + "\n\n" +
		viewportBorderStyle.Render(m.viewport.View()) + "\n\n" +
		helpStyle.Render("ctrl+c: cancel this run")
}

func (m Model) doneView() string {
	status := okStyle.Render("✓ done")
	if m.lastErr != nil {
		status = errStyle.Render(fmt.Sprintf("✗ %v", m.lastErr))
	}
	return "\n" + titleStyle.Render("Specify") + "  " + status + "\n\n" +
		viewportBorderStyle.Render(m.viewport.View()) + "\n\n" +
		helpStyle.Render("enter: run another   •   esc: back to start   •   q: quit")
}
