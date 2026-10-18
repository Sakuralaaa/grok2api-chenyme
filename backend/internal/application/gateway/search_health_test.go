package gateway

import (
	"testing"
	"time"
)

func TestSearchHealthLadderEscalatesAndResets(t *testing.T) {
	tr := newSearchHealthTracker()
	now := time.Now().UTC()
	want := []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		got, level, _ := tr.strike(1, now.Add(time.Duration(i)*time.Second))
		if got != w {
			t.Fatalf("strike %d cooldown = %s, want %s", i+1, got, w)
		}
		if level != i+1 {
			t.Fatalf("strike %d level = %d, want %d", i+1, level, i+1)
		}
	}
	// A success resets the ladder back to the first rung.
	tr.success(1, now.Add(time.Minute))
	got, level, _ := tr.strike(1, now.Add(2*time.Minute))
	if got != 15*time.Minute || level != 1 {
		t.Fatalf("after success cooldown=%s level=%d, want 15m/1", got, level)
	}
}

func TestSearchHealthStrikeDecays(t *testing.T) {
	tr := newSearchHealthTracker()
	now := time.Now().UTC()
	tr.strike(2, now)
	tr.strike(2, now.Add(time.Minute)) // level 2
	// A strike long after the decay window starts the ladder over.
	got, level, _ := tr.strike(2, now.Add(searchStrikeDecay+time.Hour))
	if got != 15*time.Minute || level != 1 {
		t.Fatalf("decayed strike cooldown=%s level=%d, want 15m/1", got, level)
	}
}

func TestSearchHealthSuccessRate(t *testing.T) {
	tr := newSearchHealthTracker()
	now := time.Now().UTC()
	tr.strike(3, now)  // 1 bad
	tr.success(3, now) // 1 good
	tr.success(3, now) // 2 good
	_, rate, samples := tr.snapshot(3)
	if samples != 3 {
		t.Fatalf("samples = %d, want 3", samples)
	}
	if rate < 0.66 || rate > 0.67 {
		t.Fatalf("rate = %f, want ~0.667", rate)
	}
}
