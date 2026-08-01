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
	"sync/atomic"
	"syscall"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

// activityWriter is a no-op io.Writer that only records when it was last
// written to — the single signal available for "is the agent still doing
// anything," since ekbn has no other visibility into an opaque agent CLI
// subprocess beyond the bytes it produces.
type activityWriter struct {
	lastWrite atomic.Int64 // unix nanos
}

func (a *activityWriter) Write(p []byte) (int, error) {
	a.lastWrite.Store(time.Now().UnixNano())
	return len(p), nil
}

func (a *activityWriter) idleFor() time.Duration {
	return time.Since(time.Unix(0, a.lastWrite.Load()))
}

// idleWatchdogInterval is how often the idle watchdog polls for silence —
// small enough that even a short test idleTimeout gets checked promptly,
// cheap enough (an atomic load and a comparison) that it costs nothing at
// real, minutes-scale production idleTimeout values.
const idleWatchdogInterval = 200 * time.Millisecond

func buildSystemPrompt(ticketPath string, card model.Card, rc serve.RoleConfig, flow serve.Flow) string {
	var parts []string
	if data, err := os.ReadFile(agentsMD); err == nil {
		parts = append(parts, string(data))
	} else if data, err := os.ReadFile(agentsMDFallback); err == nil {
		parts = append(parts, string(data))
	} else {
		log.warn("AGENTS.md not found (checked %s and %s) — agent will run without base instructions", agentsMD, agentsMDFallback)
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
	parts = append(parts, "Do not run git commands that write or mutate this repository or its history "+
		"(add, commit, checkout, reset, merge, rebase, push, branch, worktree, ...) — only read-only "+
		"inspection is permitted (e.g. "+strings.Join(readOnlyGitSubcommands, ", ")+"). Ekbn stages and "+
		"commits your changes automatically once you finish; running a disallowed git command blocks this "+
		"ticket outright rather than giving you a chance to correct course.")
	if card.Stage != "" && len(flow.States) > 0 {
		parts = append(parts, fmt.Sprintf(
			"You are working the %q state of this ticket's flow. Only do the work that belongs to this state — other states are separate agent runs.",
			card.Stage))
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
// blocked. If maxDuration is set, the subprocess is killed once that budget
// is exceeded and timedOut is reported. If idleTimeout is set, it's killed
// early — separately from maxDuration — once it produces no new stdout/
// stderr output for that long, and idleTimedOut is reported instead; a
// silent CLI is a different failure mode from a slow-but-working one, and
// callers react to the two differently. Both are duration-based rather than
// minute-counts so this is directly testable with sub-second values; callers
// convert a role's *Minutes config fields to a Duration themselves. Callers
// must not call this with an empty command — that case means "nothing is
// configured to run" and is handled before this point.
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

func runAgentAttempt(prompt, dir string, command serve.CommandSpec, maxDuration, idleTimeout time.Duration) (output string, runErr error, usedGit bool, usedGitCmd string, timedOut, idleTimedOut bool) {
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
printf '%%s\n' "git $*" > %q
echo "git '$subcmd' is not permitted here — only read-only commands (%s) are allowed; use the ekbn MCP tools for anything else" >&2
exit 1
`, strings.Join(readOnlyGitSubcommands, "|"), realGit, projectGitDir, realGit, marker, strings.Join(readOnlyGitSubcommands, ", "))
		os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0755)
		env = envWithPath(shimDir + string(os.PathListSeparator) + os.Getenv("PATH"))
	}

	// cancel always exists, regardless of whether maxDuration is set, so the
	// idle watchdog below has a way to kill the subprocess independently of
	// (and typically well before) any configured hard deadline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if maxDuration > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, maxDuration)
		defer timeoutCancel()
	}

	// cmd.Dir alone isn't always enough: some agent CLIs (opencode included)
	// resolve "the project" through their own persistent state rather than
	// process cwd — e.g. keyed off the repo's shared git commondir, which is
	// identical for a worktree and the main checkout — and silently operate
	// on a remembered path instead of dir. {workdir} lets a role's command
	// pass dir explicitly (e.g. `args: [--dir, "{workdir}", ...]`) to any
	// CLI that supports it, without ekbn hardcoding a specific tool's flag.
	program, args := serve.BuildArgv(command, dir, prompt)

	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.Env = env
	// A killed direct child (a shell script, typically) can leave its own
	// children — e.g. a long-running tool it invoked — as orphans still
	// holding the stdout/stderr pipes open; Wait() then blocks until those
	// grandchildren exit on their own, regardless of cancellation. Killing
	// the whole process group instead of just cmd.Process, plus a WaitDelay
	// as a backstop, is what actually makes ctx cancellation (idle-timeout
	// or the hard maxDuration) terminate the run promptly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	var buf bytes.Buffer
	activity := &activityWriter{}
	activity.Write(nil) // seed lastWrite at launch, not the zero time
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf, activity)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf, activity)

	var idleTriggered atomic.Bool
	watchdogDone := make(chan struct{})
	if idleTimeout > 0 {
		go func() {
			interval := idleWatchdogInterval
			if idleTimeout/4 < interval {
				interval = idleTimeout / 4
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-watchdogDone:
					return
				case <-ticker.C:
					if activity.idleFor() > idleTimeout {
						idleTriggered.Store(true)
						cancel()
						return
					}
				}
			}
		}()
	}

	runErr = cmd.Run()
	close(watchdogDone)
	timedOut = ctx.Err() == context.DeadlineExceeded
	idleTimedOut = idleTriggered.Load()
	if marker != "" {
		if data, readErr := os.ReadFile(marker); readErr == nil {
			usedGit = true
			usedGitCmd = strings.TrimSpace(string(data))
		}
	}
	return strings.TrimSpace(buf.String()), runErr, usedGit, usedGitCmd, timedOut, idleTimedOut
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

// advanceStage moves a ticket to the next state in its flow and returns it
// to todo so the ready-set query picks it up again for that state — the
// same mechanism used for a fresh ticket, just re-entered mid-flow. Round
// always resets: each state gets its own attempt budget (a repeated
// back-and-forth between two states never itself trips either state's own
// cap, since each resets to 0 on re-entry — TotalAttempts, which always
// increments here and never resets on its own, is the backstop that catches
// that case). BaseSHA is deliberately *not* touched here — unlike the old
// per-array-position stage model, a whole implement/verify/review cycle is
// now one continuous unit of work sharing a single cumulative diff; BaseSHA
// is set once, at first claim (see runTicket), and stays fixed until the
// ticket reaches a terminal state, regardless of how many states it visits
// or in which direction. Resetting it on every hop would make the reviewer
// see only the most recent fix's diff instead of the whole ticket's.
func advanceStage(column, filename, nextStage, checkpoint string) {
	withCard(column, filename, func(c *model.Card) {
		c.Stage = nextStage
		c.Round = 0
		c.TotalAttempts++
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
	withCard(column, filename, func(c *model.Card) { c.Round = round; c.TotalAttempts++ })
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

// runTicket runs exactly one flow state of one ticket to completion (or to
// a terminal/paused state) directly in the project's working directory,
// then dispatches to that state's type-specific handler.

func runTicket(cfg serve.Config, card model.Card) {
	id := orDefault(card.ID, "???")
	title := card.Title
	column := card.Column
	filename := card.Filename

	goal := orDefault(card.Goal, "feature")
	flow := cfg.FlowFor(goal)
	if len(flow.States) == 0 {
		log.error("✗  No flow configured for goal %q on ticket #%s — refusing to advance", goal, id)
		return
	}
	stateName := orDefault(card.Stage, flow.Entry)
	state, stateExists := flow.States[stateName]
	if !stateExists {
		log.error("✗  Ticket #%s: state %q is not defined in the %q flow — refusing to advance", id, stateName, goal)
		return
	}

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

	mainCheckoutMu.Lock()
	mainHead, headErr := git(projectRoot, "rev-parse", "HEAD")
	mainCheckoutMu.Unlock()
	if headErr != nil {
		log.warn("Ticket #%s: failed to resolve HEAD for base_sha (%v) — cumulative review diff will fall back to per-commit for this round", id, headErr)
	}

	// A worktree is created (or reused, for a later round/state) right after
	// claim, unconditionally — including for spikes and tickets that turn
	// out to have no agent configured. Both of those tear it straight back
	// down (see below) rather than needing a second code path that skips
	// creation, keeping this one claim sequence uniform for every ticket.
	workDir, wtErr := ensureWorktree(projectRoot, id, mainHead)
	if wtErr != nil {
		log.error("Failed to prepare worktree for ticket #%s: %v — will retry next cycle", id, wtErr)
		return
	}
	headSha := mainHead
	if wh, whErr := git(workDir, "rev-parse", "HEAD"); whErr == nil {
		headSha = wh
	}

	withCard(column, filename, func(c *model.Card) {
		c.Stage = stateName
		c.LeaseOwner = leaseOwner()
		c.LeaseExpires = time.Now().Add(leaseDuration).Format(time.RFC3339)
		c.Checkpoint = "claimed"
		c.Worktree = workDir
		// BaseSHA is set once, the first time a ticket is ever claimed, and
		// deliberately never reset by a state transition (see advanceStage) —
		// the whole flow run shares one cumulative diff.
		if c.BaseSHA == "" {
			c.BaseSHA = headSha
		}
	})
	card, err = model.ReadCard(path, column)
	if err != nil {
		log.error("Failed to re-read ticket #%s after claim: %v", id, err)
		return
	}

	log.info("▶  Starting ticket #%s (goal=%s state=%s round=%d/%d total=%d/%d base_sha=%s worktree=%s) — %s",
		id, goal, stateName, card.Round, stateMaxAttempts(state), card.TotalAttempts, defaultMaxTotalAttempts, previewSHA(card.BaseSHA), workDir, title)

	if goal == "spike" {
		removeWorktreeIfPresent(projectRoot, id)
		withCard(column, filename, func(c *model.Card) { c.Worktree = "" })
		runSpike(cfg, path, column, filename, card, projectRoot)
		return
	}

	switch state.Type {
	case "verify":
		runVerifyState(cfg, flow, stateName, state, card, column, filename, projectRoot, workDir, id)
	case "review":
		runReviewState(cfg, flow, stateName, state, card, column, filename, projectRoot, workDir, id)
	default:
		runImplementState(cfg, flow, stateName, state, card, column, filename, projectRoot, workDir, id, title)
	}
}

// runVerifyState runs a verify-type state's command (its own Command,
// falling back to cfg.Verify) against the ticket's worktree — its pass/fail
// *is* its outcome, fed straight into the flow's transition table. Whether
// this specific verify state's success means "the ticket is fine" or "the
// bug still doesn't reproduce" is entirely up to what that state's own On
// map routes success to — no expectVerifyFailure special case needed.
func runVerifyState(cfg serve.Config, flow serve.Flow, stateName string, state serve.FlowState, card model.Card, column, filename, projectRoot, workDir, id string) {
	cmdStr := state.Command
	if cmdStr == "" {
		cmdStr = cfg.Verify
	}
	if strings.TrimSpace(cmdStr) == "" {
		log.error("✗  Ticket #%s: no command configured for verify state %q (set state.command or the top-level verify:) — refusing to advance", id, stateName)
		return
	}
	outcome := model.OutcomeSuccess
	if verr := runVerifyIn(cmdStr, workDir); verr != nil {
		log.warn("   Verify failed: %v", verr)
		outcome = model.OutcomeFailed
	} else {
		log.info("   Verify passed for ticket #%s", id)
	}
	advanceOrBlock(projectRoot, id, card, flow, stateName, outcome, column, filename, "verify:"+string(outcome), "")
}

// runReviewState runs the review gate (and, if the accumulated diff touches
// a configured security path, the security gate too) against the ticket's
// cumulative diff since BaseSHA — an isolated review session, not an
// implementer agent working in the worktree, so this is its own state type
// rather than a Role-backed implement-shaped one.
func runReviewState(cfg serve.Config, flow serve.Flow, stateName string, state serve.FlowState, card model.Card, column, filename, projectRoot, workDir, id string) {
	currentHead, headErr := git(workDir, "rev-parse", "HEAD")
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

	path := filepath.Join(kanbanRoot, column, filename)
	ticketContent, _ := os.ReadFile(path)
	reviewableContent := stripReviewFindings(string(ticketContent))
	log.info("   Running reviewer for ticket #%s (diff=%d bytes, ticket content=%d bytes)", id, len(diff), len(ticketContent))
	findings, revErr := runReviewer(cfg, reviewableContent, diff)
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
		log.warn("👀  Ticket #%s has reviewer findings — routing back per the flow's findings transition", id)
		advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeFindings, column, filename, "review-findings", findings)
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
			secFindings, secErr := runSecurityReviewer(cfg, reviewableContent, diff)
			if secErr != nil {
				log.error("Security reviewer failed to run: %v", secErr)
				transitionBlocked(column, filename, "review-error")
				return
			}
			log.info("   Security reviewer returned %d bytes: %q", len(secFindings), previewHeadTail(secFindings, 150, 250))
			if secFindings != "" && isConcreteSecurityFinding(secFindings) {
				log.warn("🔒  Ticket #%s has a concrete security finding — routing back per the flow's findings transition", id)
				advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeFindings, column, filename, "security-findings", secFindings)
				return
			}
			if secFindings != "" {
				log.warn("Ticket #%s: security reviewer output rejected — no concrete reproduction", id)
			}
		}
	}

	advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeSuccess, column, filename, "reviewed:"+previewSHA(currentHead), "")
}

// runImplementState runs a Role-backed executor stage: the agent CLI in the
// ticket's own worktree, then commit/diff/outcome classification. Role
// falls back to the card's own Role when the state doesn't name one —
// preserving "the card picks its implementer role" for implement-shaped
// states while letting review-shaped states (handled separately, above)
// pick their own fixed role regardless of what the card says.
func runImplementState(cfg serve.Config, flow serve.Flow, stateName string, state serve.FlowState, card model.Card, column, filename, projectRoot, workDir, id, title string) {
	roleName := state.Role
	if roleName == "" {
		roleName = card.Role
	}
	ec, rc, fellBack, resolveErr := serve.ResolveExecutor(roleName, cfg)
	if resolveErr != nil {
		log.warn("✗  Ticket #%s: %v — blocking", id, resolveErr)
		transitionBlocked(column, filename, "unknown-executor")
		return
	}
	if fellBack {
		if roleName == "" {
			log.warn("Ticket #%s has no role set — using default agent", id)
		} else {
			log.warn("Ticket #%s has role %q, which is not configured — using default agent", id, roleName)
		}
	}
	if strings.TrimSpace(ec.Command.Program) == "" {
		log.warn("✗  Ticket #%s: no agent command configured for role %q — blocking", id, orDefault(roleName, "default"))
		transitionBlocked(column, filename, "no-agent-command-configured")
		return
	}

	// A model told only "say so if nothing needs to change" doesn't reliably
	// reach for that marker on a state whose entire job is confirmation, not
	// modification. Telling it explicitly that this is normal (the same fix
	// that made the reviewer's "no concerns" output reliable) closes that gap.
	noChangesInstruction := "If, after reviewing the current code, this ticket's goal is already fully satisfied and no further changes are needed, " +
		"say so explicitly under a section titled exactly \"## No Changes Needed\", explaining why — and do not modify any files. " +
		"Only use this heading when you're certain nothing needs to change; if you make any edits, do not include it. Reaching this state's " +
		"conclusion with no further code changes needed is a normal, common, and expected outcome, not a failure."
	path := filepath.Join(kanbanRoot, column, filename)
	prompt := buildSystemPrompt(path, card, rc, flow) + "\n\n---\n\n" + noChangesInstruction

	preexisting := untrackedFiles(workDir)
	mainCheckoutMu.Lock()
	mainBefore, _ := git(projectRoot, "status", "--short")
	mainCheckoutMu.Unlock()

	output, agentErr, usedGit, usedGitCmd, timedOut, idleTimedOut := runAgentAttempt(prompt, workDir, ec.Command,
		time.Duration(ec.MaxDurationMinutes)*time.Minute, time.Duration(ec.IdleTimeoutMinutes)*time.Minute)

	mainCheckoutMu.Lock()
	escErr := checkMainCheckoutUntouched(projectRoot, mainBefore)
	mainCheckoutMu.Unlock()
	if escErr != nil {
		log.error("✋  Ticket #%s: %v — the agent likely escaped its worktree", id, escErr)
		abandonAttempt(workDir, id, column, filename, preexisting)
		transitionBlockedWithFindings(column, filename, "agent-escaped-worktree", escErr.Error())
		return
	}
	maxAttempts := stateMaxAttempts(state)
	// A stall (no output at all for idleTimeout, distinct from just taking a
	// while to finish) is treated as a transient CLI glitch worth retrying,
	// bounded the same way a reviewer's findings cycle a ticket back — rather
	// than immediately spending the ticket's one shot at budget-exhausted the
	// way a genuine MaxDurationMinutes timeout below still does.
	if idleTimedOut {
		round := card.Round + 1
		if round > maxAttempts {
			log.warn("🔒  Ticket #%s exhausted its %d attempt budget — the agent kept stalling with no output", id, maxAttempts)
			abandonAttempt(workDir, id, column, filename, preexisting)
			transitionBlockedWithFindings(column, filename, "agent-idle-rounds-exhausted",
				fmt.Sprintf("The agent produced no output for %d minutes and was killed, on every attempt up to this state's %d-round budget. Its last output before stalling:\n\n%s",
					ec.IdleTimeoutMinutes, maxAttempts, previewText(output, 2000)))
			return
		}
		log.warn("💤  Ticket #%s's agent produced no output for %dm — treating as stalled (attempt %d/%d), retrying", id, ec.IdleTimeoutMinutes, round, maxAttempts)
		abandonAttempt(workDir, id, column, filename, preexisting)
		cycleForReview(column, filename, round,
			fmt.Sprintf("Attempt %d was killed after producing no output for %d minutes — likely stalled; retrying automatically. Its last output before stalling:\n\n%s",
				round, ec.IdleTimeoutMinutes, previewText(output, 2000)))
		return
	}
	if timedOut {
		log.warn("⏱  Ticket #%s exceeded its %dm attempt budget", id, ec.MaxDurationMinutes)
		abandonAttempt(workDir, id, column, filename, preexisting)
		transitionBudgetExhausted(column, filename, fmt.Sprintf("exceeded %dm attempt budget for role %q", ec.MaxDurationMinutes, roleName))
		return
	}
	if usedGit {
		log.warn("✋  Ticket #%s used git directly (%s) — blocking", id, usedGitCmd)
		abandonAttempt(workDir, id, column, filename, preexisting)
		transitionBlockedWithFindings(column, filename, "agent-used-git",
			fmt.Sprintf("The agent ran a disallowed git command directly instead of using the ekbn MCP tools: `%s`", usedGitCmd))
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

	sha, commitErr := commitCardWork(workDir, id, title, preexisting)
	if commitErr != nil {
		log.error("Failed to commit ticket #%s: %v", id, commitErr)
		abandonAttempt(workDir, id, column, filename, preexisting)
		transitionBlocked(column, filename, "commit-failed")
		return
	}
	if sha != "" {
		log.info("   Committed %s", previewSHA(sha))
	}

	currentHead, headErr := git(workDir, "rev-parse", "HEAD")
	if headErr != nil {
		log.error("Failed to resolve HEAD for ticket #%s: %v", id, headErr)
		transitionBlocked(column, filename, "review-error")
		return
	}
	diff, diffErr := rangeDiff(card.BaseSHA, currentHead)
	if diffErr != nil {
		log.error("Failed to compute the cumulative diff: %v", diffErr)
		transitionBlocked(column, filename, "review-error")
		return
	}
	log.info("   Cumulative diff base=%s..head=%s: %d bytes", previewSHA(card.BaseSHA), previewSHA(currentHead), len(diff))

	if diff == "" {
		if agentDeclaredNoChangesNeeded(output) {
			log.info("   Ticket #%s: agent confirmed no changes are needed — treating as success", id)
			advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeSuccess, column, filename, "no-changes-needed", "")
			return
		}
		// An agent that exited with an error before touching anything never
		// got to attempt the work at all — retrying is more useful than
		// misreporting a system failure (a crashed CLI, a resource conflict)
		// as "nothing needed changing."
		if agentErr != nil {
			round := card.Round + 1
			if round > maxAttempts {
				log.warn("🔒  Ticket #%s exhausted its %d attempt budget — the agent kept erroring out before making any changes", id, maxAttempts)
				transitionBlockedWithFindings(column, filename, "agent-error-rounds-exhausted",
					fmt.Sprintf("The agent exited with an error (exit code %d) and made no changes, on every attempt up to this state's %d-round budget. Its last output:\n\n%s",
						exitCode(agentErr), maxAttempts, previewText(output, 2000)))
				return
			}
			log.warn("🔁  Ticket #%s's agent exited with an error (exit code %d) before making any changes (attempt %d/%d) — retrying", id, exitCode(agentErr), round, maxAttempts)
			cycleForReview(column, filename, round,
				fmt.Sprintf("Attempt %d exited with an error (exit code %d) before making any changes — retrying automatically:\n\n%s",
					round, exitCode(agentErr), previewText(output, 2000)))
			return
		}
		log.warn("✋  Ticket #%s has no changes since this state began — blocking for human review", id)
		advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeAmbiguous, column, filename, "no-changes",
			"No code has changed since this state began (base "+previewSHA(card.BaseSHA)+" == current HEAD "+previewSHA(currentHead)+"). "+
				"Either the feature is already implemented and this ticket can be closed manually, or the agent made no progress — see .kanban/orchestrator.log for this run's agent output.")
		return
	}

	if state.ForbidTestEdits {
		files, filesErr := rangeDiffFiles(card.BaseSHA, currentHead)
		if filesErr != nil {
			log.error("Failed to compute touched files: %v", filesErr)
			transitionBlocked(column, filename, "review-error")
			return
		}
		if touched := testFilesTouched(files); len(touched) > 0 {
			log.warn("✋  Ticket #%s modified tests during a refactor: %s", id, strings.Join(touched, ", "))
			advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeBlocked, column, filename, "tests-modified",
				"Tests must stay frozen during a refactor. Touched: "+strings.Join(touched, ", "))
			return
		}
	}

	advanceOrBlock(projectRoot, id, card, flow, stateName, model.OutcomeSuccess, column, filename, "committed:"+sha, "")
}

// runSpike runs the agent and records whatever it produced as a finding on
// the card itself. Nothing is committed — a spike's output is a written
// finding, not a change for anyone to review, so its edits are simply
// discarded afterward (via resetWorkingTree) regardless of what it did to
// the tree.
func runSpike(cfg serve.Config, path, column, filename string, card model.Card, projectRoot string) {
	id := orDefault(card.ID, "???")
	ec, rc, fellBack, resolveErr := serve.ResolveExecutor(card.Role, cfg)
	if resolveErr != nil {
		log.warn("✗  Spike #%s: %v — blocking", id, resolveErr)
		transitionBlocked(column, filename, "unknown-executor")
		return
	}
	if fellBack {
		log.warn("Ticket #%s (spike) has no matching role — using default agent", id)
	}
	if strings.TrimSpace(ec.Command.Program) == "" {
		log.warn("✗  Spike #%s: no agent command configured for role %q — blocking", id, orDefault(card.Role, "default"))
		transitionBlocked(column, filename, "no-agent-command-configured")
		return
	}
	flow := cfg.FlowFor("spike")
	prompt := buildSystemPrompt(path, card, rc, flow)

	preexisting := untrackedFiles(projectRoot)

	output, agentErr, usedGit, usedGitCmd, timedOut, idleTimedOut := runAgentAttempt(prompt, projectRoot, ec.Command,
		time.Duration(ec.MaxDurationMinutes)*time.Minute, time.Duration(ec.IdleTimeoutMinutes)*time.Minute)
	if idleTimedOut {
		round := card.Round + 1
		maxAttempts := defaultMaxAttempts
		if state, ok := flow.States[orDefault(card.Stage, flow.Entry)]; ok {
			maxAttempts = stateMaxAttempts(state)
		}
		if round > maxAttempts {
			resetWorkingTree(projectRoot, preexisting)
			transitionBlockedWithFindings(column, filename, "agent-idle-rounds-exhausted",
				fmt.Sprintf("The agent produced no output for %d minutes and was killed. Its last output before stalling:\n\n%s",
					ec.IdleTimeoutMinutes, previewText(output, 2000)))
			return
		}
		log.warn("💤  Spike #%s's agent produced no output for %dm — treating as stalled (attempt %d/%d), retrying", id, ec.IdleTimeoutMinutes, round, maxAttempts)
		resetWorkingTree(projectRoot, preexisting)
		cycleForReview(column, filename, round,
			fmt.Sprintf("Attempt %d was killed after producing no output for %d minutes — likely stalled; retrying automatically. Its last output before stalling:\n\n%s",
				round, ec.IdleTimeoutMinutes, previewText(output, 2000)))
		return
	}
	if timedOut {
		resetWorkingTree(projectRoot, preexisting)
		transitionBudgetExhausted(column, filename, fmt.Sprintf("exceeded %dm attempt budget for role %q", ec.MaxDurationMinutes, card.Role))
		return
	}
	if usedGit {
		resetWorkingTree(projectRoot, preexisting)
		transitionBlockedWithFindings(column, filename, "agent-used-git",
			fmt.Sprintf("The agent ran a disallowed git command directly instead of using the ekbn MCP tools: `%s`", usedGitCmd))
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
