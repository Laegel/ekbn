// Package orchestrator polls the kanban board, schedules ready tickets, and
// runs each one's agent pipeline directly in the project's working
// directory. It used to be its own binary; Run is now the entry point a
// single ekbn process calls, whether from the `orchestrator` subcommand or
// from combined/TUI mode.
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

const (
	pollInterval = 150
	kanbanRoot   = ".kanban"
	logFile      = ".kanban/orchestrator.log"
	agentsMD     = "AGENTS.md"
	// agentsMDFallback is checked if agentsMD isn't found at the project
	// root — .agents/AGENTS.md is a common enough convention that ekbn
	// shouldn't force every project onto a single fixed layout.
	agentsMDFallback = ".agents/AGENTS.md"
	securityMD       = "SECURITY.md"
	leaseDuration    = 30 * time.Minute
)

// logger opens its file lazily, on first actual log call, rather than at
// construction. It's assigned to a package-level var initialized at import
// time — and this package is now imported by every ekbn subcommand's binary,
// not just the orchestrator — so eager creation would leave a stray
// .kanban/orchestrator.log behind even for invocations (`ekbn serve`,
// `ekbn mcp`) that never call anything in this package.
type logger struct {
	path string
	once sync.Once
	w    io.Writer
}

func newLogger(path string) *logger {
	return &logger{path: path}
}

func (l *logger) ensureOpen() io.Writer {
	l.once.Do(func() {
		os.MkdirAll(filepath.Dir(l.path), 0755)
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			l.w = os.Stdout
			return
		}
		l.w = io.MultiWriter(f, os.Stdout)
	})
	return l.w
}

func (l *logger) log(level, format string, args ...any) {
	now := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.ensureOpen(), "%s  %-8s  %s\n", now, level, msg)
}
func (l *logger) info(format string, args ...any)  { l.log("INFO", format, args...) }
func (l *logger) warn(format string, args ...any)  { l.log("WARNING", format, args...) }
func (l *logger) error(format string, args ...any) { l.log("ERROR", format, args...) }

var log = newLogger(logFile)

func leaseOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

func indexOf(items []string, s string) int {
	for i, v := range items {
		if v == s {
			return i
		}
	}
	return -1
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func ticketPath(c model.Card) string {
	return filepath.Join(kanbanRoot, c.Column, c.Filename)
}

// findCard locates a card by its stable ID, wherever it currently lives.
// Needed after an agent run: declare_blocked (the one write path a read-only
// agent retains) can move the card out from under a ticket that's still
// mid-pipeline, and the column/filename captured at claim time no longer
// point at it once that happens.
func findCard(id string) (model.Card, bool) {
	cards, err := model.LoadAllCards(kanbanRoot)
	if err != nil {
		return model.Card{}, false
	}
	for _, c := range cards {
		if c.ID == id {
			return c, true
		}
	}
	return model.Card{}, false
}

func ensureKanbanDirs() {
	os.MkdirAll(kanbanRoot, 0755)
	for _, def := range model.DefaultColumns {
		os.MkdirAll(filepath.Join(kanbanRoot, fmt.Sprintf("%d-%s", def.Index, def.Slug)), 0755)
	}
}

// Run ensures the kanban directory tree exists, registers ekbn's MCP server as --read-only, then polls every pollInterval seconds — calling
// runCycle — until ctx is cancelled. Cancellation is only observed between
// cycles (at the top of the loop and during the inter-cycle sleep): an
// in-flight runCycle always finishes before Run returns.
func Run(ctx context.Context) error {
	ensureKanbanDirs()

	log.info("Orchestrator running — polling every %ds", pollInterval)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		cycleStart := time.Now()
		cfg := serve.LoadConfig()
		runCycle(cfg)

		elapsed := time.Since(cycleStart)
		remaining := pollInterval - int(elapsed.Seconds())
		if remaining > 0 {
			deadline := time.Now().Add(time.Duration(remaining) * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
			}
		}
	}
}
