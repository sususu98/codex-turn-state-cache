package prewarm

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func minimalConfig() Config {
	return Config{Enabled: true, Models: []string{"gpt-test"}, MaxProbesPerHour: 384, Proxies: []string{"socks5h://u:p@127.0.0.1:1080"}}
}
func TestMinimalYAMLAndLegacyConfig(t *testing.T) {
	var c Config
	raw := `enabled: true
models: [gpt-test]
max_probes_per_hour: 384
proxies:
  - "socks5h://user:p%40ss@127.0.0.1:1080"
  - "127.0.0.1:1080:user:p@ss"
`
	if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	proxies, err := c.LoadProxies()
	if err != nil || len(proxies) != 1 {
		t.Fatalf("inline dedup: %d %v", len(proxies), err)
	}
	if !c.AutoAccounts() || c.Workers != 2 || c.MaxAttempts != 6 || c.MaxProbesPerHour != 384 {
		t.Fatal("minimal defaults or budget changed")
	}
	legacy := Config{Enabled: true, ProxyFile: "pool.txt", Accounts: []Account{{AuthID: "a", Plan: "pro", Models: []string{"gpt-test"}}}}
	legacy.Defaults()
	if err := legacy.Validate(); err != nil || legacy.AutoAccounts() {
		t.Fatal("legacy config broken", err)
	}
}
func TestMinimalSelectorsValidation(t *testing.T) {
	c := minimalConfig()
	c.Accounts = []Account{{Email: "Alice@example.com"}, {AuthID: "auth-b"}}
	c.Defaults()
	if err := c.Validate(); err != nil || !c.AutoAccounts() {
		t.Fatal("selectors rejected", err)
	}
	if !c.Selected(Account{AuthID: "a", Email: "alice@example.com"}) || !c.Selected(Account{AuthID: "auth-b"}) || c.Selected(Account{AuthID: "auth-c", Email: "not-alice@example.com"}) {
		t.Fatal("selector matching")
	}
	for _, accounts := range [][]Account{
		{{Email: "bad"}}, {{Email: "a@example.com", AuthID: "a"}}, {{}},
		{{Email: "A@example.com"}, {Email: "a@example.com"}},
		{{AuthID: "a"}, {AuthID: "a", Plan: "pro", Models: []string{"gpt-test"}}},
	} {
		bad := c
		bad.Accounts = accounts
		if bad.Validate() == nil {
			t.Fatalf("invalid selectors accepted: %#v", accounts)
		}
	}
}
func TestMisspelledAccountFilterIsRejected(t *testing.T) {
	for _, raw := range []string{
		"enabled: true\nmodels: [gpt-test]\nproxies: [socks5h://u:SECRET@127.0.0.1:1080]\naccount: [{email: a@example.com}]\n",
		"defaults: &base {account: [{email: a@example.com}]}\n",
		"<<: &base {account: [{email: a@example.com}]}\nenabled: true\n",
	} {
		var c Config
		err := yaml.Unmarshal([]byte(raw), &c)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("misspelled selector silently accepted or leaked")
		}
	}
}

func TestMinimalProxyErrorsAreCredentialSafe(t *testing.T) {
	for _, raw := range []string{"socks5://user:SUPERSECRET@127.0.0.1:1080", "socks5h://user:SUPERSECRET@127.0.0.1:0", "socks5h://user:SUPERSECRET@127.0.0.1:1080/path"} {
		_, err := ParseProxies([]string{raw})
		if err == nil || strings.Contains(err.Error(), "SUPERSECRET") {
			t.Fatal("unsafe proxy parser error")
		}
	}
	c := minimalConfig()
	c.Defaults()
	c.ProxyFile = "legacy.txt"
	if c.Validate() == nil {
		t.Fatal("ambiguous proxy sources accepted")
	}
	c = minimalConfig()
	c.Defaults()
	c.Models = nil
	if c.Validate() == nil {
		t.Fatal("automatic discovery without explicit models accepted")
	}
}
