package orchestrator

import (
	"os"
	"strings"
	"testing"
)

func TestPreviewHeadTail_ShortStringUnchanged(t *testing.T) {
	s := "a short line of output"
	got := previewHeadTail(s, 100, 100)
	if got != s {
		t.Errorf("got %q, want unchanged %q", got, s)
	}
}

func TestPreviewHeadTail_LongStringKeepsTail(t *testing.T) {
	head := strings.Repeat("h", 400)
	middle := strings.Repeat("m", 4000)
	needle := "permission requested: external_directory ... auto-rejecting"
	s := head + middle + needle

	got := previewHeadTail(s, 200, 300)

	if !strings.Contains(got, needle) {
		t.Errorf("tail needle %q missing from preview: %q", needle, got)
	}
	if !strings.HasPrefix(got, strings.Repeat("h", 200)) {
		t.Errorf("preview does not start with expected head: %q", got)
	}
	if !strings.Contains(got, "...[truncated]...") {
		t.Errorf("preview missing truncation marker: %q", got)
	}
}

func TestFix_ResetBlockedClearsStateAndStripsFindings(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(origWd) })
	ensureKanbanDirs()

	mustWrite(t, ".kanban/400-blocked/card.md", `---
id: card-x
title: Blocked Test
author: mcp
status: blocked
stage: implement
round: 2
reason: review-rounds-exhausted
lease_owner: host:1234
lease_expires: "2026-07-31T12:06:18+02:00"
base_sha: abc123
checkpoint: claimed
depends_on: []
categories: []
comments: []
---

Original ticket description.

## Review Findings

## Concern
first round issue

## Review Findings

## Concern
second round issue
`)

	n := ResetBlocked()
	if n != 1 {
		t.Fatalf("ResetBlocked() = %d, want 1", n)
	}

	if fileExists(".kanban/400-blocked/card.md") {
		t.Error("card still in 400-blocked after reset")
	}
	if !fileExists(".kanban/100-todo/card.md") {
		t.Fatal("card did not move to 100-todo")
	}

	card := mustReadCard(t, ".kanban/100-todo/card.md")
	if card.Round != 0 {
		t.Errorf("Round = %d, want 0", card.Round)
	}
	if card.Stage != "" {
		t.Errorf("Stage = %q, want empty", card.Stage)
	}
	if card.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want empty", card.BaseSHA)
	}
	if card.Checkpoint != "" {
		t.Errorf("Checkpoint = %q, want empty", card.Checkpoint)
	}
	if card.Reason != "" {
		t.Errorf("Reason = %q, want empty", card.Reason)
	}
	if card.LeaseOwner != "" {
		t.Errorf("LeaseOwner = %q, want empty", card.LeaseOwner)
	}
	if card.LeaseExpires != "" {
		t.Errorf("LeaseExpires = %q, want empty", card.LeaseExpires)
	}
	if !strings.Contains(card.Content, "Original ticket description.") {
		t.Errorf("Content lost the original description: %q", card.Content)
	}
	if strings.Contains(card.Content, "Review Findings") {
		t.Errorf("Content still contains a Review Findings section: %q", card.Content)
	}
}
