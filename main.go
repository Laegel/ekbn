package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mark3labs/mcp-go/server"

	"ekbn/internal/orchestrator"
	"ekbn/internal/serve"
	"ekbn/internal/specify"
	"ekbn/internal/tui"
	"ekbn/mcp"
)

//go:embed dist
var distFS embed.FS

func main() {
	if len(os.Args) < 2 {
		runCombined()
		return
	}

	switch os.Args[1] {
	case "serve":
		runServe()
	case "mcp":
		runMCP()
	case "orchestrator":
		runOrchestrator()
	case "specify":
		runSpecifyCmd()
	case "reset-blocked":
		runResetBlocked()
	case "tui":
		runCombined()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: ekbn [command] [options]

Running ekbn with no command starts combined mode: the orchestrator's
polling loop, the HTTP kanban UI, and a terminal UI for decomposing specs —
all in one process. Same as 'ekbn tui'.

Commands:
  serve         Start only the HTTP server with kanban UI
  mcp           Start only the MCP stdio server
  orchestrator  Start only the orchestrator (polling loop + HTTP UI), no TUI
  specify       Decompose a spec into kanban cards from the command line
  reset-blocked Return every blocked card to todo, clearing review findings
  tui           Explicit alias for combined mode (orchestrator + MCP + TUI)

Run 'ekbn <command> -h' for command-specific help.
`)
}

// embeddedDistFS returns the frontend assets embedded in this binary.
// Combined/orchestrator mode always serves the embedded build — dev-mode
// frontend iteration already has its own path ('npm run dev'), and adding a
// -dev flag to three more entry points would only duplicate it.
func embeddedDistFS() fs.FS {
	root, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("failed to access embedded dist: %v", err)
	}
	return root
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
		root = embeddedDistFS()
	}

	serve.Run(root)
	log.Println("Server stopped")
}

func runMCP() {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	columnsDir := fs.String("columns", "", "Path to the columns directory (default: $EKB_COLUMNS, then ekbn.config.yml folder-name, then './columns')")
	readOnly := fs.Bool("read-only", false, "restrict to read-only tools (list_columns, get_card); refuses all mutating tool calls")
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

	ekbnMCP := mcp.New(dir, *readOnly)
	srv := ekbnMCP.MCPServer()

	if err := server.ServeStdio(srv); err != nil {
		log.Fatalf("MCP server error: %v", err)
	}
}

// runOrchestrator starts the HTTP kanban UI in-process and runs the
// orchestrator's polling loop, with no TUI. Replaces the old standalone
// `orchestrator` binary — starting ekbn's own HTTP server in-process
// (instead of orchestrator shelling out to an `ekbn serve` subprocess on
// PATH) is exactly the fragility this consolidation removes.
func runOrchestrator() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() { serve.Run(embeddedDistFS()) }()

	if err := orchestrator.Run(ctx); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
}

// runSpecifyCmd is the standalone `ekbn specify` subcommand: decompose one or
// more specs from the command line, streaming progress to stdout exactly as
// the old standalone `specify` binary did.
func runSpecifyCmd() {
	fs := flag.NewFlagSet("specify", flag.ExitOnError)
	promptFlag := fs.String("prompt", "", "Custom prompt (overrides ekbn.config.yml and default)")
	promptFile := fs.String("prompt-file", "", "Path to a file containing a custom prompt")
	textFlag := fs.String("text", "", "Raw spec content, as an alternative to passing file paths")
	forceFlag := fs.Bool("force", false, "Reprocess a spec even if a spec with the same filename was already processed")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ekbn specify [flags] [<spec-file.md> ...]\n\nPass -text to supply the spec as raw content instead of a file.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[2:])

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	opts := specify.Options{
		Text:       *textFlag,
		Files:      fs.Args(),
		Prompt:     *promptFlag,
		PromptFile: *promptFile,
		Force:      *forceFlag,
	}
	if err := specify.Run(ctx, opts, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// runResetBlocked is the standalone `ekbn reset-blocked` subcommand: return
// every blocked card to todo with its review-findings history cleared, so a
// human can re-run tickets that got stuck for reasons unrelated to the code
// itself (e.g. a sandboxed agent that couldn't do its job).
func runResetBlocked() {
	fs := flag.NewFlagSet("reset-blocked", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ekbn reset-blocked\n\nMoves every blocked card back to todo, clearing stage/round/lease state\nand stripping any appended review-findings sections from the body.\n")
	}
	fs.Parse(os.Args[2:])

	n := orchestrator.ResetBlocked()
	fmt.Printf("Reset %d blocked ticket(s) to todo\n", n)
}

// runCombined is ekbn's default mode: the orchestrator's polling loop, the
// in-process HTTP kanban UI, and a terminal UI for decomposing specs, all
// running together. Quitting the TUI cancels ctx, which stops the
// orchestrator loop (after its current cycle finishes) and cancels any
// specify run still in flight from the TUI.
func runCombined() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() { serve.Run(embeddedDistFS()) }()

	orchDone := make(chan error, 1)
	go func() { orchDone <- orchestrator.Run(ctx) }()

	p := tea.NewProgram(tui.New(ctx, specify.Run), tea.WithAltScreen())
	_, tuiErr := p.Run()

	cancel()
	fmt.Println("Shutting down — waiting for the current orchestrator cycle to finish...")
	<-orchDone

	if tuiErr != nil {
		log.Fatalf("tui: %v", tuiErr)
	}
}
