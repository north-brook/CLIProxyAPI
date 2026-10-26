package cliproxy

import (
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeUsageHeaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	body := `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":61,"resets_at":"2027-02-01T00:00:00Z"},"seven_day_opus":{"utilization":81,"resets_at":"2027-02-01T00:00:00Z"},"seven_day_sonnet":null}`
	h, err := claudeUsageHeaders(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("Anthropic-Ratelimit-Unified-7d-Utilization") != "0.61" || h.Get("Anthropic-Ratelimit-Unified-7d-Opus-Utilization") != "0.81" {
		t.Fatalf("wrong percentages: %v", h)
	}
	if h.Get("Anthropic-Ratelimit-Unified-5h-Reset") == "" {
		t.Fatal("unused window needs a conservative initial reset")
	}
	for _, bad := range []string{`{}`, `{"five_hour":null}`, `{"five_hour":{"utilization":10},"seven_day":{"utilization":0}}`, `not-json`} {
		if _, err := claudeUsageHeaders(strings.NewReader(bad), now); err == nil {
			t.Fatalf("accepted invalid usage %q", bad)
		}
	}
}

func TestQuotaAwareRoutingConfigReload(t *testing.T) {
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "quota-aware", QuotaAware: internalconfig.QuotaAwareRoutingConfig{Reserves: []internalconfig.QuotaReserve{{Provider: "claude", Account: "personal@example.com", Percent: 20}}}}}
	state := normalizedRoutingRuntimeState(cfg)
	if _, ok := newRoutingSelector(state).(*coreauth.QuotaAwareSelector); !ok {
		t.Fatal("quota-aware strategy was not selected")
	}
	cfg.Routing.QuotaAware.Reserves[0].Percent = 30
	if normalizedRoutingRuntimeState(cfg) == state {
		t.Fatal("reserve change must rebuild routing policy")
	}
	service := &Service{cfg: cfg}
	invalid := cfg.CloneForRuntime()
	invalid.Routing.QuotaAware.Reserves[0].Percent = 101
	if service.commitConfigUpdate(invalid).cfg != nil || service.currentConfig() != cfg {
		t.Fatal("invalid reserve replaced current configuration")
	}
}
