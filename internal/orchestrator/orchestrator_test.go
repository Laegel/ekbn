package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

func mustWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIndexOf(t *testing.T) {
	items := []string{"a", "b", "c"}
	if got := indexOf(items, "b"); got != 1 {
		t.Errorf("indexOf(b) = %d, want 1", got)
	}
	if got := indexOf(items, "z"); got != -1 {
		t.Errorf("indexOf(z) = %d, want -1", got)
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault("", "fallback"); got != "fallback" {
		t.Errorf("orDefault(\"\", fallback) = %q, want fallback", got)
	}
	if got := orDefault("set", "fallback"); got != "set" {
		t.Errorf("orDefault(set, fallback) = %q, want set", got)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.txt")
	os.WriteFile(present, []byte("x"), 0644)
	if !fileExists(present) {
		t.Error("fileExists = false for a file that exists")
	}
	if fileExists(filepath.Join(dir, "absent.txt")) {
		t.Error("fileExists = true for a file that doesn't exist")
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != -1 {
		t.Errorf("exitCode(nil) = %d, want -1", got)
	}
	_, err := exec.Command("sh", "-c", "exit 3").Output()
	if got := exitCode(err); got != 3 {
		t.Errorf("exitCode(exit 3) = %d, want 3", got)
	}
}

func TestSecurityPathsMatch(t *testing.T) {
	t.Run("recursive glob matches a nested path", func(t *testing.T) {
		if !securityPathsMatch([]string{"server/rng/table.ts"}, []string{"**/rng/**"}) {
			t.Error("want match: server/rng/table.ts against **/rng/**")
		}
	})

	t.Run("unrelated path does not match", func(t *testing.T) {
		if securityPathsMatch([]string{"web/App.tsx"}, []string{"**/rng/**"}) {
			t.Error("want no match: web/App.tsx against **/rng/**")
		}
	})

	t.Run("multiple touched files, one matches", func(t *testing.T) {
		touched := []string{"README.md", "server/trpc/summon.ts"}
		if !securityPathsMatch(touched, []string{"server/trpc/**"}) {
			t.Error("want match: server/trpc/summon.ts against server/trpc/**")
		}
	})

	t.Run("empty glob list never matches", func(t *testing.T) {
		if securityPathsMatch([]string{"server/rng/table.ts"}, nil) {
			t.Error("want no match when no globs are configured")
		}
	})
}

func TestTestFilesTouched(t *testing.T) {
	t.Run("go test file", func(t *testing.T) {
		got := testFilesTouched([]string{"model/card.go", "model/card_test.go"})
		if len(got) != 1 || got[0] != "model/card_test.go" {
			t.Errorf("testFilesTouched = %v, want [model/card_test.go]", got)
		}
	})

	t.Run("ts spec file", func(t *testing.T) {
		got := testFilesTouched([]string{"src/app.ts", "src/app.test.ts"})
		if len(got) != 1 || got[0] != "src/app.test.ts" {
			t.Errorf("testFilesTouched = %v, want [src/app.test.ts]", got)
		}
	})

	t.Run("no test files touched", func(t *testing.T) {
		if got := testFilesTouched([]string{"model/card.go", "README.md"}); len(got) != 0 {
			t.Errorf("testFilesTouched = %v, want empty", got)
		}
	})
}

func TestIsConcreteSecurityFinding(t *testing.T) {
	t.Run("reproduction heading with content is concrete", func(t *testing.T) {
		if !isConcreteSecurityFinding("## Reproduction\n\nPOST /api/summon with seed=123 ten times bypasses pity.") {
			t.Error("want concrete: has ## Reproduction with real content")
		}
	})

	t.Run("advisory text without the heading is not concrete", func(t *testing.T) {
		if isConcreteSecurityFinding("Consider adding rate limiting to this endpoint.") {
			t.Error("want not concrete: no ## Reproduction heading")
		}
	})

	t.Run("heading present but nothing after it is not concrete", func(t *testing.T) {
		if isConcreteSecurityFinding("## Reproduction\n\n   ") {
			t.Error("want not concrete: heading with only whitespace after it")
		}
	})

	t.Run("empty output is not concrete", func(t *testing.T) {
		if isConcreteSecurityFinding("") {
			t.Error("want not concrete: empty output")
		}
	})
}

func TestResolveRoleConfig(t *testing.T) {
	roles := map[string]serve.RoleConfig{
		"default":  {Prompt: "You are a general-purpose agent."},
		"frontend": {Prompt: "You are a frontend specialist."},
	}

	t.Run("known role", func(t *testing.T) {
		rc, fellBack := resolveRoleConfig("frontend", roles)
		if fellBack {
			t.Error("fellBack = true, want false for a known role")
		}
		if rc.Prompt != "You are a frontend specialist." {
			t.Errorf("rc.Prompt = %q, want the frontend prompt", rc.Prompt)
		}
	})

	t.Run("unknown role falls back to default", func(t *testing.T) {
		rc, fellBack := resolveRoleConfig("nonexistent-role", roles)
		if !fellBack {
			t.Error("fellBack = false, want true for an unknown role")
		}
		if rc.Prompt != "You are a general-purpose agent." {
			t.Errorf("rc.Prompt = %q, want the default prompt", rc.Prompt)
		}
	})

	t.Run("empty role falls back to default", func(t *testing.T) {
		rc, fellBack := resolveRoleConfig("", roles)
		if !fellBack {
			t.Error("fellBack = false, want true for an unset role")
		}
		if rc.Prompt != "You are a general-purpose agent." {
			t.Errorf("rc.Prompt = %q, want the default prompt", rc.Prompt)
		}
	})

	t.Run("no roles configured preserves current behavior", func(t *testing.T) {
		rc, fellBack := resolveRoleConfig("frontend", map[string]serve.RoleConfig{})
		if fellBack {
			t.Error("fellBack = true, want false when roles is unconfigured entirely")
		}
		if rc.Prompt != "" || len(rc.Tools) != 0 || len(rc.Skills) != 0 {
			t.Errorf("rc = %+v, want zero value when roles is unconfigured", rc)
		}
	})
}

// initGitRepo makes dir (the current directory) a git repo with one commit.
func initGitRepo(t *testing.T) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile("README.md", []byte("initial\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
}

// ---------------------------------------------------------------------------
// Scheduling: ready-set selection, capacity, and lease reclamation.

func setupKanban(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	ensureKanbanDirs()
	return dir
}

func TestCountInProgress(t *testing.T) {
	cards := []model.Card{
		{ID: "a", Status: model.StatusInProgress},
		{ID: "b", Status: model.StatusTodo},
		{ID: "c", Status: model.StatusInProgress},
	}
	if got := countInProgress(cards); got != 2 {
		t.Errorf("countInProgress = %d, want 2", got)
	}
}

func TestReclaimExpiredLeases(t *testing.T) {
	setupKanban(t)

	filename, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{Title: "Leased", Author: "test", Content: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.TransitionStatus(kanbanRoot, "100-todo", filename, model.StatusInProgress, ""); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(kanbanRoot, "200-in-progress", filename)
	card, err := model.ReadCard(path, "200-in-progress")
	if err != nil {
		t.Fatal(err)
	}
	card.LeaseExpires = time.Now().Add(-time.Minute).Format(time.RFC3339)
	if err := model.SaveCard(kanbanRoot, "200-in-progress", filename, card); err != nil {
		t.Fatal(err)
	}

	cards, err := model.LoadAllCards(kanbanRoot)
	if err != nil {
		t.Fatal(err)
	}
	reclaimExpiredLeases(cards)

	if fileExists(path) {
		t.Error("card with an expired lease should have moved out of in-progress")
	}
	got, err := model.ReadCard(filepath.Join(kanbanRoot, "400-blocked", filename), "400-blocked")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusBlocked || got.Reason != "lease-expired" {
		t.Errorf("Status/Reason = %q/%q, want blocked/lease-expired", got.Status, got.Reason)
	}
}

func TestReclaimExpiredLeases_LiveLeaseUntouched(t *testing.T) {
	setupKanban(t)

	filename, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{Title: "Leased", Author: "test", Content: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.TransitionStatus(kanbanRoot, "100-todo", filename, model.StatusInProgress, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(kanbanRoot, "200-in-progress", filename)
	card, _ := model.ReadCard(path, "200-in-progress")
	card.LeaseExpires = time.Now().Add(time.Hour).Format(time.RFC3339)
	model.SaveCard(kanbanRoot, "200-in-progress", filename, card)

	cards, _ := model.LoadAllCards(kanbanRoot)
	reclaimExpiredLeases(cards)

	if !fileExists(path) {
		t.Error("card with a live lease should not be touched")
	}
}

func TestSelectReadyTickets(t *testing.T) {
	setupKanban(t)

	mkTicket := func(title string, priority int) string {
		filename, err := model.CreateCard(kanbanRoot, "100-todo", model.CardFields{Title: title, Author: "test", Content: "body", Priority: priority})
		if err != nil {
			t.Fatal(err)
		}
		return filename
	}

	mkTicket("Low priority", 3)
	time.Sleep(10 * time.Millisecond)
	mkTicket("High priority", 0)

	cards, err := model.LoadAllCards(kanbanRoot)
	if err != nil {
		t.Fatal(err)
	}

	ready := selectReadyTickets(cards, 10)
	if len(ready) != 2 {
		t.Fatalf("len(ready) = %d, want 2", len(ready))
	}
	if ready[0].Priority != 0 {
		t.Errorf("ready[0].Priority = %d, want 0 (higher priority first)", ready[0].Priority)
	}

	t.Run("capacity limits how many are returned", func(t *testing.T) {
		ready := selectReadyTickets(cards, 1)
		if len(ready) != 1 {
			t.Fatalf("len(ready) = %d, want 1", len(ready))
		}
		if ready[0].Priority != 0 {
			t.Error("the single slot should go to the higher-priority ticket")
		}
	})

	t.Run("zero capacity returns nothing", func(t *testing.T) {
		if ready := selectReadyTickets(cards, 0); len(ready) != 0 {
			t.Errorf("selectReadyTickets(cards, 0) = %v, want empty", ready)
		}
	})
}

func TestSelectReadyTickets_CycleBlocksAllScheduling(t *testing.T) {
	cards := []model.Card{
		{ID: "a", Status: model.StatusTodo, DependsOn: []string{"b"}},
		{ID: "b", Status: model.StatusTodo, DependsOn: []string{"a"}},
		{ID: "c", Status: model.StatusTodo},
	}
	if got := selectReadyTickets(cards, 10); got != nil {
		t.Errorf("selectReadyTickets with a cycle present = %v, want nil (refuse to schedule anything)", got)
	}
}
