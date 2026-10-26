package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// QuotaAwareSelector spends Claude OAuth allowance according to the tightest
// remaining budget per second until reset. Other providers keep round-robin.
// It performs no network I/O on the request path.
type QuotaAwareSelector struct {
	policy     config.QuotaAwareRoutingConfig
	roundRobin RoundRobinSelector
}

func NewQuotaAwareSelector(policy config.QuotaAwareRoutingConfig) *QuotaAwareSelector {
	policy.Reserves = append([]config.QuotaReserve(nil), policy.Reserves...)
	return &QuotaAwareSelector{policy: policy}
}

func quotaSelector(selector Selector) *QuotaAwareSelector {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	q, _ := selector.(*QuotaAwareSelector)
	return q
}

func routingFallbackCandidates(fallback Selector, available []*Auth) []*Auth {
	if quotaSelector(fallback) != nil {
		return available
	}
	return highestPriorityAuths(available)
}

func (q *QuotaAwareSelector) reserve(auth *Auth) float64 {
	if auth == nil || auth.Provider != "claude" {
		return 0
	}
	email, _ := auth.Metadata["email"].(string)
	for _, reserve := range q.policy.Reserves {
		if reserve.Provider == auth.Provider && strings.EqualFold(strings.TrimSpace(reserve.Account), strings.TrimSpace(email)) {
			return reserve.Percent
		}
	}
	return 0
}

type quotaBudget struct {
	score          float64
	known, blocked bool
}

func (q *QuotaAwareSelector) budget(auth *Auth, model string, now time.Time) quotaBudget {
	if auth == nil || auth.Provider != "claude" || auth.AuthKind() == AuthKindAPIKey {
		return quotaBudget{}
	}
	reserve := q.reserve(auth)
	if reserve >= 100 {
		return quotaBudget{blocked: true}
	}
	if auth.Quota.ObservedAt.IsZero() || now.Before(auth.Quota.ObservedAt) || now.Sub(auth.Quota.ObservedAt) > q.policy.MaxAgeDuration() {
		return quotaBudget{blocked: reserve > 0}
	}
	windows := []string{"5h", "7d"}
	lowerModel := strings.ToLower(model)
	for _, family := range []string{"opus", "sonnet"} {
		window := "7d-" + family
		if strings.Contains(lowerModel, family) && auth.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-"+window+"-Utilization")] != "" {
			windows = append(windows, window)
		}
	}
	score := math.Inf(1)
	complete := true
	for _, window := range windows {
		prefix := "Anthropic-Ratelimit-Unified-" + window
		used, errUsed := strconv.ParseFloat(auth.Quota.Signals[http.CanonicalHeaderKey(prefix+"-Utilization")], 64)
		reset, errReset := strconv.ParseInt(auth.Quota.Signals[http.CanonicalHeaderKey(prefix+"-Reset")], 10, 64)
		if errUsed != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || errReset != nil || reset <= now.Unix() {
			complete = false
			continue
		}
		remaining := 100*(1-used) - reserve
		if remaining <= 0 {
			return quotaBudget{known: true, blocked: true}
		}
		rate := remaining / math.Max(1, time.Unix(reset, 0).Sub(now).Seconds())
		score = math.Min(score, rate)
	}
	if !complete {
		return quotaBudget{blocked: reserve > 0}
	}
	return quotaBudget{score: score, known: true}
}

func (q *QuotaAwareSelector) eligible(auths []*Auth, model string, now time.Time) ([]*Auth, error) {
	eligible := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth != nil && !q.budget(auth, model, now).blocked {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil, &Error{Code: "quota_reserve", Message: "no account has spendable quota; reserved or unverified allowance is protected", HTTPStatus: http.StatusServiceUnavailable}
	}
	return eligible, nil
}

func (q *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuthsAcrossPriorities(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available, err = q.eligible(available, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	bestScore := -1.0
	var best []*Auth
	for _, auth := range available {
		budget := q.budget(auth, model, now)
		if !budget.known {
			continue
		}
		if budget.score > bestScore {
			bestScore = budget.score
			best = []*Auth{auth}
		} else if budget.score == bestScore {
			best = append(best, auth)
		}
	}
	if len(best) == 0 {
		best = available
	}
	return q.roundRobin.Pick(ctx, provider, model, opts, highestPriorityAuths(best))
}

// ObserveQuotaHeaders seeds the existing passive snapshot from an authenticated
// usage probe. It cannot overwrite a newer response or a replaced credential.
func (m *Manager) ObserveQuotaHeaders(snapshot *Auth, headers http.Header, observedAt time.Time) bool {
	if m == nil || snapshot == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.auths[snapshot.ID]
	if current == nil || current.Disabled || current.RegistrationEpoch != snapshot.RegistrationEpoch || current.CredentialVersion != snapshot.CredentialVersion || current.Quota.ObservedAt.After(observedAt) {
		return false
	}
	return current.Quota.ObserveResponseHeadersForProvider(current.Provider, headers, observedAt)
}
