package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

// fixSetup builds a temp git repo with the kanban columns and a config using
// a single-stage "feature" flow (so a ticket reaches its terminal state in
// one runTicket call) and the given verify command. Returns the repo dir.
func fixSetup(t *testing.T, verify string) string {
	t.Helper()
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	initGitRepo(t)
	cfg := "verify: \"" + verify + "\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [work]\n    max_rounds: 2\n" +
		testRolesConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()
	return dir
}

// testRolesConfig configures the default (implementer) and reviewer roles to
// invoke the same fake "opencode" shim (installed on PATH by
// fakeAgentAndReviewer), distinguished by a marker argument the shim itself
// checks — "implement" vs "review" — since that's now entirely up to
// whatever command each role configures, not a flag ekbn adds.
const testRolesConfig = "roles:\n  default:\n    command: opencode implement\n  reviewer:\n    command: opencode review\n"

// fakeAgentAndReviewer installs an "opencode" shim on PATH serving both
// configured roles (see testRolesConfig). In implement mode ($1 ==
// "implement") it appends to callsFile and runs agentBody (with cwd inside
// the project's own working directory); otherwise it is the reviewer,
// recording its prompt (now argument $2 — the marker is $1) and emitting
// findingsFile's content if that file exists.
func fakeAgentAndReviewer(t *testing.T, agentBody, findingsFile string) (callsFile, promptFile string) {
	t.Helper()
	helperDir := t.TempDir()
	callsFile = filepath.Join(helperDir, "agent-calls.txt")
	promptFile = filepath.Join(helperDir, "review-prompt.txt")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"implement\" ]; then\n" +
		"  echo call >> " + shq(callsFile) + "\n" +
		agentBody + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf '%s' \"$2\" > " + shq(promptFile) + "\n" +
		"[ -f " + shq(findingsFile) + " ] && cat " + shq(findingsFile) + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(helperDir, "opencode"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return callsFile, promptFile
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// countingAgentBody returns a shell body creating file1.txt on the first
// call, file2.txt on the second, and so on. The counter lives outside the
// repo so the orchestrator's own commit never touches it.
func countingAgentBody(t *testing.T) string {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "n")
	return "  n=$(cat " + shq(counter) + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + shq(counter) + "; touch \"file$n.txt\""
}

// firstCallOnlyAgentBody returns a shell body creating file1.txt on the
// first call and doing nothing on every later call — simulating an agent
// whose round-1 work already satisfies the ticket, so it makes no further
// changes on subsequent rounds. The counter lives outside the repo, same as
// countingAgentBody.
func firstCallOnlyAgentBody(t *testing.T) string {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "n")
	return "  n=$(cat " + shq(counter) + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + shq(counter) + "; if [ \"$n\" -eq 1 ]; then touch file1.txt; fi"
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func writeCard(t *testing.T, path, title, id string) {
	t.Helper()
	mustWrite(t, path, "---\ntitle: "+title+"\nid: "+id+"\nstatus: todo\n---\n\nDo the thing.\n")
}

func mustReadCard(t *testing.T, path string) model.Card {
	t.Helper()
	dir, filename := filepath.Split(path)
	column := filepath.Base(filepath.Clean(dir))
	card, err := model.ReadCard(path, column)
	if err != nil {
		t.Fatalf("reading card %s: %v", filename, err)
	}
	return card
}

func TestFix_SuccessAdvancesToReview(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t))

	if fileExists(".kanban/200-in-progress/card.md") {
		t.Error("card still in 200-in-progress after a successful run")
	}
	if !fileExists(".kanban/250-review/card.md") {
		t.Fatal("card did not reach 250-review")
	}
	card := mustReadCard(t, ".kanban/250-review/card.md")
	if card.Status != model.StatusReview {
		t.Errorf("Status = %q, want %q", card.Status, model.StatusReview)
	}
	if card.Reason != "verify-green" {
		t.Errorf("Reason = %q, want verify-green", card.Reason)
	}
	// The agent's work must actually have been committed.
	if !fileExists("feature.txt") {
		t.Error("feature.txt not present after a successful run — the ticket's commit never landed")
	}
}

// TestFix_RunAgentAttemptSubstitutesWorkdirTemplate confirms {workdir} in a
// role's command is replaced with the actual directory the agent is meant
// to run in — needed because cmd.Dir alone isn't always respected by an
// agent CLI's own project-resolution (see the comment in runAgentAttempt).
func TestFix_RunAgentAttemptSubstitutesWorkdirTemplate(t *testing.T) {
	dir := t.TempDir()
	helperDir := t.TempDir()
	argsFile := filepath.Join(helperDir, "args.txt")
	script := "#!/bin/sh\necho \"$1\" > " + shq(argsFile) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(helperDir, "fakecmd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	command := filepath.Join(helperDir, "fakecmd") + " {workdir}"
	if _, err, _, _, _, _ := runAgentAttempt("prompt", dir, command, 0, 0); err != nil {
		t.Fatalf("runAgentAttempt failed: %v", err)
	}

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("shim never ran: %v", err)
	}
	if strings.TrimSpace(string(got)) != dir {
		t.Errorf("{workdir} substituted to %q, want %q", strings.TrimSpace(string(got)), dir)
	}
}

// TestFix_RunAgentAttemptIdleTimeoutKillsAndReportsStall is the precise,
// fast test of the idle watchdog mechanism itself: a shim that writes one
// line then goes silent for far longer than idleTimeout must be killed
// promptly (well before its own sleep would end), reported as idleTimedOut
// (not timedOut — no maxDuration is set here at all), distinguishing "the
// agent stopped producing output" from "the agent simply ran long."
func TestFix_RunAgentAttemptIdleTimeoutKillsAndReportsStall(t *testing.T) {
	dir := t.TempDir()
	helperDir := t.TempDir()
	script := "#!/bin/sh\necho started\nsleep 5\necho should-not-reach-here\n"
	if err := os.WriteFile(filepath.Join(helperDir, "stallcmd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	command := filepath.Join(helperDir, "stallcmd")
	start := time.Now()
	output, _, _, _, timedOut, idleTimedOut := runAgentAttempt("prompt", dir, command, 0, 100*time.Millisecond)
	elapsed := time.Since(start)

	if !idleTimedOut {
		t.Error("idleTimedOut = false, want true — the shim went silent for far longer than idleTimeout")
	}
	if timedOut {
		t.Error("timedOut = true, want false — no maxDuration was configured")
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v — the idle watchdog should have killed the shim well before its 5s sleep ended", elapsed)
	}
	if !strings.Contains(output, "started") {
		t.Errorf("output = %q, want it to contain the shim's output before it stalled", output)
	}
}

// TestFix_AgentEscapedWorktreeBlocksTicket simulates the exact bug worktree
// isolation was removed over previously: an agent writing outside its
// assigned worktree, directly into the shared main checkout. The escape
// detector must catch this and block the ticket rather than let it proceed
// as if nothing happened.
func TestFix_AgentEscapedWorktreeBlocksTicket(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	escapeBody := "  touch " + shq(filepath.Join(dir, "escaped.txt"))
	fakeAgentAndReviewer(t, escapeBody, filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")
	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("card did not block after the agent escaped its worktree")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "agent-escaped-worktree" {
		t.Errorf("Reason = %q, want agent-escaped-worktree", card.Reason)
	}
	if !strings.Contains(card.Content, "escaped.txt") {
		t.Errorf("blocked card's findings should mention the escaped file: %q", card.Content)
	}
	if want := worktreeDir(dir, "1"); card.Worktree != want {
		t.Errorf("Worktree = %q, want %q — should stay visible on a blocked card", card.Worktree, want)
	}
}

// TestFix_AgentUsedGitRecordsTheActualCommand guards against a real gap: the
// git shim only ever recorded *that* a disallowed command ran, via an empty
// touch, not *what* it was — leaving every "agent-used-git" block a guess.
// The blocked card's findings should now name the exact command.
func TestFix_AgentUsedGitRecordsTheActualCommand(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  git commit -am 'sneaky'", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")
	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("card did not block after the agent used git directly")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "agent-used-git" {
		t.Errorf("Reason = %q, want agent-used-git", card.Reason)
	}
	if !strings.Contains(card.Content, "git commit -am") {
		t.Errorf("blocked card's findings should name the actual command that was run: %q", card.Content)
	}
}

// TestFix_MergeRemovesWorktreeAndBranch confirms that reaching a terminal
// state doesn't just move the commit onto main — it also tears down the
// ticket's worktree and branch, so nothing lingers once a ticket finishes.
func TestFix_MergeRemovesWorktreeAndBranch(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "9")
	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/250-review/card.md") {
		t.Fatal("card did not reach 250-review")
	}
	if fileExists(worktreeDir(dir, "9")) {
		t.Error("worktree directory should have been removed after merging")
	}
	if branches := gitOut(t, "branch", "--list", worktreeBranch("9")); branches != "" {
		t.Errorf("branch %q should have been deleted after merging, got %q", worktreeBranch("9"), branches)
	}
	if log := gitOut(t, "log", "--oneline"); !strings.Contains(log, "9: Test") {
		t.Errorf("main branch log should contain the ticket's commit: %q", log)
	}
	card := mustReadCard(t, ".kanban/250-review/card.md")
	if card.Worktree != "" {
		t.Errorf("Worktree = %q, want empty once the worktree is merged and removed", card.Worktree)
	}
}

// TestFix_MergeRebasesOntoNewMainWhenSecondTicketFinishesLater is a narrow
// unit test of mergeAndRemoveWorktree itself, with no agent/runCycle
// machinery: two tickets' worktrees both branch from the same mainHead — the
// situation any two tickets running concurrently are in. Ticket A merges
// first (a trivial fast-forward); ticket B's branch is no longer a
// fast-forward once A has landed, so its merge must rebase onto main's new
// tip and retry rather than simply failing.
func TestFix_MergeRebasesOntoNewMainWhenSecondTicketFinishesLater(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	initGitRepo(t)
	mainHead := gitOut(t, "rev-parse", "HEAD")

	wtA, err := ensureWorktree(dir, "A", mainHead)
	if err != nil {
		t.Fatalf("ensureWorktree(A): %v", err)
	}
	mustWrite(t, filepath.Join(wtA, "fileA.txt"), "a\n")
	if _, err := git(wtA, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(wtA, "commit", "-m", "A"); err != nil {
		t.Fatal(err)
	}

	wtB, err := ensureWorktree(dir, "B", mainHead)
	if err != nil {
		t.Fatalf("ensureWorktree(B): %v", err)
	}
	mustWrite(t, filepath.Join(wtB, "fileB.txt"), "b\n")
	if _, err := git(wtB, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(wtB, "commit", "-m", "B"); err != nil {
		t.Fatal(err)
	}

	if err := mergeAndRemoveWorktree(dir, "A"); err != nil {
		t.Fatalf("merging A (trivial fast-forward): %v", err)
	}
	if err := mergeAndRemoveWorktree(dir, "B"); err != nil {
		t.Fatalf("merging B should have rebased onto main and retried, got: %v", err)
	}

	if !fileExists(filepath.Join(dir, "fileA.txt")) {
		t.Error("fileA.txt missing from main after both merges")
	}
	if !fileExists(filepath.Join(dir, "fileB.txt")) {
		t.Error("fileB.txt missing from main after both merges")
	}
}

// TestFix_MergeReturnsClearErrorOnRebaseConflict confirms a genuine conflict
// between two concurrent tickets' changes is surfaced as an error rather
// than silently resolved or corrupting the shared repo: the rebase aborts
// cleanly, and the losing ticket's worktree/branch are left in place for a
// human to resolve manually — nothing is discarded.
func TestFix_MergeReturnsClearErrorOnRebaseConflict(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	initGitRepo(t)
	mustWrite(t, filepath.Join(dir, "shared.txt"), "base\n")
	if _, err := git(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(dir, "commit", "-m", "seed shared.txt"); err != nil {
		t.Fatal(err)
	}
	mainHead := gitOut(t, "rev-parse", "HEAD")

	wtA, err := ensureWorktree(dir, "A", mainHead)
	if err != nil {
		t.Fatalf("ensureWorktree(A): %v", err)
	}
	mustWrite(t, filepath.Join(wtA, "shared.txt"), "from-a\n")
	if _, err := git(wtA, "commit", "-am", "A edits shared.txt"); err != nil {
		t.Fatal(err)
	}

	wtB, err := ensureWorktree(dir, "B", mainHead)
	if err != nil {
		t.Fatalf("ensureWorktree(B): %v", err)
	}
	mustWrite(t, filepath.Join(wtB, "shared.txt"), "from-b\n")
	if _, err := git(wtB, "commit", "-am", "B edits shared.txt"); err != nil {
		t.Fatal(err)
	}

	if err := mergeAndRemoveWorktree(dir, "A"); err != nil {
		t.Fatalf("merging A (trivial fast-forward): %v", err)
	}

	if err := mergeAndRemoveWorktree(dir, "B"); err == nil {
		t.Fatal("expected an error when B's rebase conflicts with A's already-merged change")
	}

	if !fileExists(wtB) {
		t.Error("ticket B's worktree should remain after a failed merge, not be discarded")
	}
	if out, _ := git(wtB, "status", "--short"); strings.Contains(out, "UU") {
		t.Errorf("rebase conflict should have been aborted, not left unresolved: status %q", out)
	}
}

// TestFix_DoneOnCleanReviewSkipsReviewStage confirms the opt-in
// done-on-clean-review config flag lets a ticket with no Acceptance
// criteria go straight to done on a clean reviewer pass, instead of
// stopping at review for a human to promote manually. The off-by-default
// case (flag unset) is already covered by TestFix_SuccessAdvancesToReview.
func TestFix_DoneOnCleanReviewSkipsReviewStage(t *testing.T) {
	fixSetup(t, "exit 0")
	f, err := os.OpenFile("ekbn.config.yml", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("done-on-clean-review: true\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t))

	if fileExists(".kanban/250-review/card.md") {
		t.Error("card should not stop at 250-review when done-on-clean-review is set")
	}
	if !fileExists(".kanban/300-done/card.md") {
		t.Fatal("card did not reach 300-done")
	}
	card := mustReadCard(t, ".kanban/300-done/card.md")
	if card.Status != model.StatusDone {
		t.Errorf("Status = %q, want %q", card.Status, model.StatusDone)
	}
	if card.Reason != "verify-green" {
		t.Errorf("Reason = %q, want verify-green", card.Reason)
	}
}

// TestFix_ProseAcceptanceDoesNotBlock guards against a real bug: Acceptance
// is documented as either a real command or descriptive prose, but a prose
// value handed to sh -c fails with "command not found" (exit 127) and used
// to block the ticket outright. Prose should behave exactly like no
// acceptance at all — reviewed, not blocked, not silently auto-done.
func TestFix_ProseAcceptanceDoesNotBlock(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	mustWrite(t, ".kanban/100-todo/card.md",
		"---\ntitle: Test\nid: 1\nstatus: todo\nacceptance: \"Prose: some criteria are met\"\n---\n\nDo the thing.\n")

	runCycle(loadTestConfig(t))

	if fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("card should not block on prose acceptance text")
	}
	if !fileExists(".kanban/250-review/card.md") {
		t.Fatal("card with prose acceptance should land in review, same as no acceptance at all")
	}
}

// TestFix_PassingAcceptanceStillGoesDone is a regression check: a real,
// passing acceptance command must still auto-promote the ticket to done.
func TestFix_PassingAcceptanceStillGoesDone(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	mustWrite(t, ".kanban/100-todo/card.md",
		"---\ntitle: Test\nid: 1\nstatus: todo\nacceptance: \"exit 0\"\n---\n\nDo the thing.\n")

	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/300-done/card.md") {
		t.Fatal("a real, passing acceptance command should still auto-promote the ticket to done")
	}
}

// TestFix_FailingAcceptanceStillBlocks is a regression check: a real
// command that genuinely fails (not "command not found") must still block.
func TestFix_FailingAcceptanceStillBlocks(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	mustWrite(t, ".kanban/100-todo/card.md",
		"---\ntitle: Test\nid: 1\nstatus: todo\nacceptance: \"exit 1\"\n---\n\nDo the thing.\n")

	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("a real, failing acceptance command should still block the ticket")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "acceptance-check-failed" {
		t.Errorf("Reason = %q, want acceptance-check-failed", card.Reason)
	}
}

// TestFix_PreexistingUntrackedFileNotCommitted guards against a real bug:
// a naive `git add -A` at commit time, run inside a ticket's own worktree,
// sweeps in whatever untracked scaffolding an agent CLI drops there on its
// own (its own config file, in this case) right along with the agent's
// actual work — worktree isolation from the rest of the project doesn't by
// itself keep a single worktree free of that kind of incidental clutter.
func TestFix_PreexistingUntrackedFileNotCommitted(t *testing.T) {
	fixSetup(t, "exit 0")
	os.WriteFile("opencode.json", []byte(`{"mcp":{}}`), 0644)
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")
	runCycle(loadTestConfig(t))

	if !fileExists("feature.txt") {
		t.Fatal("feature.txt not present — the ticket's commit never landed")
	}
	files, err := commitDiffFiles(gitOut(t, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "opencode.json" {
			t.Error("opencode.json (preexisting, unrelated to the ticket) was committed alongside the agent's work")
		}
	}
	if !fileExists("opencode.json") {
		t.Error("opencode.json should still be present on disk, just not committed")
	}
}

// TestFix_WIPLimitClampedToOne asserts that, even with a higher WIPLimit
// configured, capacity is clamped to 1: each ticket has its own worktree
// again, but merging one back into main on completion only handles a
// fast-forward, which assumes tickets finish in the order they were
// claimed — real concurrency needs its own conflict/rebase story first.
// TestFix_WIPLimitTwoRunsBothConcurrently confirms raising wip-limit actually
// lets two tickets run in the same poll cycle — each in its own worktree —
// and both reach their terminal state, including merging both commits back
// into main (exercising mergeAndRemoveWorktree's rebase-retry path for
// whichever of the two merges second, since both branch from the same
// mainHead).
func TestFix_WIPLimitTwoRunsBothConcurrently(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, countingAgentBody(t), filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/a.md", "First", "1")
	writeCard(t, ".kanban/100-todo/b.md", "Second", "2")

	cfg := loadTestConfig(t)
	cfg.WIPLimit = 2
	runCycle(cfg)

	if !fileExists(".kanban/250-review/a.md") {
		t.Error("a.md did not reach 250-review")
	}
	if !fileExists(".kanban/250-review/b.md") {
		t.Error("b.md did not reach 250-review — both tickets should run concurrently with wip-limit 2")
	}
	log := gitOut(t, "log", "--oneline")
	if !strings.Contains(log, "1: First") {
		t.Errorf("main branch log should contain ticket 1's commit: %q", log)
	}
	if !strings.Contains(log, "2: Second") {
		t.Errorf("main branch log should contain ticket 2's commit: %q", log)
	}
}

func TestFix_DeclaredBlockedMidRunIsRespected(t *testing.T) {
	fixSetup(t, "exit 0")

	// Simulates declare_blocked: writes the card file directly via an
	// absolute path, standing in for the MCP tool call a real agent would
	// make instead of touching .kanban itself.
	abs, err := filepath.Abs(".kanban/200-in-progress/card.md")
	if err != nil {
		t.Fatal(err)
	}
	body := "  printf -- '---\\ntitle: Test\\nid: 3\\nstatus: blocked\\nreason: needs-design-input\\n---\\n\\nstuck\\n' > " + shq(abs)
	fakeAgentAndReviewer(t, body, filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "3")

	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/200-in-progress/card.md") {
		t.Fatal("declared-blocked card should stay wherever declare_blocked put it (200-in-progress in this test double)")
	}
	card := mustReadCard(t, ".kanban/200-in-progress/card.md")
	if card.Status != model.StatusBlocked {
		t.Errorf("Status = %q, want blocked (declared mid-run, not overwritten)", card.Status)
	}
	if card.Reason != "needs-design-input" {
		t.Errorf("Reason = %q, want needs-design-input", card.Reason)
	}
}

func TestFix_NoRetryOnVerifyFailure(t *testing.T) {
	fixSetup(t, "exit 1")
	calls, _ := fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "4")
	runCycle(loadTestConfig(t))

	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(string(data))); n != 1 {
		t.Errorf("agent invoked %d times, want 1 — a verify failure must not retry", n)
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Status != model.StatusBlocked || card.Reason != "verify-failed" {
		t.Errorf("Status/Reason = %q/%q, want blocked/verify-failed", card.Status, card.Reason)
	}
}

func TestFix_ReviewerSeesOnlyItsOwnCard(t *testing.T) {
	fixSetup(t, "exit 0")
	_, prompt := fakeAgentAndReviewer(t, countingAgentBody(t), filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/a.md", "First", "1")
	writeCard(t, ".kanban/100-todo/b.md", "Second", "2")

	cfg := loadTestConfig(t)
	runCycle(cfg) // processes one of the two (WIP limit 1)
	runCycle(cfg) // processes the other

	data, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "file2.txt") {
		t.Error("second card's reviewer prompt does not mention its own file")
	}
	if strings.Contains(got, "file1.txt") {
		t.Error("second card's reviewer prompt contains the first card's file — the diff is accumulating")
	}
}

// TestFix_ReviewerSeesCumulativeDiffAcrossRounds guards against the inverse
// bug from TestFix_ReviewerSeesOnlyItsOwnCard above: a round-2+ reviewer,
// given a stage whose latest round made no new commit (because round 1's
// work already satisfies the ticket), must still see round 1's real,
// already-committed work — not an empty diff.
func TestFix_ReviewerSeesCumulativeDiffAcrossRounds(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	findings := filepath.Join(dir, "findings.txt")
	os.WriteFile(findings, []byte("## Concern\n\nneeds another look"), 0644)
	_, prompt := fakeAgentAndReviewer(t, firstCallOnlyAgentBody(t), findings)

	writeCard(t, ".kanban/100-todo/card.md", "Test", "7")
	cfg := loadTestConfig(t) // max_rounds: 2 for the "work" stage

	runCycle(cfg) // round 1: reviewer finds issues, cycles back
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Round != 1 {
		t.Fatalf("Round = %d, want 1", card.Round)
	}

	runCycle(cfg) // round 2: agent makes no new commit; reviewer must still see file1.txt
	data, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "file1.txt") {
		t.Error("round-2 reviewer prompt does not mention file1.txt — it was handed an empty diff instead of the cumulative one")
	}
}

// TestFix_EmptyDiffBlocksWithoutInvokingReviewer guards against asking the
// reviewer to judge a diff ekbn itself already knows is empty: an agent that
// makes no changes at all must block immediately and deterministically,
// without ever invoking the reviewer — whose only possible response to an
// empty diff ("there's nothing here") is not guaranteed to avoid the
// "## Concern" heading, since the model isn't deterministic about that.
func TestFix_EmptyDiffBlocksWithoutInvokingReviewer(t *testing.T) {
	fixSetup(t, "exit 0")
	_, prompt := fakeAgentAndReviewer(t, "  :", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "8")
	runCycle(loadTestConfig(t))

	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "no-changes-to-review" {
		t.Errorf("Reason = %q, want no-changes-to-review", card.Reason)
	}
	if fileExists(prompt) {
		t.Error("reviewer was invoked despite an empty diff — it should have been skipped entirely")
	}
}

// TestFix_AgentDeclaredNoChangesNeededPassesCleanly guards the companion
// case: an implementer that explicitly states, via the "## No Changes
// Needed" marker, that it looked at the code and nothing needs to change
// must pass through to review — not block — and must never invoke the
// reviewer either, since there's still nothing to review.
func TestFix_AgentDeclaredNoChangesNeededPassesCleanly(t *testing.T) {
	fixSetup(t, "exit 0")
	agentBody := `  printf '## No Changes Needed\n\nThe feature already exists and passes verify.'`
	_, prompt := fakeAgentAndReviewer(t, agentBody, filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "9")
	runCycle(loadTestConfig(t))

	card := mustReadCard(t, ".kanban/250-review/card.md")
	if card.Reason != "no-changes-needed" {
		t.Errorf("Reason = %q, want no-changes-needed", card.Reason)
	}
	if fileExists(prompt) {
		t.Error("reviewer was invoked despite the agent explicitly declaring no changes needed")
	}
}

func TestFix_AttemptRefOnVerifyFailure(t *testing.T) {
	fixSetup(t, "exit 1")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	headBefore := gitOut(t, "rev-parse", "HEAD")
	writeCard(t, ".kanban/100-todo/card.md", "Test", "5")
	runCycle(loadTestConfig(t))

	if refs := gitOut(t, "for-each-ref", "--format=%(refname)", "refs/attempts"); refs == "" {
		t.Error("no refs/attempts/* created — the failed attempt was discarded")
	}
	if fileExists("feature.txt") {
		t.Error("feature.txt present on the main branch — a failed attempt must never merge")
	}
	if got := gitOut(t, "rev-parse", "HEAD"); got != headBefore {
		t.Error("HEAD moved on a failed card")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.AttemptRef == "" {
		t.Error("attempt_ref not recorded on the blocked card")
	}
	// The working directory must be exactly as clean as before the attempt.
	if gitTreeDirty(".") {
		t.Error("working tree left dirty after a failed attempt was reverted")
	}
}

func TestFix_MultiStageAdvancement(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [implement, gates]\n    max_rounds: 2\n" +
		testRolesConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()

	fakeAgentAndReviewer(t, countingAgentBody(t), filepath.Join(t.TempDir(), "no-findings"))
	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t)) // stage 1: implement
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("after finishing a non-terminal stage, the card should return to todo for the next stage")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "gates" {
		t.Errorf("Stage = %q, want %q", card.Stage, "gates")
	}
	if card.Round != 0 {
		t.Errorf("Round = %d, want 0 (reset on stage advance)", card.Round)
	}
	if fileExists("file1.txt") {
		t.Error("the implement stage's commit isn't merged yet — it should not be on the main branch until the ticket reaches its terminal state")
	}
	if !fileExists(filepath.Join(worktreeDir(dir, "1"), "file1.txt")) {
		t.Error("the implement stage's commit should be sitting in the ticket's own worktree, isolated from main")
	}

	runCycle(loadTestConfig(t)) // stage 2: gates (last stage) -> review
	if !fileExists(".kanban/250-review/card.md") {
		t.Fatal("after finishing the last stage, the card should reach review")
	}
	if !fileExists("file1.txt") {
		t.Error("the implement stage's commit should be on the main branch now that the ticket has merged")
	}
	if !fileExists("file2.txt") {
		t.Error("the gates stage's commit should already be on the main branch")
	}
}

// TestFix_ReviewerGetsStageContextForNonFinalStage guards against a real
// bug: a reviewer given only the ticket's full body has no way to know a
// multi-stage flow exists, so it judges an early stage's diff against the
// ticket's overall acceptance criteria and flags legitimately-incomplete
// work as a concern. This confirms the reviewer prompt now says otherwise.
func TestFix_ReviewerGetsStageContextForNonFinalStage(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [reproduce, fix]\n    max_rounds: 2\n" +
		testRolesConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()

	_, promptFile := fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))
	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t))

	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("reviewer was never invoked: %v", err)
	}
	got := string(prompt)
	if !strings.Contains(got, `this diff is only stage "reproduce" (1 of 2)`) {
		t.Errorf("reviewer prompt missing stage context: %q", got)
	}
	if !strings.Contains(got, "Do not expect the ticket's overall acceptance criteria to be met yet") {
		t.Errorf("reviewer prompt missing the don't-expect-full-criteria guidance: %q", got)
	}
}

// TestFix_ReviewerGetsNoStageContextForSingleStageFlow confirms the common
// case (a single-stage flow) is unaffected — no stage-context text at all.
func TestFix_ReviewerGetsNoStageContextForSingleStageFlow(t *testing.T) {
	fixSetup(t, "exit 0")
	_, promptFile := fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))
	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t))

	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("reviewer was never invoked: %v", err)
	}
	if strings.Contains(string(prompt), "This ticket is worked in stages") {
		t.Errorf("single-stage flow should get no stage context, got: %q", string(prompt))
	}
}

// TestFix_BuildSystemPromptFallsBackToDotAgentsDir guards against a real
// gap: AGENTS.md is a common enough convention to live at the project root,
// but .agents/AGENTS.md is also common, and ekbn only ever checked the
// former — silently running every ticket without base instructions for any
// project using the latter layout, with nothing but a log line to notice.
func TestFix_BuildSystemPromptFallsBackToDotAgentsDir(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	if err := os.Mkdir(".agents", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".agents", "AGENTS.md"), []byte("project base instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	prompt := buildSystemPrompt("card.md", model.Card{}, serve.RoleConfig{}, serve.StageFlow{})
	if !strings.Contains(prompt, "project base instructions") {
		t.Errorf("prompt should fall back to .agents/AGENTS.md when AGENTS.md is absent, got: %q", prompt)
	}
}

// TestFix_PromptExplicitlyForbidsMutatingGit guards against a real gap: the
// agent only ever discovered the git restriction reactively, by tripping the
// read-only git shim and getting the whole ticket blocked outright with no
// chance to correct course. The prompt should say so upfront instead.
func TestFix_PromptExplicitlyForbidsMutatingGit(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	prompt := buildSystemPrompt("card.md", model.Card{}, serve.RoleConfig{}, serve.StageFlow{})
	if !strings.Contains(prompt, "Do not run git commands that write or mutate") {
		t.Errorf("prompt should explicitly forbid mutating git commands, got: %q", prompt)
	}
	if !strings.Contains(prompt, "status") || !strings.Contains(prompt, "diff") {
		t.Errorf("prompt should name at least some allowed read-only git subcommands, got: %q", prompt)
	}
}

// TestFix_NoChangesNeededInstructionIsStageAware guards against a real bug:
// the implementer's final stage often has nothing left to change (earlier
// stages already did the work), but a generic "say so if nothing needs to
// change" instruction didn't reliably get the agent to use the "## No
// Changes Needed" marker there, leaving the ticket blocked with a low-value
// message instead. The final stage's prompt must reassure the agent this is
// a normal, expected outcome; a non-final stage's prompt must not.
func TestFix_NoChangesNeededInstructionIsStageAware(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [reproduce, fix]\n    max_rounds: 2\n" +
		testRolesConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()

	agentPromptFile := filepath.Join(t.TempDir(), "agent-prompt.txt")
	agentBody := "  printf '%s' \"$2\" > " + shq(agentPromptFile) + "\n  touch file1.txt"
	fakeAgentAndReviewer(t, agentBody, filepath.Join(t.TempDir(), "no-findings"))
	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t)) // stage 1: reproduce (not last stage)

	prompt1, err := os.ReadFile(agentPromptFile)
	if err != nil {
		t.Fatalf("agent was never invoked: %v", err)
	}
	if strings.Contains(string(prompt1), "final stage of a multi-stage ticket") {
		t.Errorf("non-final stage's prompt should not get the final-stage reassurance: %q", prompt1)
	}

	runCycle(loadTestConfig(t)) // stage 2: fix (last stage)

	prompt2, err := os.ReadFile(agentPromptFile)
	if err != nil {
		t.Fatalf("agent was never invoked on stage 2: %v", err)
	}
	got := string(prompt2)
	if !strings.Contains(got, "final stage of a multi-stage ticket") {
		t.Errorf("final stage's prompt should get the final-stage reassurance: %q", got)
	}
	if !strings.Contains(got, "multi-stage ticket (reproduce → fix)") {
		t.Errorf("final stage's prompt should name the full stage sequence: %q", got)
	}
}

// idleStallAgentBody is a shell body that writes nothing and sleeps well
// past any idle_timeout_minutes used in these tests (which use the smallest
// real config value, 1 minute) — simulating an agent CLI that has gone
// completely silent, the behavior the user observed live with opencode.
const idleStallAgentBody = "  sleep 90"

// idleTimeoutRoleConfig mirrors testRolesConfig but adds a 1-minute
// idle_timeout_minutes to the default role — the smallest real value the
// actual RoleConfig.IdleTimeoutMinutes (whole minutes) can express, so these
// two tests necessarily take about a minute of real wall-clock time each to
// prove the actual config -> kill -> retry path, on top of the fast,
// sub-second unit test of the watchdog mechanism itself
// (TestFix_RunAgentAttemptIdleTimeoutKillsAndReportsStall).
const idleTimeoutRoleConfig = "roles:\n  default:\n    command: opencode implement\n    idle_timeout_minutes: 1\n  reviewer:\n    command: opencode review\n"

// TestFix_IdleStallCyclesBackToTodo confirms a stalled agent (no output at
// all) is treated as a transient glitch worth retrying — cycling back to
// todo with Round incremented and a note about the stall, not going straight
// to budget-exhausted the way a genuinely slow timeout still does.
func TestFix_IdleStallCyclesBackToTodo(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [work]\n    max_rounds: 2\n" +
		idleTimeoutRoleConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()
	fakeAgentAndReviewer(t, idleStallAgentBody, filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("a ticket whose agent stalled with no output should cycle back to todo for a retry, not block")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Status != model.StatusTodo {
		t.Errorf("Status = %q, want %q", card.Status, model.StatusTodo)
	}
	if card.Round != 1 {
		t.Errorf("Round = %d, want 1", card.Round)
	}
	if !strings.Contains(card.Content, "no output for 1 minutes") {
		t.Error("the stall note was not written back onto the card")
	}
}

// TestFix_IdleStallRoundsExhaustedBlocks confirms the idle-retry budget is
// bounded: a ticket whose Round already sits at flow.MaxRounds (simulating
// prior exhausted stall attempts) blocks with a distinct, honest reason
// instead of cycling forever. Starting Round at the cap avoids re-running
// the full multi-round march for real (the identical round > MaxRounds cap
// logic is already proven generically by TestFix_ReviewRoundsCycleThenBlock
// and TestFix_AgentErrorWithNoChangesCyclesThenBlocks) while still
// confirming this new branch reaches it correctly.
func TestFix_IdleStallRoundsExhaustedBlocks(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [work]\n    max_rounds: 2\n" +
		idleTimeoutRoleConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()
	fakeAgentAndReviewer(t, idleStallAgentBody, filepath.Join(t.TempDir(), "no-findings"))

	mustWrite(t, ".kanban/100-todo/card.md",
		"---\ntitle: Test\nid: 1\nstatus: todo\nround: 2\n---\n\nDo the thing.\n")

	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("a ticket already at its round budget should block, not cycle back, on another stall")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "agent-idle-rounds-exhausted" {
		t.Errorf("Reason = %q, want agent-idle-rounds-exhausted", card.Reason)
	}
}

func TestFix_ReviewRoundsCycleThenBlock(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	findings := filepath.Join(dir, "findings.txt")
	os.WriteFile(findings, []byte("## Concern\n\nthis needs work"), 0644)
	fakeAgentAndReviewer(t, "  touch feature.txt", findings)

	writeCard(t, ".kanban/100-todo/card.md", "Test", "6")
	cfg := loadTestConfig(t) // max_rounds: 2 for the "work" stage

	runCycle(cfg) // round 1: findings, cycles back to todo
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("card with round-1 findings should cycle back to todo for another attempt")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Round != 1 {
		t.Errorf("Round = %d, want 1", card.Round)
	}
	if !strings.Contains(card.Content, "this needs work") {
		t.Error("round-1 findings were not written back onto the card")
	}

	runCycle(cfg) // round 2: findings again, still within max_rounds (2) -> cycles again
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("card with round-2 findings should still cycle back — max_rounds is 2")
	}

	runCycle(cfg) // round 3: findings a third time, exceeds max_rounds -> blocked
	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("card should be blocked once review rounds are exhausted")
	}
	card = mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "review-rounds-exhausted" {
		t.Errorf("Reason = %q, want review-rounds-exhausted", card.Reason)
	}
}

// TestFix_AgentErrorWithNoChangesCyclesThenBlocks guards against a real bug
// hit live: an implementer agent that crashes before making any changes
// (e.g. a resource conflict in the agent CLI itself) used to fall straight
// into the generic "no changes to review" block, misreporting a system
// failure as "nothing needed changing" — and the reviewer was never even
// invoked. It should instead retry (cycle back to todo) the same bounded way
// reviewer findings do, only blocking once that budget is exhausted.
func TestFix_AgentErrorWithNoChangesCyclesThenBlocks(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  exit 1", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "7")
	cfg := loadTestConfig(t) // max_rounds: 2 for the "work" stage

	runCycle(cfg) // attempt 1: agent errors out, no changes -> cycle back to todo
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("a ticket whose agent errored out with no changes should cycle back to todo for a retry, not go to the reviewer or block")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Status != model.StatusTodo {
		t.Errorf("Status = %q, want %q", card.Status, model.StatusTodo)
	}
	if card.Round != 1 {
		t.Errorf("Round = %d, want 1", card.Round)
	}
	if !strings.Contains(card.Content, "exited with an error") {
		t.Error("the agent's crash output was not written back onto the card")
	}

	runCycle(cfg) // attempt 2: still within max_rounds (2) -> cycles again
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("should still cycle back for attempt 2 — max_rounds is 2")
	}

	runCycle(cfg) // attempt 3: exceeds max_rounds -> blocked
	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("card should be blocked once the agent-error retry budget is exhausted")
	}
	card = mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "agent-error-rounds-exhausted" {
		t.Errorf("Reason = %q, want agent-error-rounds-exhausted", card.Reason)
	}
}

// TestStripReviewFindings guards against a real bug: a reviewer that sees
// its own or a prior round's appended finding inside the ticket content
// starts arguing with that past finding instead of reviewing the diff.
func TestStripReviewFindings(t *testing.T) {
	original := "Original ticket description.\n"
	want := strings.TrimRight(original, "\n")

	withOneFinding := original + "\n## Review Findings\n\n## Concern\n\nsomething wrong\n"
	if got := stripReviewFindings(withOneFinding); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	withTwoFindings := withOneFinding + "\n## Review Findings\n\n## Concern\n\nanother thing\n"
	if got := stripReviewFindings(withTwoFindings); got != want {
		t.Errorf("should strip from the first occurrence: got %q, want %q", got, want)
	}

	if got := stripReviewFindings(original); got != original {
		t.Errorf("content with no Review Findings section should be returned unchanged: got %q, want %q", got, original)
	}
}

// TestFix_ReviewerNeverSeesPastFindings guards against a real bug found
// live: round 1's reviewer finding gets appended to the card, and round 2's
// reviewer — shown the raw card content — started re-litigating round 1's
// (wrong) finding instead of reviewing the diff fresh, still wrapping its
// rebuttal in "## Concern" and needlessly cycling the ticket again.
func TestFix_ReviewerNeverSeesPastFindings(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	findings := filepath.Join(dir, "findings.txt")
	os.WriteFile(findings, []byte("## Concern\n\nthis needs work"), 0644)
	_, promptFile := fakeAgentAndReviewer(t, "  touch feature.txt", findings)

	writeCard(t, ".kanban/100-todo/card.md", "Test", "6")
	cfg := loadTestConfig(t)

	runCycle(cfg) // round 1: finding appended to the card, cycles back to todo
	runCycle(cfg) // round 2: reviewer runs again — must not see round 1's appended finding

	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("reviewer was never invoked: %v", err)
	}
	got := string(prompt)
	if strings.Contains(got, "## Review Findings") {
		t.Errorf("reviewer prompt still contains an accumulated Review Findings section: %q", got)
	}
	if strings.Contains(got, "this needs work") {
		t.Errorf("reviewer prompt still contains a prior round's finding text: %q", got)
	}
}

// TestFix_NoAgentCommandConfiguredBlocksTicket verifies opencode (or any
// agent) is invoked only if configured: with no roles/command at all, a
// ticket must be blocked with a clear reason rather than silently stuck in
// todo, and the fake shim (installed on PATH, but never referenced by any
// configured command) must never actually run.
func TestFix_NoAgentCommandConfiguredBlocksTicket(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		"flows:\n  feature:\n    stages: [work]\n    max_rounds: 2\n"
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()

	calls, _ := fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")
	runCycle(loadTestConfig(t))

	if fileExists(calls) {
		t.Error("no command was configured — the agent shim should never have been invoked")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Status != model.StatusBlocked || card.Reason != "no-agent-command-configured" {
		t.Errorf("Status/Reason = %q/%q, want blocked/no-agent-command-configured", card.Status, card.Reason)
	}
}

// loadTestConfig loads ekbn.config.yml from the current directory, the way
// runCycle is normally handed a freshly loaded config each poll.
func loadTestConfig(t *testing.T) serve.Config {
	t.Helper()
	return serve.LoadConfig()
}

// TestFix_ManualFolderMoveSyncsStatusAndUnblocksDependents simulates a human
// dragging a reviewed card straight to "done" on the filesystem (a raw mv,
// not through TransitionStatus/MoveCard) — status is left stale even though
// the folder says done, so a dependent ticket would stay stuck forever
// without reconciliation.
func TestFix_ManualFolderMoveSyncsStatusAndUnblocksDependents(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	mustWrite(t, ".kanban/250-review/card-a.md",
		"---\ntitle: A\nid: card-a\nstatus: review\n---\n\nAlready reviewed.\n")
	if err := os.Rename(".kanban/250-review/card-a.md", ".kanban/300-done/card-a.md"); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, ".kanban/100-todo/card-b.md",
		"---\ntitle: B\nid: card-b\nstatus: todo\ndepends_on: [card-a]\n---\n\nDo the thing.\n")

	runCycle(loadTestConfig(t))

	cardA := mustReadCard(t, ".kanban/300-done/card-a.md")
	if cardA.Status != model.StatusDone {
		t.Errorf("card A Status = %q, want %q", cardA.Status, model.StatusDone)
	}
	if cardA.Reason != "manual-move" {
		t.Errorf("card A Reason = %q, want manual-move", cardA.Reason)
	}

	if fileExists(".kanban/100-todo/card-b.md") {
		t.Error("card B still in 100-todo — dependent was not unblocked by the reconciled status")
	}
	if !fileExists(".kanban/250-review/card-b.md") {
		t.Fatal("card B did not reach 250-review")
	}
}
