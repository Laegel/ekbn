package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// newInProcessClient creates an in-process MCP client connected to a fresh EKBNServer
// with a temp directory and a 100-todo column. Returns the client, temp dir, and a
// cleanup function. The client is already started and initialized.
func newInProcessClient(t *testing.T) (*client.Client, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "100-todo"), 0755); err != nil {
		t.Fatal(err)
	}
	c, cancel := newInProcessClientForDir(t, dir, false)
	return c, dir, cancel
}

// newInProcessClientReadOnly is like newInProcessClient but the server is
// started in read-only mode.
func newInProcessClientReadOnly(t *testing.T) (*client.Client, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "100-todo"), 0755); err != nil {
		t.Fatal(err)
	}
	c, cancel := newInProcessClientForDir(t, dir, true)
	return c, dir, cancel
}

// newInProcessClientForDir connects a client to an EKBNServer rooted at an
// existing columns directory, e.g. one already populated by another client —
// useful for testing a read-only client against a board a write client set up.
func newInProcessClientForDir(t *testing.T, dir string, readOnly bool) (*client.Client, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	srv := New(dir, readOnly)
	c, err := client.NewInProcessClient(srv.MCPServer())
	if err != nil {
		cancel()
		t.Fatalf("NewInProcessClient: %v", err)
	}

	if err := c.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "ekbn-test",
		Version: "1.0.0",
	}

	if _, err := c.Initialize(ctx, initReq); err != nil {
		cancel()
		t.Fatalf("Initialize: %v", err)
	}

	return c, cancel
}

func TestInProcess_InitializeAndPing(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	if err := c.Ping(context.Background()); err != nil {
		t.Fatal("Ping failed:", err)
	}

	capabilities := c.GetServerCapabilities()
	if capabilities.Tools == nil {
		t.Fatal("expected server to declare tool capabilities")
	}
	if capabilities.Resources == nil {
		t.Fatal("expected server to declare resource capabilities")
	}
}

func TestInProcess_ListTools(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	result, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal("ListTools failed:", err)
	}

	expected := []string{
		"list_columns", "create_column", "create_card", "get_card",
		"update_card", "delete_card", "move_card", "add_comment",
		"reorder_column", "declare_blocked",
	}
	if len(result.Tools) != len(expected) {
		t.Fatalf("expected %d tools, got %d: %+v", len(expected), len(result.Tools), toolNames(result.Tools))
	}

	names := make(map[string]bool)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, name := range expected {
		if !names[name] {
			t.Fatalf("missing tool %q", name)
		}
	}
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

func TestInProcess_ListColumns(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.CallToolRequest{}
	req.Params.Name = "list_columns"

	result, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatal("list_columns failed:", err)
	}
	if result.IsError {
		for _, content := range result.Content {
			if text, ok := content.(mcp.TextContent); ok {
				t.Fatalf("list_columns returned error: %s", text.Text)
			}
		}
		t.Fatal("list_columns returned isError=true")
	}
	if len(result.Content) == 0 {
		t.Fatal("list_columns returned no content")
	}

	var text string
	for _, content := range result.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if text == "" {
		t.Fatal("list_columns returned no text content")
	}
	if !strings.Contains(text, "100-todo") {
		t.Fatalf("list_columns output should contain '100-todo': %s", text)
	}
}

func TestInProcess_CreateColumn(t *testing.T) {
	c, dir, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.CallToolRequest{}
	req.Params.Name = "create_column"
	req.Params.Arguments = map[string]any{
		"slug": "blocked",
		"name": "Blocked",
	}

	result, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatal("create_column failed:", err)
	}
	if result.IsError {
		for _, entry := range result.Content {
			if tc, ok := entry.(mcp.TextContent); ok {
				t.Fatalf("create_column returned error: %s", tc.Text)
			}
		}
		t.Fatal("create_column returned isError=true")
	}

	// Verify directory was created on disk
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), "blocked") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected blocked column directory to exist on disk")
	}
}

func TestInProcess_CreateColumnRejectsInvalidStatus(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.CallToolRequest{}
	req.Params.Name = "create_column"
	req.Params.Arguments = map[string]any{
		"slug": "backlog",
		"name": "Backlog",
	}

	result, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatal("create_column failed:", err)
	}
	if !result.IsError {
		t.Fatal("expected create_column to reject a slug that isn't a real status")
	}
}

func TestInProcess_FullCardLifecycle(t *testing.T) {
	c, dir, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	ctx := context.Background()

	// Step 1: Create a card in 100-todo
	createReq := mcp.CallToolRequest{}
	createReq.Params.Name = "create_card"
	createReq.Params.Arguments = map[string]any{
		"column":   "100-todo",
		"title":    "Test Card",
		"content":  "Hello from integration test",
		"priority": float64(3),
		"role":     "dev",
	}

	createResult, err := c.CallTool(ctx, createReq)
	if err != nil {
		t.Fatal("create_card failed:", err)
	}
	if createResult.IsError {
		t.Fatal("create_card returned isError=true")
	}

	// Find the created filename by reading the directory
	entries, _ := os.ReadDir(filepath.Join(dir, "100-todo"))
	var filename string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
			filename = e.Name()
			break
		}
	}
	if filename == "" {
		t.Fatal("no .md file created")
	}

	// Step 2: Get the card
	getReq := mcp.CallToolRequest{}
	getReq.Params.Name = "get_card"
	getReq.Params.Arguments = map[string]any{
		"column":   "100-todo",
		"filename": filename,
	}

	getResult, err := c.CallTool(ctx, getReq)
	if err != nil {
		t.Fatal("get_card failed:", err)
	}
	if getResult.IsError {
		t.Fatal("get_card returned isError=true")
	}

	// Step 3: Update the card
	updateReq := mcp.CallToolRequest{}
	updateReq.Params.Name = "update_card"
	updateReq.Params.Arguments = map[string]any{
		"column":     "100-todo",
		"filename":   filename,
		"title":      "Updated Card",
		"priority":   float64(5),
		"goal":       "bug",
		"acceptance": "npm test",
	}

	updateResult, err := c.CallTool(ctx, updateReq)
	if err != nil {
		t.Fatal("update_card failed:", err)
	}
	if updateResult.IsError {
		t.Fatal("update_card returned isError=true")
	}

	// Verify update via get
	getReq2 := mcp.CallToolRequest{}
	getReq2.Params.Name = "get_card"
	getReq2.Params.Arguments = map[string]any{
		"column":   "100-todo",
		"filename": filename,
	}
	getResult2, err := c.CallTool(ctx, getReq2)
	if err != nil {
		t.Fatal("get_card (after update) failed:", err)
	}
	if getResult2.IsError {
		t.Fatal("get_card (after update) returned isError=true")
	}

	// Step 4: Move the card to new column
	if err := os.MkdirAll(filepath.Join(dir, "200-in-progress"), 0755); err != nil {
		t.Fatal(err)
	}

	moveReq := mcp.CallToolRequest{}
	moveReq.Params.Name = "move_card"
	moveReq.Params.Arguments = map[string]any{
		"column":    "100-todo",
		"filename":  filename,
		"to_column": "200-in-progress",
	}

	moveResult, err := c.CallTool(ctx, moveReq)
	if err != nil {
		t.Fatal("move_card failed:", err)
	}
	if moveResult.IsError {
		t.Fatal("move_card returned isError=true")
	}

	// Verify card moved
	if _, err := os.Stat(filepath.Join(dir, "100-todo", filename)); err == nil {
		t.Fatal("card should not exist in source column after move")
	}
	if _, err := os.Stat(filepath.Join(dir, "200-in-progress", filename)); err != nil {
		t.Fatal("card should exist in destination column after move")
	}

	// Step 5: Add a comment
	commentReq := mcp.CallToolRequest{}
	commentReq.Params.Name = "add_comment"
	commentReq.Params.Arguments = map[string]any{
		"column":   "200-in-progress",
		"filename": filename,
		"text":     "Reviewed and approved",
	}

	commentResult, err := c.CallTool(ctx, commentReq)
	if err != nil {
		t.Fatal("add_comment failed:", err)
	}
	if commentResult.IsError {
		t.Fatal("add_comment returned isError=true")
	}

	// Step 6: Delete the card
	deleteReq := mcp.CallToolRequest{}
	deleteReq.Params.Name = "delete_card"
	deleteReq.Params.Arguments = map[string]any{
		"column":   "200-in-progress",
		"filename": filename,
	}

	deleteResult, err := c.CallTool(ctx, deleteReq)
	if err != nil {
		t.Fatal("delete_card failed:", err)
	}
	if deleteResult.IsError {
		t.Fatal("delete_card returned isError=true")
	}

	// Verify deletion
	if _, err := os.Stat(filepath.Join(dir, "200-in-progress", filename)); err == nil {
		t.Fatal("card should not exist after delete")
	}
}

func TestInProcess_ReorderColumn(t *testing.T) {
	c, dir, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	ctx := context.Background()

	// Create additional columns. Slugs must be real statuses now — columns
	// are the view of the status enum, not an arbitrary namespace.
	for _, col := range []struct{ slug, name string }{
		{"blocked", "Blocked"},
		{"budget-exhausted", "Budget Exhausted"},
	} {
		req := mcp.CallToolRequest{}
		req.Params.Name = "create_column"
		req.Params.Arguments = map[string]any{
			"slug": col.slug,
			"name": col.name,
		}
		if _, err := c.CallTool(ctx, req); err != nil {
			t.Fatalf("create_column %s failed: %v", col.slug, err)
		}
	}

	// Reorder the blocked column before 100-todo
	reorderReq := mcp.CallToolRequest{}
	reorderReq.Params.Name = "reorder_column"
	reorderReq.Params.Arguments = map[string]any{
		"slug":        "blocked",
		"before_slug": "todo",
	}

	result, err := c.CallTool(ctx, reorderReq)
	if err != nil {
		t.Fatal("reorder_column failed:", err)
	}
	if result.IsError {
		t.Fatal("reorder_column returned isError=true")
	}

	// Verify directory was renamed
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), "blocked") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected blocked column directory to exist after reorder")
	}
}

func TestInProcess_ListResources(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	result, err := c.ListResources(context.Background(), mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatal("ListResources failed:", err)
	}
	if len(result.Resources) == 0 {
		t.Fatal("expected at least one resource")
	}

	var foundColumns bool
	for _, r := range result.Resources {
		if r.URI == "ekbn://columns" {
			foundColumns = true
			break
		}
	}
	if !foundColumns {
		t.Fatal("expected resource ekbn://columns")
	}
}

func TestInProcess_ListResourceTemplates(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	result, err := c.ListResourceTemplates(context.Background(), mcp.ListResourceTemplatesRequest{})
	if err != nil {
		t.Fatal("ListResourceTemplates failed:", err)
	}
	if len(result.ResourceTemplates) == 0 {
		t.Fatal("expected at least one resource template")
	}

	uris := make([]string, len(result.ResourceTemplates))
	for i, t := range result.ResourceTemplates {
		uris[i] = t.URITemplate.Raw()
	}

	expectedURIs := []string{
		"ekbn://column/{slug}",
		"ekbn://card/{column}/{filename}",
	}
	for _, exp := range expectedURIs {
		var found bool
		for _, u := range uris {
			if u == exp {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected template URI %q in %v", exp, uris)
		}
	}
}

func TestInProcess_ReadResource_Columns(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.ReadResourceRequest{}
	req.Params.URI = "ekbn://columns"

	result, err := c.ReadResource(context.Background(), req)
	if err != nil {
		t.Fatal("ReadResource ekbn://columns failed:", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected at least one content entry")
	}

	textContent, ok := result.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatal("expected TextResourceContents")
	}
	if textContent.URI != "ekbn://columns" {
		t.Fatalf("expected URI ekbn://columns, got %s", textContent.URI)
	}
	if !strings.Contains(textContent.Text, "100-todo") {
		t.Fatalf("resource content should contain '100-todo': %s", textContent.Text)
	}
}

func TestInProcess_ReadResource_ColumnTemplate(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.ReadResourceRequest{}
	req.Params.URI = "ekbn://column/100-todo"

	result, err := c.ReadResource(context.Background(), req)
	if err != nil {
		t.Fatal("ReadResource ekbn://column/100-todo failed:", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected content for column resource")
	}

	textContent, ok := result.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatal("expected TextResourceContents")
	}
	if !strings.Contains(textContent.Text, "100-todo") {
		t.Fatalf("column resource should contain '100-todo': %s", textContent.Text)
	}
}

func TestInProcess_ReadResource_CardTemplate(t *testing.T) {
	c, dir, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	// First create a card
	cardPath := filepath.Join(dir, "100-todo", "test-card.md")
	if err := os.WriteFile(cardPath, []byte("---\ntitle: Test Resource Card\n---\n\nHello"), 0644); err != nil {
		t.Fatal(err)
	}

	req := mcp.ReadResourceRequest{}
	req.Params.URI = "ekbn://card/100-todo/test-card.md"

	result, err := c.ReadResource(context.Background(), req)
	if err != nil {
		t.Fatal("ReadResource ekbn://card/100-todo/test-card.md failed:", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected content for card resource")
	}

	textContent, ok := result.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatal("expected TextResourceContents")
	}
	if !strings.Contains(textContent.Text, "Test Resource Card") {
		t.Fatalf("card resource should contain title: %s", textContent.Text)
	}
}

func TestInProcess_ReadResource_NotFound(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.ReadResourceRequest{}
	req.Params.URI = "ekbn://nonexistent"

	_, err := c.ReadResource(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-existent resource")
	}
}

func TestInProcess_CallTool_NotFound(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	req := mcp.CallToolRequest{}
	req.Params.Name = "nonexistent_tool"

	_, err := c.CallTool(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-existent tool")
	}
}

func TestInProcess_StructuredContent(t *testing.T) {
	c, _, cancel := newInProcessClient(t)
	defer cancel()
	defer c.Close()

	createReq := mcp.CallToolRequest{}
	createReq.Params.Name = "create_column"
	createReq.Params.Arguments = map[string]any{
		"slug": "blocked",
		"name": "Test Structure",
	}

	result, err := c.CallTool(context.Background(), createReq)
	if err != nil {
		t.Fatal("create_column failed:", err)
	}
	if result.IsError {
		for _, entry := range result.Content {
			if tc, ok := entry.(mcp.TextContent); ok {
				t.Fatalf("create_column returned error: %s", tc.Text)
			}
		}
		t.Fatal("create_column returned isError=true")
	}

	// create_column returns structured content via mcp.NewToolResultStructured
	if result.StructuredContent == nil {
		t.Fatal("expected structured content from create_column")
	}
	data, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result.StructuredContent)
	}
	if data["slug"] != "blocked" {
		t.Fatalf("expected slug 'blocked', got %v", data["slug"])
	}
	if data["name"] != "Test Structure" {
		t.Fatalf("expected name 'Test Structure', got %v", data["name"])
	}
}

func TestInProcess_ReadOnly_CannotMoveCard(t *testing.T) {
	// Create the card via a write-mode client, since the read-only client
	// under test must not be able to do this itself.
	writeClient, dir, writeCancel := newInProcessClient(t)
	defer writeCancel()
	defer writeClient.Close()

	if err := os.MkdirAll(filepath.Join(dir, "200-in-progress"), 0755); err != nil {
		t.Fatal(err)
	}

	createReq := mcp.CallToolRequest{}
	createReq.Params.Name = "create_card"
	createReq.Params.Arguments = map[string]any{
		"column":  "100-todo",
		"title":   "Implement feature",
		"content": "Do the thing.",
	}
	createResult, err := writeClient.CallTool(context.Background(), createReq)
	if err != nil {
		t.Fatal("create_card failed:", err)
	}
	if createResult.IsError {
		t.Fatal("create_card returned isError=true")
	}

	// Connect a second, read-only client to the same board the card was
	// just created on.
	roClient, roCancel := newInProcessClientForDir(t, dir, true)
	defer roCancel()
	defer roClient.Close()

	moveReq := mcp.CallToolRequest{}
	moveReq.Params.Name = "move_card"
	moveReq.Params.Arguments = map[string]any{
		"column":    "100-todo",
		"filename":  "implement-feature.md",
		"to_column": "200-in-progress",
	}

	result, err := roClient.CallTool(context.Background(), moveReq)
	if err != nil {
		t.Fatal("move_card call errored instead of returning a tool error:", err)
	}
	if !result.IsError {
		t.Fatal("move_card succeeded in read-only mode, want it refused")
	}

	found := false
	for _, entry := range result.Content {
		if tc, ok := entry.(mcp.TextContent); ok && strings.Contains(strings.ToLower(tc.Text), "read-only") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected move_card error message to mention read-only mode, got: %v", result.Content)
	}
}

func TestInProcess_ReadOnly_ListsOnlyReadTools(t *testing.T) {
	c, _, cancel := newInProcessClientReadOnly(t)
	defer cancel()
	defer c.Close()

	result, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal("ListTools failed:", err)
	}

	got := make(map[string]bool)
	for _, tool := range result.Tools {
		got[tool.Name] = true
	}
	want := map[string]bool{"list_columns": true, "get_card": true, "declare_blocked": true}
	if len(got) != len(want) {
		t.Fatalf("tools/list = %v, want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("tools/list is missing %q", name)
		}
	}
}
