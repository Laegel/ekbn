package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

// resolveRoleConfig looks up role in roles, falling back to roles["default"]
// (reporting fellBack=true) when role is unset or not a configured key.
// Routing is a lookup, not a judgment: no model is ever asked which
// specialist to use. If roles is empty entirely (unconfigured), returns a
// zero-value RoleConfig with fellBack=false, preserving pre-role-routing
// behavior exactly.
func resolveRoleConfig(role string, roles map[string]serve.RoleConfig) (rc serve.RoleConfig, fellBack bool) {
	if len(roles) == 0 {
		return serve.RoleConfig{}, false
	}
	if role != "" {
		if found, ok := roles[role]; ok {
			return found, false
		}
	}
	return roles["default"], true
}

func buildSystemPrompt(ticketPath string, card model.Card, rc serve.RoleConfig, flow serve.StageFlow) string {
	var parts []string
	if data, err := os.ReadFile(agentsMD); err == nil {
		parts = append(parts, string(data))
	} else {
		log.warn("AGENTS.md not found — agent will run without base instructions")
	}
	if rc.Prompt != "" {
		parts = append(parts, rc.Prompt)
	}
	if len(rc.Tools) > 0 {
		parts = append(parts, "Available tools: "+strings.Join(rc.Tools, ", "))
	}
	if len(rc.Skills) > 0 {
		parts = append(parts, "Skills: "+strings.Join(rc.Skills, ", "))
	}
	if card.Stage != "" && len(flow.Stages) > 0 {
		parts = append(parts, fmt.Sprintf(
			"You are working stage %q of this ticket's flow (%s). Only do the work that belongs to this stage — later stages are separate agent runs.",
			card.Stage, strings.Join(flow.Stages, " -> ")))
	}
	if card.Security {
		if data, err := os.ReadFile(securityMD); err == nil {
			parts = append(parts, "---\n\n"+string(data))
		} else {
			log.warn("Ticket #%s: security: true but SECURITY.md not found", card.ID)
		}
	}
	if data, err := os.ReadFile(ticketPath); err == nil {
		parts = append(parts, "---\n\n## Current ticket\n\n"+string(data))
	}
	return strings.Join(parts, "\n\n")
}

func envWithPath(path string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "PATH=") {
			out = append(out, e)
		}
	}
	return append(out, "PATH="+path)
}

// runAgentAttempt runs the role's configured command with `git` shimmed out
// on PATH for that subprocess only: any git invocation is captured (a marker
// file is touched) and fails immediately, so the agent's own attempt to use
// git directly — bypassing the ekbn MCP tools — is both detected and
// blocked. If maxDurationMinutes is set, the subprocess is killed once that
// budget is exceeded and timedOut is reported — the one budget dimension
// ekbn can enforce itself without depending on any particular CLI's own
// accounting. Callers must not call this with an empty command — that case
// means "nothing is configured to run" and is handled before this point.
// readOnlyGitSubcommands cannot alter commit history or working tree
// content under any arguments — the agent's own git shim executes these
// against the real git rather than blocking the ticket. config/symbolic-ref/
// remote can technically write local repo state (a config value, which
// branch HEAD points at, a remote entry) but never commits, blobs, or refs
// that carry history, and agent CLIs commonly probe them at session start
// (e.g. `git config --bool core.bare`, `git symbolic-ref --short HEAD`) —
// this is why blocking them broke every ticket that used opencode as the
// implementer, even when it never touched history. Everything else (commit,
// add, checkout, reset, merge, push, branch, worktree, ...) still blocks:
// ekbn owns mutating git operations in the project directory.
var readOnlyGitSubcommands = []string{
	"status", "diff", "log", "show", "describe", "blame",
	"shortlog", "ls-files", "ls-tree", "rev-parse", "cat-file", "grep",
	"config", "symbolic-ref", "remote", "for-each-ref", "check-ignore",
}

func runAgentAttempt(prompt, dir, command string, maxDurationMinutes int) (output string, runErr error, usedGit, timedOut bool) {
	shimDir, mkErr := os.MkdirTemp("", "ekbn-git-shim-")
	env := os.Environ()
	marker := ""
	if mkErr != nil {
		log.warn("Failed to create git shim dir: %v — running without it", mkErr)
	} else {
		defer os.RemoveAll(shimDir)
		marker = filepath.Join(shimDir, "git-used")
		realGit, lookErr := exec.LookPath("git")
		if lookErr != nil {
			realGit = "/usr/bin/git"
		}
		projectGitDir := ""
		if out, err := exec.Command(realGit, "-C", dir, "rev-parse", "--path-format=absolute", "--git-dir").Output(); err == nil {
			projectGitDir = strings.TrimSpace(string(out))
		}
		// Some agent CLIs (opencode included) keep their own private shadow
		// git repo elsewhere — e.g. for undo/diff-snapshot features — and
		// invoke real git against it dozens of times a turn via -c/-C/
		// --git-dir/--work-tree, with the actual subcommand appearing after
		// those global flags rather than in $1. That traffic can't touch
		// this project's history no matter what it does, since it targets a
		// different --git-dir entirely, so it's let through unconditionally.
		// Anything else is only let through if the (correctly located)
		// subcommand is one of readOnlyGitSubcommands.
		script := fmt.Sprintf(`#!/bin/sh
classify() (
	gitdir=""
	subcmd=""
	while [ $# -gt 0 ]; do
		case "$1" in
			-c|-C|--namespace|--super-prefix) shift 2 ;;
			--git-dir) gitdir=$2; shift 2 ;;
			--git-dir=*) gitdir=${1#--git-dir=}; shift ;;
			--work-tree) shift 2 ;;
			--work-tree=*) shift ;;
			-*) shift ;;
			*) subcmd=$1; break ;;
		esac
	done
	echo "$subcmd|$gitdir"
)
result=$(classify "$@")
subcmd=${result%%|*}
gitdir=${result#*|}
case "$subcmd" in
	%s)
		exec %q "$@"
		;;
esac
if [ -n "$gitdir" ] && [ "$gitdir" != %q ]; then
	exec %q "$@"
fi
touch %q
echo "git '$subcmd' is not permitted here — only read-only commands (%s) are allowed; use the ekbn MCP tools for anything else" >&2
exit 1
`, strings.Join(readOnlyGitSubcommands, "|"), realGit, projectGitDir, realGit, marker, strings.Join(readOnlyGitSubcommands, ", "))
		os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0755)
		env = envWithPath(shimDir + string(os.PathListSeparator) + os.Getenv("PATH"))
	}

	ctx := context.Background()
	if maxDurationMinutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(maxDurationMinutes)*time.Minute)
		defer cancel()
	}

	argv := strings.Fields(command)
	args := append(append([]string{}, argv[1:]...), prompt)

	cmd := exec.CommandContext(ctx, argv[0], args...)
	cmd.Dir = dir
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)

	runErr = cmd.Run()
	timedOut = ctx.Err() == context.DeadlineExceeded
	usedGit = marker != "" && fileExists(marker)
	return strings.TrimSpace(buf.String()), runErr, usedGit, timedOut
}

// Card status transitions. TransitionStatus (in model) is the only thing
// that moves a card's file; everything below just decides which status and
// what to record.

func appendSection(column, filename, heading, body string) {
	if body == "" {
		return
	}
	path := filepath.Join(kanbanRoot, column, filename)
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	section := fmt.Sprintf("\n## %s\n\n%s\n", heading, body)
	os.WriteFile(path, append(content, []byte(section)...), 0644)
}

func withCard(column, filename string, mutate func(*model.Card)) {
	path := filepath.Join(kanbanRoot, column, filename)
	card, err := model.ReadCard(path, column)
	if err != nil {
		log.error("Failed to read %s to update it: %v", path, err)
		return
	}
	mutate(&card)
	if err := model.SaveCard(kanbanRoot, column, filename, card); err != nil {
		log.error("Failed to save %s: %v", path, err)
	}
}

func transitionBlocked(column, filename, reason string) {
	if _, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusBlocked, reason); err != nil {
		log.error("Failed to mark ticket blocked: %v", err)
		return
	}
	log.warn("⛔  Ticket blocked — %s", reason)
}

func transitionBlockedWithFindings(column, filename, reason, findings string) {
	appendSection(column, filename, "Review Findings", findings)
	transitionBlocked(column, filename, reason)
}

func transitionBudgetExhausted(column, filename, reason string) {
	if _, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusBudgetExhausted, reason); err != nil {
		log.error("Failed to mark ticket budget-exhausted: %v", err)
		return
	}
	log.warn("⏱  Ticket budget-exhausted — %s", reason)
}

func transitionDone(column, filename, checkpoint, reason string) {
	withCard(column, filename, func(c *model.Card) { c.Checkpoint = checkpoint })
	if _, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusDone, reason); err != nil {
		log.error("Failed to mark ticket done: %v", err)
		return
	}
	log.info("✓  Ticket done (%s)", reason)
}

func transitionReview(column, filename, checkpoint, reason string) {
	withCard(column, filename, func(c *model.Card) { c.Checkpoint = checkpoint })
	if _, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusReview, reason); err != nil {
		log.error("Failed to move ticket to review: %v", err)
		return
	}
	log.info("✓  Ticket moved to review (%s)", reason)
}

// advanceStage moves a ticket to the next stage in its flow and returns it to
// todo so the ready-set query picks it up again for that stage — the same
// mechanism used for a fresh ticket, just re-entered mid-flow. Round resets:
// each stage gets its own review-round budget. BaseSHA resets too, for the
// same reason — the new stage's cumulative diff must start from where that
// stage begins, not carry over the prior stage's already-reviewed work.
func advanceStage(column, filename, nextStage, checkpoint string) {
	withCard(column, filename, func(c *model.Card) {
		c.Stage = nextStage
		c.Round = 0
		c.BaseSHA = ""
		c.Checkpoint = checkpoint
	})
	if _, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusTodo, ""); err != nil {
		log.error("Failed to advance ticket to stage %q: %v", nextStage, err)
		return
	}
	log.info("✓  Ticket advanced to stage %q", nextStage)
}

// cycleForReview sends a ticket with reviewer findings back to todo for
// another attempt at the same stage, with the findings appended to the
// ticket body so the next attempt's prompt (which includes the full ticket
// content) can see what went wrong — the thing a blind retry could not do.
func cycleForReview(column, filename string, round int, findings string) {
	appendSection(column, filename, "Review Findings", findings)
	withCard(column, filename, func(c *model.Card) { c.Round = round })
	if _, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusTodo, ""); err != nil {
		log.error("Failed to cycle ticket back for another review round: %v", err)
	}
}

// previewSHA renders an empty sha as "(none)" and a real one shortened to 8
// characters, so log lines stay scannable.
func previewSHA(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// previewText collapses newlines into spaces and truncates for a single-line
// log preview, so a multi-KB reviewer transcript doesn't flood the log.
func previewText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// previewHeadTail is like previewText but keeps both ends of long output.
// Real failures (permission denials, tool errors) tend to surface right
// before an agent gives up, at the very end of the transcript — a head-only
// preview silently hides exactly the part that explains why nothing happened.
func previewHeadTail(s string, headMax, tailMax int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= headMax+tailMax {
		return s
	}
	return s[:headMax] + " ...[truncated]... " + s[len(s)-tailMax:]
}

// agentDeclaredNoChangesNeeded reports whether the implementer explicitly
// stated the ticket needs no further changes, via the same kind of
// structural marker the reviewer uses for concerns — so ekbn can tell "the
// agent looked at the code and decided nothing needs to change" apart from
// "the agent produced no explanation for why it made no changes," which
// stays a blocked, human-reviewed outcome.
func agentDeclaredNoChangesNeeded(output string) bool {
	idx := strings.Index(output, "## No Changes Needed")
	if idx == -1 {
		return false
	}
	rest := strings.TrimSpace(output[idx+len("## No Changes Needed"):])
	return rest != ""
}

// abandonAttempt discards ticket id's uncommitted changes in dir, preserving
// them as a refs/attempts/... ref when there's anything to preserve. Without
// a worktree to simply throw away, this is the only way to undo an agent's
// edits — every failure branch that isn't a round-cycle-back calls this.
func abandonAttempt(dir, ticketID, column, filename string, preexisting map[string]bool) {
	ref, err := saveAttempt(dir, ticketID, preexisting)
	if err != nil {
		log.error("Failed to preserve/discard the attempt for ticket #%s: %v", ticketID, err)
	}
	if ref != "" {
		withCard(column, filename, func(c *model.Card) { c.AttemptRef = ref })
		log.info("   Failed attempt preserved at %s", ref)
	}
}

// runTicket runs exactly one stage of one ticket to completion (or to a
// terminal/paused state) directly in the project's working directory.

func runTicket(cfg serve.Config, card model.Card) {
	id := orDefault(card.ID, "???")
	title := card.Title
	column := card.Column
	filename := card.Filename

	if strings.TrimSpace(cfg.Verify) == "" {
		log.error("✗  No `verify` command configured in ekbn.config.yml — refusing to advance ticket #%s", id)
		return
	}

	goal := orDefault(card.Goal, "feature")
	flow := cfg.FlowFor(goal)
	if len(flow.Stages) == 0 {
		log.error("✗  No stage flow configured for goal %q on ticket #%s — refusing to advance", goal, id)
		return
	}
	stage := orDefault(card.Stage, flow.Stages[0])

	projectRoot, err := os.Getwd()
	if err != nil {
		log.error("Failed to resolve project directory: %v — will retry next cycle", err)
		return
	}
	if gitTreeDirty(projectRoot) {
		log.warn("Working tree has uncommitted changes to tracked files — deferring ticket #%s until it's clean", id)
		return
	}

	newColumn, err := model.TransitionStatus(kanbanRoot, column, filename, model.StatusInProgress, "")
	if err != nil {
		log.error("Failed to claim ticket #%s: %v", id, err)
		return
	}
	column = newColumn
	path := filepath.Join(kanbanRoot, column, filename)

	headSha, headErr := git(projectRoot, "rev-parse", "HEAD")
	if headErr != nil {
		log.warn("Ticket #%s: failed to resolve HEAD for base_sha (%v) — cumulative review diff will fall back to per-commit for this round", id, headErr)
	}

	withCard(column, filename, func(c *model.Card) {
		c.Stage = stage
		c.LeaseOwner = leaseOwner()
		c.LeaseExpires = time.Now().Add(leaseDuration).Format(time.RFC3339)
		c.Checkpoint = "claimed"
		if c.BaseSHA == "" {
			c.BaseSHA = headSha
		}
	})
	card, err = model.ReadCard(path, column)
	if err != nil {
		log.error("Failed to re-read ticket #%s after claim: %v", id, err)
		return
	}

	log.info("▶  Starting ticket #%s (goal=%s stage=%s round=%d base_sha=%s) — %s", id, goal, stage, card.Round, previewSHA(card.BaseSHA), title)

	if goal == "spike" {
		runSpike(cfg, path, column, filename, card, projectRoot)
		return
	}

	rc, fellBack := resolveRoleConfig(card.Role, cfg.Roles)
	if fellBack {
		if card.Role == "" {
			log.warn("Ticket #%s has no role set — using default agent", id)
		} else {
			log.warn("Ticket #%s has role %q, which is not configured — using default agent", id, card.Role)
		}
	}
	if strings.TrimSpace(rc.Command) == "" {
		log.warn("✗  Ticket #%s: no agent command configured for role %q — blocking", id, orDefault(card.Role, "default"))
		transitionBlocked(column, filename, "no-agent-command-configured")
		return
	}
	prompt := buildSystemPrompt(path, card, rc, flow) + "\n\n---\n\n" +
		"If, after reviewing the current code, this ticket's goal is already fully satisfied and no further changes are needed, " +
		"say so explicitly under a section titled exactly \"## No Changes Needed\", explaining why — and do not modify any files. " +
		"Only use this heading when you're certain nothing needs to change; if you make any edits, do not include it."

	preexisting := untrackedFiles(projectRoot)

	output, agentErr, usedGit, timedOut := runAgentAttempt(prompt, projectRoot, rc.Command, rc.MaxDurationMinutes)
	if timedOut {
		log.warn("⏱  Ticket #%s exceeded its %dm attempt budget", id, rc.MaxDurationMinutes)
		abandonAttempt(projectRoot, id, column, filename, preexisting)
		transitionBudgetExhausted(column, filename, fmt.Sprintf("exceeded %dm attempt budget for role %q", rc.MaxDurationMinutes, card.Role))
		return
	}
	if usedGit {
		log.warn("✋  Ticket #%s used git directly — blocking", id)
		abandonAttempt(projectRoot, id, column, filename, preexisting)
		transitionBlocked(column, filename, "agent-used-git")
		return
	}
	if agentErr == nil {
		log.info("   Agent ran to completion")
	} else {
		log.info("   Agent stopped early (exit code %d)", exitCode(agentErr))
	}
	log.info("   Agent output: %d bytes: %q", len(output), previewHeadTail(output, 200, 300))

	// declare_blocked can move this card out from under us mid-run — the one
	// write path a read-only agent retains. If that happened, the agent's own
	// declaration stands; nothing below should overwrite it with a verify/
	// review/done outcome.
	current, ok := findCard(id)
	if !ok {
		log.warn("Ticket #%s disappeared during its run — nothing more to do", id)
		return
	}
	if current.Status != model.StatusInProgress {
		log.info("Ticket #%s status changed to %q during its run — leaving it as declared", id, current.Status)
		return
	}
	column, filename = current.Column, current.Filename
	path = filepath.Join(kanbanRoot, column, filename)

	stageIdx := indexOf(flow.Stages, stage)
	isLastStage := stageIdx == len(flow.Stages)-1
	expectVerifyFailure := goal == "bug" && stageIdx == 0

	verr := runVerifyIn(cfg.Verify, projectRoot)
	verifyFailed := verr != nil

	if expectVerifyFailure && !verifyFailed {
		log.warn("✋  Ticket #%s could not reproduce the bug — verify passed at the reproduce stage", id)
		commitCardWork(projectRoot, id, title, preexisting)
		transitionBlocked(column, filename, "could-not-reproduce")
		return
	}
	if !expectVerifyFailure && verifyFailed {
		log.warn("   Verify failed: %v", verr)
		abandonAttempt(projectRoot, id, column, filename, preexisting)
		transitionBlocked(column, filename, "verify-failed")
		return
	}

	// Acceptance runs alongside verify, before the commit lands — the same
	// reasoning as verify itself: checking it against a later state would
	// make the result depend on whatever else landed in the meantime.
	if isLastStage && card.Acceptance != "" {
		if err := runVerifyIn(card.Acceptance, projectRoot); err != nil {
			log.warn("   Acceptance check failed: %v", err)
			abandonAttempt(projectRoot, id, column, filename, preexisting)
			transitionBlocked(column, filename, "acceptance-check-failed")
			return
		}
	}

	log.info("   Verify passed for ticket #%s — committing", id)
	sha, commitErr := commitCardWork(projectRoot, id, title, preexisting)
	if commitErr != nil {
		log.error("Failed to commit ticket #%s: %v", id, commitErr)
		abandonAttempt(projectRoot, id, column, filename, preexisting)
		transitionBlocked(column, filename, "commit-failed")
		return
	}
	if sha == "" {
		log.warn("   Ticket #%s changed nothing — reviewing an empty diff", id)
	}
	log.info("   Commit result: sha=%s (base_sha=%s)", previewSHA(sha), previewSHA(card.BaseSHA))

	currentHead, headErr := git(projectRoot, "rev-parse", "HEAD")
	if headErr != nil {
		log.error("Failed to resolve HEAD for ticket #%s's review diff: %v", id, headErr)
		transitionBlocked(column, filename, "review-error")
		return
	}

	diff, diffErr := rangeDiff(card.BaseSHA, currentHead)
	if diffErr != nil {
		log.error("Failed to compute the cumulative diff for review: %v", diffErr)
		transitionBlocked(column, filename, "review-error")
		return
	}
	log.info("   Cumulative diff base=%s..head=%s: %d bytes", previewSHA(card.BaseSHA), previewSHA(currentHead), len(diff))

	if diff == "" {
		if agentDeclaredNoChangesNeeded(output) {
			log.info("   Ticket #%s: agent confirmed no changes are needed — treating as passed", id)
			if !isLastStage {
				advanceStage(column, filename, flow.Stages[stageIdx+1], "no-changes-needed")
				return
			}
			if card.Acceptance != "" || cfg.DoneOnCleanReview {
				transitionDone(column, filename, "no-changes-needed", "no-changes-needed")
				return
			}
			transitionReview(column, filename, "no-changes-needed", "no-changes-needed")
			return
		}
		log.warn("✋  Ticket #%s has no changes to review since this stage began — blocking for human review", id)
		transitionBlockedWithFindings(column, filename, "no-changes-to-review",
			"No code has changed since this stage began (base "+previewSHA(card.BaseSHA)+" == current HEAD "+previewSHA(currentHead)+"). "+
				"Either the feature is already implemented and this ticket can be closed manually, or the agent made no progress this stage — see .kanban/orchestrator.log for this run's agent output.")
		return
	}

	if goal == "refactor" && stage == "tests-frozen" && card.BaseSHA != "" {
		files, filesErr := rangeDiffFiles(card.BaseSHA, currentHead)
		if filesErr != nil {
			log.error("Failed to compute touched files: %v", filesErr)
			transitionBlocked(column, filename, "review-error")
			return
		}
		if touched := testFilesTouched(files); len(touched) > 0 {
			log.warn("✋  Ticket #%s modified tests during a refactor: %s", id, strings.Join(touched, ", "))
			transitionBlockedWithFindings(column, filename, "tests-modified-during-refactor",
				"Tests must stay frozen during a refactor. Touched: "+strings.Join(touched, ", "))
			return
		}
	}

	ticketContent, _ := os.ReadFile(path)
	log.info("   Running reviewer for ticket #%s (diff=%d bytes, ticket content=%d bytes)", id, len(diff), len(ticketContent))
	findings, revErr := runReviewer(cfg, string(ticketContent), diff)
	if revErr != nil {
		log.error("Reviewer failed to run: %v", revErr)
		transitionBlocked(column, filename, "review-error")
		return
	}
	log.info("   Reviewer returned %d bytes: %q", len(findings), previewHeadTail(findings, 150, 250))
	if findings != "" && !isConcreteReviewFinding(findings) {
		log.info("   Reviewer output has no \"## Concern\" section — discarded, not treated as a finding")
	}
	if isConcreteReviewFinding(findings) {
		round := card.Round + 1
		if round > flow.MaxRounds {
			log.warn("🔒  Ticket #%s exhausted its %d review rounds", id, flow.MaxRounds)
			transitionBlockedWithFindings(column, filename, "review-rounds-exhausted", findings)
			return
		}
		log.warn("👀  Ticket #%s has reviewer findings (round %d/%d) — cycling back for another attempt", id, round, flow.MaxRounds)
		cycleForReview(column, filename, round, findings)
		return
	}

	if len(cfg.SecurityPaths) > 0 {
		touchedFiles, filesErr := rangeDiffFiles(card.BaseSHA, currentHead)
		if filesErr != nil {
			log.error("Failed to compute touched files for security review: %v", filesErr)
			transitionBlocked(column, filename, "review-error")
			return
		}
		if securityPathsMatch(touchedFiles, cfg.SecurityPaths) {
			log.info("   Ticket #%s touches security-sensitive paths — running security review", id)
			secFindings, secErr := runSecurityReviewer(cfg, string(ticketContent), diff)
			if secErr != nil {
				log.error("Security reviewer failed to run: %v", secErr)
				transitionBlocked(column, filename, "review-error")
				return
			}
			log.info("   Security reviewer returned %d bytes: %q", len(secFindings), previewHeadTail(secFindings, 150, 250))
			if secFindings != "" && isConcreteSecurityFinding(secFindings) {
				round := card.Round + 1
				if round > flow.MaxRounds {
					log.warn("🔒  Ticket #%s exhausted its %d review rounds on a security finding", id, flow.MaxRounds)
					transitionBlockedWithFindings(column, filename, "security-rounds-exhausted", secFindings)
					return
				}
				log.warn("🔒  Ticket #%s has a concrete security finding (round %d/%d)", id, round, flow.MaxRounds)
				cycleForReview(column, filename, round, secFindings)
				return
			}
			if secFindings != "" {
				log.warn("Ticket #%s: security reviewer output rejected — no concrete reproduction", id)
			}
		}
	}

	if !isLastStage {
		advanceStage(column, filename, flow.Stages[stageIdx+1], "committed:"+sha)
		return
	}

	if card.Acceptance != "" || cfg.DoneOnCleanReview {
		transitionDone(column, filename, "done:"+sha, "verify-green")
		return
	}
	transitionReview(column, filename, "reviewed:"+sha, "verify-green")
}

// runSpike runs the agent and records whatever it produced as a finding on
// the card itself. Nothing is committed — a spike's output is a written
// finding, not a change for anyone to review, so its edits are simply
// discarded afterward (via resetWorkingTree) regardless of what it did to
// the tree.
func runSpike(cfg serve.Config, path, column, filename string, card model.Card, projectRoot string) {
	id := orDefault(card.ID, "???")
	rc, fellBack := resolveRoleConfig(card.Role, cfg.Roles)
	if fellBack {
		log.warn("Ticket #%s (spike) has no matching role — using default agent", id)
	}
	if strings.TrimSpace(rc.Command) == "" {
		log.warn("✗  Spike #%s: no agent command configured for role %q — blocking", id, orDefault(card.Role, "default"))
		transitionBlocked(column, filename, "no-agent-command-configured")
		return
	}
	prompt := buildSystemPrompt(path, card, rc, cfg.FlowFor("spike"))

	preexisting := untrackedFiles(projectRoot)

	output, agentErr, usedGit, timedOut := runAgentAttempt(prompt, projectRoot, rc.Command, rc.MaxDurationMinutes)
	if timedOut {
		resetWorkingTree(projectRoot, preexisting)
		transitionBudgetExhausted(column, filename, fmt.Sprintf("exceeded %dm attempt budget for role %q", rc.MaxDurationMinutes, card.Role))
		return
	}
	if usedGit {
		resetWorkingTree(projectRoot, preexisting)
		transitionBlocked(column, filename, "agent-used-git")
		return
	}
	if agentErr != nil {
		log.info("   Spike agent #%s stopped early (exit code %d)", id, exitCode(agentErr))
	}
	log.info("   Agent output: %d bytes: %q", len(output), previewHeadTail(output, 200, 300))
	resetWorkingTree(projectRoot, preexisting)

	current, ok := findCard(id)
	if !ok {
		log.warn("Spike #%s disappeared during its run — nothing more to do", id)
		return
	}
	if current.Status != model.StatusInProgress {
		log.info("Spike #%s status changed to %q during its run — leaving it as declared", id, current.Status)
		return
	}
	column, filename = current.Column, current.Filename

	appendSection(column, filename, "Finding", orDefault(output, "(the agent produced no output)"))
	transitionDone(column, filename, "spike", "spike")
	log.info("✓  Spike #%s recorded its finding — nothing merged", id)
}

func runVerifyIn(cmdStr, dir string) error {
	log.info("   Running: %s", cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
