package parity

import (
	"strings"
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func frame(t *testing.T, typ protocol.Type, seq uint64, body any) protocol.Frame {
	t.Helper()
	f, err := protocol.NewFrame(typ, 1, body)
	if err != nil {
		t.Fatal(err)
	}
	f.Seq = seq
	return *f
}

func TestReplayRebuildsTheDocument(t *testing.T) {
	snap := &protocol.Snapshot{
		Strings: []string{"html", "body", "p", "hello"},
		Nodes: []protocol.Node{
			{ID: 1, Kind: protocol.KindElement, Ref: 0},
			{ID: 2, Parent: 1, Kind: protocol.KindElement, Ref: 1},
			{ID: 3, Parent: 2, Kind: protocol.KindElement, Ref: 2},
			{ID: 4, Parent: 3, Kind: protocol.KindText, Ref: 3},
		},
		CSS: []string{"p { color: red }"},
	}
	// Mutation strings extend the snapshot's intern table, so the spliced-in
	// text is at index 4, after the snapshot's four.
	mut := &protocol.Mutation{
		Strings: []string{" world"},
		Ops: []protocol.Op{
			{Op: protocol.OpSplice, Node: 4, Off: 5, Del: 0, Ref: 4},
		},
	}
	model, err := Replay([]protocol.Frame{
		frame(t, protocol.TypeSnapshot, 0, snap),
		frame(t, protocol.TypeMutation, 1, mut),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Text(); got != "hello world" {
		t.Fatalf("text = %q", got)
	}
	if got := model.Stylesheet(); !strings.Contains(got, "color: red") {
		t.Fatalf("css = %q", got)
	}
	if model.Meta().Seq != 1 {
		t.Fatalf("seq = %d", model.Meta().Seq)
	}
}

func TestReplayReportsAStreamItCannotApply(t *testing.T) {
	mut := &protocol.Mutation{Ops: []protocol.Op{
		{Op: protocol.OpInsert, Parent: 99, Nodes: []protocol.Node{
			{ID: 5, Kind: protocol.KindElement, Ref: 0},
		}},
	}}
	_, err := Replay([]protocol.Frame{frame(t, protocol.TypeMutation, 1, mut)})
	if err == nil || !strings.Contains(err.Error(), "unknown parent") {
		t.Fatalf("got %v", err)
	}
}

func TestReplaySkipsWhatTheReplicaDoesNotModel(t *testing.T) {
	model, err := Replay([]protocol.Frame{
		frame(t, protocol.TypeStats, 0, &protocol.Stats{Tabs: 1}),
	})
	if err != nil || model.NodeCount() != 0 {
		t.Fatalf("got %d nodes, %v", model.NodeCount(), err)
	}
}
