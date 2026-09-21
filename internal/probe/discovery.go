package probe

import (
	"context"
	"errors"
	"sort"

	"github.com/5345asda/codex-turn-state-cache/internal/prewarm"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// readDirectory replaces (rather than grows) the index cache. It makes no
// upstream requests and never changes the Host's account status.
func (c *Client) readDirectory(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	var listing struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if c.call(ctx, pluginabi.MethodHostAuthList, struct{}{}, &listing) != nil {
		return nil, errors.New("existing Host account list unavailable")
	}
	indices := map[string]string{}
	for _, a := range listing.Files {
		if a.ID != "" && a.AuthIndex != "" && (a.Type == "codex" || a.Provider == "codex") {
			indices[a.ID] = a.AuthIndex
		}
	}
	c.mu.Lock()
	c.indices = indices
	c.mu.Unlock()
	return listing.Files, nil
}

// DiscoverAccounts uses the same identity/disabled/route validation as probes,
// but performs only local read-only callbacks and file reads. Unknown plans,
// disabled/runtime-only accounts and custom upstreams have no eligible plan.
// Non-Codex accounts are excluded entirely.
func (c *Client) DiscoverAccounts(ctx context.Context) ([]prewarm.Account, error) {
	// A broken source is not a successful empty directory snapshot.
	if _, err := c.readConfig(); err != nil {
		return nil, err
	}
	files, err := c.readDirectory(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
	result := []prewarm.Account{}
	seen := map[string]bool{}
	for _, a := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.ID == "" || a.AuthIndex == "" || (a.Type != "codex" && a.Provider != "codex") {
			continue
		}
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		account := prewarm.Account{AuthID: a.ID, Email: a.Email}
		// Empty plan means known but ineligible. Keep identity/email for selector
		// ownership so disabling an account cannot fall back to passive state reuse.
		if !a.Disabled && a.Status != "disabled" && !a.RuntimeOnly {
			if s, err := c.resolve(ctx, a.ID); err == nil {
				account.Plan = prewarm.CanonicalPlan(s.plan)
				if account.Email == "" {
					account.Email = s.email
				}
			}
		}
		result = append(result, account)
		if len(result) > 1000 {
			return nil, errors.New("too many automatically discovered Codex accounts")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
