package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/prewarm"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const endpoint = "https://chatgpt.com/backend-api/codex/responses"
const defaultUA = "codex_cli_rs/0.153.4"

// Call invokes an EXISTING read-only Host callback. No private extension, auth
// writes, token refresh, auth enable/disable, or Host model execution is used.
type Call func(context.Context, string, any, any) error
type Client struct {
	mu          sync.Mutex
	call        Call
	configFile  string
	indices     map[string]string
	transport   func(string) (http.RoundTripper, error)
	sourceGuard func() error
}
type hostConfig struct {
	Proxy   string `yaml:"proxy-url"`
	Headers struct {
		UserAgent string `yaml:"user-agent"`
	} `yaml:"codex-header-defaults"`
	Codex struct {
		DisableCloaking bool `yaml:"disable-codex-cloaking"`
	} `yaml:"codex"`
}
type credentials struct {
	Type        string            `json:"type"`
	Email       string            `json:"email"`
	Disabled    bool              `json:"disabled"`
	Token       string            `json:"access_token"`
	IDToken     string            `json:"id_token"`
	AccountID   string            `json:"account_id"`
	Proxy       string            `json:"proxy_url"`
	BaseURL     string            `json:"base_url"`
	Plan        string            `json:"plan_type"`
	ChatGPTPlan string            `json:"chatgpt_plan_type"`
	Headers     map[string]string `json:"headers"`
	// Deliberately no refresh_token field; the plugin never refreshes/saves auth.
}
type snapshot struct {
	scope, plan, token, accountID, route, email string
	headers                                     http.Header
}

func New(call Call, configFile string, guards ...func() error) (*Client, error) {
	if call == nil || configFile == "" {
		return nil, errors.New("prewarm requires existing Host callbacks and host_config_file")
	}
	c := &Client{call: call, configFile: configFile, indices: map[string]string{}, transport: newTransport}
	if len(guards) > 0 {
		c.sourceGuard = guards[0]
	}
	if _, err := c.readConfig(); err != nil {
		return nil, err
	}
	// Plugin registration precedes auth-manager attachment during CPA startup.
	// Resolve lazily; never fail plugin registration merely because auth isn't ready.
	return c, nil
}
func (c *Client) authIndex(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	index := c.indices[id]
	c.mu.Unlock()
	if index != "" {
		return index, nil
	}
	if _, err := c.readDirectory(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.indices[id] == "" {
		return "", errors.New("account not found in Host")
	}
	return c.indices[id], nil
}

func (c *Client) readConfig() (hostConfig, error) {
	var cfg hostConfig
	if c.sourceGuard != nil {
		if err := c.sourceGuard(); err != nil {
			return cfg, err
		}
	}
	f, err := os.Open(c.configFile)
	if err != nil {
		return cfg, errors.New("cannot read Host configuration")
	}
	defer f.Close()
	before, statErr := f.Stat()
	if statErr != nil {
		return cfg, errors.New("cannot stat Host configuration")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	after, afterErr := f.Stat()
	if afterErr != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != after.Size() {
		return cfg, errors.New("Host configuration changed while reading")
	}
	if err != nil || len(raw) == 0 || len(raw) > 4<<20 {
		return cfg, errors.New("Host configuration too large or unreadable")
	}
	var document yaml.Node
	if yaml.Unmarshal(raw, &document) != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || document.Decode(&cfg) != nil {
		return cfg, errors.New("invalid Host configuration")
	}
	return cfg, nil
}
func (c *Client) resolve(ctx context.Context, authID string) (snapshot, error) {
	var s snapshot
	if err := ctx.Err(); err != nil {
		return s, err
	}
	index, indexErr := c.authIndex(ctx, authID)
	if indexErr != nil {
		return s, indexErr
	}
	var runtime pluginapi.HostAuthGetRuntimeResponse
	if c.call(ctx, pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: index}, &runtime) != nil {
		return s, errors.New("account runtime unavailable")
	}
	a := runtime.Auth
	if a.ID != authID || a.AuthIndex != index || a.Disabled || a.Status == "disabled" || (a.Type != "codex" && a.Provider != "codex") || a.RuntimeOnly {
		return s, errors.New("enabled file-backed Codex account required")
	}
	if a.BaseURL != "" && strings.TrimRight(a.BaseURL, "/") != "https://chatgpt.com/backend-api/codex" {
		return s, errors.New("custom upstream unsupported for probes")
	}
	var file pluginapi.HostAuthGetResponse
	if c.call(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index}, &file) != nil {
		return s, errors.New("account credentials unavailable")
	}
	if file.AuthIndex != index || len(file.JSON) > 1<<20 {
		return s, errors.New("invalid account credential response")
	}
	var auth credentials
	if json.Unmarshal(file.JSON, &auth) != nil || auth.Type != "codex" || auth.Disabled || auth.Token == "" {
		return s, errors.New("enabled Codex OAuth credentials required")
	}
	if auth.BaseURL != "" && strings.TrimRight(auth.BaseURL, "/") != "https://chatgpt.com/backend-api/codex" {
		return s, errors.New("custom upstream unsupported for probes")
	}
	cfg, err := c.readConfig()
	if err != nil {
		return s, err
	}
	s.token = auth.Token
	s.email = auth.Email
	s.accountID = auth.AccountID
	s.plan = auth.Plan
	if s.plan == "" {
		s.plan = auth.ChatGPTPlan
	}
	// Saved JWT claims are local identity hints, not independently verified proof.
	parts := strings.Split(auth.IDToken, ".")
	if len(parts) == 3 {
		if decoded, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Email string `json:"email"`
				Auth  struct {
					AccountID string `json:"chatgpt_account_id"`
					Plan      string `json:"chatgpt_plan_type"`
				} `json:"https://api.openai.com/auth"`
			}
			if json.Unmarshal(decoded, &claims) == nil {
				if s.email == "" {
					s.email = claims.Email
				}
				if s.accountID == "" {
					s.accountID = claims.Auth.AccountID
				}
				if s.plan == "" {
					s.plan = claims.Auth.Plan
				}
			}
		}
	}
	if s.accountID == "" {
		return snapshot{}, errors.New("account identity unavailable")
	}
	s.route = strings.TrimSpace(auth.Proxy)
	if s.route == "" {
		s.route = strings.TrimSpace(cfg.Proxy)
	}
	if s.route == "" {
		s.route = "direct"
	}
	// Validate without connecting; malformed configured proxies never fall back.
	if _, err := newTransport(s.route); err != nil {
		return snapshot{}, err
	}
	s.headers = make(http.Header)
	s.headers.Set("User-Agent", defaultUA)
	s.headers.Set("Originator", "codex_cli_rs")
	if cfg.Headers.UserAgent != "" {
		s.headers.Set("User-Agent", cfg.Headers.UserAgent)
	}
	for k, v := range auth.Headers {
		if strings.ContainsAny(k+v, "\r\n") {
			return snapshot{}, errors.New("invalid configured auth header")
		}
		s.headers.Set(k, v)
	}
	if !cfg.Codex.DisableCloaking {
		s.headers.Set("User-Agent", defaultUA)
		s.headers.Set("Originator", "codex_cli_rs")
	}
	// Credentials and state have dedicated owners, never header-map overrides.
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Codex-Turn-State", "Chatgpt-Account-Id", "Host", "Content-Length", "Transfer-Encoding", "Connection"} {
		s.headers.Del(name)
	}
	raw, _ := json.Marshal([]any{"plugin-probe-v1", authID, s.accountID, s.plan, s.route, s.headers})
	sum := sha256.Sum256(raw)
	s.scope = hex.EncodeToString(sum[:])
	if err := ctx.Err(); err != nil {
		return snapshot{}, err
	}
	return s, nil
}
func (c *Client) Describe(ctx context.Context, authID string) (prewarm.Identity, error) {
	s, err := c.resolve(ctx, authID)
	return prewarm.Identity{Scope: s.scope, Plan: s.plan}, err
}
func (c *Client) Probe(ctx context.Context, r prewarm.ProbeRequest) (prewarm.ProbeResult, error) {
	var result prewarm.ProbeResult
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 60 {
		return result, errors.New("invalid probe timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutSeconds)*time.Second)
	defer cancel()
	s, err := c.resolve(ctx, r.AuthID)
	if err != nil {
		return result, err
	}
	if r.ExpectedScope == "" || r.ExpectedScope != s.scope {
		return result, errors.New("probe scope changed")
	}
	route := s.route
	if r.ProxyOverride != nil {
		route = *r.ProxyOverride
		if !strings.HasPrefix(route, "socks5h://") {
			return result, errors.New("harvest proxy must be SOCKS5H")
		}
	}
	transport, err := c.transport(route)
	if err != nil {
		return result, errors.New("invalid probe route")
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return result, errors.New("cannot generate probe session")
	}
	random[6] = (random[6] & 0x0f) | 0x40
	random[8] = (random[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(random)
	session := encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	payload, _ := json.Marshal(map[string]any{"model": r.Model, "store": false, "stream": true, "instructions": "Reply with exactly: pong", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return result, errors.New("cannot create probe")
	}
	request.Header = s.headers.Clone()
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Chatgpt-Account-Id", s.accountID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	request.Header.Set("Version", "0.153.4")
	request.Header.Set("Session-Id", session)
	if r.State != "" {
		request.Header.Set(prewarm.StateHeader, r.State)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return result, errors.New("probe transport failed")
	}
	defer response.Body.Close()
	result.Scope = s.scope
	result.Plan = s.plan
	result.Status = response.StatusCode
	if response.StatusCode == 200 {
		result.Body, err = io.ReadAll(io.LimitReader(response.Body, prewarm.MaxProbeBytes+1))
		if err != nil || len(result.Body) > prewarm.MaxProbeBytes || ctx.Err() != nil {
			return prewarm.ProbeResult{}, errors.New("probe response incomplete or oversized")
		}
		values := response.Header.Values(prewarm.StateHeader)
		if len(values) > 1 {
			return prewarm.ProbeResult{}, errors.New("conflicting probe states")
		}
		if len(values) == 1 {
			result.State = strings.TrimSpace(values[0])
		}
		_, result.ActualModel = prewarm.EvaluateProbeBody(result.Body, r.Model)
		plans := response.Header.Values("X-Codex-Plan-Type")
		if len(plans) > 1 {
			return prewarm.ProbeResult{}, errors.New("conflicting probe plans")
		}
		if len(plans) == 1 {
			result.Plan = strings.TrimSpace(plans[0])
		}
	}
	current, err := c.resolve(ctx, r.AuthID)
	if err != nil || current.scope != s.scope {
		return prewarm.ProbeResult{}, errors.New("account or formal route changed")
	}
	return result, nil
}
