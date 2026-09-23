package managed

import (
	"encoding/json"
	"testing"
)

// What Claude Code 2.1.280 answers a get_context_usage control request with, trimmed to the top level.
const contextUsageBody = `{"categories":[{"name":"System prompt","tokens":3100,"color":"blue"}],"totalTokens":48213,"maxTokens":200000,"rawMaxTokens":200000,
 "autocompactSource":"model-default","percentage":24,"gridRows":[],"model":"claude-fable-5-1","memoryFiles":[],"mcpTools":[]}`

func TestParseContextUsage(t *testing.T) {
	u := parseContextUsage(json.RawMessage(contextUsageBody), 42)
	if u == nil {
		t.Fatal("nil")
	}
	if u.TotalTokens != 48213 || u.MaxTokens != 200000 || u.Percentage != 24 || u.Model != "claude-fable-5-1" || u.At != 42 {
		t.Errorf("got %+v", u)
	}
	// No percentage field: derive it.
	d := parseContextUsage(json.RawMessage(`{"totalTokens":500000,"maxTokens":1000000}`), 1)
	if d == nil || d.Percentage != 50 {
		t.Errorf("derived percentage: %+v", d)
	}
	for _, bad := range []string{``, `nope`, `{"totalTokens":1}`, `{"totalTokens":1,"maxTokens":0}`} {
		if parseContextUsage(json.RawMessage(bad), 1) != nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestSetCompacting(t *testing.T) {
	p := &proc{}
	if !p.setCompacting(true, 10) || !p.info.Compacting || p.info.CompactingSince != 10 {
		t.Errorf("start: %+v", p.info)
	}
	if p.setCompacting(true, 20) || p.info.CompactingSince != 10 {
		t.Error("a repeated compacting status (Claude Code re-sends it every 30 s) must not restart the clock")
	}
	if !p.setCompacting(false, 30) || p.info.Compacting || p.info.CompactingSince != 0 {
		t.Errorf("end: %+v", p.info)
	}
	if p.setCompacting(false, 40) {
		t.Error("ending twice is not a change")
	}
}
