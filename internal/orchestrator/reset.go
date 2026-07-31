package orchestrator

import (
	"strings"

	"ekbn/model"
)

// findingsMarker is the exact heading appendSection writes before review
// findings. Truncating a card's body at the first occurrence removes every
// round's stacked findings at once, since each is appended after the last.
const findingsMarker = "\n## Review Findings"

// ResetBlocked returns every blocked card to todo with a clean slate: stage
// progress, lease, and round bookkeeping cleared, and any appended
// review-findings sections stripped back out of the body, so the ticket
// runs as if it were fresh. Returns how many cards were reset.
func ResetBlocked() int {
	cards, err := model.LoadAllCards(kanbanRoot)
	if err != nil {
		log.error("Failed to load cards: %v", err)
		return 0
	}

	n := 0
	for _, c := range cards {
		if c.Status != model.StatusBlocked {
			continue
		}
		withCard(c.Column, c.Filename, func(card *model.Card) {
			if idx := strings.Index(card.Content, findingsMarker); idx != -1 {
				card.Content = strings.TrimRight(card.Content[:idx], "\n") + "\n"
			}
			card.Round = 0
			card.Stage = ""
			card.BaseSHA = ""
			card.Checkpoint = ""
			card.Reason = ""
			card.LeaseOwner = ""
			card.LeaseExpires = ""
		})
		if _, err := model.TransitionStatus(kanbanRoot, c.Column, c.Filename, model.StatusTodo, ""); err != nil {
			log.error("Failed to reset ticket #%s to todo: %v", c.ID, err)
			continue
		}
		log.info("↺  Reset ticket #%s to todo (cleared review findings + stage state)", c.ID)
		n++
	}
	return n
}
