package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// mainCheckoutMu serializes the quick administrative git calls that touch
// the shared main checkout — resolving mainHead, creating/merging/removing a
// worktree, the escape-check status reads — so two tickets' git plumbing
// never races on the same .git directory. It deliberately does not cover the
// agent's own run, which stays fully concurrent inside each ticket's own
// worktree; only these brief calls need to be serialized.
var mainCheckoutMu sync.Mutex

// worktreeDir returns the deterministic filesystem path for a ticket's
// isolated worktree, derived from projectRoot and the ticket's ID alone —
// no new Card field is needed to remember where it lives. It's a sibling of
// the project directory rather than somewhere under the project's own tree
// (e.g. os.TempDir()): nothing inside it can walk up and find the main
// repo's real .git — the concrete mitigation for the escape bug worktree
// isolation was removed over previously — while still preserving whatever
// the project resolves via ".." (a sibling path dependency, for instance),
// which a location like os.TempDir() silently breaks.
func worktreeDir(projectRoot, id string) string {
	return filepath.Join(filepath.Dir(projectRoot), "ekbn-worktree-"+id)
}

// worktreeBranch returns the branch name a ticket's worktree is checked out
// on, matching the ekbn/<id> convention already visible on a leftover
// branch from before worktrees were first removed.
func worktreeBranch(id string) string {
	return "ekbn/" + id
}

// ensureWorktree returns a ticket's worktree directory, creating it (and
// its branch, based at baseSHA) on first use and reusing it across later
// rounds/stages of the same ticket so reviewer-requested changes build on
// the same in-progress branch instead of starting over.
func ensureWorktree(projectRoot, id, baseSHA string) (string, error) {
	mainCheckoutMu.Lock()
	defer mainCheckoutMu.Unlock()

	dir := worktreeDir(projectRoot, id)
	branch := worktreeBranch(id)

	list, err := git(projectRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("listing worktrees: %w", err)
	}
	if strings.Contains(list, dir) {
		return dir, nil
	}

	// A branch can survive without its worktree (e.g. a previous crash) —
	// -b would fail on an existing branch, so reuse it instead of trying
	// to recreate it.
	if _, err := git(projectRoot, "rev-parse", "--verify", branch); err == nil {
		if _, err := git(projectRoot, "worktree", "add", dir, branch); err != nil {
			return "", fmt.Errorf("adding worktree for existing branch %s: %w", branch, err)
		}
		return dir, nil
	}

	// A stale directory can also survive at this path with no worktree
	// registered in it (same crash scenario) — clear it so `worktree add`
	// doesn't fail on a non-empty target.
	if _, err := os.Stat(dir); err == nil {
		os.RemoveAll(dir)
	}

	if _, err := git(projectRoot, "worktree", "add", "-b", branch, dir, baseSHA); err != nil {
		return "", fmt.Errorf("adding worktree for ticket #%s: %w", id, err)
	}
	return dir, nil
}

// mergeAndRemoveWorktree merges a ticket's branch into projectRoot and tears
// down its worktree. With more than one ticket in flight, both can branch
// from the same mainHead — whichever merges second is no longer a
// fast-forward once the first has landed. The fast path (plain --ff-only)
// still handles the common case cheaply; on failure, the ticket's branch is
// rebased onto main's new tip (rewriting it in place, since the worktree's
// HEAD is a symbolic ref to the branch) and the fast-forward is retried. If
// the rebase itself conflicts, it's aborted — restoring the worktree exactly
// as it was — and the conflict is surfaced as an error rather than resolved
// automatically; the worktree and branch are left in place for a human to
// resolve, since this returns before reaching the teardown calls below.
func mergeAndRemoveWorktree(projectRoot, id string) error {
	mainCheckoutMu.Lock()
	defer mainCheckoutMu.Unlock()

	branch := worktreeBranch(id)
	dir := worktreeDir(projectRoot, id)

	if _, err := git(projectRoot, "merge", "--ff-only", branch); err != nil {
		mainHead, headErr := git(projectRoot, "rev-parse", "HEAD")
		if headErr != nil {
			return fmt.Errorf("resolving main HEAD after non-fast-forward merge of %s: %w", branch, headErr)
		}
		if _, rebaseErr := git(dir, "rebase", mainHead); rebaseErr != nil {
			git(dir, "rebase", "--abort")
			return fmt.Errorf("rebasing %s onto main (%s) after non-fast-forward merge: %w", branch, previewSHA(mainHead), rebaseErr)
		}
		if _, err := git(projectRoot, "merge", "--ff-only", branch); err != nil {
			return fmt.Errorf("fast-forward merging %s after rebase onto %s: %w", branch, previewSHA(mainHead), err)
		}
	}
	if _, err := git(projectRoot, "worktree", "remove", dir, "--force"); err != nil {
		return fmt.Errorf("removing worktree %s: %w", dir, err)
	}
	if _, err := git(projectRoot, "branch", "-d", branch); err != nil {
		return fmt.Errorf("deleting branch %s: %w", branch, err)
	}
	return nil
}

// removeWorktreeIfPresent tears down a ticket's worktree and branch without
// merging — used when a blocked ticket is reset back to todo (so its next
// claim starts a completely fresh worktree from a fresh base_sha instead of
// resuming a stale one) and when a spike ticket finishes (spikes reset
// their own scratch state directly and never commit, so they never need
// worktree isolation in the first place).
func removeWorktreeIfPresent(projectRoot, id string) {
	dir := worktreeDir(projectRoot, id)
	branch := worktreeBranch(id)

	list, err := git(projectRoot, "worktree", "list", "--porcelain")
	if err != nil || !strings.Contains(list, dir) {
		return
	}
	if _, err := git(projectRoot, "worktree", "remove", dir, "--force"); err != nil {
		log.error("Failed to remove worktree %s: %v", dir, err)
		return
	}
	if _, err := git(projectRoot, "branch", "-D", branch); err != nil {
		log.error("Failed to delete branch %s: %v", branch, err)
	}
}

// checkMainCheckoutUntouched compares the shared main checkout's status
// before and after an agent's run. Ticket work is supposed to happen
// entirely inside the ticket's own worktree; if the main checkout changed
// at all, the agent wrote outside its assigned directory — exactly the
// failure mode worktree isolation was removed over previously, this time
// caught and logged instead of silently corrupting the shared checkout.
func checkMainCheckoutUntouched(projectRoot, before string) error {
	after, err := git(projectRoot, "status", "--short")
	if err != nil {
		return fmt.Errorf("checking main checkout status: %w", err)
	}
	if after != before {
		return fmt.Errorf("main checkout changed during the run — before: %q, after: %q", before, after)
	}
	return nil
}
