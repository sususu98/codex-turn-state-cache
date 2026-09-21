//go:build cgo

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/prewarm"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestMinimalPluginConfigurationWithEmailSelector(t *testing.T) {
	original := newProbeClient
	t.Cleanup(func() { resetRuntime(); newProbeClient = original })
	p := &integrationProber{state: "gAAAAA" + strings.Repeat("x", 286)}
	newProbeClient = func(path string) (prewarm.Host, error) {
		if path != "" {
			t.Error("unexpected explicit config path")
		}
		return p, nil
	}
	config := `prewarm:
  enabled: true
  models: [gpt-test]
  max_probes_per_hour: 384
  accounts:
    - email: ALICE@example.com
  proxies:
    - "socks5h://u:p@127.0.0.1:1080"
`
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion, ConfigYAML: []byte(config)}, &registration{})
	deadline := time.Now().Add(8 * time.Second)
	for {
		var out pluginapi.RequestInterceptResponse
		callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("minimal", "a", "gpt-test"), &out)
		if out.Headers.Get(prewarm.StateHeader) == p.state {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic selected account never warmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.calls.Load() != 2 {
		t.Fatal("unexpected background probe count")
	}
}
func TestInvalidInlineYAMLDoesNotLeakProxyCredentials(t *testing.T) {
	configureTestPlugin(t)
	previous := currentPlugin()
	raw, _ := json.Marshal(lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion, ConfigYAML: []byte("prewarm:\n  enabled: true\n  proxies: socks5h://u:VERY_SECRET_PASSWORD@127.0.0.1:1080\n")})
	_, err := handleMethod(pluginabi.MethodPluginReconfigure, raw)
	if err == nil || strings.Contains(err.Error(), "VERY_SECRET_PASSWORD") {
		t.Fatal("YAML error leaked or accepted credentials")
	}
	if currentPlugin() != previous {
		t.Fatal("failed YAML reload replaced instance")
	}
}
func TestLiveEnvironmentReader(t *testing.T) {
	const key = "TURN_STATE_TEST_LIVE_ENV"
	t.Setenv(key, "initial")
	if liveProcessEnv(key) != "initial" {
		t.Fatal("cannot read process environment")
	}
	t.Setenv(key, "updated")
	if liveProcessEnv(key) != "updated" {
		t.Fatal("live environment was cached")
	}
}
