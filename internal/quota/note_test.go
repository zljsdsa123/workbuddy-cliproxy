package quota

import (
	"testing"
	"time"
)

func TestCreditsNoteRendering(t *testing.T) {
	now := time.Now()
	if got := creditsNote(creditsState{}, now); got != "" {
		t.Fatalf("an unknown balance must render no note, got %q", got)
	}

	funded := creditsState{known: true, remaining: 699.01, checkedAt: now}
	if got := creditsNote(funded, now); got != "剩余积分 699.01" {
		t.Fatalf("creditsNote() = %q, want \"剩余积分 699.01\"", got)
	}

	cooling := creditsState{known: true, remaining: 0, checkedAt: now, exhaustedAt: now}
	if got := creditsNote(cooling, now); got != "剩余积分 0.00 · 冷却中" {
		t.Fatalf("creditsNote() = %q, want the cooling-down variant", got)
	}

	// Once the cooldown lapses the note drops the suffix again.
	if got := creditsNote(cooling, now.Add(cooldown+time.Minute)); got != "剩余积分 0.00" {
		t.Fatalf("creditsNote() after cooldown = %q, want \"剩余积分 0.00\"", got)
	}
}

func TestCreditsNoteMarksEstimatedBalance(t *testing.T) {
	now := time.Now()
	state := creditsState{known: true, remaining: 98.5, checkedAt: now, estimated: true}
	if got := creditsNote(state, now); got != "剩余积分 ~98.50" {
		t.Fatalf("creditsNote() = %q, want \"剩余积分 ~98.50\"", got)
	}
}
