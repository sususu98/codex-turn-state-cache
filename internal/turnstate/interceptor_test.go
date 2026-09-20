package turnstate

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func newTestPlugin(now func() time.Time) *Plugin {
	return NewPlugin(NewCache(10, 10, now), nil)
}

func afterAuth(requestID, authID, model string, headers http.Header) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		ToFormat:  "codex",
		Model:     model,
		Headers:   headers,
		Metadata:  map[string]any{selectedAuthIDMetadataKey: authID},
	}
}

func TestPluginReusesOnlyMatchingAuthAndModelAcrossIPs(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	cacheState := state("s")
	callerState := state("c")

	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("capture", "auth-a", "gpt-5.3-codex", http.Header{"X-Forwarded-For": {"198.51.100.10"}})); err != nil {
		t.Fatalf("bind capture: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{TurnStateHeader: {cacheState}},
	}); err != nil {
		t.Fatalf("capture response: %v", err)
	}

	response, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("reuse", "auth-a", "gpt-5.3-codex", http.Header{
		TurnStateHeader:   {callerState},
		"X-Forwarded-For": {"203.0.113.45"},
	}))
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != cacheState {
		t.Fatalf("cross-IP state = %q", got)
	}

	for _, request := range []pluginapi.RequestInterceptRequest{
		afterAuth("other-auth", "auth-b", "gpt-5.3-codex", nil),
		afterAuth("other-model", "auth-a", "gpt-5.3-codex-mini", nil),
	} {
		response, err := plugin.InterceptRequestAfterAuth(context.Background(), request)
		if err != nil {
			t.Fatalf("isolation request: %v", err)
		}
		if got := response.Headers.Get(TurnStateHeader); got != "" {
			t.Fatalf("isolated request received state %q", got)
		}
	}
}

func TestPluginCapturesAgainstAfterAuthModel(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("capture", "auth-a", "gpt-5.3-codex", nil)); err != nil {
		t.Fatalf("bind capture: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		Model:           "normalized-client-alias",
		ResponseHeaders: http.Header{TurnStateHeader: {state("s")}},
	}); err != nil {
		t.Fatalf("capture response: %v", err)
	}

	selected, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("selected", "auth-a", "gpt-5.3-codex", nil))
	if err != nil {
		t.Fatalf("selected model: %v", err)
	}
	if got := selected.Headers.Get(TurnStateHeader); got != state("s") {
		t.Fatalf("selected model state = %q", got)
	}
	alias, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("alias", "auth-a", "normalized-client-alias", nil))
	if err != nil {
		t.Fatalf("alias model: %v", err)
	}
	if got := alias.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("response model unexpectedly received state %q", got)
	}
}

func TestPluginCapturesOnlyStreamHeaderInitAndDropsCompletedRequests(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("stream", "auth-a", "gpt-stream", nil)); err != nil {
		t.Fatalf("bind stream: %v", err)
	}
	if _, err := plugin.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{
		RequestID:       "stream",
		ChunkIndex:      0,
		ResponseHeaders: http.Header{TurnStateHeader: {state("s")}},
	}); err != nil {
		t.Fatalf("capture payload chunk: %v", err)
	}
	miss, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("payload-miss", "auth-a", "gpt-stream", nil))
	if err != nil || miss.Headers.Get(TurnStateHeader) != "" {
		t.Fatalf("payload chunk stored state: response=%#v error=%v", miss, err)
	}
	if _, err := plugin.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{
		RequestID:       "stream",
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{TurnStateHeader: {state("s")}},
	}); err != nil {
		t.Fatalf("capture header init: %v", err)
	}
	hit, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("stream-hit", "auth-a", "gpt-stream", nil))
	if err != nil || hit.Headers.Get(TurnStateHeader) != state("s") {
		t.Fatalf("header init state: response=%#v error=%v", hit, err)
	}

	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("late", "auth-a", "gpt-late", nil)); err != nil {
		t.Fatalf("bind late response: %v", err)
	}
	if err := plugin.HandleRequestComplete(context.Background(), pluginapi.RequestCompletion{RequestID: "late"}); err != nil {
		t.Fatalf("complete request: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "late",
		ResponseHeaders: http.Header{TurnStateHeader: {state("l")}},
	}); err != nil {
		t.Fatalf("late response: %v", err)
	}
	late, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("late-hit", "auth-a", "gpt-late", nil))
	if err != nil || late.Headers.Get(TurnStateHeader) != "" {
		t.Fatalf("late response stored state: response=%#v error=%v", late, err)
	}
}

func TestPluginPlanHeaderEnforcesStrictPairing(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })

	s292 := strings.Repeat("p", StateLength)
	s332 := strings.Repeat("t", StateLengthTeam)

	// 1. Team plan with 292 state -> must be rejected
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("team-mismatch", "team-auth-1", "gpt-6-astra", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID: "team-mismatch",
		ResponseHeaders: http.Header{
			TurnStateHeader: {s292},
			PlanTypeHeader:  {"team"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	missTeam, _ := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("check-team-miss", "team-auth-1", "gpt-6-astra", nil))
	if got := missTeam.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("team with 292 state should not be cached, got %q", got)
	}

	// 2. Team plan with 332 state -> must be accepted and injected
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("team-match", "team-auth-2", "gpt-6-astra", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID: "team-match",
		ResponseHeaders: http.Header{
			TurnStateHeader: {s332},
			PlanTypeHeader:  {"team"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	hitTeam, _ := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("check-team-hit", "team-auth-2", "gpt-6-astra", nil))
	if got := hitTeam.Headers.Get(TurnStateHeader); got != s332 {
		t.Fatalf("team with 332 state should be cached, got %q, want %q", got, s332)
	}

	// 3. Pro plan with 332 state -> must be rejected
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("pro-mismatch", "pro-auth-1", "gpt-6-astra", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID: "pro-mismatch",
		ResponseHeaders: http.Header{
			TurnStateHeader: {s332},
			PlanTypeHeader:  {"pro"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	missPro, _ := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("check-pro-miss", "pro-auth-1", "gpt-6-astra", nil))
	if got := missPro.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("pro with 332 state should not be cached, got %q", got)
	}

	// 4. Pro plan with 292 state -> must be accepted and injected
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("pro-match", "pro-auth-2", "gpt-6-astra", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID: "pro-match",
		ResponseHeaders: http.Header{
			TurnStateHeader: {s292},
			PlanTypeHeader:  {"pro"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	hitPro, _ := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("check-pro-hit", "pro-auth-2", "gpt-6-astra", nil))
	if got := hitPro.Headers.Get(TurnStateHeader); got != s292 {
		t.Fatalf("pro with 292 state should be cached, got %q, want %q", got, s292)
	}
}

func TestPluginObserveWebSocketResponseEvent(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	s332 := strings.Repeat("t", StateLengthTeam)

	// Step 1: Bind WebSocket request
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("ws-req-1", "ws-auth-1", "gpt-6-astra", nil)); err != nil {
		t.Fatal(err)
	}

	// Step 2: Receive codex.rate_limits frame first
	rateLimitsFrame := []byte(`{"type":"codex.rate_limits","plan_type":"self_serve_business_prolite","rate_limits":{"allowed":true}}`)
	if err := plugin.ObserveWebSocketResponseEvent(context.Background(), pluginapi.WebSocketResponseEvent{
		RequestID: "ws-req-1",
		EventType: "codex.rate_limits",
		Payload:   rateLimitsFrame,
	}); err != nil {
		t.Fatalf("observe rate_limits: %v", err)
	}

	// Step 3: Receive codex.response.metadata frame with 332-byte Astra state
	metadataFrame := []byte(`{"type":"codex.response.metadata","headers":{"x-models-etag":"W/\"etag\"","x-codex-turn-state":"` + s332 + `","x-codex-safety-buffering-enabled":"true"}}`)
	if err := plugin.ObserveWebSocketResponseEvent(context.Background(), pluginapi.WebSocketResponseEvent{
		RequestID: "ws-req-1",
		EventType: "codex.response.metadata",
		Payload:   metadataFrame,
	}); err != nil {
		t.Fatalf("observe metadata: %v", err)
	}

	// Step 4: Verify state is injected into next request
	hit, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("ws-req-2", "ws-auth-1", "gpt-6-astra", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := hit.Headers.Get(TurnStateHeader); got != s332 {
		t.Fatalf("expected injected state %q, got %q", s332, got)
	}
}

func TestPluginObserveWebSocketResponseEventRejectsLuna356(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	s356 := strings.Repeat("l", 356)

	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("ws-req-luna", "ws-auth-luna", "gpt-6-astra", nil)); err != nil {
		t.Fatal(err)
	}

	// Receive rate_limits then 356-byte downgraded state
	rateLimitsFrame := []byte(`{"type":"codex.rate_limits","plan_type":"self_serve_business_prolite"}`)
	_ = plugin.ObserveWebSocketResponseEvent(context.Background(), pluginapi.WebSocketResponseEvent{
		RequestID: "ws-req-luna",
		EventType: "codex.rate_limits",
		Payload:   rateLimitsFrame,
	})

	metadataFrame := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + s356 + `"}}`)
	_ = plugin.ObserveWebSocketResponseEvent(context.Background(), pluginapi.WebSocketResponseEvent{
		RequestID: "ws-req-luna",
		EventType: "codex.response.metadata",
		Payload:   metadataFrame,
	})

	miss, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("ws-req-check", "ws-auth-luna", "gpt-6-astra", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := miss.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("356 luna state should not be cached, got %q", got)
	}
}
