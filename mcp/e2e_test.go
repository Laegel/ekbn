package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MCP protocol version matching the mcp-go library
const e2eProtocolVersion = "2025-06-18"

var ekbnBinary string

func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod starting from %s", dir)
		}
		dir = parent
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ekbn-e2e-*")
	if err != nil {
		log.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dir)

	moduleRoot, err := findModuleRoot()
	if err != nil {
		log.Fatalf("find module root: %v", err)
	}

	bin := filepath.Join(dir, "ekbn")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("failed to build ekbn binary:\n%s\n%v", out, err)
	}

	ekbnBinary = bin
	os.Exit(m.Run())
}

// e2eServer wraps a running ekbn mcp subprocess for JSON-RPC communication.
type e2eServer struct {
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	cmd     *exec.Cmd
	nextID  int
}

// startE2EServer starts the ekbn mcp binary with a columns directory and
// performs the MCP initialize handshake. The server is automatically killed
// in t.Cleanup.
func startE2EServer(t *testing.T, columnsDir string) *e2eServer {
	t.Helper()

	cmd := exec.Command(ekbnBinary, "mcp", "--columns", columnsDir)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s := &e2eServer{
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
		cmd:     cmd,
		nextID:  1,
	}

	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})

	s.doInitialize(t)
	return s
}

func (s *e2eServer) doInitialize(t *testing.T) {
	t.Helper()

	// Send initialize request — the result contains serverInfo/capabilities
	resp := s.sendRequest(t, "initialize", map[string]any{
		"protocolVersion": e2eProtocolVersion,
		"clientInfo": map[string]any{
			"name":    "ekbn-e2e-test",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{},
	})

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("initialize response missing result")
	}
	if result["serverInfo"] == nil {
		t.Fatal("initialize response missing serverInfo")
	}
	if result["capabilities"] == nil {
		t.Fatal("initialize response missing capabilities")
	}

	s.sendNotification("notifications/initialized")
}

func (s *e2eServer) sendRequest(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	id := s.nextID
	s.nextID++

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := fmt.Fprintf(s.stdin, "%s\n", raw); err != nil {
		t.Fatalf("write request: %v", err)
	}

	return s.readResponse(t)
}

func (s *e2eServer) sendNotification(method string) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	raw, _ := json.Marshal(msg)
	fmt.Fprintf(s.stdin, "%s\n", raw)
}

func (s *e2eServer) readResponse(t *testing.T) map[string]any {
	t.Helper()
	done := make(chan map[string]any, 1)

	go func() {
		if s.scanner.Scan() {
			line := s.scanner.Text()
			var result map[string]any
			if err := json.Unmarshal([]byte(line), &result); err == nil {
				done <- result
				return
			}
		}
		done <- nil
	}()

	select {
	case resp := <-done:
		if resp == nil {
			t.Fatalf("no response (scanner err: %v)", s.scanner.Err())
		}
		if errObj, ok := resp["error"].(map[string]any); ok {
			t.Fatalf("JSON-RPC error: %v (code=%.0f)", errObj["message"], errObj["code"])
		}
		return resp
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for MCP response")
		return nil
	}
}

func (s *e2eServer) callTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	return s.sendRequest(t, "tools/call", params)
}

func (s *e2eServer) readResource(t *testing.T, uri string) map[string]any {
	t.Helper()
	return s.sendRequest(t, "resources/read", map[string]any{"uri": uri})
}

func (s *e2eServer) listTools(t *testing.T) map[string]any {
	t.Helper()
	return s.sendRequest(t, "tools/list", nil)
}

func (s *e2eServer) listResources(t *testing.T) map[string]any {
	t.Helper()
	return s.sendRequest(t, "resources/list", nil)
}

func (s *e2eServer) listResourceTemplates(t *testing.T) map[string]any {
	t.Helper()
	return s.sendRequest(t, "resources/templates/list", nil)
}

// ─── Tests ────────────────────────────────────────────────────────────────────────

func TestE2E_Initialize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	resp := s.sendRequest(t, "ping", nil)
	if _, ok := resp["result"]; !ok {
		t.Fatal("ping expected a result")
	}
}

func TestE2E_ListTools(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	resp := s.listTools(t)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatal("list_tools expected a result object")
	}

	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		t.Fatal("list_tools result missing tools array")
	}

	expected := []string{
		"list_columns", "create_column", "create_card", "get_card",
		"update_card", "delete_card", "move_card", "add_comment",
		"reorder_column",
	}

	names := make(map[string]bool)
	for _, toolRaw := range toolsRaw {
		if tool, ok := toolRaw.(map[string]any); ok {
			if name, ok := tool["name"].(string); ok {
				names[name] = true
			}
		}
	}

	for _, exp := range expected {
		if !names[exp] {
			t.Fatalf("missing tool %q", exp)
		}
	}
}

func TestE2E_ListResources(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	resp := s.listResources(t)

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("list_resources expected a result")
	}

	resources, _ := result["resources"].([]any)
	found := false
	for _, r := range resources {
		if res, ok := r.(map[string]any); ok {
			if uri, _ := res["uri"].(string); uri == "ekbn://columns" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("expected resource ekbn://columns")
	}
}

func TestE2E_ListResourceTemplates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	resp := s.listResourceTemplates(t)

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("list_resource_templates expected a result")
	}

	templates, _ := result["resourceTemplates"].([]any)
	uris := make(map[string]bool)
	for _, tRaw := range templates {
		if tmpl, ok := tRaw.(map[string]any); ok {
			if uriTemplate, ok := tmpl["uriTemplate"].(string); ok {
				uris[uriTemplate] = true
			}
		}
	}

	for _, exp := range []string{"ekbn://column/{slug}", "ekbn://card/{column}/{filename}"} {
		if !uris[exp] {
			t.Fatalf("missing template %q", exp)
		}
	}
}

func TestE2E_CallTool_ListColumns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	resp := s.callTool(t, "list_columns", nil)

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("expected result from list_columns")
	}
	text := extractText(result)
	if !strings.Contains(text, "100-todo") {
		t.Fatalf("list_columns should contain '100-todo', got: %s", text)
	}
}

func TestE2E_CallTool_CreateAndGetCard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	// Create a card
	createResp := s.callTool(t, "create_card", map[string]any{
		"column":   "100-todo",
		"title":    "E2E Test Card",
		"content":  "Created via stdio",
		"priority": float64(2),
		"role":     "dev",
	})

	createResult, _ := createResp["result"].(map[string]any)
	if createResult == nil {
		t.Fatal("create_card expected result")
	}

	// Check structured content for the filename
	structured, _ := createResult["structuredContent"].(map[string]any)
	if structured == nil {
		t.Fatal("expected structuredContent from create_card")
	}
	filename, _ := structured["filename"].(string)
	if filename == "" {
		t.Fatalf("expected filename in structured content: %+v", structured)
	}

	// Get the card and verify title
	getResp := s.callTool(t, "get_card", map[string]any{
		"column":   "100-todo",
		"filename": filename,
	})

	getResult, _ := getResp["result"].(map[string]any)
	if getResult == nil {
		t.Fatal("get_card expected result")
	}
	text := extractText(getResult)
	if !strings.Contains(text, "E2E Test Card") {
		t.Fatalf("get_card should contain title, got: %s", text)
	}
}

func TestE2E_ReadResource_Columns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	resp := s.readResource(t, "ekbn://columns")

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatal("expected result from read_resource")
	}

	contents, _ := result["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("expected contents array from read_resource")
	}

	first, _ := contents[0].(map[string]any)
	if first == nil {
		t.Fatal("expected content object")
	}
	if uri, _ := first["uri"].(string); uri != "ekbn://columns" {
		t.Fatalf("expected uri ekbn://columns, got %s", uri)
	}
	text, _ := first["text"].(string)
	if !strings.Contains(text, "100-todo") {
		t.Fatalf("resource text should contain '100-todo': %s", text)
	}
}

func TestE2E_Error_UnknownTool(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	// Send request manually to avoid readResponse which fails on errors
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      100,
		"method":  "tools/call",
		"params":  map[string]any{"name": "nonexistent_tool"},
	})
	fmt.Fprintf(s.stdin, "%s\n", raw)

	resp := waitForResponse(t, s)
	if resp == nil {
		t.Fatal("expected error response")
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("expected error object for non-existent tool")
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "nonexistent_tool") {
		t.Fatalf("error should mention tool name, got: %s", msg)
	}
}

func TestE2E_Error_UnknownResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "100-todo"), 0755)
	s := startE2EServer(t, dir)

	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      100,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "ekbn://nonexistent"},
	})
	fmt.Fprintf(s.stdin, "%s\n", raw)

	resp := waitForResponse(t, s)
	if resp == nil {
		t.Fatal("expected error response")
	}
	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error object for non-existent resource")
	}
}

// waitForResponse reads a single JSON-RPC response without failing on errors.
func waitForResponse(t *testing.T, s *e2eServer) map[string]any {
	t.Helper()
	done := make(chan map[string]any, 1)
	go func() {
		if s.scanner.Scan() {
			var resp map[string]any
			json.Unmarshal([]byte(s.scanner.Text()), &resp)
			done <- resp
		} else {
			done <- nil
		}
	}()
	select {
	case resp := <-done:
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for response")
		return nil
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────────────

func extractText(result map[string]any) string {
	content, _ := result["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if text, ok := m["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}
