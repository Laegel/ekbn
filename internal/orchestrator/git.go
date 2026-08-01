package orchestrator

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// Git plumbing. Every function here takes an explicit working directory
// rather than relying on process cwd.

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, errBuf.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func gitEnv(dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), extraEnv...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, errBuf.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// gitTreeDirty gates ticket scheduling: a tracked-file modification (staged,
// unstaged, or conflicted) means the working directory isn't ours to touch
// right now, so the caller should defer rather than fold a developer's own
// uncommitted work into an automated commit or risk reverting it. Untracked
// files (ekbn's own scaffolding, or output the agent is about to produce)
// never block — only kanbanRoot's own churn is excluded outright.
func gitTreeDirty(dir string) bool {
	out, err := git(dir, "status", "--porcelain", "--", ".", ":(exclude)"+kanbanRoot)
	if err != nil {
		return false // can't tell — don't wedge scheduling over a transient git error
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "??") {
			continue
		}
		return true
	}
	return false
}

// resetWorkingTree restores dir to HEAD and removes any untracked file not
// already present in preexisting — the "hand the tree back exactly as it
// was" step shared by every way an attempt can be abandoned.
func resetWorkingTree(dir string, preexisting map[string]bool) error {
	if _, err := git(dir, "reset", "--hard"); err != nil {
		return err
	}
	for path := range untrackedFiles(dir) {
		if !preexisting[path] {
			os.RemoveAll(filepath.Join(dir, path))
		}
	}
	return nil
}

// stageCardWork stages everything except the kanban directory and any
// preexisting untracked file. Card files are the orchestrator's bookkeeping,
// not the agent's work, and committing them would put every lease timestamp
// into the project's history; preexisting untracked files (ekbn's own
// scaffolding — AGENTS.md, ekbn.config.yml, an agent CLI's own project
// config, specs/ — or anything else already sitting in the working
// directory before the agent ran) are the operator's own setup, not the
// ticket's deliverable, and would otherwise get swept into its commit now
// that there's no separate worktree keeping them out of the tree entirely.
func stageCardWork(dir string, preexisting map[string]bool) error {
	if _, err := git(dir, "add", "-A"); err != nil {
		return err
	}
	unstage := append([]string{"reset", "-q", "HEAD", "--", kanbanRoot}, mapKeys(preexisting)...)
	git(dir, unstage...)
	return nil
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// commitCardWork commits the agent's work and returns the new commit's sha.
// An empty sha means the agent changed nothing.
func commitCardWork(dir, ticketID, title string, preexisting map[string]bool) (string, error) {
	if err := stageCardWork(dir, preexisting); err != nil {
		return "", err
	}
	staged, _ := git(dir, "diff", "--cached", "--name-only")
	log.info("   Staged for commit (excluding %d preexisting file(s): %s): %s",
		len(preexisting), previewText(strings.Join(mapKeys(preexisting), ", "), 150), previewText(staged, 200))
	if _, err := git(dir, "diff", "--cached", "--quiet"); err == nil {
		log.info("   Nothing staged relative to HEAD — commit skipped")
		return "", nil
	}
	if _, err := git(dir, "commit", "-m", fmt.Sprintf("%s: %s", ticketID, title)); err != nil {
		return "", err
	}
	sha, err := git(dir, "rev-parse", "HEAD")
	if err == nil {
		log.info("   Committed %s", previewSHA(sha))
	}
	return sha, err
}

func untrackedFiles(dir string) map[string]bool {
	set := map[string]bool{}
	out, err := git(dir, "ls-files", "--others", "--exclude-standard", "--", ".", ":(exclude)"+kanbanRoot)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}

// snapshotTree records the current working tree as a commit object without
// touching HEAD, the branch, or the real index — a throwaway index keeps the
// capture side-effect free. preexisting untracked files (ekbn's own
// scaffolding, an agent CLI's config, ...) are excluded, the same as
// stageCardWork, so the preserved attempt reflects only what the agent
// actually changed.
func snapshotTree(dir, ticketID string, preexisting map[string]bool) (string, error) {
	f, err := os.CreateTemp("", "ekbn-index-")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	f.Close()
	os.Remove(idx)
	defer os.Remove(idx)

	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := gitEnv(dir, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	pathspec := []string{"add", "-A", "--", ".", ":(exclude)" + kanbanRoot}
	for path := range preexisting {
		pathspec = append(pathspec, ":(exclude)"+path)
	}
	if _, err := gitEnv(dir, env, pathspec...); err != nil {
		return "", err
	}
	tree, err := gitEnv(dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if headTree, err := git(dir, "rev-parse", "HEAD^{tree}"); err == nil && headTree == tree {
		return "", nil
	}
	return gitEnv(dir, env, "commit-tree", tree, "-p", head, "-m",
		fmt.Sprintf("failed attempt for ticket %s", ticketID))
}

// saveAttempt preserves a failed attempt as a ref, then restores the tree.
// Returns the ref name, or "" if there was nothing to save.
func saveAttempt(dir, ticketID string, preexisting map[string]bool) (string, error) {
	sha, err := snapshotTree(dir, ticketID, preexisting)
	if err != nil {
		return "", err
	}
	ref := ""
	if sha != "" {
		ref = fmt.Sprintf("refs/attempts/%s/%s", ticketID, time.Now().UTC().Format("20060102T150405Z"))
		if _, err := git(dir, "update-ref", ref, sha); err != nil {
			return "", err
		}
	}
	if err := resetWorkingTree(dir, preexisting); err != nil {
		return ref, err
	}
	return ref, nil
}

// commitDiff returns the patch introduced by one commit. Object storage is
// keyed by sha, not by working directory, so this needs no dir.
func commitDiff(sha string) (string, error) {
	if sha == "" {
		return "", nil
	}
	return git("", "show", "--format=", sha)
}

func commitDiffFiles(sha string) ([]string, error) {
	if sha == "" {
		return nil, nil
	}
	out, err := git("", "show", "--format=", "--name-only", sha)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// rangeDiff returns the cumulative patch from base to head — the diff a
// human looking at the repo would see for everything done on this
// ticket/stage so far, across every review round, not just the latest
// round's own commit. History here is always linear (ekbn never merges),
// so a plain two-dot diff is exactly the range of commits between the two.
func rangeDiff(base, head string) (string, error) {
	if base == "" || head == "" || base == head {
		return "", nil
	}
	return git("", "diff", base, head)
}

func rangeDiffFiles(base, head string) ([]string, error) {
	if base == "" || head == "" || base == head {
		return nil, nil
	}
	out, err := git("", "diff", "--name-only", base, head)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// securityPathsMatch is the entire "selection" logic for the security review
// gate: a mechanical glob lookup against touched files, never a model asked
// whether a change "seems security-relevant."
func securityPathsMatch(touchedFiles []string, globs []string) bool {
	for _, f := range touchedFiles {
		for _, g := range globs {
			if matched, _ := doublestar.Match(g, f); matched {
				return true
			}
		}
	}
	return false
}

var testFileGlobs = []string{
	"**/*_test.go", "**/*.test.ts", "**/*.test.tsx", "**/*.spec.ts", "**/*.spec.tsx",
	"**/test_*.py", "**/*_test.py",
}

// testFilesTouched is the mechanical check behind the refactor flow's
// "tests frozen" stage: a change that touches a test file during a refactor
// is rejected structurally, not by asking a model whether behavior changed.
func testFilesTouched(files []string) []string {
	var touched []string
	for _, f := range files {
		for _, g := range testFileGlobs {
			if matched, _ := doublestar.Match(g, f); matched {
				touched = append(touched, f)
				break
			}
		}
	}
	return touched
}
