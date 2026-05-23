package main

import (
	"strings"
	"testing"
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
		if strings.Contains(prompt, bp) {
			t.Errorf("prompt contains blocked pattern %q — agent must not know about filesystem paths", bp)
		}
	}
}

func TestPromptUsesMCPTools(t *testing.T) {
	if !strings.Contains(prompt, "list_columns") {
		t.Error("prompt must mention list_columns tool")
	}
	if !strings.Contains(prompt, "create_card") {
		t.Error("prompt must mention create_card tool")
	}
}

func TestPromptExplicitSteps(t *testing.T) {
	// The prompt should give numbered steps, not leave the agent guessing.
	if !strings.Contains(prompt, "1.") {
		t.Error("prompt should contain numbered steps")
	}
	if !strings.Contains(prompt, "call") && !strings.Contains(prompt, "Call") {
		t.Error("prompt should tell the agent to call tools")
	}
}

func TestPromptColumnSlug(t *testing.T) {
	// The agent should discover the column slug dynamically, not hardcode "todo".
	if strings.Contains(prompt, `"todo"`) || strings.Contains(prompt, "'todo'") {
		t.Error("prompt should not hardcode 'todo' column slug — agent must discover via list_columns")
	}
}

func TestLine40(t *testing.T) {
	got := line40()
	if len(got) != 40 {
		t.Errorf("line40() length = %d, want 40", len(got))
	}
}

func TestExitCode(t *testing.T) {
	t.Run("nil error returns -1", func(t *testing.T) {
		if got := exitCode(nil); got != -1 {
			t.Errorf("exitCode(nil) = %d, want -1", got)
		}
	})
}
