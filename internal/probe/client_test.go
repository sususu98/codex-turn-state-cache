package probe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5345asda/codex-turn-state-cache/internal/prewarm"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	entry   pluginapi.HostAuthFileEntry
	auth    map[string]any
	methods []string
	path    string
}

func fixtureClient(t *testing.T) (*Client, *fixture) {
	t.Helper()
	f := &fixture{entry: pluginapi.HostAuthFileEntry{ID: "a", AuthIndex: "index-a", Type: "codex", Provider: "codex"}, auth: map[string]any{"type": "codex", "access_token": "TOKEN_SECRET", "refresh_token": "DO_NOT_USE", "account_id": "account-a", "plan_type": "pro", "proxy_url": "socks5h://formal:password@127.0.0.1:1080", "headers": map[string]string{"Authorization": "do-not-use", "X-Codex-Turn-State": "stale"}}}
	f.path = filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(f.path, []byte("proxy-url: socks5h://global:password@127.0.0.1:1081\n"), 0600)
	call := func(ctx context.Context, method string, in, out any) error {
		f.methods = append(f.methods, method)
		var value any
		switch method {
		case pluginabi.MethodHostAuthList:
			value = map[string]any{"files": []pluginapi.HostAuthFileEntry{f.entry}}
		case pluginabi.MethodHostAuthGetRuntime:
			value = pluginapi.HostAuthGetRuntimeResponse{Auth: f.entry}
		case pluginabi.MethodHostAuthGet:
			raw, _ := json.Marshal(f.auth)
			value = pluginapi.HostAuthGetResponse{AuthIndex: f.entry.AuthIndex, JSON: raw}
		default:
			return errors.New("forbidden callback")
		}
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	}
	c, err := New(call, f.path)
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}
func body(model string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"model":"` + model + `","status":"completed"}}` + "\n\n"))
}
func TestPluginProbeUsesOnlyExistingCallbacksAndPrivateRoutes(t *testing.T) {
	c, f := fixtureClient(t)
	id, err := c.Describe(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(id)
	if strings.Contains(string(encoded), "SECRET") || strings.Contains(string(encoded), "password") {
		t.Fatal("identity leaked secret")
	}
	var routes, states []string
	c.transport = func(route string) (http.RoundTripper, error) {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			routes = append(routes, route)
			states = append(states, r.Header.Get(prewarm.StateHeader))
			if r.URL.String() != endpoint || r.Header.Get("Authorization") != "Bearer TOKEN_SECRET" || r.Header.Get("Chatgpt-Account-Id") != "account-a" {
				t.Fatal("wrong destination or identity")
			}
			var payload map[string]any
			json.NewDecoder(r.Body).Decode(&payload)
			if payload["model"] != "gpt-test" || payload["tools"] != nil || payload["store"] != false {
				t.Fatal("not a fixed tool-free probe")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{prewarm.StateHeader: {"candidate"}}, Body: body("gpt-test")}, nil
		}), nil
	}
	dynamic := "socks5h://dynamic:password@127.0.0.1:1082"
	req := prewarm.ProbeRequest{AuthID: "a", Model: "gpt-test", ExpectedScope: id.Scope, TimeoutSeconds: 2, ProxyOverride: &dynamic}
	first, err := c.Probe(context.Background(), req)
	if err != nil || !prewarm.SuccessfulProbe(first, "gpt-test") {
		t.Fatal("acquisition failed", err)
	}
	req.ProxyOverride = nil
	req.State = first.State
	if _, err := c.Probe(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0] != dynamic || routes[1] != f.auth["proxy_url"] || states[0] != "" || states[1] != "candidate" {
		t.Fatal("route/state sequence")
	}
	if f.auth["proxy_url"] != routes[1] {
		t.Fatal("formal route mutated")
	}
	for _, method := range f.methods {
		if method != pluginabi.MethodHostAuthList && method != pluginabi.MethodHostAuthGet && method != pluginabi.MethodHostAuthGetRuntime {
			t.Fatal("non-read callback used")
		}
	}
}
func TestDisabledAndChangedIdentityNeverProbe(t *testing.T) {
	c, f := fixtureClient(t)
	id, _ := c.Describe(context.Background(), "a")
	calls := 0
	c.transport = func(string) (http.RoundTripper, error) { calls++; return nil, errors.New("unexpected") }
	req := prewarm.ProbeRequest{AuthID: "a", Model: "gpt-test", ExpectedScope: id.Scope, TimeoutSeconds: 2}
	f.entry.Disabled = true
	if _, err := c.Probe(context.Background(), req); err == nil || calls != 0 {
		t.Fatal("disabled auth executed")
	}
	f.entry.Disabled = false
	f.auth["account_id"] = "different-account"
	if _, err := c.Probe(context.Background(), req); err == nil || calls != 0 {
		t.Fatal("identity change executed")
	}
}
func TestFormalProxyFallbackAndScope(t *testing.T) {
	c, f := fixtureClient(t)
	first, _ := c.Describe(context.Background(), "a")
	f.auth["access_token"] = "REFRESHED_SECRET"
	refreshed, _ := c.Describe(context.Background(), "a")
	if first.Scope != refreshed.Scope {
		t.Fatal("token refresh invalidated stable identity")
	}
	delete(f.auth, "proxy_url")
	global, err := c.resolve(context.Background(), "a")
	if err != nil || !strings.Contains(global.route, "global:") {
		t.Fatal("global proxy not used", err)
	}
	os.WriteFile(f.path, []byte("proxy-url: ''\n"), 0600)
	direct, err := c.resolve(context.Background(), "a")
	if err != nil || direct.route != "direct" || direct.scope == global.scope {
		t.Fatal("direct/scope mismatch", err)
	}
	os.WriteFile(f.path, []byte("proxy-url: not-a-valid-proxy\n"), 0600)
	if _, err := c.Describe(context.Background(), "a"); err == nil {
		t.Fatal("bad proxy fell back to direct")
	}
}
func TestProbeBoundsConflictsAndErrorRedaction(t *testing.T) {
	for _, which := range []string{"oversize", "duplicate", "network", "scope-change", "redirect"} {
		t.Run(which, func(t *testing.T) {
			c, f := fixtureClient(t)
			id, _ := c.Describe(context.Background(), "a")
			c.transport = func(string) (http.RoundTripper, error) {
				return roundTripFunc(func(r *http.Request) (*http.Response, error) {
					out := &http.Response{StatusCode: 200, Header: http.Header{}, Body: body("gpt-test")}
					switch which {
					case "oversize":
						out.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", prewarm.MaxProbeBytes+1)))
					case "duplicate":
						out.Header[prewarm.StateHeader] = []string{"a", "b"}
					case "network":
						return nil, errors.New("TOKEN_SECRET password")
					case "scope-change":
						f.entry.Disabled = true
					case "redirect":
						out.StatusCode = 302
						out.Header.Set("Location", "https://example.invalid/")
					}
					return out, nil
				}), nil
			}
			out, err := c.Probe(context.Background(), prewarm.ProbeRequest{AuthID: "a", Model: "gpt-test", ExpectedScope: id.Scope, TimeoutSeconds: 2})
			if which == "redirect" {
				if err != nil || out.Status != 302 {
					t.Fatal("redirect handling", err)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "password") {
				t.Fatal("unsafe error or invalid probe accepted")
			}
		})
	}
}
