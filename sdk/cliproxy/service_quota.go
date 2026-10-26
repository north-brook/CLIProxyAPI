package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type claudeUsageWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// claudeUsageHeaders converts the usage endpoint's percentages to the same
// fractional header format already observed on normal Claude responses.
func claudeUsageHeaders(body io.Reader, observedAt time.Time) (http.Header, error) {
	var usage map[string]json.RawMessage
	if errDecode := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&usage); errDecode != nil {
		return nil, fmt.Errorf("decode Claude usage: %w", errDecode)
	}
	headers := http.Header{}
	for name, window := range map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_opus": "7d-opus", "seven_day_sonnet": "7d-sonnet"} {
		var w *claudeUsageWindow
		if raw := usage[name]; len(raw) > 0 {
			if errWindow := json.Unmarshal(raw, &w); errWindow != nil {
				return nil, fmt.Errorf("decode Claude quota window %s: %w", name, errWindow)
			}
		}
		if w == nil || w.Utilization == nil {
			continue
		}
		used := *w.Utilization
		if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
			return nil, fmt.Errorf("invalid Claude usage percentage")
		}
		reset := w.ResetsAt
		if reset == nil {
			// An unused window has no reset until its first request starts it.
			if used != 0 {
				return nil, fmt.Errorf("used Claude window has no reset")
			}
			duration := 7 * 24 * time.Hour
			if name == "five_hour" {
				duration = 5 * time.Hour
			}
			future := observedAt.Add(duration)
			reset = &future
		}
		prefix := "Anthropic-Ratelimit-Unified-" + window
		headers.Set(prefix+"-Utilization", strconv.FormatFloat(used/100, 'f', -1, 64))
		headers.Set(prefix+"-Reset", strconv.FormatInt(reset.Unix(), 10))
	}
	if headers.Get("Anthropic-Ratelimit-Unified-5h-Utilization") == "" || headers.Get("Anthropic-Ratelimit-Unified-7d-Utilization") == "" {
		return nil, fmt.Errorf("Claude usage is missing shared quota windows")
	}
	return headers, nil
}

func (s *Service) refreshClaudeQuota(ctx context.Context, auth *coreauth.Auth) (time.Duration, error) {
	startedAt := time.Now()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if errRequest != nil {
		return 0, errRequest
	}
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, errHTTP := s.coreManager.HttpRequest(ctx, auth, req)
	if errHTTP != nil {
		return 0, fmt.Errorf("Claude quota request failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("failed to close quota response body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		backoff := time.Duration(0)
		if resp.StatusCode == http.StatusTooManyRequests {
			backoff = 15 * time.Minute
			if seconds, errParse := strconv.Atoi(resp.Header.Get("Retry-After")); errParse == nil && seconds > 0 {
				backoff = max(backoff, time.Duration(seconds)*time.Second)
			}
		}
		return backoff, fmt.Errorf("Claude quota request returned HTTP %d", resp.StatusCode)
	}
	headers, errParse := claudeUsageHeaders(resp.Body, startedAt)
	if errParse != nil {
		return 0, errParse
	}
	s.coreManager.ObserveQuotaHeaders(auth, headers, startedAt)
	log.WithField("auth_id", auth.ID).Debug("refreshed Claude quota routing snapshot")
	return 0, nil
}

// runQuotaRefresh is independent of request selection. Existing response headers
// continue updating active accounts between these low-frequency idle probes.
// The service context cancels network requests during shutdown.
func (s *Service) runQuotaRefresh(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	next := map[string]time.Time{}
	for {
		cfg := s.currentConfig()
		if cfg != nil && !cfg.Home.Enabled && strings.EqualFold(strings.TrimSpace(cfg.Routing.Strategy), "quota-aware") && s.coreManager != nil {
			for _, auth := range s.coreManager.List() {
				if ctx.Err() != nil {
					return
				}
				if auth.Provider != "claude" || auth.Disabled || auth.AuthKind() == coreauth.AuthKindAPIKey || time.Now().Before(next[auth.ID]) {
					continue
				}
				backoff, errRefresh := s.refreshClaudeQuota(ctx, auth)
				if errRefresh != nil && ctx.Err() == nil {
					log.WithField("auth_id", auth.ID).WithError(errRefresh).Warn("quota refresh failed; reserved accounts require a fresh snapshot")
				}
				next[auth.ID] = time.Now().Add(max(cfg.Routing.QuotaAware.RefreshDuration(), backoff))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
