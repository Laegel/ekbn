package model

// Outcome is the typed, machine-classifiable result of running one flow
// state — what the state-machine flow engine switches on to decide what
// happens next, replacing the ad hoc mix of free-text Reason strings and
// grepped structural markers (## Concern, ## No Changes Needed) a flow
// transition used to be inferred from. Reason stays exactly what it always
// was: a free-text human-readable detail alongside the typed Outcome, not
// replaced by it.
type Outcome string

const (
	OutcomeSuccess         Outcome = "success"
	OutcomeFailed          Outcome = "failed"
	OutcomeFindings        Outcome = "findings"
	OutcomeBlocked         Outcome = "blocked"
	OutcomeAmbiguous       Outcome = "ambiguous"
	OutcomeBudgetExhausted Outcome = "budget-exhausted"
)
