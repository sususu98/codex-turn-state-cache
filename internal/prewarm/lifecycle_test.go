package prewarm

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestReconfigureSharesBudget(t *testing.T) {
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	shared := &Budget{}
	first := testEngine(t, h)
	first.cfg.MaxProbesPerHour = 2
	first.SetBudget(shared)
	if first.collect(job{key: testKey, epoch: 0}) != "ready" {
		t.Fatal("initial collection failed")
	}
	first.Close()
	second := testEngine(t, h)
	second.cfg.MaxProbesPerHour = 2
	second.SetBudget(shared)
	if second.collect(job{key: testKey, epoch: 0}) != "budget_exhausted" || len(h.calls) != 2 {
		t.Fatal("replacement engine reset budget")
	}
	second.now = func() time.Time { return time.Now().Add(time.Hour + time.Second) }
	if second.collect(job{key: testKey, epoch: 0}) != "ready" || len(h.calls) != 4 {
		t.Fatal("budget did not expire")
	}
}
func TestReusedActiveRequestIDSuppressesAmbiguousCallbacks(t *testing.T) {
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	e := testEngine(t, h)
	e.collect(job{key: testKey, epoch: 0})
	if _, ok := e.Bind(context.Background(), "retry-id", testKey, false); !ok {
		t.Fatal("initial bind")
	}
	e.collect(job{key: testKey, epoch: 0})
	version := e.records[testKey].value.version
	if _, ok := e.Bind(context.Background(), "retry-id", testKey, false); !ok {
		t.Fatal("retry bind")
	}
	// These may belong to the first attempt, not the second. The ABI does not
	// expose an attempt identity: conservatively ignore instead of corrupting.
	e.Headers("retry-id", http.Header{StateHeader: {state(312)}})
	e.Chunk("retry-id", completed("wrong"))
	e.Complete("retry-id")
	if e.records[testKey].value == nil || e.records[testKey].value.version != version {
		t.Fatal("late callback revoked replacement ticket")
	}
	if e.requests["retry-id"] == nil || e.requests["retry-id"].observer != nil {
		t.Fatal("ambiguous ID tombstone lost")
	}
	e.mu.Lock()
	e.requests["retry-id"].created = time.Now().Add(-3 * time.Hour)
	e.mu.Unlock()
	e.schedule()
	if e.requests["retry-id"] != nil {
		t.Fatal("expired tombstone retained")
	}
}
func TestIncompleteBusinessResponseIsNotDowngradeEvidence(t *testing.T) {
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	e := testEngine(t, h)
	e.collect(job{key: testKey, epoch: 0})
	e.Bind(context.Background(), "cancelled", testKey, false)
	e.Chunk("cancelled", []byte("data: {\"type\":\"response.created\"}\n\n"))
	e.Complete("cancelled")
	if e.records[testKey].value == nil {
		t.Fatal("incomplete business stream was treated as a downgrade")
	}
}
func TestPlanEvidence(t *testing.T) {
	if !PlanMatches("self_serve_business_prolite", "team") || PlanMatches("team", "pro") || PlanMatches("unknown", "pro") || !PlanMatches("", "pro") {
		t.Fatal("plan matching")
	}
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	e := testEngine(t, h)
	e.collect(job{key: testKey, epoch: 0})
	e.Bind(context.Background(), "plan", testKey, false)
	e.Headers("plan", http.Header{"X-Codex-Plan-Type": {"team"}})
	if e.records[testKey].value != nil {
		t.Fatal("known plan change ignored")
	}
	bad := e.cfg
	bad.Accounts = []Account{{AuthID: "a", Plan: "pro", Models: []string{"model(with-suffix)"}}}
	if bad.Validate() == nil {
		t.Fatal("unsupported Host model syntax accepted")
	}
}
