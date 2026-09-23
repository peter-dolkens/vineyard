package managed

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

const initWithCommands = `{"commands":[
 {"name":"compact","description":"Clear conversation history but keep a summary in context","argumentHint":"<optional summary instructions>"},
 {"name":"/clear","description":"Clear conversation history and free up context"},
 {"name":"  ","description":"nameless"},
 {"name":"code-review","description":"Review the current diff","argumentHint":""}
],"models":[],"account":{"email":" peter@example.com "}}`

func TestParseCommands(t *testing.T) {
	got := parseCommands(json.RawMessage(initWithCommands))
	if len(got) != 3 {
		t.Fatalf("want 3 commands (nameless dropped), got %d: %+v", len(got), got)
	}
	if got[0].Name != "compact" || got[0].ArgumentHint != "<optional summary instructions>" {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Name != "clear" {
		t.Errorf("leading slash should be stripped, got %q", got[1].Name)
	}
	if out, _ := json.Marshal(got[2]); string(out) != `{"name":"code-review","description":"Review the current diff"}` {
		t.Errorf("wire shape: %s", out)
	}
	if parseCommands(json.RawMessage(`nope`)) != nil {
		t.Error("garbage should give nil")
	}
}

func TestParseAccount(t *testing.T) {
	if got := parseAccount(json.RawMessage(initWithCommands)); got != "peter@example.com" {
		t.Errorf("account: %q", got)
	}
	if got := parseAccount(json.RawMessage(`{"models":[]}`)); got != "" {
		t.Errorf("no account: %q", got)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestContentBlocks(t *testing.T) {
	atts := []model.Attachment{
		{Name: "shot.png", MediaType: "image/png", Data: b64("PNG")},
		{Name: "notes.md", MediaType: "text/markdown", Data: b64("# hi\n\nthere\n")},
	}
	blocks, err := ContentBlocks("look at these", atts)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 {
		t.Fatalf("want image, text, prompt; got %d blocks", len(blocks))
	}
	if blocks[0]["type"] != "image" {
		t.Errorf("first block should be the image, got %v", blocks[0])
	}
	src := blocks[0]["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != b64("PNG") || src["type"] != "base64" {
		t.Errorf("image source: %v", src)
	}
	if txt := blocks[1]["text"].(string); !strings.HasPrefix(txt, `<attached-file name="notes.md">`+"\n# hi") || !strings.HasSuffix(txt, "there\n</attached-file>") {
		t.Errorf("inlined file: %q", txt)
	}
	if blocks[2]["text"] != "look at these" {
		t.Errorf("prompt last: %v", blocks[2])
	}

	// Files with no words: no empty text block is added.
	only, _ := ContentBlocks("  ", atts[:1])
	if len(only) != 1 {
		t.Errorf("image alone should be one block, got %d", len(only))
	}
	// Nothing at all still yields a (blank) text block so the message is well formed.
	none, _ := ContentBlocks("", nil)
	if len(none) != 1 || none[0]["type"] != "text" {
		t.Errorf("empty message: %v", none)
	}
	if _, err := ContentBlocks("x", []model.Attachment{{Name: "bad.txt", MediaType: "text/plain", Data: "!!not base64!!"}}); err == nil {
		t.Error("malformed base64 should be an error")
	}
}

func TestInlineAttachments(t *testing.T) {
	text, err := InlineAttachments("please read", []model.Attachment{{Name: "a.txt", MediaType: "text/plain", Data: b64("alpha")}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "<attached-file name=\"a.txt\">\nalpha\n</attached-file>\n\nplease read" {
		t.Errorf("inlined: %q", text)
	}
	if _, err := InlineAttachments("x", []model.Attachment{{Name: "p.png", MediaType: "image/png", Data: b64("PNG")}}); err == nil || !strings.Contains(err.Error(), "p.png") {
		t.Errorf("images must be refused by name, got %v", err)
	}
	if same, _ := InlineAttachments("unchanged", nil); same != "unchanged" {
		t.Errorf("no attachments should pass through, got %q", same)
	}
}
