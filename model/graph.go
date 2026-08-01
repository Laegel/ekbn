package model

// LoadAllCards reads every card across every column, giving the scheduler a
// single flat view of the DAG to walk. Cards without an ID (created before
// stable IDs existed) are still returned — they just can't be referenced by
// depends_on and won't be found as a dependency target.
func LoadAllCards(baseDir string) ([]Card, error) {
	columns, err := ListColumns(baseDir)
	if err != nil {
		return nil, err
	}
	var cards []Card
	for _, col := range columns {
		cards = append(cards, col.Cards...)
	}
	return cards, nil
}

func cardsByID(cards []Card) map[string]Card {
	m := make(map[string]Card, len(cards))
	for _, c := range cards {
		if c.ID != "" {
			m[c.ID] = c
		}
	}
	return m
}

// DetectCycle walks depends_on edges and returns the first cycle found, as an
// ordered slice of card IDs (the last element depends on the first, closing
// the loop). Returns nil if the graph is acyclic. Must run before scheduling:
// ReadySet's cycle-guard treats any ID stuck in a cycle as permanently
// not-done rather than looping forever, but that only makes scheduling safe,
// not correct — a card sitting in a cycle can never become ready, and DAG
// authors need to be told why, not have it silently starve.
func DetectCycle(cards []Card) []string {
	byID := cardsByID(cards)

	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(byID))
	var path []string
	var cycle []string

	var visit func(id string) bool
	visit = func(id string) bool {
		switch color[id] {
		case black:
			return false
		case gray:
			idx := -1
			for i, p := range path {
				if p == id {
					idx = i
					break
				}
			}
			if idx >= 0 {
				cycle = append(append([]string{}, path[idx:]...), id)
			}
			return true
		}
		color[id] = gray
		path = append(path, id)
		for _, dep := range byID[id].DependsOn {
			if visit(dep) {
				return true
			}
		}
		path = path[:len(path)-1]
		color[id] = black
		return false
	}

	for id := range byID {
		if color[id] == white && visit(id) {
			return cycle
		}
	}
	return nil
}

// isDoneTransitive reports whether id's own status is done AND every one of
// its dependencies resolves the same way, recursively. A dependency that
// isn't in byID (dangling reference), that sits in a cycle (visiting[id]),
// or whose own status isn't done all resolve to false — an item blocked by a
// blocked dependency several hops up is excluded, not just one whose direct
// dependency is unfinished.
func isDoneTransitive(id string, byID map[string]Card, memo, visiting map[string]bool) bool {
	if v, ok := memo[id]; ok {
		return v
	}
	if visiting[id] {
		return false
	}
	card, ok := byID[id]
	if !ok || card.Status != StatusDone {
		memo[id] = false
		return false
	}
	visiting[id] = true
	for _, dep := range card.DependsOn {
		if !isDoneTransitive(dep, byID, memo, visiting) {
			delete(visiting, id)
			memo[id] = false
			return false
		}
	}
	delete(visiting, id)
	memo[id] = true
	return true
}

// ReadySet is the scheduler's core query: every card whose own status is
// todo, whose dependencies are all, transitively, done, and which has no
// unresolved question outstanding. This is the only definition of "ready"
// the orchestrator uses — there is no separate column-position scan
// standing in for it.
//
// A non-empty Unresolved excludes a card the same way an unfinished
// dependency does: the spec left something undetermined about this item,
// and guessing an answer at decomposition time is exactly what Unresolved
// exists to avoid. The card only becomes schedulable once a human clears
// the field.
func ReadySet(cards []Card) []Card {
	byID := cardsByID(cards)
	memo := make(map[string]bool, len(cards))
	var ready []Card
	for _, c := range cards {
		if c.Status != StatusTodo || c.Unresolved != "" {
			continue
		}
		blocked := false
		for _, dep := range c.DependsOn {
			if !isDoneTransitive(dep, byID, memo, map[string]bool{}) {
				blocked = true
				break
			}
		}
		if !blocked {
			ready = append(ready, c)
		}
	}
	return ready
}

// DependencyIssue describes a card whose depends_on list references one or
// more IDs that don't exist among the cards it was checked against.
type DependencyIssue struct {
	Card    Card
	Missing []string
}

// ValidateDependencies reports every card whose depends_on references an ID
// not present in cards. Meant to run right after a batch of cards is
// created (e.g. by a decomposition run): a dangling reference almost always
// means the referencing agent invented or mistyped an ID, and catching it
// immediately is cheaper than waiting for the orchestrator to silently
// never schedule that card.
func ValidateDependencies(cards []Card) []DependencyIssue {
	byID := cardsByID(cards)
	var issues []DependencyIssue
	for _, c := range cards {
		var missing []string
		for _, dep := range c.DependsOn {
			if _, ok := byID[dep]; !ok {
				missing = append(missing, dep)
			}
		}
		if len(missing) > 0 {
			issues = append(issues, DependencyIssue{Card: c, Missing: missing})
		}
	}
	return issues
}
