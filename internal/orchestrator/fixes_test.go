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
// the default "feature" flow (implement -> verify -> review -> done/blocked,
// see internal/serve/serve.go's defaultFlows) and the given verify command.
// Returns the repo dir.
func fixSetup(t *testing.T, verify string) string {
	t.Helper()
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })

	initGitRepo(t)
	cfg := "verify: \"" + verify + "\"\n" +
		"wip-limit: 1\n" +
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
const testRolesConfig = "executors:\n  implement-exec:\n    command:\n      program: opencode\n      args: [implement]\n  review-exec:\n    command:\n      program: opencode\n      args: [review]\n" +
	"roles:\n  default:\n    executor: implement-exec\n  reviewer:\n    executor: review-exec\n"

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

// runUntilSettled calls runCycle repeatedly (up to maxCycles times) until
// ticket id is no longer todo or in-progress — i.e. it reached a terminal or
// paused state (done/blocked/budget-exhausted). A ticket's flow can now
// visit a different number of states depending on its path (retries, review
// rounds), so tests express "let it run to completion" this way instead of
// hand-counting cycles.
func runUntilSettled(t *testing.T, cfg serve.Config, id string, maxCycles int) model.Card {
	t.Helper()
	for i := 0; i < maxCycles; i++ {
		if card, ok := findCard(id); ok && card.Status != model.StatusTodo && card.Status != model.StatusInProgress {
			return card
		}
		runCycle(cfg)
	}
	card, ok := findCard(id)
	if !ok {
		t.Fatalf("ticket #%s not found after %d cycles", id, maxCycles)
	}
	t.Fatalf("ticket #%s did not settle within %d cycles (status=%s stage=%s)", id, maxCycles, card.Status, card.Stage)
	return card
}

// runUntilAllSettled is runUntilSettled for several tickets running
// concurrently under the same WIP limit.
func runUntilAllSettled(t *testing.T, cfg serve.Config, ids []string, maxCycles int) {
	t.Helper()
	for i := 0; i < maxCycles; i++ {
		allSettled := true
		for _, id := range ids {
			card, ok := findCard(id)
			if !ok || card.Status == model.StatusTodo || card.Status == model.StatusInProgress {
				allSettled = false
				break
			}
		}
		if allSettled {
			return
		}
		runCycle(cfg)
	}
	t.Fatalf("tickets %v did not all settle within %d cycles", ids, maxCycles)
}

func TestFix_SuccessAdvancesToDone(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	// implement -> verify -> review -> done: no acceptance-gating distinction
	// exists anymore, so a clean run lands straight on done (see the v2
	// flow-engine rewrite — acceptance-gating was dropped entirely).
	runUntilSettled(t, loadTestConfig(t), "1", 6)

	if fileExists(".kanban/200-in-progress/card.md") {
		t.Error("card still in 200-in-progress after a successful run")
	}
	if !fileExists(".kanban/300-done/card.md") {
		t.Fatal("card did not reach 300-done")
	}
	card := mustReadCard(t, ".kanban/300-done/card.md")
	if card.Status != model.StatusDone {
		t.Errorf("Status = %q, want %q", card.Status, model.StatusDone)
	}
	if card.Reason != "done" {
		t.Errorf("Reason = %q, want done", card.Reason)
	}
	// The agent's work must actually have been committed and merged.
	if !fileExists("feature.txt") {
		t.Error("feature.txt not present after a successful run — the ticket's commit never landed on main")
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

	command := serve.CommandSpec{Program: filepath.Join(helperDir, "fakecmd"), Args: []string{"{workdir}"}}
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

	command := serve.CommandSpec{Program: filepath.Join(helperDir, "stallcmd")}
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
// as if nothing happened. This is detected within the implement state's own
// single attempt, so one poll cycle is enough.
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
	runUntilSettled(t, loadTestConfig(t), "9", 6)

	if !fileExists(".kanban/300-done/card.md") {
		t.Fatal("card did not reach 300-done")
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
	card := mustReadCard(t, ".kanban/300-done/card.md")
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
	runUntilSettled(t, loadTestConfig(t), "1", 6)

	if !fileExists("feature.txt") {
		t.Fatal("feature.txt not present — the ticket's commit never landed on main")
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

// TestFix_WIPLimitTwoRunsBothConcurrently confirms raising wip-limit actually
// lets two tickets run in the same poll cycle — each in its own worktree —
// and both eventually reach their terminal state, including merging both
// commits back into main (exercising mergeAndRemoveWorktree's rebase-retry
// path for whichever of the two merges second, since both branch from the
// same mainHead).
func TestFix_WIPLimitTwoRunsBothConcurrently(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, countingAgentBody(t), filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/a.md", "First", "1")
	writeCard(t, ".kanban/100-todo/b.md", "Second", "2")

	cfg := loadTestConfig(t)
	cfg.WIPLimit = 2
	runUntilAllSettled(t, cfg, []string{"1", "2"}, 12)

	if !fileExists(".kanban/300-done/a.md") {
		t.Error("a.md did not reach 300-done")
	}
	if !fileExists(".kanban/300-done/b.md") {
		t.Error("b.md did not reach 300-done — both tickets should run concurrently with wip-limit 2")
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

// TestFix_VerifyFailureCyclesBackToImplement guards a genuine, intentional
// v2 behavior change: a failing verify state no longer discards the attempt
// and blocks outright (the old model's "no retry on verify failure"). It now
// routes back to the implement state per the flow's own `on: {failed:
// implement}` transition — fix-forward, not discard-and-block — the same way
// every other classified outcome is routed. The already-committed work stays
// in the worktree; only once the flow's total-attempt budget is exhausted
// does it actually block (see TestFix_TotalAttemptsBackstopClosesPingPong).
func TestFix_VerifyFailureCyclesBackToImplement(t *testing.T) {
	fixSetup(t, "exit 1")
	calls, _ := fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "4")
	cfg := loadTestConfig(t)

	runCycle(cfg) // implement: succeeds, commits, advances to verify
	runCycle(cfg) // verify: fails, routes back to implement

	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("a verify failure should cycle the ticket back to todo (implement), not block it")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "implement" {
		t.Errorf("Stage = %q, want implement", card.Stage)
	}
	if fileExists(".kanban/400-blocked/card.md") {
		t.Error("card should not be blocked yet — a single verify failure is not the total-attempt budget")
	}

	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(string(data))); n != 1 {
		t.Errorf("agent invoked %d times, want 1 — verify running doesn't itself invoke the implementer again", n)
	}
}

// TestFix_TotalAttemptsBackstopClosesPingPong guards the ping-pong case
// TotalAttempts exists for: a verify state that always fails sends the
// ticket back to implement forever, and since a state's own Round resets to
// 0 every time the ticket leaves and re-enters it, neither state's own
// MaxAttempts is ever itself exceeded. TotalAttempts, which increments on
// every transition and never resets, is the real backstop that eventually
// blocks the ticket.
func TestFix_TotalAttemptsBackstopClosesPingPong(t *testing.T) {
	fixSetup(t, "exit 1")
	fakeAgentAndReviewer(t, "  touch feature.txt", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "5")
	cfg := loadTestConfig(t)

	card := runUntilSettled(t, cfg, "5", 30)
	if card.Status != model.StatusBlocked {
		t.Fatalf("Status = %q, want blocked once the total-attempt budget is exhausted", card.Status)
	}
	if card.Reason != "flow-total-attempts-exhausted" {
		t.Errorf("Reason = %q, want flow-total-attempts-exhausted", card.Reason)
	}
}

func TestFix_ReviewerSeesOnlyItsOwnCard(t *testing.T) {
	fixSetup(t, "exit 0")
	_, prompt := fakeAgentAndReviewer(t, countingAgentBody(t), filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/a.md", "First", "1")
	writeCard(t, ".kanban/100-todo/b.md", "Second", "2")

	cfg := loadTestConfig(t) // WIP limit 1: only one ticket in progress at a time
	runUntilAllSettled(t, cfg, []string{"1", "2"}, 20)

	data, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	sawFile1 := strings.Contains(got, "file1.txt")
	sawFile2 := strings.Contains(got, "file2.txt")
	if sawFile1 && sawFile2 {
		t.Errorf("the last reviewer prompt mentions both cards' files — the diff is not isolated per ticket: %q", got)
	}
	if !sawFile1 && !sawFile2 {
		t.Errorf("the last reviewer prompt mentions neither card's file: %q", got)
	}
}

// TestFix_ReviewerSeesCumulativeDiffAcrossRounds guards against the inverse
// bug from TestFix_ReviewerSeesOnlyItsOwnCard above: after reviewer findings
// send a ticket back to implement, and that round's agent makes no new
// commit (because the first round's work already satisfies the ticket), the
// next review must still see the first round's real, already-committed
// work — not an empty diff.
func TestFix_ReviewerSeesCumulativeDiffAcrossRounds(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	findings := filepath.Join(dir, "findings.txt")
	os.WriteFile(findings, []byte("## Concern\n\nneeds another look"), 0644)
	_, prompt := fakeAgentAndReviewer(t, firstCallOnlyAgentBody(t), findings)

	writeCard(t, ".kanban/100-todo/card.md", "Test", "7")
	cfg := loadTestConfig(t)

	runCycle(cfg) // implement: creates file1.txt, commits, advances to verify
	runCycle(cfg) // verify: passes, advances to review
	runCycle(cfg) // review: finds a concern, routes back to implement
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "implement" {
		t.Fatalf("Stage = %q, want implement (routed back by the review's findings transition)", card.Stage)
	}
	if !strings.Contains(card.Content, "needs another look") {
		t.Error("the reviewer's finding was not written back onto the card")
	}

	runCycle(cfg) // implement: agent makes no new commit this round (round > 1)
	runCycle(cfg) // verify: passes again
	runCycle(cfg) // review: must still see file1.txt in the cumulative diff

	data, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "file1.txt") {
		t.Error("the second review's prompt does not mention file1.txt — it was handed an empty diff instead of the cumulative one")
	}
}

// TestFix_EmptyDiffBlocksWithoutInvokingReviewer guards against asking the
// reviewer to judge a diff ekbn itself already knows is empty: an agent that
// makes no changes at all must be flagged (ambiguous outcome, routed to
// blocked by the default flow) without ever invoking the reviewer — whose
// only possible response to an empty diff ("there's nothing here") is not
// guaranteed to avoid the "## Concern" heading, since the model isn't
// deterministic about that.
func TestFix_EmptyDiffBlocksWithoutInvokingReviewer(t *testing.T) {
	fixSetup(t, "exit 0")
	_, prompt := fakeAgentAndReviewer(t, "  :", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "8")
	runCycle(loadTestConfig(t))

	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	// The default "feature" flow routes an ambiguous (no-changes) outcome to
	// its single generic "blocked" terminal — see defaultFlows in
	// internal/serve/serve.go — so the card's Reason is that terminal's name,
	// not a bespoke string; the findings text (checked below) is what
	// actually explains why.
	if card.Reason != "blocked" {
		t.Errorf("Reason = %q, want blocked", card.Reason)
	}
	if !strings.Contains(card.Content, "No code has changed") {
		t.Errorf("blocked card's findings should explain that nothing changed: %q", card.Content)
	}
	if fileExists(prompt) {
		t.Error("reviewer was invoked despite an empty diff — it should have been skipped entirely")
	}
}

// TestFix_AgentDeclaredNoChangesNeededPassesCleanly guards the companion
// case: an implementer that explicitly states, via the "## No Changes
// Needed" marker, that it looked at the code and nothing needs to change
// must pass through toward the terminal state — not block — and must never
// invoke the reviewer either at this state, since there's still nothing to
// review yet (the empty-diff implement->verify->review path still runs the
// reviewer once verify passes, since verify has nothing to check against an
// empty diff either and just passes trivially).
func TestFix_AgentDeclaredNoChangesNeededPassesCleanly(t *testing.T) {
	fixSetup(t, "exit 0")
	agentBody := `  printf '## No Changes Needed\n\nThe feature already exists and passes verify.'`
	fakeAgentAndReviewer(t, agentBody, filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "9")
	runCycle(loadTestConfig(t)) // implement: declares no changes needed

	if fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("card should not block when the agent explicitly declared no changes needed")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "verify" {
		t.Errorf("Stage = %q, want verify — a declared no-changes-needed result should still be treated as success", card.Stage)
	}
}

func TestFix_MultiStageAdvancement(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, countingAgentBody(t), filepath.Join(t.TempDir(), "no-findings"))
	writeCard(t, ".kanban/100-todo/card.md", "Test", "1")

	cfg := loadTestConfig(t)
	runCycle(cfg) // implement
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("after finishing a non-terminal state, the card should return to todo for the next state")
	}
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "verify" {
		t.Errorf("Stage = %q, want %q", card.Stage, "verify")
	}
	if card.Round != 0 {
		t.Errorf("Round = %d, want 0 (reset on state advance)", card.Round)
	}
	if card.TotalAttempts != 1 {
		t.Errorf("TotalAttempts = %d, want 1", card.TotalAttempts)
	}
	if fileExists("file1.txt") {
		t.Error("the implement state's commit isn't merged yet — it should not be on the main branch until the ticket reaches its terminal state")
	}

	runCycle(cfg) // verify
	card = mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "review" {
		t.Errorf("Stage = %q, want %q", card.Stage, "review")
	}

	runCycle(cfg) // review -> terminal done
	if !fileExists(".kanban/300-done/card.md") {
		t.Fatal("after the review state finds nothing, the card should reach done")
	}
	if !fileExists("file1.txt") {
		t.Error("the implement state's commit should be on the main branch now that the ticket has merged")
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

	prompt := buildSystemPrompt("card.md", model.Card{}, serve.RoleConfig{}, serve.Flow{})
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

	prompt := buildSystemPrompt("card.md", model.Card{}, serve.RoleConfig{}, serve.Flow{})
	if !strings.Contains(prompt, "Do not run git commands that write or mutate") {
		t.Errorf("prompt should explicitly forbid mutating git commands, got: %q", prompt)
	}
	if !strings.Contains(prompt, "status") || !strings.Contains(prompt, "diff") {
		t.Errorf("prompt should name at least some allowed read-only git subcommands, got: %q", prompt)
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
const idleTimeoutRoleConfig = "executors:\n  implement-exec:\n    command:\n      program: opencode\n      args: [implement]\n    idle_timeout_minutes: 1\n  review-exec:\n    command:\n      program: opencode\n      args: [review]\n" +
	"roles:\n  default:\n    executor: implement-exec\n  reviewer:\n    executor: review-exec\n"

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
// bounded: a ticket whose Round already sits at the implement state's
// MaxAttempts (simulating prior exhausted stall attempts, default 3) blocks
// with a distinct, honest reason instead of cycling forever.
func TestFix_IdleStallRoundsExhaustedBlocks(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	initGitRepo(t)
	cfg := "verify: \"exit 0\"\n" +
		"wip-limit: 1\n" +
		idleTimeoutRoleConfig
	os.WriteFile("ekbn.config.yml", []byte(cfg), 0644)
	ensureKanbanDirs()
	fakeAgentAndReviewer(t, idleStallAgentBody, filepath.Join(t.TempDir(), "no-findings"))

	mustWrite(t, ".kanban/100-todo/card.md",
		"---\ntitle: Test\nid: 1\nstatus: todo\nround: 3\n---\n\nDo the thing.\n")

	runCycle(loadTestConfig(t))

	if !fileExists(".kanban/400-blocked/card.md") {
		t.Fatal("a ticket already at its round budget should block, not cycle back, on another stall")
	}
	card := mustReadCard(t, ".kanban/400-blocked/card.md")
	if card.Reason != "agent-idle-rounds-exhausted" {
		t.Errorf("Reason = %q, want agent-idle-rounds-exhausted", card.Reason)
	}
}

// TestFix_ReviewRoundsCycleThenBlock confirms persistent reviewer findings
// eventually block a ticket rather than looping forever — bounded by the
// flow's TotalAttempts backstop, since findings now route back to implement
// (a state hop) rather than retrying the review state in place.
func TestFix_ReviewRoundsCycleThenBlock(t *testing.T) {
	dir := fixSetup(t, "exit 0")
	findings := filepath.Join(dir, "findings.txt")
	os.WriteFile(findings, []byte("## Concern\n\nthis needs work"), 0644)
	fakeAgentAndReviewer(t, "  touch feature.txt", findings)

	writeCard(t, ".kanban/100-todo/card.md", "Test", "6")
	cfg := loadTestConfig(t)

	runCycle(cfg) // implement: commits, advances to verify
	runCycle(cfg) // verify: passes, advances to review
	runCycle(cfg) // review: finds a concern, routes back to implement
	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Stage != "implement" {
		t.Fatalf("Stage = %q, want implement", card.Stage)
	}
	if !strings.Contains(card.Content, "this needs work") {
		t.Error("the reviewer's finding was not written back onto the card")
	}

	card = runUntilSettled(t, cfg, "6", 30)
	if card.Status != model.StatusBlocked {
		t.Fatalf("Status = %q, want blocked once the ticket keeps failing review forever", card.Status)
	}
	if card.Reason != "flow-total-attempts-exhausted" {
		t.Errorf("Reason = %q, want flow-total-attempts-exhausted", card.Reason)
	}
}

// TestFix_AgentErrorWithNoChangesCyclesThenBlocks guards against a real bug
// hit live: an implementer agent that crashes before making any changes
// (e.g. a resource conflict in the agent CLI itself) used to fall straight
// into the generic "no changes to review" block, misreporting a system
// failure as "nothing needed changing" — and the reviewer was never even
// invoked. It should instead retry (cycle back to todo, same state) the same
// bounded way an idle stall does, only blocking once that state's own
// attempt budget (default 3) is exhausted.
func TestFix_AgentErrorWithNoChangesCyclesThenBlocks(t *testing.T) {
	fixSetup(t, "exit 0")
	fakeAgentAndReviewer(t, "  exit 1", filepath.Join(t.TempDir(), "no-findings"))

	writeCard(t, ".kanban/100-todo/card.md", "Test", "7")
	cfg := loadTestConfig(t)

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

	runCycle(cfg) // attempt 2: still within the default 3-attempt budget -> cycles again
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("should still cycle back for attempt 2")
	}

	runCycle(cfg) // attempt 3: still within budget -> cycles again
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("should still cycle back for attempt 3 — the default budget is 3")
	}

	runCycle(cfg) // attempt 4: exceeds the default 3-attempt budget -> blocked
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

	runCycle(cfg) // implement
	runCycle(cfg) // verify
	runCycle(cfg) // review: finding appended to the card, routes back to implement
	runCycle(cfg) // implement again
	runCycle(cfg) // verify again
	runCycle(cfg) // review again — must not see round 1's appended finding

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
	cfg := "verify: \"exit 0\"\nwip-limit: 1\n"
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

	cfg := loadTestConfig(t)
	runCycle(cfg)

	cardA := mustReadCard(t, ".kanban/300-done/card-a.md")
	if cardA.Status != model.StatusDone {
		t.Errorf("card A Status = %q, want %q", cardA.Status, model.StatusDone)
	}
	if cardA.Reason != "manual-move" {
		t.Errorf("card A Reason = %q, want manual-move", cardA.Reason)
	}

	// Card B should have been unblocked and its implement state run — it's
	// back in 100-todo, now at the "verify" state, rather than the "todo"
	// (Stage-less) it started at.
	cardB, ok := findCard("card-b")
	if !ok {
		t.Fatal("card B disappeared")
	}
	if cardB.Stage != "verify" {
		t.Errorf("card B Stage = %q, want verify — it should have been unblocked and had its first state run", cardB.Stage)
	}

	runUntilSettled(t, cfg, "card-b", 6)
	if !fileExists(".kanban/300-done/card-b.md") {
		t.Fatal("card B did not reach 300-done")
	}
}
