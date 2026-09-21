// Package prewarm acquires state through a probe-only proxy pool and verifies it
// on the account's unchanged business route before making it available to traffic.
package prewarm

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var modelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

const maxAccounts = 1000
const maxModels = 16
const maxProxies = 10000

type Key struct{ AuthID, Model string }
type Account struct {
	Email  string   `yaml:"email,omitempty"`
	AuthID string   `yaml:"auth_id,omitempty"`
	Plan   string   `yaml:"plan"`
	Models []string `yaml:"models"`
}
type Config struct {
	// The normal configuration surface: enabled, models, budget and inline proxies.
	Enabled          bool     `yaml:"enabled"`
	Models           []string `yaml:"models"`
	MaxProbesPerHour int      `yaml:"max_probes_per_hour"`
	Proxies          []string `yaml:"proxies"`

	// Legacy/advanced overrides remain compatible but are not needed normally.
	ProxyFile              string    `yaml:"proxy_file"`
	HostConfigFile         string    `yaml:"host_config_file"`
	Accounts               []Account `yaml:"accounts"`
	Workers                int       `yaml:"workers"`
	MaxAttempts            int       `yaml:"max_attempts"`
	AttemptIntervalSeconds int       `yaml:"attempt_interval_seconds"`
	CooldownSeconds        int       `yaml:"cooldown_seconds"`
	TTLSeconds             int       `yaml:"ttl_seconds"`
	RefreshBeforeSeconds   int       `yaml:"refresh_before_seconds"`
	ProbeTimeoutSeconds    int       `yaml:"probe_timeout_seconds"`
	MaxPending             int       `yaml:"max_pending"`
}

func (c *Config) Defaults() {
	if c.Workers == 0 {
		c.Workers = 2
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 6
	}
	if c.AttemptIntervalSeconds == 0 {
		c.AttemptIntervalSeconds = 2
	}
	if c.CooldownSeconds == 0 {
		c.CooldownSeconds = 300
	}
	if c.TTLSeconds == 0 {
		c.TTLSeconds = 3600
	}
	if c.RefreshBeforeSeconds == 0 {
		c.RefreshBeforeSeconds = 300
	}
	if c.ProbeTimeoutSeconds == 0 {
		c.ProbeTimeoutSeconds = 20
	}
	if c.MaxProbesPerHour == 0 {
		c.MaxProbesPerHour = 12
	}
	if c.MaxPending == 0 {
		c.MaxPending = 20000
	}
}
func (c Config) AutoAccounts() bool {
	for _, a := range c.Accounts {
		if a.Plan != "" || len(a.Models) > 0 {
			return false
		}
	}
	return true
}
func (c Config) Selected(a Account) bool {
	if len(c.Accounts) == 0 {
		return true
	}
	for _, selector := range c.Accounts {
		if selector.AuthID != "" && selector.AuthID == a.AuthID {
			return true
		}
		if selector.Email != "" && strings.EqualFold(selector.Email, strings.TrimSpace(a.Email)) {
			return true
		}
	}
	return false
}
func validModels(models []string) bool {
	if len(models) == 0 || len(models) > maxModels {
		return false
	}
	seen := map[string]bool{}
	for _, m := range models {
		if !modelPattern.MatchString(m) || seen[m] {
			return false
		}
		seen[m] = true
	}
	return true
}
func validAuthID(id string) bool { return id != "" && len(id) <= 1024 && id == strings.TrimSpace(id) }
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if (len(c.Proxies) == 0) == (c.ProxyFile == "") {
		return errors.New("prewarm requires exactly one of proxies or legacy proxy_file")
	}
	if len(c.Proxies) > maxProxies {
		return errors.New("too many prewarm proxies")
	}
	if len(c.Accounts) > maxAccounts {
		return errors.New("too many prewarm accounts")
	}
	if (c.AutoAccounts() || len(c.Models) > 0) && !validModels(c.Models) {
		return errors.New("prewarm models must contain 1..16 unique upstream model names")
	}
	if c.Workers < 1 || c.Workers > 8 || c.MaxAttempts < 1 || c.MaxAttempts > 20 || c.AttemptIntervalSeconds < 1 || c.AttemptIntervalSeconds > 300 || c.CooldownSeconds < 1 || c.CooldownSeconds > 86400 || c.TTLSeconds < 60 || c.TTLSeconds > 3600 || c.RefreshBeforeSeconds < 1 || c.RefreshBeforeSeconds >= c.TTLSeconds || c.ProbeTimeoutSeconds < 1 || c.ProbeTimeoutSeconds > 60 || c.MaxProbesPerHour < 2 || c.MaxProbesPerHour > 1000 || c.MaxPending < 1 || c.MaxPending > 100000 {
		return errors.New("prewarm limits out of range")
	}
	if c.AutoAccounts() {
		seen := map[string]bool{}
		for _, a := range c.Accounts {
			if (a.AuthID == "") == (a.Email == "") {
				return errors.New("each prewarm account selector requires either email or auth_id")
			}
			key := "id:" + a.AuthID
			if a.Email != "" {
				address, err := mail.ParseAddress(a.Email)
				if err != nil || address.Address != a.Email || len(a.Email) > 320 {
					return errors.New("invalid prewarm email selector")
				}
				key = "email:" + strings.ToLower(a.Email)
			} else if !validAuthID(a.AuthID) {
				return errors.New("invalid prewarm auth_id selector")
			}
			if seen[key] {
				return errors.New("duplicate prewarm account selector")
			}
			seen[key] = true
		}
		return nil
	}
	seen := map[Key]bool{}
	for _, a := range c.Accounts {
		if a.Email != "" || !validAuthID(a.AuthID) || (a.Plan != "pro" && a.Plan != "team") || !validModels(a.Models) {
			return errors.New("prewarm accounts require an auth_id, pro/team plan and 1..16 models")
		}
		for _, m := range a.Models {
			k := Key{a.AuthID, m}
			if seen[k] {
				return errors.New("duplicate prewarm account/model")
			}
			seen[k] = true
		}
	}
	return nil
}
func (c Config) LoadProxies() ([]string, error) {
	if len(c.Proxies) > 0 {
		return ParseProxies(c.Proxies)
	}
	return LoadProxies(c.ProxyFile)
}

// parseProxy reports only the position, never a raw URL, password or parse error.
func parseProxy(raw string, slot int) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		fields := strings.SplitN(raw, ":", 4)
		if len(fields) != 4 || fields[2] == "" || fields[3] == "" {
			return "", fmt.Errorf("invalid proxy at entry %d", slot)
		}
		raw = (&url.URL{Scheme: "socks5h", Host: net.JoinHostPort(fields[0], fields[1]), User: url.UserPassword(fields[2], fields[3])}).String()
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "socks5h" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid SOCKS5H proxy at entry %d", slot)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid proxy port at entry %d", slot)
	}
	if len(u.String()) > 8192 {
		return "", fmt.Errorf("proxy URL too long at entry %d", slot)
	}
	return u.String(), nil
}

// ParseProxies validates and deduplicates the YAML list. Four-field entries are
// supported for migration, although explicit SOCKS5H URLs are recommended.
func ParseProxies(entries []string) ([]string, error) {
	if len(entries) > maxProxies {
		return nil, errors.New("too many prewarm proxies")
	}
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for i, raw := range entries {
		proxy, err := parseProxy(raw, i+1)
		if err != nil {
			return nil, err
		}
		if proxy != "" && !seen[proxy] {
			out = append(out, proxy)
			seen[proxy] = true
		}
	}
	if len(out) == 0 {
		return nil, errors.New("prewarm proxy list is empty")
	}
	return out, nil
}

// LoadProxies supports the legacy file configuration without exposing contents.
func LoadProxies(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open prewarm proxy file")
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	out := []string{}
	seen := map[string]bool{}
	line := 0
	for scanner.Scan() {
		line++
		proxy, err := parseProxy(scanner.Text(), line)
		if err != nil {
			return nil, err
		}
		if proxy != "" && !seen[proxy] {
			out = append(out, proxy)
			seen[proxy] = true
		}
		if len(out) > maxProxies {
			return nil, errors.New("too many prewarm proxies")
		}
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot read prewarm proxy file")
	}
	if len(out) == 0 {
		return nil, errors.New("prewarm proxy file is empty")
	}
	return out, nil
}

// CanonicalPlan returns a recognized length family; unknown plans are NOT
// automatically treated as Pro. Explicit legacy accounts remain supported.
func CanonicalPlan(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pro", "plus", "free", "individual":
		return "pro"
	case "team", "business", "self_serve_business_prolite":
		return "team"
	default:
		return ""
	}
}
func PlanMatches(raw, expected string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	return CanonicalPlan(raw) != "" && CanonicalPlan(raw) == expected
}
func ValidState(s, plan string) bool {
	n := 292
	if plan == "team" {
		n = 332
	} else if plan != "pro" {
		return false
	}
	return validToken(s, n)
}
func validToken(s string, n int) bool {
	if len(s) != n || !strings.HasPrefix(s, "gAAAAA") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '=') {
			return false
		}
	}
	return true
}
