package managed

import (
	"encoding/json"
	"testing"
)

// What Claude Code 2.1.278 writes when a response's rate-limit headers change, trimmed.
const rateLimitEvent = `{"status":"allowed_warning","resetsAt":1758607200,"rateLimitType":"five_hour","utilization":0.93,
 "unifiedWindows":{"five_hour":{"utilization":0.93,"resetsAt":1758607200},"seven_day":{"utilization":0.19,"resetsAt":1759126800},"seven_day_overage_included":{"utilization":35,"resetsAt":1759126800}},
 "isUsingOverage":false}`

func TestParseRateLimit(t *testing.T) {
	u := parseRateLimit(json.RawMessage(rateLimitEvent), 42)
	if u == nil {
		t.Fatal("nil")
	}
	if u.Status != "allowed_warning" || u.RateLimitType != "five_hour" || u.Utilization != 0.93 || u.At != 42 {
		t.Errorf("head: %+v", u)
	}
	if u.ResetsAt != 1758607200_000 {
		t.Errorf("seconds should become milliseconds, got %d", u.ResetsAt)
	}
	if len(u.Windows) != 3 || u.Windows["seven_day"].Utilization != 0.19 || u.Windows["seven_day"].ResetsAt != 1759126800_000 {
		t.Errorf("windows: %+v", u.Windows)
	}
	if u.Windows["seven_day_overage_included"].Utilization != 0.35 {
		t.Errorf("a percentage should become a fraction, got %v", u.Windows["seven_day_overage_included"].Utilization)
	}
	if parseRateLimit(json.RawMessage(`{"resetsAt":1}`), 1) != nil || parseRateLimit(nil, 1) != nil || parseRateLimit(json.RawMessage(`nope`), 1) != nil {
		t.Error("no status, no usage")
	}
	// Header-only events carry no unifiedWindows; the event's own window still counts.
	solo := parseRateLimit(json.RawMessage(`{"status":"rejected","rateLimitType":"seven_day","resetsAt":1759126800000,"utilization":1}`), 1)
	if solo == nil || solo.Windows["seven_day"].Utilization != 1 || solo.Windows["seven_day"].ResetsAt != 1759126800000 {
		t.Errorf("solo window: %+v", solo)
	}
}

func TestParseAccountInfo(t *testing.T) {
	a := parseAccountInfo(json.RawMessage(`{"account":{"email":" p@example.com ","organization":"Clubspark","subscriptionType":"team","tokenSource":"keychain"}}`))
	if a.Email != "p@example.com" || a.Organization != "Clubspark" || a.Plan != "team" {
		t.Errorf("%+v", a)
	}
	if parseAccountInfo(json.RawMessage(`{}`)) != (accountInfo{}) {
		t.Error("empty")
	}
}
