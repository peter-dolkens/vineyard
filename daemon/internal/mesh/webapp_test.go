package mesh

import (
	"encoding/json"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

type fakeWebApp struct {
	listen string
	sets   int
}

func (f *fakeWebApp) Set(listen string, _ []string) error { f.listen = listen; f.sets++; return nil }
func (f *fakeWebApp) Status() model.WebAppStatus          { return model.WebAppStatus{Listen: f.listen} }
func (f *fakeWebApp) Pair() (json.RawMessage, error)      { return json.RawMessage(`{"code":"X"}`), nil }
func (f *fakeWebApp) Devices() (json.RawMessage, error)   { return json.RawMessage(`{}`), nil }
func (f *fakeWebApp) Revoke(string, bool) error           { return nil }

func webappReq(listen string, at int64) protocol.Request {
	b, _ := json.Marshal(protocol.WebAppArgs{Listen: listen, At: at})
	return protocol.Request{Op: "webapp", Args: b}
}

// Two VS Code windows with different settings must not flip a machine's web app back and forth:
// the choice made later wins, and an older one is answered with the current state.
func TestWebAppLaterChoiceWins(t *testing.T) {
	n := newTestNode(t, "atelier")
	w := &fakeWebApp{}
	n.opts.WebApp = w
	if _, err := n.handleLocal(webappReq(":7735", 200)); err != nil {
		t.Fatal(err)
	}
	if w.listen != ":7735" || n.cfg.WebApp != ":7735" || n.cfg.WebAppAt != 200 {
		t.Fatalf("not applied: %+v %q %d", w, n.cfg.WebApp, n.cfg.WebAppAt)
	}
	raw, err := n.handleLocal(webappReq("", 100)) // an older choice
	if err != nil {
		t.Fatal(err)
	}
	var st model.WebAppStatus
	_ = json.Unmarshal(raw, &st)
	if w.listen != ":7735" || w.sets != 1 || st.Listen != ":7735" || st.At != 200 {
		t.Fatalf("older choice applied: %+v, answered %+v", w, st)
	}
	if _, err := n.handleLocal(webappReq("", 300)); err != nil {
		t.Fatal(err)
	}
	if w.listen != "" || n.cfg.WebAppAt != 300 {
		t.Fatalf("newer off not applied: %+v", w)
	}
	n.opts.WebApp = nil
	if _, err := n.handleLocal(webappReq(":7735", 400)); err == nil {
		t.Fatal("a daemon without a web app accepted webapp")
	}
}
