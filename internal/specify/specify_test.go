package specify

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekbn/model"
)

// blockedPaths lists filesystem path patterns that must NOT appear in the prompt.
var blockedPaths = []string{
	".kanban",
	"/kanban",
	"kanban/",
	"100-todo",
	"todo.md",
	"write a .md",
	".md file",
	"file into",
	"mkdir",
	"os.Create",
	"ioutil",
}

func TestPromptNoHardcodedPaths(t *testing.T) {
	for _, bp := range blockedPaths {
		if strings.Contains(defaultPrompt, bp) {
			t.Errorf("prompt contains blocked pattern %q — agent must not know about filesystem paths", bp)
		}
	}
}

func TestPromptUsesMCPTools(t *testing.T) {
	if !strings.Contains(defaultPrompt, "list_columns") {
		t.Error("prompt must mention list_columns tool")
	}
	if !strings.Contains(defaultPrompt, "create_card") {
		t.Error("prompt must mention create_card tool")
	}
}

func TestPromptExplicitSteps(t *testing.T) {
	// The prompt should give numbered steps, not leave the agent guessing.
	if !strings.Contains(defaultPrompt, "1.") {
		t.Error("prompt should contain numbered steps")
	}
	if !strings.Contains(defaultPrompt, "call") && !strings.Contains(defaultPrompt, "Call") {
		t.Error("prompt should tell the agent to call tools")
	}
}

func TestPromptColumnSlug(t *testing.T) {
	// The agent should discover the column slug dynamically, not hardcode "todo".
	if strings.Contains(defaultPrompt, `"todo"`) || strings.Contains(defaultPrompt, "'todo'") {
		t.Error("prompt should not hardcode 'todo' column slug — agent must discover via list_columns")
	}
}

func TestLine40(t *testing.T) {
	got := line40()
	if len(got) != 40 {
		t.Errorf("line40() length = %d, want 40", len(got))
	}
}

func TestSpecPrompt(t *testing.T) {
	t.Run("always sets spec, roles listed when configured", func(t *testing.T) {
		got := specPrompt("base prompt", "onboarding.md", []string{"backend", "frontend"})
		if !strings.Contains(got, "base prompt") {
			t.Error("specPrompt should preserve the base prompt")
		}
		if !strings.Contains(got, "spec: onboarding.md") {
			t.Errorf("specPrompt should mention spec: onboarding.md, got: %s", got)
		}
		if !strings.Contains(got, "backend, frontend") {
			t.Errorf("specPrompt should list the configured role names, got: %s", got)
		}
	})

	t.Run("no role instruction when roles unconfigured", func(t *testing.T) {
		got := specPrompt("base prompt", "onboarding.md", nil)
		if !strings.Contains(got, "spec: onboarding.md") {
			t.Errorf("specPrompt should still mention spec: onboarding.md, got: %s", got)
		}
		if strings.Contains(got, "role") {
			t.Errorf("specPrompt should not mention role when no roles are configured, got: %s", got)
		}
	})
}

func TestExitCode(t *testing.T) {
	t.Run("nil error returns -1", func(t *testing.T) {
		if got := exitCode(nil); got != -1 {
			t.Errorf("exitCode(nil) = %d, want -1", got)
		}
	})
}

func TestPromptVerticalSliceRule(t *testing.T) {
	if !strings.Contains(defaultPrompt, "one checkable outcome") {
		t.Error("prompt should state the decomposition rule: largest chunk with one checkable outcome")
	}
	if !strings.Contains(defaultPrompt, "vertically") {
		t.Error("prompt should require vertical slicing")
	}
	if !strings.Contains(defaultPrompt, "never horizontally") {
		t.Error("prompt should call out horizontal layering as the anti-pattern")
	}
}

func TestPromptUnresolvedInstruction(t *testing.T) {
	if !strings.Contains(defaultPrompt, "unresolved") {
		t.Error("prompt should mention the unresolved field")
	}
	if !strings.Contains(defaultPrompt, "instead of guessing") {
		t.Error("prompt should tell the agent not to guess when the spec is undetermined")
	}
}

func TestResolveSpecArgs(t *testing.T) {
	t.Run("text is written to a real spec file", func(t *testing.T) {
		dir := t.TempDir()
		origWd, _ := os.Getwd()
		os.Chdir(dir)
		defer os.Chdir(origWd)

		specArgs, err := resolveSpecArgs("Add a login page.", nil, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if len(specArgs) != 1 {
			t.Fatalf("len(specArgs) = %d, want 1", len(specArgs))
		}
		data, err := os.ReadFile(specArgs[0])
		if err != nil {
			t.Fatalf("resolveSpecArgs did not write a readable spec file: %v", err)
		}
		if string(data) != "Add a login page." {
			t.Errorf("spec file content = %q, want the raw text", data)
		}
	})

	t.Run("text and file args combine", func(t *testing.T) {
		dir := t.TempDir()
		origWd, _ := os.Getwd()
		os.Chdir(dir)
		defer os.Chdir(origWd)

		specArgs, err := resolveSpecArgs("inline spec", []string{"a.md", "b.md"}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if len(specArgs) != 3 {
			t.Fatalf("len(specArgs) = %d, want 3 (1 inline + 2 file args)", len(specArgs))
		}
	})

	t.Run("no text, no files: nothing to process", func(t *testing.T) {
		specArgs, err := resolveSpecArgs("", nil, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if len(specArgs) != 0 {
			t.Errorf("specArgs = %v, want empty", specArgs)
		}
	})
}

func TestValidateGraph(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	for _, d := range model.DefaultColumns {
		os.MkdirAll(filepath.Join(kanbanRoot, dirName(d)), 0755)
	}

	t.Run("clean graph reports nothing", func(t *testing.T) {
		if _, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{Title: "Solo ticket"}); err != nil {
			t.Fatal(err)
		}
		if got := validateGraph(); got != "" {
			t.Errorf("validateGraph() = %q, want empty for an acyclic graph with no dangling refs", got)
		}
	})

	t.Run("dangling dependency is reported", func(t *testing.T) {
		dir2 := t.TempDir()
		os.Chdir(dir2)
		for _, d := range model.DefaultColumns {
			os.MkdirAll(filepath.Join(kanbanRoot, dirName(d)), 0755)
		}
		if _, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{
			Title: "Needs a ghost", DependsOn: []string{"does-not-exist"},
		}); err != nil {
			t.Fatal(err)
		}
		got := validateGraph()
		if !strings.Contains(got, "does-not-exist") {
			t.Errorf("validateGraph() = %q, want it to name the dangling id", got)
		}
	})

	t.Run("dependency cycle is reported", func(t *testing.T) {
		dir3 := t.TempDir()
		os.Chdir(dir3)
		for _, d := range model.DefaultColumns {
			os.MkdirAll(filepath.Join(kanbanRoot, dirName(d)), 0755)
		}
		aFile, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{Title: "A"})
		if err != nil {
			t.Fatal(err)
		}
		aCard, _ := model.ReadCard(filepath.Join(kanbanRoot, "100-todo", aFile), "100-todo")
		bFile, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{Title: "B", DependsOn: []string{aCard.ID}})
		if err != nil {
			t.Fatal(err)
		}
		bCard, _ := model.ReadCard(filepath.Join(kanbanRoot, "100-todo", bFile), "100-todo")
		aCard.DependsOn = []string{bCard.ID}
		if err := model.SaveCard(kanbanRoot, "100-todo", aFile, aCard); err != nil {
			t.Fatal(err)
		}

		got := validateGraph()
		if !strings.Contains(got, "cycle") {
			t.Errorf("validateGraph() = %q, want it to report the cycle", got)
		}
	})
}

func dirName(d model.ColumnDef) string {
	return fmt.Sprintf("%d-%s", d.Index, d.Slug)
}

func TestProcessSpec_RerunIsRefusedThenForced(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	for _, d := range model.DefaultColumns {
		os.MkdirAll(filepath.Join(kanbanRoot, dirName(d)), 0755)
	}

	helperDir := t.TempDir()
	callsFile := filepath.Join(helperDir, "calls.txt")
	script := fmt.Sprintf("#!/bin/sh\necho call >> %q\nexit 0\n", callsFile)
	os.WriteFile(filepath.Join(helperDir, "opencode"), []byte(script), 0755)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	specPath := filepath.Join(dir, "onboarding.md")
	os.WriteFile(specPath, []byte("Add onboarding."), 0644)

	ctx := context.Background()
	processSpec(ctx, specPath, nil, defaultPrompt, "opencode", false, io.Discard)
	calls, _ := os.ReadFile(callsFile)
	if n := len(strings.Fields(string(calls))); n != 1 {
		t.Fatalf("agent invoked %d times on first run, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(specsProcessed, "onboarding.md")); err != nil {
		t.Fatalf("spec was not archived after a successful run: %v", err)
	}

	// Re-create a spec with the same name (simulating a copy/edit) and try again without -force.
	os.WriteFile(specPath, []byte("Add onboarding."), 0644)
	processSpec(ctx, specPath, nil, defaultPrompt, "opencode", false, io.Discard)
	calls, _ = os.ReadFile(callsFile)
	if n := len(strings.Fields(string(calls))); n != 1 {
		t.Errorf("agent invoked %d times after an unforced re-run, want 1 (refused)", n)
	}
	if !fileExistsForTest(specPath) {
		t.Error("refused re-run should leave the spec in place, not archive or delete it")
	}

	// With -force, it should actually reprocess.
	processSpec(ctx, specPath, nil, defaultPrompt, "opencode", true, io.Discard)
	calls, _ = os.ReadFile(callsFile)
	if n := len(strings.Fields(string(calls))); n != 2 {
		t.Errorf("agent invoked %d times after a forced re-run, want 2", n)
	}
}

func fileExistsForTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Role/executor resolution itself (fallback-to-default, unknown-executor
// error) is tested once, at its single shared definition:
// internal/orchestrator.TestResolveExecutor. This package only needs to
// confirm Run actually uses it — see TestRun_FailsFastWithNoCommandConfigured
// below.

func TestRun_FailsFastWithNoCommandConfigured(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	err := Run(context.Background(), Options{Text: "A spec."}, io.Discard)
	if err == nil {
		t.Fatal("Run() = nil, want an error when no command is configured")
	}
	if !strings.Contains(err.Error(), "no command configured") {
		t.Errorf("err = %v, want it to mention no command configured", err)
	}
}
