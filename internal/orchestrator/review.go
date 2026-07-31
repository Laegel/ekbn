package orchestrator

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"ekbn/internal/serve"
)

// Review sessions.

// buildReviewPrompt is deliberately fixed, not configurable: this is a
// structural gate, not a customizable feature. isConcreteReviewFinding
// enforces the "## Concern" convention mechanically, the same way
// isConcreteSecurityFinding does for the security reviewer — a model that
// ignores the instructions below and produces other output (a banner, an
// exploration transcript, "LGTM") can no longer accidentally block the card.
func buildReviewPrompt(ticketContent, diff string) string {
	return "You are reviewing a code change for correctness. You are not approving it — there is no \"looks good\" response.\n\n" +
		"You have no filesystem or git access here: this directory is empty and deliberately disconnected from the real repository. " +
		"Do not run `git`, `ls`, `find`, or any other command to look for the change — there is nothing here to find. " +
		"The entire change is already given to you below, under \"## Changes made (git diff)\". Review only that.\n\n" +
		"If you have a concern, put it under a section titled exactly \"## Concern\": name the file and the line, state what is wrong, and why it matters. A vague concern is not useful.\n\n" +
		"If you have no concerns, output absolutely nothing — no \"## Concern\" section, no \"LGTM\", no summary, no explanation of what you did. " +
		"Only a \"## Concern\" section with real content after it counts as a finding; anything else you output is discarded automatically.\n\n" +
		"---\n\n## Card\n\n" + ticketContent +
		"\n\n---\n\n## Changes made (git diff)\n\n" + diff
}

// isConcreteReviewFinding mirrors isConcreteSecurityFinding: only a properly
// headed "## Concern" section with real content after it counts. This is
// what makes the gate deterministic — CLI banners, exploration noise, or any
// other stray output the reviewing agent produces despite the prompt above
// can never masquerade as a real finding.
func isConcreteReviewFinding(output string) bool {
	idx := strings.Index(output, "## Concern")
	if idx == -1 {
		return false
	}
	rest := strings.TrimSpace(output[idx+len("## Concern"):])
	return rest != ""
}

// runReviewSession runs a reviewing agent in a fresh, isolated session: its
// cwd is an empty temp directory rather than the project root, so it has no
// config file in that directory to discover any MCP/board tools through and
// no meaningful write access to the repo. command is the reviewer role's
// configured command; callers only invoke this once they've confirmed it's
// non-empty.
func runReviewSession(prompt, command string) (output string, err error) {
	tmpDir, mkErr := os.MkdirTemp("", "ekbn-review-")
	if mkErr != nil {
		return "", fmt.Errorf("create review dir: %w", mkErr)
	}
	defer os.RemoveAll(tmpDir)

	argv := strings.Fields(command)
	args := append(append([]string{}, argv[1:]...), prompt)
	log.info("   Reviewer command: %v (prompt %d bytes, cwd %s)", argv, len(prompt), tmpDir)

	cmd := exec.Command(argv[0], args...)
	cmd.Dir = tmpDir
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stdout, &buf)

	err = cmd.Run()
	log.info("   Reviewer process exited (err=%v), captured %d bytes", err, buf.Len())
	return strings.TrimSpace(buf.String()), err
}

// runReviewer runs the general review gate. If no command is configured for
// the "reviewer" role (and none for "default" either), the gate is skipped
// entirely — treated as no findings, not an error — since there's no agent
// to run and reviewing is optional infrastructure, not something ekbn can
// force without one.
func runReviewer(cfg serve.Config, ticketContent, diff string) (findings string, err error) {
	rc, _ := resolveRoleConfig("reviewer", cfg.Roles)
	if strings.TrimSpace(rc.Command) == "" {
		log.info("No reviewer command configured — skipping general review")
		return "", nil
	}
	return runReviewSession(buildReviewPrompt(ticketContent, diff), rc.Command)
}

// buildSecurityReviewPrompt requires an exact reproduction, not advice — see
// isConcreteSecurityFinding, which enforces that mechanically.
func buildSecurityReviewPrompt(ticketContent, diff string) string {
	return "You are a security reviewer. You are not approving this change — there is no \"looks good\" response.\n\n" +
		"Your only job is to find a concrete way this change lets a client influence an outcome it must not: " +
		"RNG/drop pools, summon rates and pity, battle log validation, auth/session handling, or anything reachable via a tRPC procedure.\n\n" +
		"If you find a concrete issue, describe an exact reproduction — the specific request or input sequence that produces the wrong outcome " +
		"— under a section titled exactly \"## Reproduction\". General advice (\"consider adding rate limiting\", \"you should validate X\") " +
		"is not acceptable and will be discarded automatically — only a concrete reproduction counts.\n\n" +
		"If you have no concrete finding, output absolutely nothing.\n\n" +
		"---\n\n## Card\n\n" + ticketContent +
		"\n\n---\n\n## Changes made (git diff)\n\n" + diff
}

func isConcreteSecurityFinding(output string) bool {
	idx := strings.Index(output, "## Reproduction")
	if idx == -1 {
		return false
	}
	rest := strings.TrimSpace(output[idx+len("## Reproduction"):])
	return rest != ""
}

// runSecurityReviewer runs the security review gate. Same skip-if-unconfigured
// policy as runReviewer.
func runSecurityReviewer(cfg serve.Config, ticketContent, diff string) (findings string, err error) {
	rc, _ := resolveRoleConfig("security-reviewer", cfg.Roles)
	if strings.TrimSpace(rc.Command) == "" {
		log.info("No security-reviewer command configured — skipping security review")
		return "", nil
	}
	return runReviewSession(buildSecurityReviewPrompt(ticketContent, diff), rc.Command)
}
