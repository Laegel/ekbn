package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"ekbn/internal/opencode"
	"ekbn/internal/serve"
)

type mcpConfig struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

type opencodeConfig struct {
	MCP map[string]mcpConfig `json:"mcp"`
}

const specsProcessed = "specs/processed"

const defaultPrompt = `Available tools: list_columns, create_card

Read the spec file, then:
1. Call list_columns to discover the available columns
2. Break the spec into 2-8 tickets, ordered by dependency
3. For each ticket, call create_card with:
    - column: <the column ID from step 1>
   - title: <3-8 word title>
   - content: <2-5 sentences describing the work and what "done" means>
   - priority: <0|1|2|3> (0=foundational, 1=high, 2=medium, 3=low)

Do NOT create tickets for testing, docs, or CI unless the spec requires them.
Do NOT read or modify files.
`

var prompt = defaultPrompt

func main() {
	promptFlag := flag.String("prompt", "", "Custom prompt (overrides ekbn.config.yml and default)")
	promptFile := flag.String("prompt-file", "", "Path to a file containing a custom prompt")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: plan [flags] <spec-file.md> [spec-file2.md ...]\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	resolvePrompt(*promptFlag, *promptFile)

	if err := opencode.EnsureInstalled(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to install opencode: %v\n", err)
		os.Exit(1)
	}

	registerMCP()

	for _, specArg := range flag.Args() {
		specPath, err := filepath.Abs(specArg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", specArg, err)
			continue
		}
		if _, err := os.Stat(specPath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Spec not found: %s\n", specPath)
			continue
		}
		specName := filepath.Base(specPath)
		fmt.Printf("\n%s\n  Planning: %s\n%s\n", line40(), specName, line40())

		cmd := exec.Command("opencode", "run",
			"-m", "opencode/deepseek-v4-flash-free",
			"--dangerously-skip-permissions",
			prompt,
			"-f", specPath,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  Agent exited with code %d\n", exitCode(err))
			continue
		}

		processedDir, _ := filepath.Abs(specsProcessed)
		os.MkdirAll(processedDir, 0755)
		dest := filepath.Join(processedDir, specName)
		if err := os.Rename(specPath, dest); err != nil {
			fmt.Fprintf(os.Stderr, "  Failed to move spec: %v\n", err)
			continue
		}
		fmt.Printf("  Spec moved to %s/\n", specsProcessed)
	}
}

func registerMCP() {
	cfgPath := "opencode.json"
	cfg := opencodeConfig{MCP: map[string]mcpConfig{
		"ekbn": {
			Type:    "local",
			Command: []string{"ekbn", "mcp"},
			Enabled: true,
		},
	}}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Failed to marshal MCP config: %v\n", err)
		return
	}
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "  Failed to write MCP config: %v\n", err)
	}
}

func resolvePrompt(custom, filePath string) {
	if custom != "" {
		prompt = custom
		return
	}
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading prompt file %s: %v\n", filePath, err)
			os.Exit(1)
		}
		prompt = string(data)
		return
	}
	cfg := serve.LoadConfig()
	if cfg.Prompt != "" {
		prompt = cfg.Prompt
	}
}

func line40() string {
	return "----------------------------------------"
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}
