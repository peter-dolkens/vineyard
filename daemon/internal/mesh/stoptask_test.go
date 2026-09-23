package mesh

import (
	"encoding/json"
	"testing"

	"github.com/peter-dolkens/vineyard/daemon/internal/managed"
	"github.com/peter-dolkens/vineyard/daemon/internal/protocol"
)

// stoptask needs a task id and a session this daemon spawned: an observed session (or a daemon with
// managed sessions disabled) gets the same plain refusal, so the viewer can say why rather than
// report a generic failure.
func TestStopTaskOpRefusesObservedSessions(t *testing.T) {
	n := newTestNode(t, "m1")
	if _, err := n.handleLocal(protocol.Request{Op: "stoptask", Args: json.RawMessage(`{"sessionId":"s1"}`)}); err == nil || err.Error() != "taskId is required" {
		t.Fatalf("missing taskId: got %v", err)
	}
	const want = "only sessions started by Vineyard can stop their tasks"
	args := json.RawMessage(`{"sessionId":"s1","taskId":"t1"}`)
	if _, err := n.handleLocal(protocol.Request{Op: "stoptask", Args: args}); err == nil || err.Error() != want {
		t.Fatalf("managed disabled: got %v", err)
	}
	n.opts.Managed = managed.New(nil, nil)
	if _, err := n.handleLocal(protocol.Request{Op: "stoptask", Args: args}); err == nil || err.Error() != want {
		t.Fatalf("unmanaged session: got %v", err)
	}
}
