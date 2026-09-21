package probe

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestDiscoveryReadsOnlyEligibleCredentialsAndKeepsSelectorIdentity(t *testing.T) {
	c, f := fixtureClient(t)
	f.entry.Email = "alice@example.com"
	rows, err := c.DiscoverAccounts(context.Background())
	if err != nil || len(rows) != 1 || rows[0].AuthID != "a" || rows[0].Email != "alice@example.com" || rows[0].Plan != "pro" {
		t.Fatal("eligible account discovery", rows, err)
	}
	f.entry.Disabled = true
	start := len(f.methods)
	rows, err = c.DiscoverAccounts(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Plan != "" || rows[0].Email != "alice@example.com" {
		t.Fatal("disabled identity/eligibility", rows, err)
	}
	for _, method := range f.methods[start:] {
		if method == pluginabi.MethodHostAuthGet {
			t.Fatal("read credentials for disabled account")
		}
	}
}
func TestDiscoveryUnknownPlansAndEmailFallback(t *testing.T) {
	c, f := fixtureClient(t)
	f.auth["email"] = "from-file@example.com"
	f.auth["plan_type"] = "unsupported"
	rows, err := c.DiscoverAccounts(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Plan != "" || rows[0].Email != "from-file@example.com" {
		t.Fatal("unknown plan became eligible", rows, err)
	}
	f.auth["plan_type"] = "self_serve_business_prolite"
	rows, err = c.DiscoverAccounts(context.Background())
	if err != nil || rows[0].Plan != "team" {
		t.Fatal("business plan not normalized", rows, err)
	}
}
func TestDiscoveryReplacesIndexCacheAndRetriesStartup(t *testing.T) {
	c, f := fixtureClient(t)
	call := c.call
	c.call = func(ctx context.Context, m string, in, out any) error {
		if m == pluginabi.MethodHostAuthList {
			return errors.New("manager not ready")
		}
		return call(ctx, m, in, out)
	}
	if _, err := c.DiscoverAccounts(context.Background()); err == nil {
		t.Fatal("unavailable directory treated as empty success")
	}
	c.call = call
	if _, err := c.DiscoverAccounts(context.Background()); err != nil {
		t.Fatal("startup not retried", err)
	}
	f.entry.ID = "" // Removed from the directory.
	if _, err := c.DiscoverAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n := len(c.indices)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("removed account index cached")
	}
	if _, err := c.Describe(context.Background(), "a"); err == nil {
		t.Fatal("removed account still resolved")
	}
}
func TestSourceGuardRunsAfterRegistration(t *testing.T) {
	c, _ := fixtureClient(t)
	blocked := false
	c.sourceGuard = func() error {
		if blocked {
			return errors.New("non-file backend")
		}
		return nil
	}
	if _, err := c.Describe(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	blocked = true
	if _, err := c.Describe(context.Background(), "a"); err == nil {
		t.Fatal("source guard not repeated")
	}
	if _, err := c.DiscoverAccounts(context.Background()); err == nil {
		t.Fatal("discovery ignored source change")
	}
}
