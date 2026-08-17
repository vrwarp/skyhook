package protocol_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// Conformance fixtures pin the wire format across the two implementations. The
// Go side writes them here; the TypeScript client's test suite reads the same
// file and asserts it decodes to the same values. Regenerate with:
//
//	go test ./internal/protocol -run TestWriteConformanceFixtures -update
var update = os.Getenv("SKYHOOK_UPDATE_FIXTURES") == "1"

func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "conformance.json")
}

func buildFixtures(t *testing.T) map[string]string {
	t.Helper()
	// Compression on: the client must handle a zstd-compressed frame, which is
	// what a real snapshot always is.
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	out := map[string]string{}
	enc := func(name string, ch protocol.Channel, f *protocol.Frame) {
		msg, err := c.EncodeFrame(ch, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = base64.StdEncoding.EncodeToString(msg)
	}

	snap := protocol.Snapshot{
		Strings: []string{"html", "body", "p", "hello world"},
		Nodes: []protocol.Node{
			{ID: 1, Kind: protocol.KindElement, Ref: 0},
			{ID: 2, Parent: 1, Kind: protocol.KindElement, Ref: 1},
			{ID: 3, Parent: 2, Kind: protocol.KindElement, Ref: 2, Attrs: []int32{0, 3}, Flags: protocol.FlagEditable},
			{ID: 4, Parent: 3, Kind: protocol.KindText, Ref: 3},
		},
		CSS:      []string{"body{margin:0}", "p{color:#111}"},
		URL:      "https://example.test/",
		Title:    "Conformance",
		Viewport: protocol.Viewport{W: 1280, H: 800, DPR: 1},
		Images: []protocol.ImageMeta{{
			Node: 3, Hash: "deadbeef", W: 40, H: 30,
			Blur: "LEHV6nWB2yk8pyo0adR*.7kCMdnj", Mime: "image/avif", Bytes: 812,
		}},
		ScrollY: 120,
	}
	f, err := protocol.NewFrame(protocol.TypeSnapshot, 1, snap)
	if err != nil {
		t.Fatal(err)
	}
	enc("snapshot", protocol.ChDom, f)

	mut := protocol.Mutation{
		Strings: []string{"li", "appended", "class", "row"},
		Ops: []protocol.Op{
			{Op: protocol.OpInsert, Parent: 2, Before: 3, Nodes: []protocol.Node{
				{ID: 10, Parent: 2, Kind: protocol.KindElement, Ref: 4, Attrs: []int32{6, 7}},
				{ID: 11, Parent: 10, Kind: protocol.KindText, Ref: 5},
			}},
			{Op: protocol.OpRemove, Node: 4},
			{Op: protocol.OpAttr, Node: 3, Ref: 6, Ref2: 7},
			{Op: protocol.OpText, Node: 11, Ref: 5},
			{Op: protocol.OpMove, Node: 10, Parent: 2, Before: 0},
			{Op: protocol.OpSplice, Node: 11, Off: 3, Del: 2, Ref: 5},
			{Op: protocol.OpStyle, Add: []string{".a{color:red}"}},
		},
	}
	f, err = protocol.NewFrame(protocol.TypeMutation, 1, mut)
	if err != nil {
		t.Fatal(err)
	}
	f.Seq, f.Base, f.Cause = 7, 6, 3
	enc("mutation", protocol.ChDom, f)

	f, err = protocol.NewFrame(protocol.TypeWelcome, 0, protocol.Welcome{
		Version: protocol.Version, SessionID: "session-1", Resumed: true,
		Tabs: []protocol.TabRef{{Tab: 1, URL: "https://example.test/", Title: "Example", Seq: 7, Active: true}},
		Caps: []string{"zstd"}, Server: "test", KeepaliveMS: 15000,
		Adapters: []string{"googlechat"},
		// What the server would serve a browser asking for the app today. The
		// client compares it against the build compiled into its own bytes.
		ClientVersion: "0.1.0", ClientBuild: "conformance-build",
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("welcome", protocol.ChCtrl, f)

	f, err = protocol.NewFrame(protocol.TypeTabState, 1, protocol.TabState{
		URL: "https://example.test/", Title: "Example", Loading: false, CanBack: true,
		Ref: "t7",
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("tabstate", protocol.ChCtrl, f)

	f, err = protocol.NewFrame(protocol.TypeStats, 0, protocol.Stats{
		RTTMicros: 1_200_000, LossPct: 2, QueueDepth: 3, BytesSent: 1024, BytesRecv: 65536, Tabs: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("stats", protocol.ChCtrl, f)

	f, err = protocol.NewFrame(protocol.TypeAdapterEvent, 0, protocol.AdapterBatch{
		Records: []protocol.AdapterRecord{{
			Adapter: "googlechat", Kind: "message", ID: "m1", Space: "s1",
			Author: "someone", Text: "see you at the gate", TS: 1700000000000, Seq: 3,
		}},
		Backlog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("adapter", protocol.ChBulk, f)

	// A capture is the one frame family that crosses the link in both
	// directions with the same type, so both ends have to agree about it twice.
	f, err = protocol.NewFrame(protocol.TypeCapture, 0, protocol.CaptureRequest{
		ID: "20260101-120000-abcd1234", Reason: protocol.CaptureDivergence,
		Note: "the article body stopped updating", Tabs: []uint32{1, 2},
		MaxBytes: 4 << 20, Screenshots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("capture", protocol.ChCtrl, f)

	f, err = protocol.NewFrame(protocol.TypeCapturePart, 0, protocol.CapturePart{
		ID: "20260101-120000-abcd1234", Name: "tabs/1/mirror.html.gz",
		Data: []byte{0x1f, 0x8b, 0x08, 0x00}, More: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("capturepart", protocol.ChBulk, f)

	f, err = protocol.NewFrame(protocol.TypeCaptureDone, 0, protocol.CaptureDone{
		ID:   "20260101-120000-abcd1234",
		Path: "/data/captures/20260101-120000-abcd1234.zip", Bytes: 918_273,
	})
	if err != nil {
		t.Fatal(err)
	}
	enc("capturedone", protocol.ChCtrl, f)

	return out
}

func TestConformanceFixturesMatch(t *testing.T) {
	path := fixturePath(t)
	want := buildFixtures(t)

	if update {
		data, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		t.Log("conformance fixtures updated")
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no conformance fixtures yet (%v); regenerate with SKYHOOK_UPDATE_FIXTURES=1", err)
	}
	var have map[string]string
	if err := json.Unmarshal(raw, &have); err != nil {
		t.Fatal(err)
	}
	for name, wantB64 := range want {
		if have[name] != wantB64 {
			t.Errorf("fixture %q drifted; the wire format changed.\n"+
				"If that was intentional, regenerate with SKYHOOK_UPDATE_FIXTURES=1 "+
				"and make sure the client decodes the new bytes.", name)
		}
	}
}

// TestFixturesDecodeBackInGo proves the fixtures are self-consistent before the
// client is ever asked to read them.
func TestFixturesDecodeBackInGo(t *testing.T) {
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	fixtures := buildFixtures(t)
	msg, err := base64.StdEncoding.DecodeString(fixtures["mutation"])
	if err != nil {
		t.Fatal(err)
	}
	ch, f, err := c.DecodeFrame(msg)
	if err != nil {
		t.Fatal(err)
	}
	if ch != protocol.ChDom || f.Type != protocol.TypeMutation || f.Seq != 7 {
		t.Fatalf("unexpected frame: ch=%v type=%d seq=%d", ch, f.Type, f.Seq)
	}
	var m protocol.Mutation
	if err := f.DecodeBody(&m); err != nil {
		t.Fatal(err)
	}
	if len(m.Ops) != 7 || m.Ops[0].Nodes[0].ID != 10 || m.Ops[5].Off != 3 {
		t.Fatalf("mutation body did not survive: %+v", m)
	}
}
