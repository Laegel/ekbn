package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ekbn/internal/opencode"
)

type mcpConfig struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

type opencodeConfig struct {
	MCP map[string]mcpConfig `json:"mcp"`
}

const (
	pollInterval = 300
	maxRetries   = 4
	kanbanRoot   = ".kanban"
	logFile      = ".kanban/orchestrator.log"
	agentsMD     = "AGENTS.md"
	securityMD   = "SECURITY.md"
)

var folderPattern = regexp.MustCompile(`^\d{3}-(.+)$`)

var priorityOrder = map[string]int{
	"high":   0,
	"medium": 1,
	"low":    2,
}

type logger struct{ w io.Writer }

func newLogger(path string) *logger {
	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return &logger{w: os.Stdout}
	}
	return &logger{w: io.MultiWriter(f, os.Stdout)}
}

func (l *logger) log(level, format string, args ...any) {
	now := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.w, "%s  %-8s  %s\n", now, level, msg)
}
func (l *logger) info(format string, args ...any)  { l.log("INFO", format, args...) }
func (l *logger) warn(format string, args ...any)  { l.log("WARNING", format, args...) }
func (l *logger) error(format string, args ...any) { l.log("ERROR", format, args...) }

var log = newLogger(logFile)

func findKanbanDir(slug string) string {
	root := filepath.Join(".", kanbanRoot)
	ents, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		m := folderPattern.FindStringSubmatch(d.Name())
		if m != nil && m[1] == slug {
			return filepath.Join(root, d.Name())
		}
	}
	return ""
}

type meta map[string]any

func parseFrontmatter(path string) meta {
	m := make(meta)
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	re := regexp.MustCompile(`(?s)^---\n(.*?)\n---`)
	ma := re.FindSubmatch(data)
	if ma == nil {
		return m
	}
	for _, line := range strings.Split(string(ma[1]), "\n") {
		kv := regexp.MustCompile(`^(\w+):\s*(.+)`).FindStringSubmatch(strings.TrimSpace(line))
		if kv == nil {
			continue
		}
		key := kv[1]
		val := strings.TrimSpace(kv[2])
		switch strings.ToLower(val) {
		case "true":
			m[key] = true
		case "false":
			m[key] = false
		default:
			m[key] = val
		}
	}
	return m
}

func strVal(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "true"
		}
		return "false"
	}
	return fmt.Sprint(v)
}

func setFrontmatterField(path, field string, value any) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^(%s:).*$`, regexp.QuoteMeta(field)))
	replacement := fmt.Sprintf("$1 %s", strVal(value))
	if re.Match(content) {
		content = re.ReplaceAll(content, []byte(replacement))
	} else {
		content = regexp.MustCompile(`^(---\n)`).ReplaceAll(content, []byte(fmt.Sprintf("${1}%s: %s\n", field, strVal(value))))
	}
	os.WriteFile(path, content, 0644)
}

func appendSection(path, heading, body string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	section := fmt.Sprintf("\n## %s\n\n%s\n", heading, body)
	os.WriteFile(path, append(content, []byte(section)...), 0644)
}

func pickTicket() string {
	dir := findKanbanDir("todo")
	if dir == "" {
		log.error("Could not find todo folder in %s", kanbanRoot)
		return ""
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type ticket struct {
		path    string
		prio    int
		modtime time.Time
	}
	var tickets []ticket
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		meta := parseFrontmatter(p)
		prio := 99
		if pv, ok := meta["priority"].(string); ok {
			if v, exists := priorityOrder[pv]; exists {
				prio = v
			}
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		tickets = append(tickets, ticket{p, prio, info.ModTime()})
	}
	if len(tickets) == 0 {
		return ""
	}
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].prio != tickets[j].prio {
			return tickets[i].prio < tickets[j].prio
		}
		return tickets[i].modtime.Before(tickets[j].modtime)
	})
	return tickets[0].path
}

func hasActiveTickets() bool {
	for _, slug := range []string{"in-progress", "review"} {
		dir := findKanbanDir(slug)
		if dir == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".md") {
				return true
			}
		}
	}
	return false
}

func buildSystemPrompt(ticketPath string) string {
	var parts []string
	if data, err := os.ReadFile(agentsMD); err == nil {
		parts = append(parts, string(data))
	} else {
		log.warn("AGENTS.md not found \u2014 agent will run without base instructions")
	}
	meta := parseFrontmatter(ticketPath)
	if v, ok := meta["security"]; ok {
		if b, ok := v.(bool); ok && b {
			if data, err := os.ReadFile(securityMD); err == nil {
				parts = append(parts, "---\n\n"+string(data))
			} else {
				log.warn("security: true but SECURITY.md not found")
			}
		}
	}
	if data, err := os.ReadFile(ticketPath); err == nil {
		parts = append(parts, "---\n\n## Current ticket\n\n"+string(data))
	}
	return strings.Join(parts, "\n\n")
}

func ensureOpencode() {
	if err := opencode.EnsureInstalled(); err != nil {
		log.error("Failed to ensure opencode is installed: %v", err)
		os.Exit(1)
	}
	log.info("opencode is available")
}

func registerMCP() {
	log.info("Registering ekbn MCP server...")
	cfg := opencodeConfig{MCP: map[string]mcpConfig{
		"ekbn": {
			Type:    "local",
			Command: []string{"ekbn", "mcp"},
			Enabled: true,
		},
	}}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.warn("Failed to marshal MCP config: %v", err)
		return
	}
	if err := os.WriteFile("opencode.json", data, 0644); err != nil {
		log.warn("Failed to write MCP config: %v", err)
		return
	}
	log.info("ekbn MCP server configured in opencode.json")
}

type procPair struct {
	oc   *exec.Cmd
	ekbn *exec.Cmd
}

func startServe() (*procPair, error) {
	log.info("Starting opencode serve...")
	oc := exec.Command("opencode", "serve", "--port", "4096")
	oc.Stdout = os.Stdout
	oc.Stderr = os.Stderr
	if err := oc.Start(); err != nil {
		return nil, fmt.Errorf("opencode serve: %w", err)
	}
	time.Sleep(time.Second)
	if oc.ProcessState != nil && oc.ProcessState.Exited() {
		return nil, fmt.Errorf("opencode serve exited immediately")
	}
	log.info("opencode serve running (pid %d)", oc.Process.Pid)

	log.info("Starting ekbn serve...")
	ekbn := exec.Command("ekbn", "serve")
	ekbn.Stdout = os.Stdout
	ekbn.Stderr = os.Stderr
	if err := ekbn.Start(); err != nil {
		oc.Process.Kill()
		return nil, fmt.Errorf("ekbn serve: %w", err)
	}
	time.Sleep(time.Second)
	if ekbn.ProcessState != nil && ekbn.ProcessState.Exited() {
		log.warn("ekbn serve exited immediately \u2014 continuing without it")
	} else {
		log.info("ekbn serve running (pid %d)", ekbn.Process.Pid)
	}
	return &procPair{oc, ekbn}, nil
}

func runAgent(ticketPath string) bool {
	meta := parseFrontmatter(ticketPath)
	title := ticketTitle(meta, ticketPath)
	ticketID := ticketID(meta)

	ipDir := findKanbanDir("in-progress")
	if ipDir == "" {
		log.error("Could not find in-progress folder")
		return false
	}
	dest := filepath.Join(ipDir, filepath.Base(ticketPath))
	if err := os.Rename(ticketPath, dest); err != nil {
		log.error("Failed to move ticket: %v", err)
		return false
	}
	ticketPath = dest

	log.info("▶  Starting ticket #%s \u2014 %s", ticketID, title)
	prompt := buildSystemPrompt(ticketPath)

	tmpFile, err := os.CreateTemp("", "ekbn-prompt-*.md")
	if err != nil {
		log.error("Failed to create temp prompt file: %v", err)
		return false
	}
	tmpPath := tmpFile.Name()
	tmpFile.WriteString(prompt)
	tmpFile.Close()
	defer os.Remove(tmpPath)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.info("   Attempt %d/%d", attempt, maxRetries)
		cmd := exec.Command("opencode", "run", "--attach", "http://localhost:4096", "-f", tmpPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		err := cmd.Run()
		if err == nil {
			log.info("✓  Ticket #%s completed", ticketID)
			return true
		}
		log.warn("   Agent exited with code %d", exitCode(err))

		if fileExists(ticketPath) {
			meta := parseFrontmatter(ticketPath)
			if v, ok := meta["blocked"]; ok {
				if b, ok := v.(bool); ok && b {
					log.warn("✋  Ticket #%s is blocked \u2014 stopping retries", ticketID)
					appendSection(ticketPath, "Orchestrator",
						fmt.Sprintf("Agent flagged as blocked on attempt %d. Awaiting manual intervention.", attempt))
					return false
				}
			}
		}
		if attempt < maxRetries {
			log.info("   Retrying in 5s...")
			time.Sleep(5 * time.Second)
		}
	}

	log.error("✗  Ticket #%s failed after %d attempts", ticketID, maxRetries)
	if fileExists(ticketPath) {
		setFrontmatterField(ticketPath, "failed", true)
		appendSection(ticketPath, "Orchestrator",
			fmt.Sprintf("Agent failed after %d attempts. Last exit at %s.", maxRetries, time.Now().Format(time.RFC3339)))
	}
	return false
}

func ticketTitle(m meta, path string) string {
	if t, ok := m["title"].(string); ok && t != "" {
		return t
	}
	return strings.TrimSuffix(filepath.Base(path), ".md")
}

func ticketID(m meta) string {
	switch v := m["id"].(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	}
	return "???"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func killProc(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{}, 1)
		go func() {
			cmd.Wait()
			done <- struct{}{}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			cmd.Wait()
		}
	}
}

func main() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	shutdown := false

	os.MkdirAll(kanbanRoot, 0755)
	ensureOpencode()
	registerMCP()

	pair, err := startServe()
	if err != nil {
		log.error("Failed to start services: %v", err)
		os.Exit(1)
	}
	defer func() {
		log.info("Stopping services...")
		killProc(pair.oc)
		killProc(pair.ekbn)
		log.info("Orchestrator stopped")
	}()

	log.info("Orchestrator running \u2014 polling every %ds", pollInterval)
	for !shutdown {
		cycleStart := time.Now()

		select {
		case <-sigCh:
			log.info("Shutdown signal received \u2014 finishing current cycle")
			shutdown = true
		default:
		}

		if shutdown {
			break
		}

		if hasActiveTickets() {
			log.info("Active tickets found \u2014 skipping cycle")
		} else if ticket := pickTicket(); ticket != "" {
			runAgent(ticket)
		} else {
			log.info("No tickets in todo/")
		}

		elapsed := time.Since(cycleStart)
		remaining := pollInterval - int(elapsed.Seconds())
		if remaining > 0 {
			deadline := time.Now().Add(time.Duration(remaining) * time.Second)
			for time.Now().Before(deadline) && !shutdown {
				select {
				case <-sigCh:
					shutdown = true
				case <-time.After(time.Second):
				}
			}
		}
	}
}
