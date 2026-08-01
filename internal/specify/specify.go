// Package specify decomposes an already-refined spec into kanban cards via
// an LLM agent. It used to be its own binary; Run is now the entry point a
// single ekbn process calls, whether from the `specify` subcommand or from
// the TUI's submit action.
package specify

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

// kanbanRoot matches internal/orchestrator's own hardcoded constant: specify
// and the orchestrator must agree on where the board lives regardless of
// what ekbn.config.yml's folder-name happens to say.
const kanbanRoot = ".kanban"

const specsProcessed = "specs/processed"

// defaultPrompt states the decomposition rule explicitly (largest
// independently-checkable, vertically-sliced chunk) rather than leaving it
// to per-run judgement, and gives the agent an escape hatch — unresolved —
// for anything the spec leaves undetermined, so ambiguity is reported
// rather than silently guessed away.
const defaultPrompt = `Available tools: list_columns, create_card

Read the spec file, then:
1. Call list_columns to discover the available columns
2. Break the spec into tickets. Each ticket must be the largest chunk that
   still has exactly one checkable outcome. Slice vertically — a ticket
   must be independently verifiable end-to-end (e.g. "user can log in with
   email and password") — never horizontally (never split "add the types",
   "add the service", "add the route" into separate tickets: none of those
   is checkable on its own). Create foundational tickets first.
3. For each ticket, call create_card with:
    - column: <the column ID from step 1>
   - title: <3-8 word title>
   - content: <2-5 sentences describing the work and what "done" means>
   - priority: <0|1|2|3> (0=foundational, 1=high, 2=medium, 3=low)
   - goal: <bug|feature|refactor|spike>, whichever this ticket actually is
   - depends_on: <ids of tickets this one needs finished first — each
     create_card response includes the new ticket's id, so reuse those ids
     from earlier calls; omit if there are no dependencies>
   - acceptance: <a command that proves the work is done, if one exists>
   - unresolved: <if the spec leaves something about this ticket
     undetermined, describe it here instead of guessing — do not resolve
     an ambiguity by picking an answer yourself; leave blank only when the
     spec is actually clear on this ticket>

Do NOT create tickets for testing, docs, or CI unless the spec requires them.
Do NOT read or modify files.
`

// Options mirrors the specify subcommand's flags/positional args, decoupled
// from flag parsing so both the CLI dispatcher and the TUI can construct it
// directly without going through os.Args.
type Options struct {
	Text       string   // raw spec content (TUI free-text/paste path)
	Files      []string // spec file paths (TUI file-picker path, or CLI positional args)
	Prompt     string   // -prompt override
	PromptFile string   // -prompt-file override
	Force      bool     // -force: bypass the sha256 re-run refusal
}

// runMu serializes calls to Run: the CLI subcommand and the TUI's submit
// action must never overlap.
var runMu sync.Mutex

// Run resolves prompt/spec-args/command from opts and ekbn.config.yml, then
// decomposes each resolved spec file in turn by running the configured
// command with the prompt appended as its final argument, streaming the
// subprocess's combined stdout/stderr into out as it is produced. If no
// command is configured (checking roles.specify, then roles.default), Run
// fails immediately rather than silently doing nothing. A per-spec failure
// is written into out and does not stop later specs in the same call; Run
// returns the first error encountered, if any.
func Run(ctx context.Context, opts Options, out io.Writer) error {
	runMu.Lock()
	defer runMu.Unlock()

	specArgs, err := resolveSpecArgs(opts.Text, opts.Files, out)
	if err != nil {
		return err
	}
	if len(specArgs) == 0 {
		return fmt.Errorf("no spec text or file provided")
	}

	prompt, err := resolvePrompt(opts.Prompt, opts.PromptFile)
	if err != nil {
		return err
	}

	cfg := serve.LoadConfig()
	roleNames := configuredRoleNames(cfg.Roles)
	command := resolveCommand(cfg.Roles, "specify")
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("no command configured — set roles.specify.command or roles.default.command in ekbn.config.yml")
	}

	var firstErr error
	for _, specArg := range specArgs {
		if err := processSpec(ctx, specArg, roleNames, prompt, command, opts.Force, out); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// resolveCommand looks up role in roles, falling back to roles["default"] —
// the same lookup semantics internal/orchestrator's resolveRoleConfig uses,
// duplicated here rather than shared across packages for one field. Returns
// "" if neither is configured; callers treat that as "nothing to invoke."
func resolveCommand(roles map[string]serve.RoleConfig, role string) string {
	if rc, ok := roles[role]; ok && strings.TrimSpace(rc.Command) != "" {
		return rc.Command
	}
	return roles["default"].Command
}

// resolveSpecArgs unifies the two accepted forms of spec input: file paths
// and raw text. Raw text is written to a real file under specs/ first — this
// gives it the same on-disk identity every other spec has, so retention as
// an artifact and re-run bookkeeping both apply to it without a separate
// code path.
func resolveSpecArgs(text string, fileArgs []string, out io.Writer) ([]string, error) {
	var specArgs []string
	if text != "" {
		if err := os.MkdirAll("specs", 0755); err != nil {
			return nil, fmt.Errorf("creating specs directory: %w", err)
		}
		name := fmt.Sprintf("inline-%s.md", time.Now().UTC().Format("20060102T150405Z"))
		path := filepath.Join("specs", name)
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			return nil, fmt.Errorf("writing inline spec: %w", err)
		}
		fmt.Fprintf(out, "Wrote inline spec to %s\n", path)
		specArgs = append(specArgs, path)
	}
	return append(specArgs, fileArgs...), nil
}

// processSpec runs one spec through decomposition: refuse a re-run unless
// forced, run the agent, validate the resulting dependency graph, and only
// then archive the spec. Any failure along the way leaves the spec in place
// rather than silently discarding or duplicating work.
func processSpec(ctx context.Context, specArg string, roleNames []string, prompt, command string, force bool, out io.Writer) error {
	specPath, err := filepath.Abs(specArg)
	if err != nil {
		fmt.Fprintf(out, "Error resolving path %s: %v\n", specArg, err)
		return err
	}
	if _, err := os.Stat(specPath); os.IsNotExist(err) {
		fmt.Fprintf(out, "Spec not found: %s\n", specPath)
		return err
	}
	specName := filepath.Base(specPath)

	specData, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(out, "Failed to read spec %s: %v\n", specName, err)
		return err
	}
	specHash := fmt.Sprintf("%x", sha256.Sum256(specData))

	processedDir, _ := filepath.Abs(specsProcessed)
	hashSidecar := filepath.Join(processedDir, specName+".sha256")
	if prevHash, err := os.ReadFile(hashSidecar); err == nil {
		if !force {
			if strings.TrimSpace(string(prevHash)) == specHash {
				fmt.Fprintf(out, "Skipping %s: already processed, unchanged since (use -force to reprocess anyway)\n", specName)
			} else {
				fmt.Fprintf(out, "Skipping %s: a spec with this filename was already processed, and its content has changed since (use -force to reprocess)\n", specName)
			}
			return nil
		}
		fmt.Fprintf(out, "Reprocessing %s despite a prior run (-force)\n", specName)
	}

	fmt.Fprintf(out, "\n%s\n  Specifying: %s\n%s\n", line40(), specName, line40())

	// The spec's own content is embedded directly into the prompt (like
	// buildSystemPrompt embeds ticket content) rather than passed via a
	// file-attach flag — a configured command can't be assumed to support
	// any particular one.
	fullPrompt := specPrompt(prompt, specName, roleNames) + "\n\n---\n\n## Spec\n\n" + string(specData)

	argv := strings.Fields(command)
	args := append(append([]string{}, argv[1:]...), fullPrompt)
	cmd := exec.CommandContext(ctx, argv[0], args...)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "  Agent exited with code %d\n", exitCode(err))
		return err
	}

	if problem := validateGraph(); problem != "" {
		fmt.Fprintf(out, "  %s\n  Spec left in place — fix the issue above, then rerun (use -force if this filename was already processed).\n", problem)
		return fmt.Errorf("%s", problem)
	}

	os.MkdirAll(processedDir, 0755)
	dest := filepath.Join(processedDir, specName)
	if err := os.Rename(specPath, dest); err != nil {
		fmt.Fprintf(out, "  Failed to move spec: %v\n", err)
		return err
	}
	if err := os.WriteFile(dest+".sha256", []byte(specHash), 0644); err != nil {
		fmt.Fprintf(out, "  Failed to record spec hash: %v\n", err)
	}
	fmt.Fprintf(out, "  Spec moved to %s/\n", specsProcessed)
	return nil
}

// validateGraph reloads the board and reports any dependency-graph problem
// this run may have introduced: a cycle anywhere, or a depends_on reference
// to an ID that doesn't exist. Either would otherwise starve a card
// silently once the orchestrator starts scheduling, so the spec that
// produced it is not archived until the problem is fixed.
func validateGraph() string {
	cards, err := model.LoadAllCards(kanbanRoot)
	if err != nil {
		return fmt.Sprintf("Failed to load the board to validate the dependency graph: %v", err)
	}
	if cycle := model.DetectCycle(cards); cycle != nil {
		return fmt.Sprintf("Dependency cycle detected: %s", strings.Join(cycle, " -> "))
	}
	if issues := model.ValidateDependencies(cards); len(issues) > 0 {
		var b strings.Builder
		b.WriteString("Dangling dependency reference(s):")
		for _, iss := range issues {
			fmt.Fprintf(&b, "\n  - %q depends_on %v, which do(es) not exist", iss.Card.Title, iss.Missing)
		}
		return b.String()
	}
	return ""
}

// configuredRoleNames returns the sorted set of role names in roles, or nil
// if none are configured.
func configuredRoleNames(roles map[string]serve.RoleConfig) []string {
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// specPrompt augments basePrompt with per-spec instructions: every card
// created while decomposing this spec must carry its spec filename (for
// traceability) and, when roles are configured, a role from that set —
// routing is a lookup done later by the orchestrator, not a judgment call
// made here, but the agent still has to pick a valid key from the set.
func specPrompt(basePrompt, specName string, roleNames []string) string {
	p := fmt.Sprintf("%s\n\n---\n\nFor every card you create while decomposing this spec, set `spec: %s` in its frontmatter.", basePrompt, specName)
	if len(roleNames) > 0 {
		p += fmt.Sprintf(" Set `role` to one of: %s.", strings.Join(roleNames, ", "))
	}
	return p
}

func resolvePrompt(custom, filePath string) (string, error) {
	if custom != "" {
		return custom, nil
	}
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("reading prompt file %s: %w", filePath, err)
		}
		return string(data), nil
	}
	cfg := serve.LoadConfig()
	if cfg.Prompt != "" {
		return cfg.Prompt, nil
	}
	return defaultPrompt, nil
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
