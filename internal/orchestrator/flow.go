package orchestrator

import (
	"ekbn/internal/serve"
	"ekbn/model"
)

// defaultMaxAttempts is the per-state retry budget used when a FlowState
// doesn't set its own MaxAttempts.
const defaultMaxAttempts = 3

// defaultMaxTotalAttempts bounds the *total* number of transitions a single
// ticket run may take across its whole flow, regardless of which states are
// involved. A per-state MaxAttempts alone can't catch a ping-pong loop
// (A -> B -> A -> B -> ...): each individual state's own attempt counter
// resets to 0 every time the card leaves and returns to it, so neither
// state's cap is ever itself exceeded even as the ticket bounces between
// them indefinitely. This is the circuit breaker for that case.
const defaultMaxTotalAttempts = 10

// nextState looks up what a flow state's classified Outcome routes to next.
// ok is false when the outcome has no entry in the state's On map — callers
// must not guess a default in that case; an unhandled outcome blocks the
// ticket with a clear reason instead of silently misrouting, the same
// "never guess" posture a dependency cycle or an unresolved ticket already
// gets. next is either another key in flow.States (continue the run) or a
// key in flow.Terminal (terminal is then non-nil).
func nextState(flow serve.Flow, current string, outcome model.Outcome) (next string, terminal *serve.TerminalState, ok bool) {
	state, exists := flow.States[current]
	if !exists {
		return "", nil, false
	}
	next, ok = state.On[outcome]
	if !ok {
		return "", nil, false
	}
	if t, isTerminal := flow.Terminal[next]; isTerminal {
		return next, &t, true
	}
	return next, nil, true
}

// stateMaxAttempts returns a state's configured MaxAttempts, falling back to
// the package default when unset.
func stateMaxAttempts(state serve.FlowState) int {
	if state.MaxAttempts > 0 {
		return state.MaxAttempts
	}
	return defaultMaxAttempts
}

// advanceOrBlock is the single place a state's classified outcome turns
// into what happens next: continue the flow, land on a terminal board, or
// block because the outcome has nowhere to go. findings, if non-empty, is
// attached to whichever block/terminal path is taken via
// transitionBlockedWithFindings instead of the plain, findings-less
// version — every caller that has something concrete to say passes it.
func advanceOrBlock(projectRoot, id string, card model.Card, flow serve.Flow, currentState string, outcome model.Outcome, column, filename, checkpoint, findings string) {
	next, terminal, ok := nextState(flow, currentState, outcome)
	if !ok {
		reason := "unhandled-outcome:" + string(outcome)
		log.warn("✋  Ticket #%s: outcome %q has no transition from state %q — blocking (%s)", id, outcome, currentState, reason)
		blockWith(column, filename, reason, findings)
		return
	}
	if terminal != nil {
		switch terminal.Board {
		case model.StatusDone, model.StatusReview:
			// Only a genuine terminal (the flow is actually finished) merges
			// the ticket's worktree into main — a blocked or budget-exhausted
			// terminal leaves it in place for a human to inspect.
			if err := mergeAndRemoveWorktree(projectRoot, id); err != nil {
				log.error("Failed to merge ticket #%s's work into main: %v", id, err)
				transitionBlockedWithFindings(column, filename, "merge-not-fast-forward", err.Error())
				return
			}
			withCard(column, filename, func(c *model.Card) { c.Worktree = "" })
			if terminal.Board == model.StatusDone {
				transitionDone(column, filename, checkpoint, next)
			} else {
				transitionReview(column, filename, checkpoint, next)
			}
		case model.StatusBudgetExhausted:
			transitionBudgetExhausted(column, filename, next)
		default:
			blockWith(column, filename, next, findings)
		}
		return
	}
	if card.TotalAttempts+1 > defaultMaxTotalAttempts {
		log.warn("🔒  Ticket #%s exhausted its %d total-attempt budget across the whole flow — blocking", id, defaultMaxTotalAttempts)
		blockWith(column, filename, "flow-total-attempts-exhausted", findings)
		return
	}
	// findings (e.g. reviewer concerns) must reach whichever state runs
	// next — typically back at an implement-shaped state — the same way a
	// blocked ticket's findings are attached, so the next attempt's prompt
	// (which includes the full ticket content) can see what went wrong.
	if findings != "" {
		appendSection(column, filename, "Review Findings", findings)
	}
	advanceStage(column, filename, next, checkpoint)
}

func blockWith(column, filename, reason, findings string) {
	if findings != "" {
		transitionBlockedWithFindings(column, filename, reason, findings)
	} else {
		transitionBlocked(column, filename, reason)
	}
}
