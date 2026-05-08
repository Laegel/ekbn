package model

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCard(t *testing.T, dir, filename, content string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadCard(t *testing.T) {
	dir := t.TempDir()

	t.Run("frontmatter and content", func(t *testing.T) {
		path := writeCard(t, dir, "card.md",
			"---\ntitle: My Card\nauthor: alice\npriority: 1\nrole: dev\ncategories: [go, testing]\n---\n\nThis is the content.\n\nMore content here.\n")
		card, err := ReadCard(path, "100-todo")
		if err != nil {
			t.Fatal(err)
		}
		if card.Title != "My Card" {
			t.Errorf("Title = %q, want %q", card.Title, "My Card")
		}
		if card.Author != "alice" {
			t.Errorf("Author = %q, want %q", card.Author, "alice")
		}
		if card.Priority != 1 {
			t.Errorf("Priority = %d, want %d", card.Priority, 1)
		}
		if card.Role != "dev" {
			t.Errorf("Role = %q, want %q", card.Role, "dev")
		}
		if len(card.Categories) != 2 || card.Categories[0] != "go" || card.Categories[1] != "testing" {
			t.Errorf("Categories = %v, want [go testing]", card.Categories)
		}
		wantContent := "This is the content.\n\nMore content here."
		if card.Content != wantContent {
			t.Errorf("Content = %q, want %q", card.Content, wantContent)
		}
		if len(card.Comments) != 0 {
			t.Errorf("Comments = %v, want empty", card.Comments)
		}
		if card.Filename != "card.md" {
			t.Errorf("Filename = %q, want %q", card.Filename, "card.md")
		}
		if card.Column != "100-todo" {
			t.Errorf("Column = %q, want %q", card.Column, "100-todo")
		}
	})

	t.Run("frontmatter only", func(t *testing.T) {
		path := writeCard(t, dir, "card.md", "---\ntitle: Only Frontmatter\n---\n")
		card, err := ReadCard(path, "200-progress")
		if err != nil {
			t.Fatal(err)
		}
		if card.Title != "Only Frontmatter" {
			t.Errorf("Title = %q, want %q", card.Title, "Only Frontmatter")
		}
		if card.Content != "" {
			t.Errorf("Content = %q, want empty", card.Content)
		}
	})

	t.Run("frontmatter with content and comments in frontmatter", func(t *testing.T) {
		path := writeCard(t, dir, "card.md",
			"---\ntitle: Has Comments\nauthor: bob\ncomments:\n  - author: alice\n    created: 2024-01-01T12:00:00Z\n    text: First comment\n  - author: bob\n    created: 2024-01-02T13:00:00Z\n    text: Second comment\n---\n\nContent body.\n")
		card, err := ReadCard(path, "100-todo")
		if err != nil {
			t.Fatal(err)
		}
		if card.Title != "Has Comments" {
			t.Errorf("Title = %q, want %q", card.Title, "Has Comments")
		}
		if card.Content != "Content body." {
			t.Errorf("Content = %q, want %q", card.Content, "Content body.")
		}
		if len(card.Comments) != 2 {
			t.Fatalf("len(Comments) = %d, want 2", len(card.Comments))
		}
		if card.Comments[0].Author != "alice" || card.Comments[0].Text != "First comment" || card.Comments[0].Created != "2024-01-01T12:00:00Z" {
			t.Errorf("Comments[0] = %+v, want author=alice text=First comment", card.Comments[0])
		}
		if card.Comments[1].Author != "bob" || card.Comments[1].Text != "Second comment" {
			t.Errorf("Comments[1] = %+v, want author=bob text=Second comment", card.Comments[1])
		}
	})

	t.Run("content with triple dashes", func(t *testing.T) {
		path := writeCard(t, dir, "card.md",
			"---\ntitle: Dashes in Content\n---\n\nSome text\n\n---\n\nMore text after dashes.\n")
		card, err := ReadCard(path, "100-todo")
		if err != nil {
			t.Fatal(err)
		}
		want := "Some text\n\n---\n\nMore text after dashes."
		if card.Content != want {
			t.Errorf("Content = %q, want %q", card.Content, want)
		}
	})

	t.Run("no frontmatter", func(t *testing.T) {
		path := writeCard(t, dir, "card.md", "Just raw content with no frontmatter.")
		card, err := ReadCard(path, "300-done")
		if err != nil {
			t.Fatal(err)
		}
		if card.Title != "" {
			t.Errorf("Title = %q, want empty", card.Title)
		}
		if card.Content != "" {
			t.Errorf("Content = %q, want empty", card.Content)
		}
	})

	t.Run("malformed yaml in frontmatter returns error", func(t *testing.T) {
		path := writeCard(t, dir, "card.md", "---\ntitle: Broken\n: invalid yaml\n---\n\nBody.\n")
		_, err := ReadCard(path, "100-todo")
		if err == nil {
			t.Fatal("expected error for malformed YAML")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := writeCard(t, dir, "card.md", "")
		card, err := ReadCard(path, "100-todo")
		if err != nil {
			t.Fatal(err)
		}
		if card.Title != "" {
			t.Errorf("Title = %q, want empty", card.Title)
		}
	})

	t.Run("blocked in frontmatter", func(t *testing.T) {
		path := writeCard(t, dir, "card.md", "---\ntitle: Blocked Card\nblocked: true\n---\n\nStuff.\n")
		card, err := ReadCard(path, "100-todo")
		if err != nil {
			t.Fatal(err)
		}
		if !card.Blocked {
			t.Errorf("Blocked = false, want true")
		}
	})

	t.Run("round-trip: create, add comment, update preserves everything", func(t *testing.T) {
		baseDir := t.TempDir()
		colDir := filepath.Join(baseDir, "100-todo")
		if err := os.MkdirAll(colDir, 0755); err != nil {
			t.Fatal(err)
		}

		filename, err := CreateCard(baseDir, "100-todo", "Round Trip", "test", "Hello world.", []string{"test"}, 2, "qa", true)
		if err != nil {
			t.Fatal(err)
		}

		err = AddComment(baseDir, "100-todo", filename, "alice", "A comment")
		if err != nil {
			t.Fatal(err)
		}

		card, err := ReadCard(filepath.Join(colDir, filename), "100-todo")
		if err != nil {
			t.Fatal(err)
		}

		if card.Title != "Round Trip" {
			t.Errorf("Title = %q, want %q", card.Title, "Round Trip")
		}
		if card.Content != "Hello world." {
			t.Errorf("Content = %q, want %q", card.Content, "Hello world.")
		}
		if card.Priority != 2 {
			t.Errorf("Priority = %d, want %d", card.Priority, 2)
		}
		if card.Role != "qa" {
			t.Errorf("Role = %q, want %q", card.Role, "qa")
		}
		if !card.Blocked {
			t.Errorf("Blocked = false, want true")
		}
		if len(card.Comments) != 1 {
			t.Fatalf("len(Comments) = %d, want 1", len(card.Comments))
		}
		if card.Comments[0].Author != "alice" || card.Comments[0].Text != "A comment" {
			t.Errorf("Comments[0] = %+v, want author=alice text='A comment'", card.Comments[0])
		}

		err = UpdateCard(baseDir, "100-todo", filename, "Round Trip Updated", "Updated content.", []string{"test", "updated"}, 1, "dev", false)
		if err != nil {
			t.Fatal(err)
		}

		card, err = ReadCard(filepath.Join(colDir, filename), "100-todo")
		if err != nil {
			t.Fatal(err)
		}

		if card.Title != "Round Trip Updated" {
			t.Errorf("After UpdateCard: Title = %q, want %q", card.Title, "Round Trip Updated")
		}
		if card.Content != "Updated content." {
			t.Errorf("After UpdateCard: Content = %q, want %q", card.Content, "Updated content.")
		}
		if card.Priority != 1 {
			t.Errorf("After UpdateCard: Priority = %d, want %d", card.Priority, 1)
		}
		if card.Role != "dev" {
			t.Errorf("After UpdateCard: Role = %q, want %q", card.Role, "dev")
		}
		if card.Blocked {
			t.Errorf("After UpdateCard: Blocked = true, want false")
		}
		if len(card.Comments) != 1 {
			t.Fatalf("After UpdateCard: len(Comments) = %d, want 1 (comments preserved)", len(card.Comments))
		}
		if card.Comments[0].Text != "A comment" {
			t.Errorf("After UpdateCard: Comments[0].Text = %q, want %q", card.Comments[0].Text, "A comment")
		}
	})
}
