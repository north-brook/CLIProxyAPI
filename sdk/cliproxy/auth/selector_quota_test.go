package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func quotaTestAuth(id string, now time.Time, used float64, weeklyReset time.Duration) *Auth {
	h := http.Header{}
	for window, reset := range map[string]time.Duration{"5h": 5 * time.Hour, "7d": weeklyReset} {
		h.Set("Anthropic-Ratelimit-Unified-"+window+"-Utilization", strconv.FormatFloat(used, 'f', -1, 64))
		h.Set("Anthropic-Ratelimit-Unified-"+window+"-Reset", strconv.FormatInt(now.Add(reset).Unix(), 10))
	}
	a := &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"priority": "0"}, Metadata: map[string]any{"email": id + "@example.com", "access_token": "test-token"}}
	a.Quota.ObserveResponseHeadersForProvider("claude", h, now)
	return a
}

func quotaTestSelector() *QuotaAwareSelector {
	return NewQuotaAwareSelector(config.QuotaAwareRoutingConfig{Reserves: []config.QuotaReserve{{Provider: "claude", Account: "personal@example.com", Percent: 20}}})
}

func TestQuotaAwareBudget(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name           string
		change         func(*Auth)
		known, blocked bool
	}{
		{name: "healthy", known: true},
		{name: "five hour reserve", change: func(a *Auth) { a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = "0.81" }, known: true, blocked: true},
		{name: "weekly reserve boundary", change: func(a *Auth) { a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.8" }, known: true, blocked: true},
		{name: "stale", change: func(a *Auth) { a.Quota.ObservedAt = now.Add(-16 * time.Minute) }, blocked: true},
		{name: "missing weekly", change: func(a *Auth) { delete(a.Quota.Signals, "Anthropic-Ratelimit-Unified-7d-Utilization") }, blocked: true},
		{name: "invalid", change: func(a *Auth) { a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "NaN" }, blocked: true},
		{name: "expired reset", change: func(a *Auth) {
			a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Unix(), 10)
		}, blocked: true},
		{name: "opus reserve", change: func(a *Auth) {
			a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Opus-Utilization"] = "0.81"
			a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Opus-Reset"] = strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
		}, known: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := quotaTestAuth("personal", now, 0.5, 3*24*time.Hour)
			if tc.change != nil {
				tc.change(a)
			}
			got := quotaTestSelector().budget(a, "claude-opus", now)
			if got.known != tc.known || got.blocked != tc.blocked {
				t.Fatalf("budget = %+v", got)
			}
		})
	}
	unknown := quotaTestAuth("work", now, 0.5, time.Hour)
	unknown.Quota = QuotaState{}
	if quotaTestSelector().budget(unknown, "claude-opus", now).blocked {
		t.Fatal("unprotected unknown account must remain available")
	}
}

func TestQuotaAwareRanksAcrossPriorityTiers(t *testing.T) {
	now := time.Now().Add(-time.Second)
	later := quotaTestAuth("later", now, 0.5, 6*24*time.Hour)
	later.Attributes["priority"] = "10"
	soon := quotaTestAuth("soon", now, 0.5, 24*time.Hour)
	q := quotaTestSelector()
	for _, affinity := range []bool{false, true} {
		t.Run(fmt.Sprint(affinity), func(t *testing.T) {
			var selector Selector = q
			if affinity {
				s := NewSessionAffinitySelector(q)
				defer s.Stop()
				selector = s
			}
			m := NewManager(nil, selector, nil)
			m.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			for _, a := range []*Auth{later, soon} {
				a = a.Clone()
				a.ID += fmt.Sprint(affinity)
				registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-test"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			if m.useSchedulerFastPath() {
				t.Fatal("quota routing must not use static priority fast path")
			}
			selected, _, err := m.pickNext(context.Background(), "claude", "claude-opus-test", cliproxyexecutor.Options{}, nil)
			if err != nil || selected.ID != "soon"+fmt.Sprint(affinity) {
				t.Fatalf("picked %v, %v", selected, err)
			}
			stored, _ := m.GetByID(selected.ID)
			if stored.Attributes["priority"] != "0" {
				t.Fatal("selection must not rewrite credential priorities")
			}
		})
	}
}

func TestQuotaAwareReserveOverridesAffinity(t *testing.T) {
	for _, mode := range []string{"explicit", "lcp"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().Add(-time.Second)
			personal := quotaTestAuth("personal", now, 0.1, time.Hour)
			work := quotaTestAuth("work", now, 0.5, 5*24*time.Hour)
			s := NewSessionAffinitySelector(quotaTestSelector())
			defer s.Stop()
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"reserve-session"}}}
			if mode == "lcp" {
				opts = cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"}]}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "reserve-caller"}}
			}
			auths := []*Auth{personal, work}
			first, err := s.Pick(context.Background(), "claude", "claude-opus", opts, auths)
			if err != nil || first.ID != "personal" {
				t.Fatalf("first pick %v, %v", first, err)
			}
			personal.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.82"
			second, err := s.Pick(context.Background(), "claude", "claude-opus", opts, auths)
			if err != nil || second.ID != "work" {
				t.Fatalf("reserve did not override affinity: %v, %v", second, err)
			}
			if _, err = s.Pick(context.Background(), "claude", "claude-opus", opts, []*Auth{personal}); err == nil {
				t.Fatal("must fail rather than spend reserved allowance")
			}
		})
	}
}

func TestQuotaAwareKeepsHealthyAffinityAndOtherProviderPriority(t *testing.T) {
	now := time.Now().Add(-time.Second)
	q := quotaTestSelector()
	s := NewSessionAffinitySelector(q)
	defer s.Stop()
	a, b := quotaTestAuth("a", now, 0.5, time.Hour), quotaTestAuth("b", now, 0.5, 24*time.Hour)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"sticky"}}}
	first, err := s.Pick(context.Background(), "claude", "model", opts, []*Auth{a, b})
	if err != nil {
		t.Fatal(err)
	}
	b.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10)
	again, err := s.Pick(context.Background(), "claude", "model", opts, []*Auth{a, b})
	if err != nil || again.ID != first.ID {
		t.Fatal("healthy session must stay bound")
	}
	codex := []*Auth{{ID: "low", Provider: "codex", Attributes: map[string]string{"priority": "0"}}, {ID: "high", Provider: "codex", Attributes: map[string]string{"priority": "10"}}}
	picked, err := q.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, codex)
	if err != nil || picked.ID != "high" {
		t.Fatalf("other provider priority changed: %v %v", picked, err)
	}
}

func TestQuotaProbeCannotOverwriteNewerOrReplacedCredential(t *testing.T) {
	now := time.Now()
	m := NewManager(nil, quotaTestSelector(), nil)
	a, err := m.Register(context.Background(), quotaTestAuth("personal", now, 0.1, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.9"}}
	if m.ObserveQuotaHeaders(a, h, now.Add(-time.Second)) {
		t.Fatal("old probe replaced a newer response")
	}
	stale := a.Clone()
	stale.CredentialVersion++
	if m.ObserveQuotaHeaders(stale, h, now.Add(time.Second)) {
		t.Fatal("probe from another credential was accepted")
	}
	if !m.ObserveQuotaHeaders(a, h, now.Add(time.Second)) {
		t.Fatal("fresh matching probe was rejected")
	}
}

func TestQuotaAwarePluginDelegateCannotBypassReserves(t *testing.T) {
	now := time.Now().Add(-time.Second)
	q := quotaTestSelector()
	m := NewManager(nil, q, nil)
	personal := quotaTestAuth("personal", now, 0.9, time.Hour)
	work := quotaTestAuth("work", now, 0.2, time.Hour)
	plugin := &fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}}
	selected, handled, err := m.pickViaPluginScheduler(context.Background(), plugin, "claude", nil, "model", cliproxyexecutor.Options{}, nil, []*Auth{work})
	if err != nil || handled || selected != nil {
		t.Fatalf("plugin delegate must return to quota selector: %v, %v, %v", selected, handled, err)
	}
	if _, err := q.eligible([]*Auth{personal}, "model", now); err == nil {
		t.Fatal("reserved account was eligible")
	}
}

func TestQuotaAwarePreservesCodexPriorityBeforeWebsocketPreference(t *testing.T) {
	q := quotaTestSelector()
	auths := []*Auth{
		{ID: "low-ws", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
		{ID: "high-http", Provider: "codex", Attributes: map[string]string{"priority": "10", "websockets": "false"}},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	selected, err := q.Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, auths)
	if err != nil || selected.ID != "high-http" {
		t.Fatalf("WebSocket preference bypassed priority: %v %v", selected, err)
	}
}
