package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDemo_LiveKanban(t *testing.T) {
	if ekbnBinary == "" {
		t.Skip("ekbn binary not built (run all tests or build first)")
	}

	kanbanDir := findKanbanDir(t)
	title := fmt.Sprintf("Demo Card %s", time.Now().Format("15:04:05"))
	fmt.Printf("\n── Demo: %s ──\n", title)
	fmt.Printf("  Board: %s\n\n", kanbanDir)

	cmd := exec.Command(ekbnBinary, "mcp", "--columns", kanbanDir)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})

	s := &demoSession{
		stdin: stdin,
		sc:    bufio.NewScanner(stdout),
	}

	doInitDemo(t, s)
	id := 1

	// ── Step 1: Create card in 100-todo ──
	fmt.Println("  ▶ Creating card in 100-todo …")
	id++
	sendDemo(t, s, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"method": "tools/call",
		"params": map[string]any{
			"name": "create_card",
			"arguments": map[string]any{
				"column":   "100-todo",
				"title":    title,
				"content":  "This card was created by an automated E2E demo.",
				"priority": float64(1),
			},
		},
	})
	fmt.Printf("  ✔ Card %q created in 100-todo\n", title)
	step(3)

	// ── Step 2: Find filename on disk (the serve watcher has already picked it up) ──
	filename := findMDInDir(t, filepath.Join(kanbanDir, "100-todo"), title)
	if filename == "" {
		t.Fatal("card file not found on disk after create")
	}
	fmt.Printf("  ✔ File: %s\n", filename)

	// ── Step 3: Move to 200-in-progress ──
	fmt.Println("  ▶ Moving to 200-in-progress …")
	id++
	sendDemo(t, s, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"method": "tools/call",
		"params": map[string]any{
			"name": "move_card",
			"arguments": map[string]any{
				"column":    "100-todo",
				"filename":  filename,
				"to_column": "200-in-progress",
			},
		},
	})
	fmt.Println("  ✔ Card moved to 200-in-progress")
	step(3)

	// ── Step 4: Delete card ──
	fmt.Println("  ▶ Deleting card …")
	id++
	sendDemo(t, s, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"method": "tools/call",
		"params": map[string]any{
			"name": "delete_card",
			"arguments": map[string]any{
				"column":   "200-in-progress",
				"filename": filename,
			},
		},
	})
	fmt.Println("  ✔ Card deleted")
	fmt.Print("\n── Demo complete ──\n\n")
}

// ─── Demo helpers ──────────────────────────────────────────────────────────────────

type demoSession struct {
	stdin io.WriteCloser
	sc    *bufio.Scanner
}

func doInitDemo(t *testing.T, s *demoSession) {
	t.Helper()
	_ = sendDemo(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 0,
		"method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"clientInfo": map[string]any{
				"name": "ekbn-demo", "version": "1.0.0",
			},
			"capabilities": map[string]any{},
		},
	})
	notif, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	fmt.Fprintf(s.stdin, "%s\n", notif)
}

func sendDemo(t *testing.T, s *demoSession, msg map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(msg)
	fmt.Fprintf(s.stdin, "%s\n", raw)

	done := make(chan map[string]any, 1)
	go func() {
		if s.sc.Scan() {
			var resp map[string]any
			json.Unmarshal([]byte(s.sc.Text()), &resp)
			done <- resp
		}
	}()

	select {
	case resp := <-done:
		if resp == nil {
			t.Fatalf("no response (scanner err: %v)", s.sc.Err())
		}
		if e, ok := resp["error"].(map[string]any); ok {
			t.Fatalf("JSON-RPC error: %v", e["message"])
		}
		return resp
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
		return nil
	}
}

func findKanbanDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, ".kanban")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find .kanban directory")
		}
		dir = parent
	}
}

func findMDInDir(t *testing.T, colDir, title string) string {
	t.Helper()
	entries, err := os.ReadDir(colDir)
	if err != nil {
		t.Fatalf("read dir %s: %v", colDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(colDir, e.Name()))
		if strings.Contains(string(data), title) {
			return e.Name()
		}
	}
	return ""
}

func step(seconds int) {
	for i := seconds; i > 0; i-- {
		fmt.Printf("\r  ⏳ %d …", i)
		time.Sleep(1 * time.Second)
	}
	fmt.Print("\r" + strings.Repeat(" ", 12) + "\r")
}
