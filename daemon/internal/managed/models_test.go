package managed

import (
	"encoding/json"
	"testing"
)

// A trimmed copy of what Claude Code 2.1.280 answers to an initialize control_request.
const initResponse = `{"commands":[],"models":[
 {"value":"default","resolvedModel":"claude-opus-5-5[1m]","displayName":"Default (recommended)","description":"Opus 5.5 with 1M context","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"],"supportsFastMode":true},
 {"value":"opus[1m]","resolvedModel":"claude-opus-5-5[1m]","displayName":"Opus (1M context)","description":"Opus 5.5 with 1M context","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},
 {"value":"sonnet","resolvedModel":"claude-sonnet-5","displayName":"Sonnet","description":"Sonnet 5","supportsEffort":true,"supportedEffortLevels":["low","medium","high"],"disabled":true},
 {"value":"haiku","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku","description":"Haiku 4.5"}
],"unavailable_models":[],"account":{"email":"x@y"}}`

func TestParseModels(t *testing.T) {
	got := parseModels(json.RawMessage(initResponse))
	if len(got) != 3 {
		t.Fatalf("want 3 rows (disabled dropped), got %d: %+v", len(got), got)
	}
	if got[0].Value != "default" || got[0].ResolvedModel != "claude-opus-5-5[1m]" || got[0].DisplayName != "Default (recommended)" {
		t.Errorf("first row: %+v", got[0])
	}
	if len(got[0].SupportedEffortLevels) != 5 || got[0].SupportedEffortLevels[4] != "max" {
		t.Errorf("effort levels: %v", got[0].SupportedEffortLevels)
	}
	if got[1].Value != "opus[1m]" || got[2].Value != "haiku" {
		t.Errorf("order/filter: %s, %s", got[1].Value, got[2].Value)
	}
	if got[2].SupportedEffortLevels != nil {
		t.Errorf("haiku should take no effort setting, got %v", got[2].SupportedEffortLevels)
	}
	if out, _ := json.Marshal(got[2]); string(out) != `{"value":"haiku","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku","description":"Haiku 4.5"}` {
		t.Errorf("wire shape: %s", out)
	}
}

func TestParseModelsGarbage(t *testing.T) {
	if got := parseModels(json.RawMessage(`not json`)); got != nil {
		t.Errorf("want nil, got %v", got)
	}
	if got := parseModels(json.RawMessage(`{"models":[]}`)); len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}
