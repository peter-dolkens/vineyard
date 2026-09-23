package managed

import (
	"encoding/json"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Usage limits. Claude Code writes a rate_limit_event line to stdout whenever the account's limit
// info changes (it reads it from the API's response headers): a status, the window it refers to, and
// a unifiedWindows block with every window's utilization and reset time. Limits belong to the
// account, not the session, so one managed session tells the machine what all its sessions face.

// epochMillis accepts seconds or milliseconds (Claude Code passes the header's seconds through).
func epochMillis(v float64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 1e11 {
		return int64(v * 1000)
	}
	return int64(v)
}

// fraction accepts 0..1 or a percentage.
func fraction(v float64) float64 {
	if v > 1 {
		return v / 100
	}
	return v
}

// parseRateLimit turns a rate_limit_event's rate_limit_info into a Usage stamped at now.
func parseRateLimit(raw json.RawMessage, now int64) *model.Usage {
	var v struct {
		Status         string  `json:"status"`
		RateLimitType  string  `json:"rateLimitType"`
		Utilization    float64 `json:"utilization"`
		ResetsAt       float64 `json:"resetsAt"`
		IsUsingOverage bool    `json:"isUsingOverage"`
		OverageStatus  string  `json:"overageStatus"`
		UnifiedWindows map[string]struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    float64 `json:"resetsAt"`
		} `json:"unifiedWindows"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v.Status == "" {
		return nil
	}
	u := &model.Usage{
		Status: v.Status, RateLimitType: v.RateLimitType, Utilization: fraction(v.Utilization), ResetsAt: epochMillis(v.ResetsAt),
		IsUsingOverage: v.IsUsingOverage, OverageStatus: v.OverageStatus, At: now,
	}
	for k, w := range v.UnifiedWindows {
		if u.Windows == nil {
			u.Windows = map[string]model.UsageWindow{}
		}
		u.Windows[k] = model.UsageWindow{Utilization: fraction(w.Utilization), ResetsAt: epochMillis(w.ResetsAt)}
	}
	// The event's own window is a window too; make sure it is in the map.
	if u.RateLimitType != "" && u.RateLimitType != "overage" {
		if _, ok := u.Windows[u.RateLimitType]; !ok && (u.Utilization > 0 || u.ResetsAt > 0) {
			if u.Windows == nil {
				u.Windows = map[string]model.UsageWindow{}
			}
			u.Windows[u.RateLimitType] = model.UsageWindow{Utilization: u.Utilization, ResetsAt: u.ResetsAt}
		}
	}
	return u
}

// LatestUsage is the newest limit report from any session this daemon manages, or nil.
func (m *Manager) LatestUsage() *model.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *model.Usage
	for _, p := range m.procs {
		if p.info.Usage != nil && (best == nil || p.info.Usage.At > best.At) {
			best = p.info.Usage
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

// accountInfo is what the initialize handshake says about who the session is signed in as.
type accountInfo struct {
	Email        string
	Organization string
	Plan         string
}

// parseAccountInfo reads the account block of an initialize response.
func parseAccountInfo(body json.RawMessage) accountInfo {
	var v struct {
		Account struct {
			Email            string `json:"email"`
			Organization     string `json:"organization"`
			SubscriptionType string `json:"subscriptionType"`
		} `json:"account"`
	}
	if json.Unmarshal(body, &v) != nil {
		return accountInfo{}
	}
	return accountInfo{Email: strings.TrimSpace(v.Account.Email), Organization: strings.TrimSpace(v.Account.Organization), Plan: strings.TrimSpace(v.Account.SubscriptionType)}
}
