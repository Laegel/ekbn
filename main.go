package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/server"

	"ekbn/internal/serve"
	"ekbn/mcp"
)

//go:embed dist
var distFS embed.FS

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runServe()
	case "mcp":
		runMCP()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: ekbn <command> [options]

Commands:
  serve   Start the HTTP server with kanban UI
  mcp     Start the MCP stdio server

Run 'ekbn <command> -h' for command-specific help.
`)
}

func runServe() {
	cmd := flag.NewFlagSet("serve", flag.ExitOnError)
	dev := cmd.Bool("dev", false, "serve assets from ./dist on disk instead of embedded")
	cmd.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ekbn serve [options]\n\nOptions:\n")
		cmd.PrintDefaults()
	}
	cmd.Parse(os.Args[2:])

	var root fs.FS
	if *dev {
		root = os.DirFS("./dist")
	} else {
		var err error
		root, err = fs.Sub(distFS, "dist")
		if err != nil {
			log.Fatalf("failed to access embedded dist: %v", err)
		}
	}

	serve.Run(root)
	log.Println("Server stopped")
}

func runMCP() {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	columnsDir := fs.String("columns", "", "Path to the columns directory (default: $EKB_COLUMNS, then ekbn.config.yml folder-name, then './columns')")
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
		cfg := serve.LoadConfig()
		if cfg.FolderName != "" {
			dir, _ = filepath.Abs(cfg.FolderName)
		}
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			log.Fatalf("failed to get working directory: %v", err)
		}
		dir = filepath.Join(dir, "columns")
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Warning: columns directory %q does not exist. Use -columns flag or EKB_COLUMNS env var.\n", dir)
	}

	log.Printf("Starting ekbn MCP server with columns directory: %s", dir)

	ekbnMCP := mcp.New(dir)
	srv := ekbnMCP.MCPServer()

	if err := server.ServeStdio(srv); err != nil {
		log.Fatalf("MCP server error: %v", err)
	}
}
