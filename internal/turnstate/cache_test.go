package turnstate

import (
	"strings"
	"testing"
	"time"
)

func state(value string) string {
	return strings.Repeat(value, StateLength/len(value))
}

func TestCacheAcceptsOnlyOne292ByteState(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	valid := strings.Repeat("\xC3\xA9", StateLength/2)
	cache.BindRequest("request", key)

	if gotKey, stored, replaced := cache.StoreResponseForRequest("request", []string{valid}); !stored || replaced || gotKey != key {
		t.Fatalf("first write = (%+v, %t, %t)", gotKey, stored, replaced)
	}
	for _, values := range [][]string{
		nil,
		{""},
		{strings.Repeat("x", StateLength-1)},
		{strings.Repeat("x", StateLength+1)},
		{valid, valid},
	} {
		if _, stored, _ := cache.StoreResponseForRequest("request", values); stored {
			t.Fatalf("stored invalid header values: %#v", values)
		}
		if got, ok := cache.Lookup(key); !ok || got != valid {
			t.Fatalf("invalid values replaced cache: (%q, %t)", got, ok)
		}
	}
}

func TestCacheReplacementHasFixedOneHourLifetime(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	cache.BindRequest("request", key)
	if _, stored, replaced := cache.StoreResponseForRequest("request", []string{state("a")}); !stored || replaced {
		t.Fatalf("first write stored=%t replaced=%t", stored, replaced)
	}

	now = now.Add(59 * time.Minute)
	if _, stored, replaced := cache.StoreResponseForRequest("request", []string{state("b")}); !stored || !replaced {
		t.Fatalf("replacement stored=%t replaced=%t", stored, replaced)
	}
	now = now.Add(2 * time.Minute)
	if got, ok := cache.Lookup(key); !ok || got != state("b") {
		t.Fatalf("replacement after old expiry = (%q, %t)", got, ok)
	}
	now = now.Add(58 * time.Minute)
	if got, ok := cache.Lookup(key); ok || got != "" {
		t.Fatalf("replacement at fixed expiry = (%q, %t)", got, ok)
	}
}

func TestCacheUsesLatestBindingAndRejectsUnboundResponses(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	first := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	last := CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"}
	cache.BindRequest("retry", first)
	cache.BindRequest("retry", last)

	if gotKey, stored, _ := cache.StoreResponseForRequest("retry", []string{state("s")}); !stored || gotKey != last {
		t.Fatalf("retry write = (%+v, %t)", gotKey, stored)
	}
	if _, ok := cache.Lookup(first); ok {
		t.Fatal("response was stored for the superseded binding")
	}
	if got, ok := cache.Lookup(last); !ok || got != state("s") {
		t.Fatalf("latest binding state = (%q, %t)", got, ok)
	}
	if _, stored, _ := cache.StoreResponseForRequest("unknown", []string{state("u")}); stored {
		t.Fatal("unbound response was stored")
	}

	late := CacheKey{AuthID: "auth-a", Model: "gpt-late"}
	cache.BindRequest("late", late)
	cache.ForgetRequest("late")
	if _, stored, _ := cache.StoreResponseForRequest("late", []string{state("l")}); stored {
		t.Fatal("response after completion was stored")
	}
}

func TestCacheAcceptsTeam332ByteStateByDefault(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-team", Model: "gpt-6-astra"}
	teamState := strings.Repeat("t", StateLengthTeam)
	cache.BindRequest("team-req", key)

	if gotKey, stored, replaced := cache.StoreResponseForRequest("team-req", []string{teamState}); !stored || replaced || gotKey != key {
		t.Fatalf("team write = (%+v, %t, %t)", gotKey, stored, replaced)
	}
	if got, ok := cache.Lookup(key); !ok || got != teamState {
		t.Fatalf("lookup team state = (%q, %t), want %q", got, ok, teamState)
	}
}

func TestCacheCustomAcceptedLengths(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now }, 312)
	key := CacheKey{AuthID: "auth-custom", Model: "gpt-custom"}
	cache.BindRequest("custom-req", key)

	s312 := strings.Repeat("c", 312)
	s292 := strings.Repeat("p", 292)

	if _, stored, _ := cache.StoreResponseForRequest("custom-req", []string{s292}); stored {
		t.Fatal("292 state was stored when only 312 accepted")
	}
	if gotKey, stored, _ := cache.StoreResponseForRequest("custom-req", []string{s312}); !stored || gotKey != key {
		t.Fatal("312 state was not stored")
	}
	if got, ok := cache.Lookup(key); !ok || got != s312 {
		t.Fatalf("lookup 312 state = (%q, %t)", got, ok)
	}
}

func TestCachePlanStrictPairing(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	s292 := strings.Repeat("p", StateLength)
	s332 := strings.Repeat("t", StateLengthTeam)

	tests := []struct {
		name       string
		plan       string
		state      string
		wantStored bool
	}{
		{"pro_accepts_292", "pro", s292, true},
		{"pro_rejects_332", "pro", s332, false},
		{"plus_accepts_292", "plus", s292, true},
		{"free_accepts_292", "free", s292, true},
		{"team_accepts_332", "team", s332, true},
		{"team_rejects_292", "team", s292, false},
		{"business_accepts_332", "business", s332, true},
		{"business_rejects_292", "business", s292, false},
		{"business_prolite_accepts_332", "self_serve_business_prolite", s332, true},
		{"business_prolite_rejects_292", "self_serve_business_prolite", s292, false},
		{"business_prolite_normalized", " SELF_SERVE_BUSINESS_PROLITE ", s332, true},
		{"empty_plan_accepts_292", "", s292, true},
		{"empty_plan_accepts_332", "", s332, true},
		{"empty_plan_rejects_other", "", strings.Repeat("x", 300), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewCache(10, 10, func() time.Time { return now })
			key := CacheKey{AuthID: "test-auth", Model: "gpt-model"}
			cache.BindRequest("req-1", key)

			_, stored, _ := cache.StoreResponseForRequest("req-1", []string{tt.state}, tt.plan)
			if stored != tt.wantStored {
				t.Fatalf("StoreResponseForRequest(plan=%q, len=%d) stored=%v, want %v", tt.plan, len(tt.state), stored, tt.wantStored)
			}
		})
	}
}
