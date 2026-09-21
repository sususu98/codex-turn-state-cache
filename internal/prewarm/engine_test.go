package prewarm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHost struct {
	mu    sync.Mutex
	scope string
	calls []ProbeRequest
	probe func(context.Context, ProbeRequest) (ProbeResult, error)
}

func (h *fakeHost) Describe(context.Context, string) (Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Identity{Scope: h.scope}, nil
}
func (h *fakeHost) Probe(ctx context.Context, r ProbeRequest) (ProbeResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, r)
	h.mu.Unlock()
	return h.probe(ctx, r)
}
func state(n int) string { return "gAAAAA" + strings.Repeat("x", n-6) }
func completed(model string) []byte {
	return []byte(fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":%q}}\n\n", model))
}
func okResult(r ProbeRequest) ProbeResult {
	return ProbeResult{Scope: r.ExpectedScope, Status: 200, State: state(292), Body: completed(r.Model)}
}
func testEngine(t *testing.T, h *fakeHost) *Engine {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pool.txt")
	if err := os.WriteFile(p, []byte("127.0.0.1:1080:user:pass\n127.0.0.1:1081:user:pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, ProxyFile: p, Accounts: []Account{{AuthID: "a", Plan: "pro", Models: []string{"gpt-test"}}}, MaxAttempts: 1}
	e, err := New(c, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

var testKey = Key{"a", "gpt-test"}

func TestCollectRequiresFormalValidationAndCachesOriginal(t *testing.T) {
	h := &fakeHost{scope: "formal-a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) {
		out := okResult(r)
		if r.ProxyOverride == nil {
			out.State = ""
		}
		return out, nil
	}
	e := testEngine(t, h)
	if got := e.collect(job{key: testKey, epoch: 0}); got != "ready" {
		t.Fatal(got)
	}
	if len(h.calls) != 2 || h.calls[0].ProxyOverride == nil || h.calls[0].State != "" || h.calls[1].ProxyOverride != nil || h.calls[1].State != state(292) {
		t.Fatal("acquire/validate route contract violated")
	}
	s, hit := e.Bind(context.Background(), "r", testKey, false)
	if !hit || s != state(292) {
		t.Fatal("candidate not available after formal validation")
	}
	if _, hit := e.Bind(context.Background(), "caller", testKey, true); hit {
		t.Fatal("must not overwrite caller state")
	}
	if _, ok := e.requests["caller"]; ok {
		t.Fatal("caller state must not revoke our ticket")
	}
}
func TestBadProbesNeverPublish(t *testing.T) {
	cases := []string{"candidate-model", "candidate-length", "candidate-prefix", "candidate-incomplete", "formal-model", "formal-312", "formal-error", "scope-changed", "formal-incomplete", "candidate-429", "formal-plan", "candidate-plan", "formal-malformed-312"}
	for _, which := range cases {
		t.Run(which, func(t *testing.T) {
			h := &fakeHost{scope: "formal-a"}
			h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) {
				o := okResult(r)
				if r.ProxyOverride != nil {
					switch which {
					case "candidate-model":
						o.Body = completed("wrong")
					case "candidate-length":
						o.State = state(332)
					case "candidate-prefix":
						o.State = strings.Repeat("x", 292)
					case "candidate-incomplete":
						o.Body = []byte("data: [DONE]\n\n")
					case "candidate-429":
						o.Status = 429
					case "candidate-plan":
						o.Plan = "team"
					}
				}
				if r.ProxyOverride == nil {
					switch which {
					case "formal-model":
						o.Body = completed("wrong")
					case "formal-plan":
						o.Plan = "team"
					case "formal-malformed-312":
						o.State = strings.Repeat("?", 312)
					case "formal-312":
						o.State = state(312)
					case "formal-error":
						return o, errors.New("error")
					case "scope-changed":
						h.mu.Lock()
						h.scope = "new"
						h.mu.Unlock()
					case "formal-incomplete":
						o.Body = []byte("data: {}\n\n")
					}
				}
				return o, nil
			}
			e := testEngine(t, h)
			if got := e.collect(job{key: testKey, epoch: 0}); got == "ready" {
				t.Fatal("bad candidate published")
			}
			if e.records[testKey].value != nil {
				t.Fatal("bad ticket cached")
			}
		})
	}
}
func TestBudgetCountsAcquisitionAndVerification(t *testing.T) {
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	e := testEngine(t, h)
	e.cfg.MaxProbesPerHour = 2
	if e.collect(job{key: testKey, epoch: 0}) != "ready" {
		t.Fatal("first round failed")
	}
	if e.collect(job{key: testKey, epoch: 0}) == "ready" || len(h.calls) != 2 {
		t.Fatal("hourly global probe budget exceeded")
	}
}
func TestScopeChangeTTLAndWatchdog(t *testing.T) {
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	e := testEngine(t, h)
	if e.collect(job{key: testKey, epoch: 0}) != "ready" {
		t.Fatal("collect")
	}
	if _, hit := e.Bind(context.Background(), "first", testKey, false); !hit {
		t.Fatal("bind")
	}
	e.Chunk("first", completed("wrong"))
	if e.records[testKey].value != nil {
		t.Fatal("model mismatch not revoked")
	}
	epoch := e.records[testKey].epoch
	if e.collect(job{key: testKey, epoch: epoch}) != "ready" {
		t.Fatal("recollect")
	}
	// A delayed old response must not revoke the newer version.
	e.Headers("first", http.Header{StateHeader: {state(312)}})
	if e.records[testKey].value == nil {
		t.Fatal("late response revoked newer ticket")
	}
	e.Complete("first")
	if len(e.requests) != 0 {
		t.Fatal("request leaked")
	}
	if _, hit := e.Bind(context.Background(), "new", testKey, false); !hit {
		t.Fatal("bind new")
	}
	e.Headers("new", http.Header{StateHeader: {state(312)}})
	if e.records[testKey].value != nil {
		t.Fatal("312 not revoked")
	}
	epoch = e.records[testKey].epoch
	if e.collect(job{key: testKey, epoch: epoch}) != "ready" {
		t.Fatal("recollect2")
	}
	h.mu.Lock()
	h.scope = "b"
	h.mu.Unlock()
	if _, hit := e.Bind(context.Background(), "changed", testKey, false); hit {
		t.Fatal("formal route changed but cached state reused")
	}
	epoch = e.records[testKey].epoch
	if e.collect(job{key: testKey, epoch: epoch}) != "ready" {
		t.Fatal("recollect3")
	}
	e.records[testKey].value.expires = time.Now().Add(-time.Second)
	if _, hit := e.Bind(context.Background(), "expired", testKey, false); hit {
		t.Fatal("expired ticket reused")
	}
}
func TestNewerEpochPreventsInflightPublication(t *testing.T) {
	h := &fakeHost{scope: "a"}
	var e *Engine
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) {
		if r.ProxyOverride == nil {
			e.mu.Lock()
			e.records[testKey].epoch++
			e.mu.Unlock()
		}
		return okResult(r), nil
	}
	e = testEngine(t, h)
	if e.collect(job{key: testKey, epoch: 0}) == "ready" || e.records[testKey].value != nil {
		t.Fatal("stale generation published")
	}
}
func TestFailedRefreshKeepsUsableTicket(t *testing.T) {
	h := &fakeHost{scope: "a"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	e := testEngine(t, h)
	e.collect(job{key: testKey, epoch: 0})
	previous := e.records[testKey].value
	h.probe = func(context.Context, ProbeRequest) (ProbeResult, error) { return ProbeResult{}, errors.New("offline") }
	e.collect(job{key: testKey, epoch: 0})
	if e.records[testKey].value != previous {
		t.Fatal("failed refresh replaced usable ticket")
	}
}
func TestWorkersDeduplicateAndClose(t *testing.T) {
	entered := make(chan struct{}, 1)
	h := &fakeHost{scope: "a"}
	h.probe = func(ctx context.Context, r ProbeRequest) (ProbeResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return ProbeResult{}, ctx.Err()
	}
	e := testEngine(t, h)
	e.Start()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	for i := 0; i < 100; i++ {
		e.Bind(context.Background(), fmt.Sprint(i), testKey, false)
	}
	e.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 1 {
		t.Fatalf("duplicate jobs: %d", len(h.calls))
	}
	if e.records[testKey].value != nil {
		t.Fatal("cancelled job published")
	}
}

func TestProxyFileParsingAndSafeErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proxies")
	raw := "# comment\n127.0.0.1:1080:user:p@ss:word\nsocks5h://user:p%40ss%3Aword@127.0.0.1:1080\nsocks5h://[::1]:1080\n"
	os.WriteFile(p, []byte(raw), 0600)
	proxies, err := LoadProxies(p)
	if err != nil || len(proxies) != 2 || !strings.Contains(proxies[0], "p%40ss%3Aword") {
		t.Fatalf("parse: count=%d err=%v", len(proxies), err)
	}
	for _, bad := range []string{"socks5://user:SECRET@127.0.0.1:1080", "socks5h://user:SECRET@127.0.0.1:0", "socks5h://user:SECRET@127.0.0.1:1080/path", "SECRET"} {
		os.WriteFile(p, []byte(bad), 0600)
		_, err := LoadProxies(p)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid proxy accepted or credential leaked")
		}
	}
}
func TestStateAndCompletion(t *testing.T) {
	if !ValidState(state(292), "pro") || !ValidState(state(332), "team") || ValidState(state(332), "pro") || ValidState(state(356), "team") || ValidState(strings.Repeat("x", 292), "pro") {
		t.Fatal("state validation")
	}
	raw := completed("gpt-test")
	for size := 1; size < len(raw); size++ {
		o := NewCompletion("gpt-test")
		for p := raw; len(p) > 0; {
			n := size
			if n > len(p) {
				n = len(p)
			}
			o.Feed(p[:n])
			p = p[n:]
		}
		o.Finish()
		if !o.Success() {
			t.Fatalf("split size %d", size)
		}
	}
	for _, raw := range []string{`data: [DONE]` + "\n\n", `data: {"type":"response.completed","response":{"model":"wrong"}}` + "\n\n", `data: {"type":"response.completed","response":{"model":"gpt-test","error":{"x":1}}}` + "\n\n"} {
		o := NewCompletion("gpt-test")
		o.Feed([]byte(raw))
		o.Finish()
		if o.Success() {
			t.Fatal("invalid completion accepted")
		}
	}
	o := NewCompletion("gpt-test")
	o.Feed([]byte(strings.Repeat("x", maxEventBytes+1)))
	o.Feed(completed("gpt-test"))
	if o.Success() {
		t.Fatal("overflow accepted")
	}
}
