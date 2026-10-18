package gateway

import (
	"sync"
	"time"
)

// Search-aware account health.
//
// A grok.com account that keeps answering searches with an empty turn
// ("执行了 N 次搜索" and then nothing) is degraded for a while, not broken for
// a moment. A flat 15-minute idle cooldown lets such an account come straight
// back and burn the next request. Repeated search-degraded turns escalate the
// cooldown 15m -> 1h -> 6h; a successful answer on the account resets the
// ladder, and a strike older than searchStrikeDecay is forgotten.
//
// The tracker also keeps a small rolling success rate per account so logs can
// show how healthy an account has been recently. State is in-memory: it is a
// routing heuristic, and a process restart simply starts every account fresh.

var searchDegradedLadder = []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour}

const (
	searchStrikeDecay      = 24 * time.Hour
	searchHealthWindow     = 20
	maxTrackedSearchHealth = 4096
)

type searchHealthEntry struct {
	strikes    int
	lastStrike time.Time
	outcomes   [searchHealthWindow]bool
	filled     int
	next       int
	touched    time.Time
}

type searchHealthTracker struct {
	mu      sync.Mutex
	entries map[uint64]*searchHealthEntry
}

func newSearchHealthTracker() *searchHealthTracker {
	return &searchHealthTracker{entries: make(map[uint64]*searchHealthEntry)}
}

func (t *searchHealthTracker) entryLocked(accountID uint64, now time.Time) *searchHealthEntry {
	entry := t.entries[accountID]
	if entry == nil {
		if len(t.entries) >= maxTrackedSearchHealth {
			t.evictOldestLocked()
		}
		entry = &searchHealthEntry{}
		t.entries[accountID] = entry
	}
	entry.touched = now
	return entry
}

func (t *searchHealthTracker) evictOldestLocked() {
	var oldestID uint64
	var oldest time.Time
	first := true
	for id, entry := range t.entries {
		if first || entry.touched.Before(oldest) {
			oldestID, oldest, first = id, entry.touched, false
		}
	}
	if !first {
		delete(t.entries, oldestID)
	}
}

func (e *searchHealthEntry) record(ok bool) {
	e.outcomes[e.next] = ok
	e.next = (e.next + 1) % searchHealthWindow
	if e.filled < searchHealthWindow {
		e.filled++
	}
}

func (e *searchHealthEntry) rate() (float64, int) {
	if e.filled == 0 {
		return 1, 0
	}
	ok := 0
	for i := 0; i < e.filled; i++ {
		if e.outcomes[i] {
			ok++
		}
	}
	return float64(ok) / float64(e.filled), e.filled
}

// strike records a search-degraded turn and returns the cooldown to apply,
// the strike level (1-based) and the account's recent success rate.
func (t *searchHealthTracker) strike(accountID uint64, now time.Time) (time.Duration, int, float64) {
	if t == nil {
		return searchDegradedLadder[0], 1, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entryLocked(accountID, now)
	if !entry.lastStrike.IsZero() && now.Sub(entry.lastStrike) > searchStrikeDecay {
		entry.strikes = 0
	}
	entry.strikes++
	entry.lastStrike = now
	entry.record(false)
	level := entry.strikes
	if level > len(searchDegradedLadder) {
		level = len(searchDegradedLadder)
	}
	rate, _ := entry.rate()
	return searchDegradedLadder[level-1], entry.strikes, rate
}

// success resets the strike ladder and records a good outcome.
func (t *searchHealthTracker) success(accountID uint64, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[accountID]
	if entry == nil {
		// Only accounts that have ever struck are tracked; healthy accounts
		// cost nothing.
		return
	}
	entry.touched = now
	entry.strikes = 0
	entry.record(true)
}

// snapshot returns the current strike count and success rate for logging.
func (t *searchHealthTracker) snapshot(accountID uint64) (int, float64, int) {
	if t == nil {
		return 0, 1, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[accountID]
	if entry == nil {
		return 0, 1, 0
	}
	rate, samples := entry.rate()
	return entry.strikes, rate, samples
}
