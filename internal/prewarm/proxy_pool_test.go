package prewarm

import (
	"context"
	"strings"
	"testing"
)

func TestSuccessfulProxyIsReusedForNextAccount(t *testing.T) {
	h := &fakeHost{scope: "formal"}
	var seen []string
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) {
		if r.ProxyOverride != nil {
			seen = append(seen, *r.ProxyOverride)
		}
		return okResult(r), nil
	}
	p := testEngine(t, h)
	p.plans[Key{"b", "gpt-test"}] = "pro"
	p.records[Key{"b", "gpt-test"}] = &record{}
	if p.collect(job{key: testKey, epoch: 0}) != "ready" {
		t.Fatal("first account not ready")
	}
	if p.collect(job{key: Key{"b", "gpt-test"}, epoch: 0}) != "ready" {
		t.Fatal("second account not ready")
	}
	if len(seen) != 2 || seen[0] != seen[1] {
		t.Fatalf("successful proxy not reused: %#v", seen)
	}
	if p.goodProxy < 0 || p.proxies[p.goodProxy].successes != 2 {
		t.Fatal("proxy success score not retained")
	}
}
func TestDisabledAccountDoesNotProbeOrUseTicket(t *testing.T) {
	h := &fakeHost{scope: "formal"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) { return okResult(r), nil }
	p := testEngine(t, h)
	if p.collect(job{key: testKey, epoch: 0}) != "ready" {
		t.Fatal("warm failed")
	}
	h.scope = "" // fake identity failure models a disabled runtime account
	if state, hit := p.Bind(context.Background(), "disabled", testKey, false); hit || state != "" {
		t.Fatal("disabled account retained usable ticket")
	}
	if p.records[testKey].value != nil {
		t.Fatal("disabled account ticket not cleared")
	}
}
func TestAccountAndProxyFailureLogHasOpaqueIdentifiers(t *testing.T) {
	h := &fakeHost{scope: "formal"}
	h.probe = func(_ context.Context, r ProbeRequest) (ProbeResult, error) {
		return ProbeResult{Scope: r.ExpectedScope, Status: 200, State: strings.Repeat("x", 292), Body: completed("wrong")}, nil
	}
	var logs []string
	p := testEngine(t, h)
	p.log = func(msg, model string) { logs = append(logs, msg+" "+model) }
	reason := p.collect(job{key: testKey, epoch: 0})
	p.logResult(reason, testKey, 0, "")
	if len(logs) != 1 || !strings.Contains(logs[0], "account=") || !strings.Contains(logs[0], "proxy=p1") || strings.Contains(logs[0], "auth-a") {
		t.Fatalf("unsafe or missing log: %#v", logs)
	}
}
