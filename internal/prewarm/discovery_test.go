package prewarm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type directoryHost struct {
	mu       sync.Mutex
	accounts []Account
	fail     bool
	requests []ProbeRequest
}

func (d *directoryHost) DiscoverAccounts(context.Context) ([]Account, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail {
		return nil, errors.New("not ready")
	}
	return append([]Account(nil), d.accounts...), nil
}
func (d *directoryHost) Describe(_ context.Context, id string) (Identity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.accounts {
		if a.AuthID == id && a.Plan != "" {
			return Identity{Scope: id + ":" + a.Plan, Plan: a.Plan}, nil
		}
	}
	return Identity{}, errors.New("ineligible")
}
func (d *directoryHost) Probe(_ context.Context, r ProbeRequest) (ProbeResult, error) {
	d.mu.Lock()
	d.requests = append(d.requests, r)
	d.mu.Unlock()
	o := okResult(r)
	o.Scope = r.ExpectedScope
	return o, nil
}
func (d *directoryHost) set(accounts ...Account) { d.mu.Lock(); d.accounts = accounts; d.mu.Unlock() }
func autoEngine(t *testing.T, d *directoryHost, selectors ...Account) *Engine {
	t.Helper()
	c := minimalConfig()
	c.Accounts = selectors
	e, err := New(c, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}
func collectAuto(e *Engine, k Key) string {
	e.mu.Lock()
	r := e.records[k]
	epoch := r.epoch
	e.mu.Unlock()
	return e.collect(job{key: k, epoch: epoch})
}

func TestDiscoverySelectsEmailAndIDButNeverDisabled(t *testing.T) {
	d := &directoryHost{}
	d.set(Account{AuthID: "a", Email: "alice@example.com", Plan: "pro"}, Account{AuthID: "b", Email: "bob@example.com", Plan: "pro"}, Account{AuthID: "c", Email: "other@example.com", Plan: "pro"}, Account{AuthID: "disabled", Email: "alice@example.com"})
	e := autoEngine(t, d, Account{Email: "ALICE@example.com"}, Account{AuthID: "b"})
	// Before the initial directory snapshot, no passive fallback for these models.
	if !e.Manages(Key{"a", "gpt-test"}) {
		t.Fatal("startup fallback opened")
	}
	e.refreshAccounts(true)
	if len(e.plans) != 2 || e.plans[Key{"a", "gpt-test"}] != "pro" || e.plans[Key{"b", "gpt-test"}] != "pro" {
		t.Fatal("wrong selected accounts")
	}
	if e.Manages(Key{"c", "gpt-test"}) {
		t.Fatal("unselected account captured")
	}
	if !e.Manages(Key{"disabled", "gpt-test"}) || e.plans[Key{"disabled", "gpt-test"}] != "" {
		t.Fatal("disabled selector ownership unsafe")
	}
	if _, hit := e.Bind(context.Background(), "disabled", Key{"disabled", "gpt-test"}, false); hit {
		t.Fatal("disabled ticket injected")
	}
	if collectAuto(e, Key{"a", "gpt-test"}) != "ready" || collectAuto(e, Key{"b", "gpt-test"}) != "ready" {
		t.Fatal("selected accounts did not warm")
	}
	for _, r := range d.requests {
		if r.AuthID != "a" && r.AuthID != "b" {
			t.Fatal("unselected/disabled account probed")
		}
	}
}
func TestDiscoveryRemovalPlanLossAndABARejectOldWork(t *testing.T) {
	d := &directoryHost{}
	d.set(Account{AuthID: "a", Email: "alice@example.com", Plan: "pro"})
	e := autoEngine(t, d, Account{Email: "alice@example.com"})
	e.refreshAccounts(true)
	k := Key{"a", "gpt-test"}
	oldEpoch := e.records[k].epoch
	if collectAuto(e, k) != "ready" {
		t.Fatal("warm")
	}
	// Account becomes disabled/unknown-plan, retaining only its identity metadata.
	d.set(Account{AuthID: "a", Email: "alice@example.com"})
	e.refreshAccounts(true)
	if _, ok := e.plans[k]; ok {
		t.Fatal("ineligible account still scheduled")
	}
	if !e.Manages(k) {
		t.Fatal("withdrawal reopened passive caching")
	}
	if _, hit := e.Bind(context.Background(), "removed", k, false); hit {
		t.Fatal("withdrawn ticket used")
	}
	d.set()
	e.refreshAccounts(true)
	if !e.Manages(k) {
		t.Fatal("missing account reopened passive caching")
	}
	d.set(Account{AuthID: "a", Email: "alice@example.com", Plan: "pro"})
	e.refreshAccounts(true)
	if e.records[k].epoch <= oldEpoch {
		t.Fatal("readded account reused epoch")
	}
	before := len(d.requests)
	if e.collect(job{key: k, epoch: oldEpoch}) != "cancelled" || len(d.requests) != before {
		t.Fatal("old queued work executed after re-add")
	}
	if collectAuto(e, k) != "ready" {
		t.Fatal("readded account not eligible")
	}
}
func TestDiscoveryFailureIsRetryableAndUnknownPlanNotPro(t *testing.T) {
	d := &directoryHost{fail: true}
	e := autoEngine(t, d)
	e.refreshAccounts(true)
	if len(e.plans) != 0 || !e.Manages(Key{"future", "gpt-test"}) {
		t.Fatal("failed discovery opened fallback")
	}
	d.mu.Lock()
	d.fail = false
	d.mu.Unlock()
	d.set(Account{AuthID: "a", Plan: "pro"}, Account{AuthID: "unknown"})
	e.refreshAccounts(true)
	if len(e.plans) != 1 {
		t.Fatal("unknown plan treated as pro")
	}
	if !e.Manages(Key{"unknown", "gpt-test"}) {
		t.Fatal("unknown plan reverted to passive")
	}
	if e.Manages(Key{"a", "other-model"}) {
		t.Fatal("unconfigured model managed")
	}
}
func TestConcurrentDiscoveryAndBind(t *testing.T) {
	d := &directoryHost{}
	d.set(Account{AuthID: "a", Plan: "pro"})
	e := autoEngine(t, d)
	e.refreshAccounts(true)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if i%2 == 0 {
				d.set(Account{AuthID: "a", Plan: "pro"})
			} else {
				d.set(Account{AuthID: "a"})
			}
			e.refreshAccounts(true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			e.Manages(Key{"a", "gpt-test"})
			e.Bind(context.Background(), "r", Key{"a", "gpt-test"}, false)
			e.Complete("r")
		}
	}()
	wg.Wait()
}
