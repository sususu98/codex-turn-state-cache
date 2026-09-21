package prewarm

import (
	"sync"
	"time"
)

// Budget survives Engine replacement in the native plugin runtime. It stores
// no state/proxy/account data and counts acquisition and verification equally.
// Process restart or native image replacement still resets this in-memory cap.
type Budget struct {
	mu         sync.Mutex
	timestamps []time.Time
}

func (b *Budget) allow(now time.Time, limit int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(b.timestamps) && !b.timestamps[i].After(cutoff) {
		i++
	}
	b.timestamps = b.timestamps[i:]
	if len(b.timestamps) >= limit {
		return false
	}
	b.timestamps = append(b.timestamps, now)
	return true
}

// SetBudget must be called before Start or publication of this engine.
func (e *Engine) SetBudget(b *Budget) {
	if b != nil {
		e.budget = b
	}
}
