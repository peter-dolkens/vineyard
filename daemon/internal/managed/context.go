package managed

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// Context window. The transcript already carries the tokens each API call saw (input + cache read +
// cache creation), and that is the figure Claude Code's own /context uses, so the ring follows the
// transcript for free. Claude Code is asked (get_context_usage, a local computation in the child,
// no API call) only when the transcript cannot answer: once after the handshake for the window's
// real size, after a model switch (the window may change), and after a compaction, when the size
// drops without a new call to show it. One request in flight at a time; a harness that says the
// request is unsupported is not asked again.

// parseContextUsage trims a get_context_usage response to what the ring needs, stamped at now.
func parseContextUsage(body json.RawMessage, now int64) *model.ContextUsage {
	var v struct {
		TotalTokens float64 `json:"totalTokens"`
		MaxTokens   float64 `json:"maxTokens"`
		Percentage  float64 `json:"percentage"`
		Model       string  `json:"model"`
	}
	if len(body) == 0 || json.Unmarshal(body, &v) != nil || v.MaxTokens <= 0 || v.TotalTokens < 0 {
		return nil
	}
	pct := v.Percentage
	if pct <= 0 {
		pct = v.TotalTokens / v.MaxTokens * 100
	}
	return &model.ContextUsage{TotalTokens: int64(v.TotalTokens), MaxTokens: int64(v.MaxTokens), Percentage: pct, Model: v.Model, At: now}
}

// refreshContext asks the session how full its window is and records the answer. Safe to call from
// any goroutine; overlapping calls collapse into one.
func (m *Manager) refreshContext(p *proc) {
	m.mu.Lock()
	if p.ctxBusy || p.ctxUnsupported || p.info.Exited {
		m.mu.Unlock()
		return
	}
	p.ctxBusy = true
	m.mu.Unlock()
	body, err := m.control(p, map[string]any{"subtype": "get_context_usage", "detail": "summary"})
	m.mu.Lock()
	p.ctxBusy = false
	if err != nil {
		if strings.Contains(err.Error(), "not supported") {
			p.ctxUnsupported = true
		}
		m.mu.Unlock()
		m.log.Printf("managed: %s get_context_usage: %v", p.info.SessionID, err)
		return
	}
	u := parseContextUsage(body, time.Now().UnixMilli())
	if u != nil {
		p.info.Context = u
	}
	m.mu.Unlock()
	if u != nil {
		m.changed()
	}
}

// setCompacting records the start or end of a compaction as Claude Code's status lines report it.
// Returns true when something changed.
func (p *proc) setCompacting(on bool, now int64) bool {
	if p.info.Compacting == on {
		return false
	}
	p.info.Compacting = on
	if on {
		p.info.CompactingSince = now
	} else {
		p.info.CompactingSince = 0
	}
	return true
}
