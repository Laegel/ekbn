package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

// TestFix_PreexistingUntrackedFileNotCommitted guards against a real bug:
// with no worktree isolating a ticket's edits from the rest of the project,
// a naive `git add -A` at commit time sweeps in whatever untracked
// scaffolding was already sitting in the working directory (an agent CLI's
// own config file, in this case) right along with the agent's actual work.
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
// configured, capacity is clamped to 1: tickets run directly in the shared
// working directory now (no per-ticket worktree), so only one may be in
// flight at a time regardless of config.
func TestFix_WIPLimitClampedToOne(t *testing.T) {
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
	if !fileExists(".kanban/100-todo/b.md") {
		t.Error("b.md should still be in 100-todo — capacity should be clamped to 1 regardless of WIPLimit=2")
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
	if !fileExists("file1.txt") {
		t.Error("the implement stage's commit should already be on the main branch")
	}

	runCycle(loadTestConfig(t)) // stage 2: gates (last stage) -> review
	if !fileExists(".kanban/250-review/card.md") {
		t.Fatal("after finishing the last stage, the card should reach review")
	}
	if !fileExists("file2.txt") {
		t.Error("the gates stage's commit should already be on the main branch")
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
