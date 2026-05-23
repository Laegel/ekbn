package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFrontmatter(t *testing.T) {
	t.Run("full frontmatter", func(t *testing.T) {
		path := writeFile(t, "card.md", "---\ntitle: My Card\nauthor: alice\npriority: high\nblocked: false\nsecurity: true\n---\n\nContent here.\n")
		m := parseFrontmatter(path)
		if m["title"] != "My Card" {
			t.Errorf("title = %q, want %q", m["title"], "My Card")
		}
		if m["author"] != "alice" {
			t.Errorf("author = %q, want %q", m["author"], "alice")
		}
		if m["priority"] != "high" {
			t.Errorf("priority = %q, want %q", m["priority"], "high")
		}
		if m["blocked"] != false {
			t.Errorf("blocked = %v, want false", m["blocked"])
		}
		if m["security"] != true {
			t.Errorf("security = %v, want true", m["security"])
		}
	})

	t.Run("no frontmatter", func(t *testing.T) {
		path := writeFile(t, "plain.md", "Just content, no frontmatter.")
		m := parseFrontmatter(path)
		if len(m) != 0 {
			t.Errorf("expected empty meta, got %v", m)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := writeFile(t, "empty.md", "")
		m := parseFrontmatter(path)
		if len(m) != 0 {
			t.Errorf("expected empty meta for empty file, got %v", m)
		}
	})

	t.Run("boolean values", func(t *testing.T) {
		path := writeFile(t, "bool.md", "---\nactive: true\nfinished: false\n---\n")
		m := parseFrontmatter(path)
		if m["active"] != true {
			t.Errorf("active = %v, want true", m["active"])
		}
		if m["finished"] != false {
			t.Errorf("finished = %v, want false", m["finished"])
		}
	})
}

func TestSetFrontmatterField(t *testing.T) {
	dir := t.TempDir()

	t.Run("update existing field", func(t *testing.T) {
		path := filepath.Join(dir, "update.md")
		os.WriteFile(path, []byte("---\ntitle: Old\npriority: low\n---\nBody.\n"), 0644)
		setFrontmatterField(path, "priority", "high")
		m := parseFrontmatter(path)
		if m["priority"] != "high" {
			t.Errorf("priority = %q, want %q", m["priority"], "high")
		}
	})

	t.Run("add new field", func(t *testing.T) {
		path := filepath.Join(dir, "add.md")
		os.WriteFile(path, []byte("---\ntitle: Test\n---\nBody.\n"), 0644)
		setFrontmatterField(path, "role", "dev")
		m := parseFrontmatter(path)
		if m["role"] != "dev" {
			t.Errorf("role = %q, want %q", m["role"], "dev")
		}
	})

	t.Run("set boolean field", func(t *testing.T) {
		path := filepath.Join(dir, "bool.md")
		os.WriteFile(path, []byte("---\ntitle: Test\n---\nBody.\n"), 0644)
		setFrontmatterField(path, "blocked", true)
		m := parseFrontmatter(path)
		if m["blocked"] != true {
			t.Errorf("blocked = %v, want true", m["blocked"])
		}
	})
}

func TestAppendSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "card.md")
	os.WriteFile(path, []byte("---\ntitle: Test\n---\n\nBody.\n"), 0644)

	appendSection(path, "Results", "The agent completed the task.")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !containsStr(content, "## Results") {
		t.Error("appended section missing heading")
	}
	if !containsStr(content, "The agent completed the task.") {
		t.Error("appended section missing body text")
	}
}

func TestFindKanbanDir(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	os.MkdirAll(".kanban", 0755)
	os.MkdirAll(".kanban/100-todo", 0755)
	os.MkdirAll(".kanban/200-in-progress", 0755)

	t.Run("found", func(t *testing.T) {
		got := findKanbanDir("todo")
		if got == "" {
			t.Fatal("findKanbanDir('todo') returned empty")
		}
		if !containsStr(got, "100-todo") {
			t.Errorf("findKanbanDir('todo') = %q, want path containing '100-todo'", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		got := findKanbanDir("nonexistent")
		if got != "" {
			t.Errorf("findKanbanDir('nonexistent') = %q, want empty", got)
		}
	})

	t.Run("no root", func(t *testing.T) {
		os.RemoveAll(".kanban")
		got := findKanbanDir("todo")
		if got != "" {
			t.Errorf("findKanbanDir('todo') without root = %q, want empty", got)
		}
	})
}

func TestHasActiveTickets(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	os.MkdirAll(".kanban/100-todo", 0755)

	t.Run("no tickets", func(t *testing.T) {
		if hasActiveTickets() {
			t.Error("hasActiveTickets() = true, want false (no .md files)")
		}
	})

	t.Run("ticket in in-progress", func(t *testing.T) {
		os.MkdirAll(".kanban/200-in-progress", 0755)
		os.WriteFile(".kanban/200-in-progress/test.md", []byte("---\ntitle: Active\n---\n"), 0644)
		if !hasActiveTickets() {
			t.Error("hasActiveTickets() = false, want true (ticket in in-progress)")
		}
		os.RemoveAll(".kanban/200-in-progress")
	})

	t.Run("ticket in review", func(t *testing.T) {
		os.MkdirAll(".kanban/300-review", 0755)
		os.WriteFile(".kanban/300-review/test.md", []byte("---\ntitle: Review\n---\n"), 0644)
		if !hasActiveTickets() {
			t.Error("hasActiveTickets() = false, want true (ticket in review)")
		}
	})
}

func TestPickTicket(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	os.MkdirAll(".kanban/100-todo", 0755)

	t.Run("no tickets", func(t *testing.T) {
		if ticket := pickTicket(); ticket != "" {
			t.Errorf("pickTicket() = %q, want empty", ticket)
		}
	})

	t.Run("picks by priority", func(t *testing.T) {
		mustWrite(t, ".kanban/100-todo/low.md", "---\ntitle: Low\npriority: low\n---\n")
		time.Sleep(10 * time.Millisecond)
		mustWrite(t, ".kanban/100-todo/high.md", "---\ntitle: High\npriority: high\n---\n")
		time.Sleep(10 * time.Millisecond)
		mustWrite(t, ".kanban/100-todo/medium.md", "---\ntitle: Medium\npriority: medium\n---\n")

		ticket := pickTicket()
		if ticket == "" {
			t.Fatal("pickTicket() returned empty with tickets present")
		}
		m := parseFrontmatter(ticket)
		if m["priority"] != "high" {
			t.Errorf("pickTicket() picked priority=%q, want 'high'", m["priority"])
		}
	})
}

func mustWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTicketTitle(t *testing.T) {
	t.Run("from frontmatter", func(t *testing.T) {
		m := meta{"title": "My Title"}
		got := ticketTitle(m, "/path/to/card.md")
		if got != "My Title" {
			t.Errorf("ticketTitle = %q, want %q", got, "My Title")
		}
	})

	t.Run("fallback to filename", func(t *testing.T) {
		m := meta{}
		got := ticketTitle(m, "/path/to/my-card.md")
		if got != "my-card" {
			t.Errorf("ticketTitle = %q, want %q", got, "my-card")
		}
	})
}

func TestTicketID(t *testing.T) {
	t.Run("string id", func(t *testing.T) {
		m := meta{"id": "42"}
		if got := ticketID(m); got != "42" {
			t.Errorf("ticketID = %q, want %q", got, "42")
		}
	})

	t.Run("numeric id", func(t *testing.T) {
		m := meta{"id": float64(42)}
		if got := ticketID(m); got != "42" {
			t.Errorf("ticketID = %q, want %q", got, "42")
		}
	})

	t.Run("missing id", func(t *testing.T) {
		m := meta{}
		if got := ticketID(m); got != "???" {
			t.Errorf("ticketID = %q, want %q", got, "???")
		}
	})
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.md")
	os.WriteFile(path, []byte("test"), 0644)

	if !fileExists(path) {
		t.Error("fileExists() = false for existing file, want true")
	}
	if fileExists(filepath.Join(dir, "nope.md")) {
		t.Error("fileExists() = true for missing file, want false")
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) &&
		(len(substr) == 0 ||
			(s == substr ||
				(len(s) > len(substr) &&
					(s[:len(substr)] == substr ||
						containsStr(s[1:], substr)))))
}
