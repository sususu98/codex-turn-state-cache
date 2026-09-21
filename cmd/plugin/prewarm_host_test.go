//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/prewarm"
	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type integrationProber struct {
	calls atomic.Int32
	state string
}

func (p *integrationProber) Describe(context.Context, string) (prewarm.Identity, error) {
	return prewarm.Identity{Scope: "formal", Plan: "pro"}, nil
}
func (p *integrationProber) DiscoverAccounts(context.Context) ([]prewarm.Account, error) {
	return []prewarm.Account{{AuthID: "a", Email: "alice@example.com", Plan: "pro"}}, nil
}
func (p *integrationProber) Probe(_ context.Context, r prewarm.ProbeRequest) (prewarm.ProbeResult, error) {
	n := p.calls.Add(1)
	if n == 1 && (r.ProxyOverride == nil || r.State != "") || n == 2 && (r.ProxyOverride != nil || r.State != p.state) {
		return prewarm.ProbeResult{}, errors.New("wrong route")
	}
	return prewarm.ProbeResult{Scope: "formal", Status: 200, State: p.state, Body: []byte(`data: {"type":"response.completed","response":{"model":"gpt-test","status":"completed"}}` + "\n\n")}, nil
}
func TestPluginOnlyPrewarmIntegration(t *testing.T) {
	original := newProbeClient
	t.Cleanup(func() { resetRuntime(); newProbeClient = original })
	p := &integrationProber{state: "gAAAAA" + strings.Repeat("x", 286)}
	newProbeClient = func(string) (prewarm.Host, error) { return p, nil }
	pool := filepath.Join(t.TempDir(), "pool")
	if err := os.WriteFile(pool, []byte("127.0.0.1:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("prewarm:\n  enabled: true\n  host_config_file: /unused-in-fake\n  proxy_file: %q\n  accounts:\n    - auth_id: a\n      plan: pro\n      models: [gpt-test]\n", pool)
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion, ConfigYAML: []byte(config)}, &registration{})
	deadline := time.Now().Add(8 * time.Second)
	var result pluginapi.RequestInterceptResponse
	for {
		callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("business", "a", "gpt-test"), &result)
		if result.Headers.Get(turnstate.TurnStateHeader) == p.state {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("verified state not injected")
		}
		time.Sleep(time.Millisecond)
	}
	if p.calls.Load() != 2 {
		t.Fatal("expected acquisition and formal verification")
	}
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "business", ResponseHeaders: map[string][]string{turnstate.TurnStateHeader: {strings.Repeat("z", 292)}}}, &pluginapi.ResponseInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("next", "a", "gpt-test"), &result)
	if result.Headers.Get(turnstate.TurnStateHeader) != p.state {
		t.Fatal("passive capture replaced verified ticket")
	}
	old := currentPlugin()
	raw, _ := json.Marshal(lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion, ConfigYAML: []byte(strings.Replace(config, pool, pool+"-missing", 1))})
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, raw); err == nil || currentPlugin() != old {
		t.Fatal("invalid reload replaced plugin")
	}
	callPluginMethod(t, pluginabi.MethodPluginQuiesce, struct{}{}, &struct{}{})
}
func TestProbeBridgeAllowsOnlyExistingReadCallbacks(t *testing.T) {
	original := invokeProbeHost
	defer func() { invokeProbeHost = original }()
	calls := 0
	invokeProbeHost = func(string, []byte) ([]byte, error) { calls++; return nil, errors.New("SECRET") }
	if err := existingHostCall(context.Background(), pluginabi.MethodHostAuthList, struct{}{}, &struct{}{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("unsafe callback error")
	}
	for _, method := range []string{"host.codex.state_probe", pluginabi.MethodHostAuthSave, pluginabi.MethodHostModelExecute} {
		if existingHostCall(context.Background(), method, struct{}{}, &struct{}{}) == nil {
			t.Fatal("unapproved callback accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if existingHostCall(ctx, pluginabi.MethodHostAuthList, struct{}{}, &struct{}{}) == nil {
		t.Fatal("cancel ignored")
	}
	if calls != 1 {
		t.Fatal("unexpected Host callback")
	}
}
