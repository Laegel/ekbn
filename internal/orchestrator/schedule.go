package orchestrator

import (
	"sort"
	"strings"
	"sync"
	"time"

	"ekbn/internal/serve"
	"ekbn/model"
)

// Scheduling: the ready set is a real query over the dependency graph, not a
// scan of whichever folder happens to be called "todo".

func countInProgress(cards []model.Card) int {
	n := 0
	for _, c := range cards {
		if c.Status == model.StatusInProgress {
			n++
		}
	}
	return n
}

// reclaimExpiredLeases moves any in-progress card whose lease has expired (or
// which predates leases entirely, and so has no lease_expires to check) to
// blocked. A card with a live lease is left alone — under real concurrency
// there is no single-instance "someone else is holding this" case to detect
// and exit over, since every in-progress card the ready-set logic could ever
// hand back out is, by construction, not this one (its status isn't todo).
func reclaimExpiredLeases(cards []model.Card) {
	for _, c := range cards {
		if c.Status != model.StatusInProgress {
			continue
		}
		expired := true
		if c.LeaseExpires != "" {
			if expiresAt, err := time.Parse(time.RFC3339, c.LeaseExpires); err == nil {
				expired = time.Now().After(expiresAt)
			}
		}
		if !expired {
			continue
		}
		log.warn("⏳  Lease expired for ticket #%s — reclaiming", c.ID)
		if _, err := model.TransitionStatus(kanbanRoot, c.Column, c.Filename, model.StatusBlocked, "lease-expired"); err != nil {
			log.error("Failed to reclaim ticket #%s: %v", c.ID, err)
		}
	}
}

// reconcileManualMoves treats a card's physical folder as authoritative when
// it disagrees with frontmatter Status: a human dragging a card in the board
// UI, or moving the file directly, expresses intent through the folder, but
// nothing else in this codebase reads folder location — Status alone drives
// scheduling. Without this, a card moved straight to "done" on disk silently
// keeps its old Status forever and any dependents stay stuck.
func reconcileManualMoves(cards []model.Card) {
	for _, c := range cards {
		def := model.DirToColumnDef(c.Column)
		if def == nil || !model.IsValidStatus(model.Status(def.Slug)) {
			continue
		}
		folderStatus := model.Status(def.Slug)
		if folderStatus == c.Status {
			continue
		}
		log.info("Card #%s: found in folder %q but status is %q — syncing status to match the folder", c.ID, c.Column, c.Status)
		if _, err := model.TransitionStatus(kanbanRoot, c.Column, c.Filename, folderStatus, "manual-move"); err != nil {
			log.error("Failed to reconcile ticket #%s: %v", c.ID, err)
		}
	}
}

// selectReadyTickets returns up to n cards to start this cycle, ordered by
// priority then by how long they've been waiting. Returns nil (scheduling
// nothing) if the graph contains a cycle anywhere — a card stuck in a cycle
// can never legitimately become ready, and silently excluding just the cycle
// members while scheduling everything else would hide that from whoever
// needs to fix the DAG.
func selectReadyTickets(cards []model.Card, n int) []model.Card {
	if n <= 0 {
		return nil
	}
	if cycle := model.DetectCycle(cards); cycle != nil {
		log.error("✗  Dependency cycle detected (%s) — refusing to schedule until it's broken", strings.Join(cycle, " -> "))
		return nil
	}
	ready := model.ReadySet(cards)
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority < ready[j].Priority
		}
		return fileModTime(ticketPath(ready[i])).Before(fileModTime(ticketPath(ready[j])))
	})
	if len(ready) > n {
		ready = ready[:n]
	}
	return ready
}

func runCycle(cfg serve.Config) {
	cards, err := model.LoadAllCards(kanbanRoot)
	if err != nil {
		log.error("Failed to load cards: %v", err)
		return
	}
	reconcileManualMoves(cards)
	reclaimExpiredLeases(cards)

	cards, err = model.LoadAllCards(kanbanRoot)
	if err != nil {
		log.error("Failed to load cards: %v", err)
		return
	}
	// Clamped to 1 regardless of cfg.WIPLimit: tickets now run directly in
	// this shared working directory rather than each in its own worktree, so
	// more than one in flight at a time would let two agents' edits, verify
	// runs, and commits collide. Revisit once branch/worktree-based
	// concurrency returns.
	capacity := min(1, cfg.WIPLimit) - countInProgress(cards)
	ready := selectReadyTickets(cards, capacity)
	log.info("Poll cycle: %d cards loaded, %d in progress, capacity %d, %d ready", len(cards), countInProgress(cards), capacity, len(ready))
	if len(ready) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, ticket := range ready {
		wg.Add(1)
		go func(c model.Card) {
			defer wg.Done()
			runTicket(cfg, c)
		}(ticket)
	}
	wg.Wait()
}
