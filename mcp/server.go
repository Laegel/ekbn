package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"ekbn/model"
)

func extractArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	switch val := v.(type) {
	case string:
		return val, nil
	case []string:
		if len(val) > 0 {
			return val[0], nil
		}
	case []any:
		if len(val) > 0 {
			if s, ok := val[0].(string); ok {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("missing required argument %q", key)
}

type EKBNServer struct {
	ColumnsDir string
	server     *server.MCPServer
}

func New(columnsDir string) *EKBNServer {
	return &EKBNServer{
		ColumnsDir: columnsDir,
	}
}

func (e *EKBNServer) MCPServer() *server.MCPServer {
	if e.server != nil {
		return e.server
	}

	s := server.NewMCPServer(
		"ekbn",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
		server.WithInstructions("A kanban board server for managing columns and cards."),
	)

	e.registerTools(s)
	e.registerResources(s)
	e.registerResourceTemplates(s)

	e.server = s
	return s
}

func (e *EKBNServer) registerTools(s *server.MCPServer) {
	listColumnsTool := mcp.NewTool("list_columns",
		mcp.WithDescription("List all columns in the kanban board"),
	)

	s.AddTool(listColumnsTool, e.handleListColumns)

	createColumnTool := mcp.NewTool("create_column",
		mcp.WithDescription("Create a new column"),
		mcp.WithString("slug",
			mcp.Required(),
			mcp.Description("Unique identifier for the column (e.g. 'backlog')"),
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Display name for the column (e.g. 'Backlog')"),
		),
	)

	s.AddTool(createColumnTool, e.handleCreateColumn)

	createCardTool := mcp.NewTool("create_card",
		mcp.WithDescription("Create a new card in a column"),
		mcp.WithString("column",
			mcp.Required(),
			mcp.Description("Column slug to add the card to (e.g. 'todo')"),
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("Title of the card"),
		),
		mcp.WithString("content",
			mcp.Description("Markdown content of the card"),
		),
		mcp.WithNumber("priority",
			mcp.Description("Priority level (higher = more important)"),
		),
		mcp.WithString("role",
			mcp.Description("Role responsible for the card (e.g. 'dev', 'qa', 'design')"),
		),
		mcp.WithBoolean("blocked",
			mcp.Description("Whether the card is blocked"),
		),
		mcp.WithArray("categories",
			mcp.WithStringItems(),
			mcp.Description("Categories/tags for the card"),
		),
	)

	s.AddTool(createCardTool, e.handleCreateCard)

	getCardTool := mcp.NewTool("get_card",
		mcp.WithDescription("Get a card by column and filename"),
		mcp.WithString("column",
			mcp.Required(),
			mcp.Description("Column ID (e.g. '100-todo')"),
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("Filename of the card (e.g. 'my-card.md')"),
		),
	)

	s.AddTool(getCardTool, e.handleGetCard)

	updateCardTool := mcp.NewTool("update_card",
		mcp.WithDescription("Update an existing card"),
		mcp.WithString("column",
			mcp.Required(),
			mcp.Description("Column containing the card"),
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("Filename of the card to update"),
		),
		mcp.WithString("title",
			mcp.Description("New title"),
		),
		mcp.WithString("content",
			mcp.Description("New markdown content"),
		),
		mcp.WithNumber("priority",
			mcp.Description("New priority level"),
		),
		mcp.WithString("role",
			mcp.Description("New role"),
		),
		mcp.WithBoolean("blocked",
			mcp.Description("New blocked status"),
		),
		mcp.WithArray("categories",
			mcp.WithStringItems(),
			mcp.Description("New categories"),
		),
	)

	s.AddTool(updateCardTool, e.handleUpdateCard)

	deleteCardTool := mcp.NewTool("delete_card",
		mcp.WithDescription("Delete a card"),
		mcp.WithString("column",
			mcp.Required(),
			mcp.Description("Column containing the card"),
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("Filename of the card to delete"),
		),
	)

	s.AddTool(deleteCardTool, e.handleDeleteCard)

	moveCardTool := mcp.NewTool("move_card",
		mcp.WithDescription("Move a card to another column"),
		mcp.WithString("column",
			mcp.Required(),
			mcp.Description("Current column"),
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("Filename of the card to move"),
		),
		mcp.WithString("to_column",
			mcp.Required(),
			mcp.Description("Destination column"),
		),
	)

	s.AddTool(moveCardTool, e.handleMoveCard)

	addCommentTool := mcp.NewTool("add_comment",
		mcp.WithDescription("Add a comment to a card"),
		mcp.WithString("column",
			mcp.Required(),
			mcp.Description("Column containing the card"),
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("Filename of the card"),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("Comment text"),
		),
	)

	s.AddTool(addCommentTool, e.handleAddComment)

	reorderColumnTool := mcp.NewTool("reorder_column",
		mcp.WithDescription("Reorder a column relative to another"),
		mcp.WithString("slug",
			mcp.Required(),
			mcp.Description("Slug of the column to move"),
		),
		mcp.WithString("before_slug",
			mcp.Description("Slug of the column to insert before (empty to move to end)"),
		),
	)

	s.AddTool(reorderColumnTool, e.handleReorderColumn)
}

func (e *EKBNServer) registerResources(s *server.MCPServer) {
	s.AddResource(
		mcp.NewResource("ekbn://columns",
			"All Columns",
			mcp.WithResourceDescription("All columns in the kanban board"),
			mcp.WithMIMEType("application/json"),
		),
		e.handleReadColumnsResource,
	)
}

func (e *EKBNServer) registerResourceTemplates(s *server.MCPServer) {
	s.AddResourceTemplate(
		mcp.NewResourceTemplate("ekbn://column/{slug}",
			"Column",
			mcp.WithTemplateDescription("A specific column by slug"),
			mcp.WithTemplateMIMEType("application/json"),
		),
		e.handleReadColumnResource,
	)

	s.AddResourceTemplate(
		mcp.NewResourceTemplate("ekbn://card/{column}/{filename}",
			"Card",
			mcp.WithTemplateDescription("A specific card by column and filename"),
			mcp.WithTemplateMIMEType("text/markdown"),
		),
		e.handleReadCardResource,
	)
}

func (e *EKBNServer) handleListColumns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	columns, err := model.ListColumns(e.ColumnsDir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list columns: %v", err)), nil
	}

	data, _ := json.MarshalIndent(columns, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (e *EKBNServer) handleCreateColumn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	slug, _ := req.RequireString("slug")
	name, _ := req.RequireString("name")

	dirName, idx, err := model.CreateColumn(e.ColumnsDir, slug, name)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create column: %v", err)), nil
	}

	return mcp.NewToolResultStructured(
		map[string]any{"id": dirName, "slug": slug, "name": name, "index": idx},
		fmt.Sprintf("Created column %q with slug %q (index: %d)", name, slug, idx),
	), nil
}

func (e *EKBNServer) handleCreateCard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	column, _ := req.RequireString("column")
	title, _ := req.RequireString("title")

	content := req.GetString("content", "")
	categories := req.GetStringSlice("categories", nil)
	priority := req.GetInt("priority", 0)
	role := req.GetString("role", "")
	blocked := req.GetBool("blocked", false)

	filename, err := model.CreateCard(e.ColumnsDir, column, title, "mcp", content, categories, priority, role, blocked)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create card: %v", err)), nil
	}

	card, _ := model.ReadCard(filepath.Join(e.ColumnsDir, column, filename), column)

	return mcp.NewToolResultStructured(
		map[string]any{"filename": filename, "card": card},
		fmt.Sprintf("Created card %q in column %q", title, column),
	), nil
}

func (e *EKBNServer) handleGetCard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	column, _ := req.RequireString("column")
	filename, _ := req.RequireString("filename")

	card, err := model.ReadCard(filepath.Join(e.ColumnsDir, column, filename), column)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("card not found: %v", err)), nil
	}

	data, _ := json.MarshalIndent(card, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (e *EKBNServer) handleUpdateCard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	column, _ := req.RequireString("column")
	filename, _ := req.RequireString("filename")

	if err := model.UpdateCard(
		e.ColumnsDir, column, filename,
		req.GetString("title", ""),
		req.GetString("content", ""),
		req.GetStringSlice("categories", nil),
		req.GetInt("priority", 0),
		req.GetString("role", ""),
		req.GetBool("blocked", false),
	); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update card: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Updated card %s in column %s", filename, column)), nil
}

func (e *EKBNServer) handleDeleteCard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	column, _ := req.RequireString("column")
	filename, _ := req.RequireString("filename")

	if err := model.DeleteCard(e.ColumnsDir, column, filename); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete card: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Deleted card %s from column %s", filename, column)), nil
}

func (e *EKBNServer) handleMoveCard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	column, _ := req.RequireString("column")
	filename, _ := req.RequireString("filename")
	toColumn, _ := req.RequireString("to_column")

	if err := model.MoveCard(e.ColumnsDir, column, toColumn, filename); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to move card: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Moved card %s from %s to %s", filename, column, toColumn)), nil
}

func (e *EKBNServer) handleAddComment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	column, _ := req.RequireString("column")
	filename, _ := req.RequireString("filename")
	text, _ := req.RequireString("text")

	if err := model.AddComment(e.ColumnsDir, column, filename, "mcp", text); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to add comment: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Added comment to card %s in column %s", filename, column)), nil
}

func (e *EKBNServer) handleReorderColumn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	slug, _ := req.RequireString("slug")
	beforeSlug := req.GetString("before_slug", "")

	if err := model.ReorderColumn(e.ColumnsDir, slug, beforeSlug); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to reorder column: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Reordered column %s", slug)), nil
}

func (e *EKBNServer) handleReadColumnsResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	columns, err := model.ListColumns(e.ColumnsDir)
	if err != nil {
		return nil, err
	}

	data, _ := json.MarshalIndent(columns, "", "  ")
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "ekbn://columns",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (e *EKBNServer) handleReadColumnResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	slug, err := extractArg(req.Params.Arguments, "slug")
	if err != nil {
		return nil, fmt.Errorf("slug parameter is required")
	}

	columns, err := model.ListColumns(e.ColumnsDir)
	if err != nil {
		return nil, err
	}

	for _, col := range columns {
		if col.Slug == slug || strings.Contains(col.ID, slug) {
			data, _ := json.MarshalIndent(col, "", "  ")
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      fmt.Sprintf("ekbn://column/%s", slug),
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		}
	}

	return nil, fmt.Errorf("column %q not found", slug)
}

func (e *EKBNServer) handleReadCardResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	column, err := extractArg(req.Params.Arguments, "column")
	if err != nil {
		return nil, fmt.Errorf("column parameter is required")
	}
	filename, err := extractArg(req.Params.Arguments, "filename")
	if err != nil {
		return nil, fmt.Errorf("filename parameter is required")
	}

	path := filepath.Join(e.ColumnsDir, column, filename)
	card, err := model.ReadCard(path, column)
	if err != nil {
		return nil, fmt.Errorf("card not found: %w", err)
	}

	data, _ := json.MarshalIndent(card, "", "  ")
	uri := fmt.Sprintf("ekbn://card/%s/%s", column, filename)
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}
