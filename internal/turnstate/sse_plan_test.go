package turnstate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Plan values observed from Local CPA's upstream HTTP/SSE response headers.
func TestSSEPlanPairingAndInjection(t *testing.T) {
	for _, tc := range []struct {
		plan     string
		length   int
		accepted bool
	}{
		{"pro", 292, true},
		{"pro", 332, false},
		{"self_serve_business_prolite", 332, true},
		{"self_serve_business_prolite", 292, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.plan, tc.length), func(t *testing.T) {
			var logs []string
			p := NewPlugin(NewCache(10, 10, nil), func(_ context.Context, message, _ string) { logs = append(logs, message) })
			ctx := context.Background()
			value := strings.Repeat("s", tc.length)
			if _, err := p.InterceptRequestAfterAuth(ctx, afterAuth("capture", "account", "model", nil)); err != nil {
				t.Fatal(err)
			}
			if _, err := p.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{
				RequestID: "capture", ChunkIndex: pluginapi.StreamChunkHeaderInitIndex,
				ResponseHeaders: http.Header{"x-codex-plan-type": {tc.plan}, "x-codex-turn-state": {value}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := p.HandleRequestComplete(ctx, pluginapi.RequestCompletion{RequestID: "capture"}); err != nil {
				t.Fatal(err)
			}
			resp, err := p.InterceptRequestAfterAuth(ctx, afterAuth("reuse", "account", "model", nil))
			if err != nil {
				t.Fatal(err)
			}
			got := resp.Headers.Get(TurnStateHeader)
			if tc.accepted && got != value {
				t.Fatal("valid SSE state was not injected")
			}
			if !tc.accepted && got != "" {
				t.Fatal("mismatched SSE state was injected")
			}
			if tc.accepted && (len(logs) != 2 || !strings.Contains(logs[0], "source=stream plan="+tc.plan)) {
				t.Fatalf("logs=%v", logs)
			}
			for _, message := range logs {
				if strings.Contains(message, value) {
					t.Fatal("state leaked to log")
				}
			}
			other, err := p.InterceptRequestAfterAuth(ctx, afterAuth("other", "other-account", "model", nil))
			if err != nil || other.Headers.Get(TurnStateHeader) != "" {
				t.Fatal("state crossed account boundary")
			}
		})
	}
}
