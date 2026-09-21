package prewarm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AccountDiscoverer is an optional read-only capability supplied by the plugin
// resolver, not a new CPA Host API. Models remain an explicit configuration choice.
type AccountDiscoverer interface {
	DiscoverAccounts(context.Context) ([]Account, error)
}

func (e *Engine) identityPlanMatches(raw, expected string) bool {
	if e.cfg.AutoAccounts() {
		return CanonicalPlan(raw) == expected
	}
	return PlanMatches(raw, expected)
}

func (e *Engine) bumpEpochLocked(r *record) {
	if e.epoch < r.epoch {
		e.epoch = r.epoch
	}
	e.epoch++
	r.epoch = e.epoch
}

// refreshAccounts runs outside the engine lock so credential callbacks cannot
// block response observation. Native callbacks stay local/read-only. A successful
// snapshot withdraws disabled/unknown/missing accounts; a directory failure keeps
// eligibility until retry, but every Bind/Probe still rechecks actual auth status.
func (e *Engine) refreshAccounts(force bool) {
	if e.discovery == nil {
		return
	}
	e.discoveryMu.Lock()
	defer e.discoveryMu.Unlock()
	e.mu.Lock()
	if e.closed || (!force && e.now().Before(e.nextDiscovery)) {
		e.mu.Unlock()
		return
	}
	e.nextDiscovery = e.now().Add(time.Minute)
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	accounts, err := e.discovery.DiscoverAccounts(ctx)
	cancel()
	if err == nil {
		err = e.applyAccounts(accounts)
	}
	if err != nil && e.ctx.Err() == nil {
		e.mu.Lock()
		e.nextDiscovery = e.now().Add(10 * time.Second)
		e.mu.Unlock()
		if e.log != nil {
			e.log("codex prewarm discovery_unavailable", "")
		}
	}
}

func (e *Engine) applyAccounts(accounts []Account) error {
	if len(accounts) > maxAccounts {
		return errors.New("too many discovered accounts")
	}
	desired := map[Key]string{}
	ids := map[string]bool{}
	selected := map[Key]bool{}
	eligible := 0
	for _, a := range accounts {
		if !validAuthID(a.AuthID) || ids[a.AuthID] || (a.Plan != "" && a.Plan != "pro" && a.Plan != "team") {
			return errors.New("invalid account discovery snapshot")
		}
		ids[a.AuthID] = true
		if !e.cfg.Selected(a) {
			continue
		}
		for _, model := range e.cfg.Models {
			selected[Key{a.AuthID, model}] = true
		}
		if a.Plan == "" {
			continue
		}
		eligible++
		for _, model := range e.cfg.Models {
			desired[Key{a.AuthID, model}] = a.Plan
		}
	}
	e.mu.Lock()
	if e.closed || e.ctx.Err() != nil {
		e.mu.Unlock()
		return nil
	}
	changed := e.discoveryCount < 0
	// Keep previously selected identities verified-only after removal or plan loss.
	// Bound retained ownership; on extreme churn, suppress passive fallback for
	// these models rather than forgetting ownership and injecting an old state.
	if len(e.cfg.Accounts) > 0 && !e.ownsAllModels {
		for k := range selected {
			if len(e.owned) >= maxAccounts*maxModels && !e.owned[k] {
				e.ownsAllModels = true
				e.owned = map[Key]bool{}
				break
			}
			e.owned[k] = true
		}
	}
	for k, plan := range e.plans {
		next, exists := desired[k]
		if exists && next == plan {
			continue
		}
		changed = true
		if r := e.records[k]; r != nil {
			r.value = nil
			e.bumpEpochLocked(r)
			r.cooldown = time.Time{}
			if !exists && !r.running {
				delete(e.records, k)
				delete(e.lastProxy, k)
				delete(e.lastFailure, k)
			}
		}
		delete(e.plans, k)
	}
	for k, plan := range desired {
		if e.plans[k] == plan {
			continue
		}
		changed = true
		r := e.records[k]
		if r == nil {
			r = &record{}
			e.records[k] = r
		}
		r.value = nil
		r.cooldown = time.Time{}
		e.bumpEpochLocked(r)
		e.plans[k] = plan
	}
	e.discoveryCount = eligible
	e.mu.Unlock()
	if changed && e.log != nil && e.ctx.Err() == nil {
		e.log(fmt.Sprintf("codex prewarm discovery accounts=%d models=%d", eligible, len(e.cfg.Models)), "")
	}
	return nil
}
