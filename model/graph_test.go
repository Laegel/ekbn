package model

import "testing"

func card(id string, status Status, deps ...string) Card {
	return Card{ID: id, Status: status, DependsOn: deps}
}

func TestDetectCycle(t *testing.T) {
	t.Run("acyclic", func(t *testing.T) {
		cards := []Card{
			card("a", StatusTodo),
			card("b", StatusTodo, "a"),
			card("c", StatusTodo, "b"),
		}
		if cycle := DetectCycle(cards); cycle != nil {
			t.Errorf("DetectCycle = %v, want nil", cycle)
		}
	})

	t.Run("direct cycle", func(t *testing.T) {
		cards := []Card{
			card("a", StatusTodo, "b"),
			card("b", StatusTodo, "a"),
		}
		if cycle := DetectCycle(cards); cycle == nil {
			t.Error("DetectCycle = nil, want a cycle to be found")
		}
	})

	t.Run("indirect cycle", func(t *testing.T) {
		cards := []Card{
			card("a", StatusTodo, "b"),
			card("b", StatusTodo, "c"),
			card("c", StatusTodo, "a"),
		}
		if cycle := DetectCycle(cards); cycle == nil {
			t.Error("DetectCycle = nil, want a cycle to be found")
		}
	})

	t.Run("dangling dependency is not a cycle", func(t *testing.T) {
		cards := []Card{
			card("a", StatusTodo, "does-not-exist"),
		}
		if cycle := DetectCycle(cards); cycle != nil {
			t.Errorf("DetectCycle = %v, want nil", cycle)
		}
	})
}

func TestReadySet(t *testing.T) {
	t.Run("no dependencies", func(t *testing.T) {
		cards := []Card{card("a", StatusTodo)}
		ready := ReadySet(cards)
		if len(ready) != 1 || ready[0].ID != "a" {
			t.Errorf("ReadySet = %v, want [a]", ready)
		}
	})

	t.Run("blocked by an unfinished dependency", func(t *testing.T) {
		cards := []Card{
			card("a", StatusInProgress),
			card("b", StatusTodo, "a"),
		}
		ready := ReadySet(cards)
		if len(ready) != 0 {
			t.Errorf("ReadySet = %v, want empty (b depends on a, which isn't done)", ready)
		}
	})

	t.Run("ready once the dependency is done", func(t *testing.T) {
		cards := []Card{
			card("a", StatusDone),
			card("b", StatusTodo, "a"),
		}
		ready := ReadySet(cards)
		if len(ready) != 1 || ready[0].ID != "b" {
			t.Errorf("ReadySet = %v, want [b]", ready)
		}
	})

	t.Run("transitively blocked: C depends on B depends on A, A not done", func(t *testing.T) {
		cards := []Card{
			card("a", StatusInProgress),
			card("b", StatusDone, "a"), // b claims done, but its own dependency a isn't
			card("c", StatusTodo, "b"),
		}
		ready := ReadySet(cards)
		if len(ready) != 0 {
			t.Errorf("ReadySet = %v, want empty — c depends on b, whose own dependency a is not done", ready)
		}
	})

	t.Run("a card in a dependency cycle never becomes ready", func(t *testing.T) {
		cards := []Card{
			card("a", StatusTodo, "b"),
			card("b", StatusTodo, "a"),
			card("c", StatusTodo, "a"),
		}
		ready := ReadySet(cards)
		for _, r := range ready {
			if r.ID == "c" {
				t.Error("c should not be ready: its dependency a is stuck in a cycle and can never be done")
			}
		}
	})

	t.Run("non-todo cards are never ready regardless of dependencies", func(t *testing.T) {
		cards := []Card{
			card("a", StatusDone),
			card("b", StatusInProgress, "a"),
			card("c", StatusBlocked, "a"),
		}
		if ready := ReadySet(cards); len(ready) != 0 {
			t.Errorf("ReadySet = %v, want empty", ready)
		}
	})
}
