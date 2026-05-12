package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"ekbn/mcp"
)

func runMCP() {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	columnsDir := fs.String("columns", "", "Path to the columns directory (default: $EKB_COLUMNS or './columns')")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ekbn mcp [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[2:])

	dir := *columnsDir
	if dir == "" {
		dir = os.Getenv("EKB_COLUMNS")
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			log.Fatalf("failed to get working directory: %v", err)
		}
		dir = dir + "/columns"
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Warning: columns directory %q does not exist. Use -columns flag or EKB_COLUMNS env var.\n", dir)
	}

	log.Printf("Starting ekbn MCP server with columns directory: %s", dir)

	ekbn := mcp.New(dir)
	srv := ekbn.MCPServer()

	if err := server.ServeStdio(srv); err != nil {
		log.Fatalf("MCP server error: %v", err)
	}
}
