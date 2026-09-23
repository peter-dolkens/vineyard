package managed

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// The request body of an elicitation control_request as Claude Code 2.1.280 relays it from an MCP
// server (the envelope's "request" object).
const elicitationLine = `{"type":"control_request","request_id":"req-7","request":{"subtype":"elicitation","mcp_server_name":"github",
 "display_name":"GitHub","message":"Which repository should the issue go to?","mode":"form","elicitation_id":"el-1","title":"Pick a repository",
 "requested_schema":{"type":"object","properties":{"repo":{"type":"string","title":"Repository"},"draft":{"type":"boolean"}},"required":["repo"]}}}`

func decodeRequest(t *testing.T, line string) (string, controlRequest) {
	t.Helper()
	var env struct {
		RequestID string         `json:"request_id"`
		Request   controlRequest `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatal(err)
	}
	return env.RequestID, env.Request
}

func TestParseControlRequestElicitation(t *testing.T) {
	rid, req := decodeRequest(t, elicitationLine)
	pr := parseControlRequest(rid, req, 99)
	if pr == nil {
		t.Fatal("an elicitation must become a pending request, not be auto-acknowledged")
	}
	if pr.RequestID != "req-7" || pr.Kind != model.PendingElicitation || pr.At != 99 || pr.Elicitation == nil {
		t.Fatalf("got %+v", pr)
	}
	e := pr.Elicitation
	if e.ServerName != "github" || e.DisplayName != "GitHub" || e.Mode != "form" || e.ElicitationID != "el-1" || e.Title != "Pick a repository" ||
		e.Message != "Which repository should the issue go to?" {
		t.Errorf("fields: %+v", e)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if json.Unmarshal(e.RequestedSchema, &schema) != nil || len(schema.Required) != 1 || schema.Required[0] != "repo" {
		t.Errorf("requested_schema not carried through: %s", e.RequestedSchema)
	}
	// The wire form the webview sees.
	b, _ := json.Marshal(pr)
	for _, want := range []string{`"kind":"elicitation"`, `"requestId":"req-7"`, `"serverName":"github"`, `"requestedSchema":{`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
	if got := elicitationSummary(e); got != "GitHub asks: Pick a repository" {
		t.Errorf("summary %q", got)
	}
}

func TestParseControlRequestURLMode(t *testing.T) {
	_, req := decodeRequest(t, `{"type":"control_request","request_id":"r","request":{"subtype":"elicitation","mcp_server_name":"linear","url":"https://example.com/auth","message":"Sign in"}}`)
	pr := parseControlRequest("r", req, 1)
	if pr == nil || pr.Elicitation == nil || pr.Elicitation.Mode != "url" || pr.Elicitation.URL != "https://example.com/auth" {
		t.Fatalf("a url without a mode is url mode: %+v", pr)
	}
	if got := elicitationSummary(pr.Elicitation); got != "linear asks: Sign in" {
		t.Errorf("summary %q", got)
	}
	long := &model.ElicitationRequest{ServerName: "s", Message: strings.Repeat("word ", 40)}
	if got := elicitationSummary(long); len([]rune(got)) > 90 || !strings.HasSuffix(got, "…") {
		t.Errorf("long message not truncated: %q", got)
	}
}

func TestParseControlRequestOthers(t *testing.T) {
	_, req := decodeRequest(t, `{"type":"control_request","request_id":"p","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tu"}}`)
	pr := parseControlRequest("p", req, 5)
	if pr == nil || pr.Kind != "" || pr.ToolName != "Bash" || pr.ToolUseID != "tu" || pr.Elicitation != nil {
		t.Fatalf("permission prompt changed shape: %+v", pr)
	}
	for _, sub := range []string{"hook_callback", "mcp_message", "something_new"} {
		if parseControlRequest("x", controlRequest{Subtype: sub}, 1) != nil {
			t.Errorf("%s should be acknowledged, not shown", sub)
		}
	}
}
