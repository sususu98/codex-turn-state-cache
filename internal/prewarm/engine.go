package prewarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"
)

const StateHeader = "X-Codex-Turn-State"

var errProbeBudget = errors.New("probe_budget_exhausted")

// Scope is an opaque hash of the current account identity and formal route.
// The plugin resolver reads credentials through existing Host callbacks, keeps
// them only for its own probes, and never includes them in this identity.
type Identity struct {
	Scope string `json:"scope"`
	Plan  string `json:"plan,omitempty"`
}
type ProbeRequest struct {
	AuthID         string  `json:"auth_id"`
	Model          string  `json:"model"`
	ProxyOverride  *string `json:"proxy_override,omitempty"`
	State          string  `json:"state,omitempty"`
	ExpectedScope  string  `json:"expected_scope"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}
type ProbeResult struct {
	Scope       string `json:"scope"`
	Status      int    `json:"status"`
	State       string `json:"state"`
	ActualModel string `json:"actual_model,omitempty"`
	Plan        string `json:"plan,omitempty"`
	Body        []byte `json:"body"`
}
type Host interface {
	Describe(context.Context, string) (Identity, error)
	Probe(context.Context, ProbeRequest) (ProbeResult, error)
}
type ticket struct {
	state, scope string
	version      uint64
	expires      time.Time
}
type job struct {
	key        Key
	epoch      uint64
	proxyIndex int
}
type observation struct {
	key      Key
	version  uint64
	observer *Completion
	created  time.Time
}
type record struct {
	value    *ticket
	epoch    uint64
	running  bool
	cooldown time.Time
}
type proxyNode struct {
	url       string
	successes int
	failures  int
	cooldown  time.Time
}

// Engine owns a bounded worker pool and an independent verified-ticket cache.
// Passive captures never overwrite these tickets. There is no persistence of
// state or proxy credentials in S1; restart performs acquisition and verification.
type Engine struct {
	mu             sync.Mutex
	cfg            Config
	host           Host
	discovery      AccountDiscoverer
	discoveryMu    sync.Mutex
	nextDiscovery  time.Time
	discoveryCount int
	owned          map[Key]bool
	ownsAllModels  bool
	epoch          uint64
	plans          map[Key]string
	proxies        []proxyNode
	nextProxy      int
	goodProxy      int
	lastProxy      map[Key]int
	lastFailure    map[Key]string
	records        map[Key]*record
	requests       map[string]*observation
	queue          chan job
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	closed         bool
	version        uint64
	startupDelay   time.Duration
	budget         *Budget
	now            func() time.Time
	log            func(string, string)
}

func New(c Config, host Host, log func(string, string)) (*Engine, error) {
	c.Defaults()
	if !c.Enabled {
		return nil, errors.New("prewarm is disabled")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	proxies, err := c.LoadProxies()
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("prewarm Host is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	nodes := make([]proxyNode, len(proxies))
	for i, proxy := range proxies {
		nodes[i] = proxyNode{url: proxy}
	}
	e := &Engine{cfg: c, host: host, plans: map[Key]string{}, proxies: nodes, goodProxy: -1, lastProxy: map[Key]int{}, lastFailure: map[Key]string{}, records: map[Key]*record{}, requests: map[string]*observation{}, ctx: ctx, cancel: cancel, now: time.Now, log: log, budget: &Budget{}, discoveryCount: -1, owned: map[Key]bool{}}
	if c.AutoAccounts() {
		discoverer, ok := host.(AccountDiscoverer)
		if !ok {
			cancel()
			return nil, errors.New("automatic accounts require read-only account discovery")
		}
		e.discovery = discoverer
	}
	for _, a := range c.Accounts {
		for _, m := range a.Models {
			k := Key{a.AuthID, m}
			e.plans[k] = a.Plan
			e.records[k] = &record{}
		}
	}
	capacity := len(e.plans)
	if c.AutoAccounts() {
		capacity = maxAccounts * len(c.Models)
	}
	e.queue = make(chan job, capacity)
	return e, nil
}

// DelayStart is set before publication, allowing an existing Host to attach
// its account manager after plugin registration without changing Host startup.
func (e *Engine) DelayStart(delay time.Duration) { e.startupDelay = delay }

// Start is called exactly once, after the configured plugin is published.
func (e *Engine) Start() {
	e.wg.Add(e.cfg.Workers + 1)
	for i := 0; i < e.cfg.Workers; i++ {
		go e.worker()
	}
	go func() {
		defer e.wg.Done()
		if e.startupDelay > 0 {
			timer := time.NewTimer(e.startupDelay)
			defer timer.Stop()
			select {
			case <-e.ctx.Done():
				return
			case <-timer.C:
			}
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		e.schedule()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.schedule()
			}
		}
	}()
}
func (e *Engine) Close() { e.mu.Lock(); e.closed = true; e.cancel(); e.mu.Unlock(); e.wg.Wait() }
func (e *Engine) Manages(k Key) bool {
	// Configured models stay in verified-only mode even while discovery is empty,
	// an account is disabled, or its plan is unknown. Never fall back to passive
	// captures merely because an automatic discovery snapshot changed.
	if e.cfg.AutoAccounts() {
		matchesModel := false
		for _, model := range e.cfg.Models {
			if model == k.Model {
				matchesModel = true
				break
			}
		}
		if !matchesModel {
			return false
		}
		if len(e.cfg.Accounts) == 0 {
			return true
		}
		for _, selector := range e.cfg.Accounts {
			if selector.AuthID != "" && selector.AuthID == k.AuthID {
				return true
			}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.discoveryCount < 0 || e.ownsAllModels || e.owned[k] || e.plans[k] != ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.plans[k]
	return ok
}
func (e *Engine) schedule() {
	e.refreshAccounts(false)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for id, o := range e.requests {
		if now.Sub(o.created) > 2*time.Hour {
			delete(e.requests, id)
		}
	}
	for k, r := range e.records {
		if r.value == nil || !now.Add(time.Duration(e.cfg.RefreshBeforeSeconds)*time.Second).Before(r.value.expires) {
			e.enqueueLocked(k)
		}
	}
}
func (e *Engine) enqueueLocked(k Key) {
	r := e.records[k]
	_, managed := e.plans[k]
	if e.closed || !managed || r == nil || r.running || e.now().Before(r.cooldown) {
		return
	}
	r.running = true
	e.lastProxy[k] = -1
	e.lastFailure[k] = ""
	select {
	case e.queue <- job{key: k, epoch: r.epoch, proxyIndex: -1}:
	default:
		r.running = false
	}
}
func (e *Engine) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case j := <-e.queue:
			reason := e.collect(j)
			e.mu.Lock()
			r := e.records[j.key]
			if r != nil {
				r.running = false
				if reason != "ready" && !e.closed && r.epoch == j.epoch {
					r.cooldown = e.now().Add(time.Duration(e.cfg.CooldownSeconds) * time.Second)
				}
			}
			proxyIndex := -1
			if index, ok := e.lastProxy[j.key]; ok {
				proxyIndex = index
			}
			failure := e.lastFailure[j.key]
			if _, managed := e.plans[j.key]; !managed {
				delete(e.records, j.key)
				delete(e.lastProxy, j.key)
				delete(e.lastFailure, j.key)
			}
			e.mu.Unlock()
			e.logResult(reason, j.key, proxyIndex, failure)
		}
	}
}
func (e *Engine) active(j job) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.records[j.key]
	_, managed := e.plans[j.key]
	return !e.closed && managed && r != nil && r.epoch == j.epoch
}
func (e *Engine) reserveProbe() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && e.budget.allow(e.now(), e.cfg.MaxProbesPerHour)
}
func (e *Engine) probe(req ProbeRequest) (ProbeResult, error) {
	if !e.reserveProbe() {
		return ProbeResult{}, errProbeBudget
	}
	ctx, cancel := context.WithTimeout(e.ctx, time.Duration(e.cfg.ProbeTimeoutSeconds)*time.Second)
	defer cancel()
	req.TimeoutSeconds = e.cfg.ProbeTimeoutSeconds
	return e.host.Probe(ctx, req)
}
func (e *Engine) describe(ctx context.Context, authID string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	id, err := e.host.Describe(ctx, authID)
	if err == nil && id.Scope == "" {
		err = errors.New("empty_scope")
	}
	return id, err
}
func stopStatus(status int) bool { return status == 401 || status == 403 || status == 429 }
func (e *Engine) noteFailure(k Key, reason string) {
	e.mu.Lock()
	e.lastFailure[k] = reason
	e.mu.Unlock()
}
func (e *Engine) logResult(reason string, k Key, proxyIndex int, failure string) {
	if e.log == nil || e.ctx.Err() != nil {
		return
	}
	sum := sha256.Sum256([]byte(k.AuthID))
	account := hex.EncodeToString(sum[:4])
	proxy := "none"
	if proxyIndex >= 0 {
		proxy = "p" + itoa(proxyIndex+1)
	}
	msg := "codex prewarm " + reason + " account=" + account + " proxy=" + proxy
	if failure != "" {
		msg += " failure=" + failure
	}
	e.log(msg, k.Model)
}
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	out := ""
	for v > 0 {
		out = string(byte('0'+v%10)) + out
		v /= 10
	}
	return out
}
func (e *Engine) chooseProxyLocked() (int, string) {
	now := e.now()
	if e.goodProxy >= 0 && now.Before(e.proxies[e.goodProxy].cooldown) {
		e.goodProxy = -1
	}
	if e.goodProxy >= 0 {
		return e.goodProxy, e.proxies[e.goodProxy].url
	}
	for i := 0; i < len(e.proxies); i++ {
		idx := (e.nextProxy + i) % len(e.proxies)
		if now.Before(e.proxies[idx].cooldown) {
			continue
		}
		e.nextProxy = (idx + 1) % len(e.proxies)
		return idx, e.proxies[idx].url
	}
	return -1, ""
}
func (e *Engine) markProxy(index int, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.proxies) {
		return
	}
	n := &e.proxies[index]
	if ok {
		n.successes++
		n.failures = 0
		n.cooldown = time.Time{}
		e.goodProxy = index
		return
	}
	n.failures++
	backoff := time.Duration(n.failures) * time.Minute
	if backoff > time.Duration(e.cfg.CooldownSeconds)*time.Second {
		backoff = time.Duration(e.cfg.CooldownSeconds) * time.Second
	}
	n.cooldown = e.now().Add(backoff)
	if e.goodProxy == index {
		e.goodProxy = -1
	}
}
func (e *Engine) collect(j job) string {
	e.mu.Lock()
	plan, managed := e.plans[j.key]
	e.mu.Unlock()
	if !managed {
		return "cancelled"
	}
	for attempt := 0; attempt < e.cfg.MaxAttempts; attempt++ {
		if !e.active(j) || e.ctx.Err() != nil {
			return "cancelled"
		}
		if attempt > 0 {
			select {
			case <-e.ctx.Done():
				return "cancelled"
			case <-time.After(time.Duration(e.cfg.AttemptIntervalSeconds) * time.Second):
			}
		}
		id, err := e.describe(e.ctx, j.key.AuthID)
		if err != nil {
			e.noteFailure(j.key, "identity_unavailable")
			return "identity_unavailable"
		}
		if !e.identityPlanMatches(id.Plan, plan) {
			e.noteFailure(j.key, "harvest_plan_mismatch")
			return "plan_mismatch"
		}
		e.mu.Lock()
		proxyIndex, proxy := e.chooseProxyLocked()
		if proxyIndex >= 0 {
			e.lastProxy[j.key] = proxyIndex
		}
		e.mu.Unlock()
		if proxyIndex < 0 {
			e.noteFailure(j.key, "all_proxies_cooldown")
			return "proxy_pool_cooldown"
		}
		j.proxyIndex = proxyIndex
		captured := e.now()
		candidate, err := e.probe(ProbeRequest{AuthID: j.key.AuthID, Model: j.key.Model, ProxyOverride: &proxy, ExpectedScope: id.Scope})
		if stopStatus(candidate.Status) {
			e.markProxy(proxyIndex, false)
			return "upstream_rejected"
		}
		if err != nil {
			if errors.Is(err, errProbeBudget) {
				return "budget_exhausted"
			}
			if e.ctx.Err() != nil {
				return "cancelled"
			}
			e.noteFailure(j.key, "harvest_transport")
			e.markProxy(proxyIndex, false)
			continue
		}
		if candidate.Scope != id.Scope || !SuccessfulProbe(candidate, j.key.Model) || !PlanMatches(candidate.Plan, plan) || !ValidState(candidate.State, plan) {
			failure := "harvest_model_plan_or_state"
			if candidate.ActualModel != "" && candidate.ActualModel != j.key.Model {
				failure = "harvest_model_mismatch_" + candidate.ActualModel
			}
			e.noteFailure(j.key, failure)
			e.markProxy(proxyIndex, false)
			continue
		}
		// Re-resolve before validation. Identity/route changes discard the candidate.
		fixed, err := e.describe(e.ctx, j.key.AuthID)
		if err != nil {
			return "identity_unavailable"
		}
		if fixed.Scope != id.Scope {
			e.noteFailure(j.key, "harvest_scope_changed")
			e.markProxy(proxyIndex, false)
			continue
		}
		checked, err := e.probe(ProbeRequest{AuthID: j.key.AuthID, Model: j.key.Model, State: candidate.State, ExpectedScope: fixed.Scope})
		if stopStatus(checked.Status) {
			e.markProxy(proxyIndex, false)
			return "upstream_rejected"
		}
		if errors.Is(err, errProbeBudget) {
			return "budget_exhausted"
		}
		if err != nil || checked.Scope != fixed.Scope || !SuccessfulProbe(checked, j.key.Model) || !PlanMatches(checked.Plan, plan) || len(checked.State) == 312 {
			failure := "formal_model_plan_state_or_transport"
			if checked.ActualModel != "" && checked.ActualModel != j.key.Model {
				failure = "formal_model_mismatch_" + checked.ActualModel
			}
			e.noteFailure(j.key, failure)
			e.markProxy(proxyIndex, false)
			continue
		}
		current, err := e.describe(e.ctx, j.key.AuthID)
		if err != nil || current.Scope != fixed.Scope {
			e.noteFailure(j.key, "formal_scope_changed")
			e.markProxy(proxyIndex, false)
			continue
		}
		e.markProxy(proxyIndex, true)
		e.mu.Lock()
		r := e.records[j.key]
		if e.closed || e.ctx.Err() != nil || r == nil || r.epoch != j.epoch || e.plans[j.key] != plan || !e.now().Before(captured.Add(time.Duration(e.cfg.TTLSeconds)*time.Second)) {
			e.mu.Unlock()
			return "cancelled"
		}
		e.version++
		r.value = &ticket{candidate.State, current.Scope, e.version, captured.Add(time.Duration(e.cfg.TTLSeconds) * time.Second)}
		r.cooldown = time.Time{}
		e.lastFailure[j.key] = ""
		e.mu.Unlock()
		return "ready"
	}
	return "attempts_exhausted"
}

// Bind validates current scope on the hot path using a local Host callback only.
// Missing tickets trigger background work; they never block on network probes.
// Caller-owned state takes precedence and is not observed as our ticket.
func (e *Engine) Bind(ctx context.Context, requestID string, k Key, callerState bool) (string, bool) {
	if requestID == "" || !e.Manages(k) {
		return "", false
	}
	e.mu.Lock()
	_, eligible := e.plans[k]
	e.mu.Unlock()
	if !eligible {
		return "", false
	}
	id, err := e.describe(ctx, k.AuthID)
	e.mu.Lock()
	defer e.mu.Unlock()
	previous := e.requests[requestID]
	// The current Host ABI has no attempt ID on every callback. Once an active
	// request ID is rebound, callbacks cannot safely be attributed to an attempt.
	// Keep a bounded tombstone and suppress its watchdog, rather than letting a
	// late old response revoke the new ticket. Request IDs must be unique between
	// completed executions as required by the Host API contract.
	if previous != nil {
		previous.observer = nil
		previous.created = e.now()
	}
	if e.closed {
		return "", false
	}
	r := e.records[k]
	if _, eligible := e.plans[k]; !eligible || r == nil {
		return "", false
	}
	if r.value != nil && (err != nil || r.value.scope != id.Scope || !e.identityPlanMatches(id.Plan, e.plans[k]) || !e.now().Before(r.value.expires)) {
		r.value = nil
		e.bumpEpochLocked(r)
	}
	if r.value == nil {
		e.enqueueLocked(k)
		return "", false
	}
	if callerState {
		return "", false
	}
	if len(e.requests) >= e.cfg.MaxPending {
		return "", false
	}
	t := r.value
	observer := NewCompletion(k.Model)
	if previous != nil {
		observer = nil
	}
	e.requests[requestID] = &observation{k, t.version, observer, e.now()}
	return t.state, true
}
func (e *Engine) invalidateLocked(o *observation, reason string) {
	if o.observer == nil {
		return
	}
	r := e.records[o.key]
	if r == nil || r.value == nil || r.value.version != o.version {
		return
	}
	r.value = nil
	e.bumpEpochLocked(r)
	r.cooldown = time.Time{}
	e.enqueueLocked(o.key)
	// Logging is intentionally deferred to worker events: no Host calls under this lock.
	_ = reason
}
func (e *Engine) Headers(requestID string, h http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.requests[requestID]
	if o == nil {
		return
	}
	for name, values := range h {
		if http.CanonicalHeaderKey(name) == "X-Codex-Plan-Type" {
			for _, v := range values {
				if !PlanMatches(v, e.plans[o.key]) {
					e.invalidateLocked(o, "plan_mismatch")
				}
			}
		}
		if http.CanonicalHeaderKey(name) == StateHeader {
			if len(values) > 1 {
				e.invalidateLocked(o, "conflicting_state")
			}
			for _, v := range values {
				if len(v) == 312 {
					e.invalidateLocked(o, "state_312")
				}
			}
		}
	}
}
func (e *Engine) Chunk(requestID string, p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.requests[requestID]
	if o == nil || o.observer == nil {
		return
	}
	o.observer.Feed(p)
	if o.observer.Mismatch() {
		e.invalidateLocked(o, "model_mismatch")
	}
}
func (e *Engine) JSON(requestID string, p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.requests[requestID]
	if o == nil || o.observer == nil {
		return
	}
	o.observer.JSON(p)
	if o.observer.Mismatch() {
		e.invalidateLocked(o, "model_mismatch")
	}
}
func (e *Engine) Complete(requestID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.requests[requestID]
	if o == nil || o.observer == nil {
		return
	}
	o.observer.Finish()
	if o.observer.Mismatch() {
		e.invalidateLocked(o, "model_mismatch")
	}
	delete(e.requests, requestID)
}
