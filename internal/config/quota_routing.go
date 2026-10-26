package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// QuotaAwareRoutingConfig applies only when routing.strategy is quota-aware.
type QuotaAwareRoutingConfig struct {
	RefreshInterval string         `yaml:"refresh-interval,omitempty" json:"refresh-interval,omitempty"`
	MaxAge          string         `yaml:"max-age,omitempty" json:"max-age,omitempty"`
	Reserves        []QuotaReserve `yaml:"reserves,omitempty" json:"reserves,omitempty"`
}

// QuotaReserve identifies an OAuth account by provider and email, not filename.
type QuotaReserve struct {
	Provider string  `yaml:"provider" json:"provider"`
	Account  string  `yaml:"account" json:"account"`
	Percent  float64 `yaml:"percent" json:"percent"`
}

func (q QuotaAwareRoutingConfig) RefreshDuration() time.Duration {
	if d, err := time.ParseDuration(q.RefreshInterval); err == nil && d > 0 {
		return d
	}
	return 5 * time.Minute
}

func (q QuotaAwareRoutingConfig) MaxAgeDuration() time.Duration {
	if d, err := time.ParseDuration(q.MaxAge); err == nil && d > 0 {
		return d
	}
	return 15 * time.Minute
}

// ValidateQuotaRouting rejects typos and invalid reserves rather than silently
// disabling account protection. Other providers retain their existing routing.
func (cfg *Config) ValidateQuotaRouting() error {
	if cfg == nil {
		return nil
	}
	q := cfg.Routing.QuotaAware
	quotaAware := strings.EqualFold(strings.TrimSpace(cfg.Routing.Strategy), "quota-aware")
	if len(q.Reserves) > 0 && !quotaAware {
		return fmt.Errorf("quota reserves require routing.strategy: quota-aware")
	}
	if quotaAware && cfg.Home.Enabled {
		return fmt.Errorf("quota-aware routing is supported only in standalone mode, not Home dispatch")
	}
	for name, value := range map[string]string{"refresh-interval": q.RefreshInterval, "max-age": q.MaxAge} {
		if value == "" {
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil || d < time.Second {
			return fmt.Errorf("routing.quota-aware.%s must be a duration of at least 1s", name)
		}
	}
	if q.MaxAgeDuration() < q.RefreshDuration() {
		return fmt.Errorf("routing.quota-aware.max-age must be at least refresh-interval")
	}
	seen := map[string]bool{}
	for _, r := range q.Reserves {
		if r.Provider != "claude" || strings.TrimSpace(r.Account) == "" {
			return fmt.Errorf("quota reserves require provider claude and a nonempty account email")
		}
		if math.IsNaN(r.Percent) || math.IsInf(r.Percent, 0) || r.Percent < 0 || r.Percent > 100 {
			return fmt.Errorf("quota reserve percent must be between 0 and 100")
		}
		key := strings.ToLower(strings.TrimSpace(r.Account))
		if seen[key] {
			return fmt.Errorf("duplicate quota reserve for account %q", r.Account)
		}
		seen[key] = true
	}
	return nil
}
