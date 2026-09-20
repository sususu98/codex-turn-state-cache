package turnstate

import (
	"strings"
	"sync"
	"time"
)

const (
	TurnStateHeader = "X-Codex-Turn-State"
	PlanTypeHeader  = "X-Codex-Plan-Type"
	StateLength     = 292
	StateLengthTeam = 332

	stateTTL   = time.Hour
	pendingTTL = 2 * time.Hour
)

var defaultAcceptedLengths = []int{StateLength, StateLengthTeam}

// CacheKey scopes state to the exact CPA-selected credential and model.
type CacheKey struct {
	AuthID string
	Model  string
}

type stateEntry struct {
	value      string
	capturedAt time.Time
	expiresAt  time.Time
}

type pendingBinding struct {
	key       CacheKey
	planType  string
	createdAt time.Time
	expiresAt time.Time
}

type Cache struct {
	mu                sync.Mutex
	now               func() time.Time
	maxEntries        int
	maxPendingEntries int
	acceptedLengths   []int
	entries           map[CacheKey]stateEntry
	pending           map[string]pendingBinding
}

func NewCache(maxEntries, maxPendingEntries int, now func() time.Time, acceptedLengths ...int) *Cache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxPendingEntries < 1 {
		maxPendingEntries = 1
	}
	if now == nil {
		now = time.Now
	}
	lengths := make([]int, 0, len(acceptedLengths))
	for _, l := range acceptedLengths {
		if l > 0 {
			lengths = append(lengths, l)
		}
	}
	if len(lengths) == 0 {
		lengths = append(lengths, defaultAcceptedLengths...)
	}
	return &Cache{
		now:               now,
		maxEntries:        maxEntries,
		maxPendingEntries: maxPendingEntries,
		acceptedLengths:   lengths,
		entries:           make(map[CacheKey]stateEntry),
		pending:           make(map[string]pendingBinding),
	}
}

// BindPlan associates an observed plan type with a pending request.
func (c *Cache) BindPlan(requestID, planType string) {
	if requestID == "" || planType == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.pending[requestID]; ok {
		b.planType = planType
		c.pending[requestID] = b
	}
}

// BindRequest associates a request ID with the trusted after-auth key.
func (c *Cache) BindRequest(requestID string, key CacheKey) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	if !validKey(key) {
		delete(c.pending, requestID)
		return
	}
	if _, exists := c.pending[requestID]; !exists {
		c.evictPendingLocked()
	}
	c.pending[requestID] = pendingBinding{
		key:       key,
		createdAt: now,
		expiresAt: now.Add(pendingTTL),
	}
}

func (c *Cache) ForgetRequest(requestID string) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, requestID)
}

// Lookup returns a state only while its capture-time lifetime is valid.
func (c *Cache) Lookup(key CacheKey) (string, bool) {
	if !validKey(key) {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	return entry.value, true
}

// StoreResponseForRequest writes a valid state for the request's after-auth key.
// It enforces strict matching between plan type (pro/team) and state length when planType is supplied.
func (c *Cache) StoreResponseForRequest(requestID string, values []string, planType ...string) (key CacheKey, stored, replaced bool) {
	if requestID == "" {
		return CacheKey{}, false, false
	}
	rawPlan := ""
	if len(planType) > 0 {
		rawPlan = planType[0]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	binding, ok := c.pending[requestID]
	if !ok {
		return CacheKey{}, false, false
	}
	if rawPlan == "" && binding.planType != "" {
		rawPlan = binding.planType
	}
	value, ok := c.validatePlanAndLength(values, rawPlan)
	if !ok {
		return CacheKey{}, false, false
	}
	return binding.key, true, c.storeLocked(binding.key, value, now)
}

func (c *Cache) storeLocked(key CacheKey, value string, now time.Time) bool {
	_, replaced := c.entries[key]
	if !replaced {
		c.evictEntriesLocked()
	}
	c.entries[key] = stateEntry{
		value:      value,
		capturedAt: now,
		expiresAt:  now.Add(stateTTL),
	}
	return replaced
}

func (c *Cache) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	for requestID, binding := range c.pending {
		if !now.Before(binding.expiresAt) {
			delete(c.pending, requestID)
		}
	}
}

func (c *Cache) evictEntriesLocked() {
	for len(c.entries) >= c.maxEntries {
		var oldestKey CacheKey
		var oldest stateEntry
		first := true
		for key, entry := range c.entries {
			if first || entry.capturedAt.Before(oldest.capturedAt) || (entry.capturedAt.Equal(oldest.capturedAt) && cacheKeyLess(key, oldestKey)) {
				oldestKey = key
				oldest = entry
				first = false
			}
		}
		delete(c.entries, oldestKey)
	}
}

func (c *Cache) evictPendingLocked() {
	for len(c.pending) >= c.maxPendingEntries {
		var oldestRequestID string
		var oldest pendingBinding
		first := true
		for requestID, binding := range c.pending {
			if first || binding.createdAt.Before(oldest.createdAt) || (binding.createdAt.Equal(oldest.createdAt) && requestID < oldestRequestID) {
				oldestRequestID = requestID
				oldest = binding
				first = false
			}
		}
		delete(c.pending, oldestRequestID)
	}
}

func validKey(key CacheKey) bool {
	return key.AuthID != "" && key.Model != ""
}

func (c *Cache) validatePlanAndLength(values []string, rawPlan string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	val := values[0]
	plan := strings.ToLower(strings.TrimSpace(rawPlan))
	switch plan {
	case "team", "business", "self_serve_business_prolite":
		// Team / Business accounts strictly require 332-byte state
		if len(val) != StateLengthTeam {
			return "", false
		}
		return val, true
	case "pro", "plus", "free", "individual":
		// Individual / Pro accounts strictly require 292-byte state
		if len(val) != StateLength {
			return "", false
		}
		return val, true
	default:
		// When plan is not specified or unrecognized, fall back to acceptedLengths
		for _, expected := range c.acceptedLengths {
			if len(val) == expected {
				return val, true
			}
		}
		return "", false
	}
}

func (c *Cache) validStateValue(values []string) (string, bool) {
	return c.validatePlanAndLength(values, "")
}

func validStateValue(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	val := values[0]
	if len(val) == StateLength || len(val) == StateLengthTeam {
		return val, true
	}
	return "", false
}

func cacheKeyLess(left, right CacheKey) bool {
	if left.AuthID != right.AuthID {
		return left.AuthID < right.AuthID
	}
	return left.Model < right.Model
}
