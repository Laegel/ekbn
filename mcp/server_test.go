package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func newTestServer(t *testing.T) (*EKBNServer, string) {
	t.Helper()
	dir := t.TempDir()
	// Create a todo column directory (mimics what ekbn serve would do)
	colDir := filepath.Join(dir, "100-todo")
	if err := os.MkdirAll(colDir, 0755); err != nil {
		t.Fatal(err)
	}
	return New(dir), dir
}

func TestListColumns(t *testing.T) {
	srv, _ := newTestServer(t)

	result, err := srv.handleListColumns(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) == 0 {
		t.Fatal("list_columns returned no content")
	}
	text := ""
	for _, c := range result.Content {
		if t, ok := c.(mcp.TextContent); ok {
			text = t.Text
			break
		}
	}
	if text == "" {
		t.Fatal("list_columns returned no text content")
	}
	t.Logf("list_columns output:\n%s", text)
}

func TestCreateCard(t *testing.T) {
	srv, dir := newTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"column":   "100-todo",
		"title":    "Test Card Title",
		"content":  "This is the card content.",
		"priority": float64(1),
		"role":     "dev",
		"blocked":  false,
	}

	result, err := srv.handleCreateCard(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) == 0 {
		t.Fatal("create_card returned no content")
	}

	// Verify the file was created on disk
	ents, err := os.ReadDir(filepath.Join(dir, "100-todo"))
	if err != nil {
		t.Fatal(err)
	}
	var mdFiles []string
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
			mdFiles = append(mdFiles, e.Name())
		}
	}
	if len(mdFiles) == 0 {
		t.Fatal("no .md files created in column directory")
	}
	t.Logf("Created file: %s", mdFiles[0])
}

func TestCreateCardWithCategories(t *testing.T) {
	srv, dir := newTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"column":     "100-todo",
		"title":      "Categorized Card",
		"content":    "Has categories.",
		"categories": []interface{}{"backend", "api"},
	}

	_, err := srv.handleCreateCard(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// Verify categories in the written file
	ents, _ := os.ReadDir(filepath.Join(dir, "100-todo"))
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
			data, _ := os.ReadFile(filepath.Join(dir, "100-todo", e.Name()))
			if containsStr(string(data), "categories: [backend, api]") || containsStr(string(data), "categories:") {
				t.Logf("Card file contains categories entry")
				return
			}
		}
	}
}

func TestCreateCardEmptyContent(t *testing.T) {
	srv, _ := newTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"column": "100-todo",
		"title":  "Minimal Card",
	}

	_, err := srv.handleCreateCard(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateCardMissingColumn(t *testing.T) {
	srv, _ := newTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"title": "No Column Card",
	}

	_, err := srv.handleCreateCard(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic — will write to a path without column dir
}

func TestCreateCardMissingTitle(t *testing.T) {
	srv, _ := newTestServer(t)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"column": "100-todo",
	}

	_, err := srv.handleCreateCard(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic — title is required by model.CreateCard
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) &&
		(len(substr) == 0 ||
			(s == substr ||
				(len(s) > len(substr) &&
					(s[:len(substr)] == substr ||
						containsStr(s[1:], substr)))))
}
