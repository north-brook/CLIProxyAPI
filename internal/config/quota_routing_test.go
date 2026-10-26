package config

import (
	"math"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestQuotaRoutingValidationAndRoundTrip(t *testing.T) {
	good := &Config{Routing: RoutingConfig{Strategy: "quota-aware", QuotaAware: QuotaAwareRoutingConfig{Reserves: []QuotaReserve{{Provider: "claude", Account: "personal@example.com", Percent: 20}}}}}
	if err := good.ValidateQuotaRouting(); err != nil {
		t.Fatal(err)
	}
	if good.Routing.QuotaAware.RefreshDuration() != 5*time.Minute || good.Routing.QuotaAware.MaxAgeDuration() != 15*time.Minute {
		t.Fatal("unexpected quota observation defaults")
	}
	raw, err := yaml.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err = yaml.Unmarshal(raw, &restored); err != nil || restored.Routing.QuotaAware.Reserves[0].Percent != 20 {
		t.Fatalf("reserve lost on config roundtrip: %v", err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Routing.Strategy = "round-robin" },
		func(c *Config) { c.Home.Enabled = true },
		func(c *Config) { c.Routing.QuotaAware.Reserves[0].Percent = -1 },
		func(c *Config) { c.Routing.QuotaAware.Reserves[0].Percent = 101 },
		func(c *Config) { c.Routing.QuotaAware.Reserves[0].Percent = math.NaN() },
		func(c *Config) { c.Routing.QuotaAware.Reserves[0].Provider = "codex" },
		func(c *Config) { c.Routing.QuotaAware.Reserves[0].Account = "" },
		func(c *Config) { c.Routing.QuotaAware.RefreshInterval = "invalid" },
		func(c *Config) { c.Routing.QuotaAware.MaxAge = "1m" },
		func(c *Config) {
			c.Routing.QuotaAware.Reserves = append(c.Routing.QuotaAware.Reserves, c.Routing.QuotaAware.Reserves[0])
		},
	} {
		bad := good.CloneForRuntime()
		change(bad)
		if bad.ValidateQuotaRouting() == nil {
			t.Fatalf("invalid quota config accepted: %+v", bad.Routing.QuotaAware)
		}
	}
	if good.Routing.QuotaAware.Reserves[0].Percent != 20 {
		t.Fatal("runtime clone shared reserve configuration")
	}
}
