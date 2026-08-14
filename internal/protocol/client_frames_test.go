package protocol_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// The client -> server direction, pinned with fixtures the TypeScript client
// actually produces (client/test/encoding.test.ts writes them).
//
// This exists because of a real bug: cbor-x encodes any integer above 2^32-1 as
// a float64, and the decoder below refuses to put a float into an int64 field.
// A wall-clock timestamp in an input event was therefore rejected in full, and
// every click and keystroke from the browser client was silently dropped. The
// Go test client never reproduced it because Go sends proper integers.
func loadClientFrames(t *testing.T) map[string][]byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "client-frames.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no client fixtures (%v); run the client test suite to generate them", err)
	}
	var encoded map[string]string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte, len(encoded))
	for name, b64 := range encoded {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("fixture %q: %v", name, err)
		}
		out[name] = data
	}
	return out
}

func decodeClientFrame(t *testing.T, frames map[string][]byte, name string) *protocol.Frame {
	t.Helper()
	data, ok := frames[name]
	if !ok {
		t.Fatalf("no fixture named %q", name)
	}
	var f protocol.Frame
	if err := protocol.Unmarshal(data, &f); err != nil {
		t.Fatalf("fixture %q does not decode: %v", name, err)
	}
	return &f
}

func TestClientInputFramesDecode(t *testing.T) {
	frames := loadClientFrames(t)

	f := decodeClientFrame(t, frames, "input")
	if f.Type != protocol.TypeInput || f.Tab != 1 {
		t.Fatalf("header = type %d tab %d", f.Type, f.Tab)
	}
	var ev protocol.InputEvent
	if err := f.DecodeBody(&ev); err != nil {
		// This is the exact failure the bug produced.
		t.Fatalf("input body does not decode: %v", err)
	}
	if ev.Kind != protocol.InClick || ev.Node != 42 || ev.Seq != 7 {
		t.Fatalf("input event = %+v", ev)
	}
	if ev.TS <= 0 {
		t.Fatalf("timestamp did not survive: %d", ev.TS)
	}
	// The pointer measurements the client takes for the server to replay.
	if ev.Hold != 83 {
		t.Errorf("hold = %d, want 83", ev.Hold)
	}
	if len(ev.Point) != 2 || ev.Point[0] != 250 || ev.Point[1] != 500 {
		t.Errorf("point = %v, want [250 500]", ev.Point)
	}
	if len(ev.Path) != 9 || ev.Path[0] != 100 || ev.Path[8] != 21 {
		t.Errorf("path = %v, want 9 elements starting 100 and ending 21", ev.Path)
	}

	f = decodeClientFrame(t, frames, "text")
	var text protocol.InputEvent
	if err := f.DecodeBody(&text); err != nil {
		t.Fatalf("text body does not decode: %v", err)
	}
	if text.Kind != protocol.InText || text.Text != "hello" {
		t.Fatalf("text event = %+v", text)
	}
}

func TestClientControlFramesDecode(t *testing.T) {
	frames := loadClientFrames(t)

	var ack protocol.TabAck
	if err := decodeClientFrame(t, frames, "ack").DecodeBody(&ack); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if ack.Tab != 2 || ack.Seq != 99 || ack.Hash != 0xdeadbeef {
		t.Fatalf("ack = %+v", ack)
	}

	var scroll protocol.ScrollEvent
	if err := decodeClientFrame(t, frames, "scroll").DecodeBody(&scroll); err != nil {
		t.Fatalf("scroll: %v", err)
	}
	if scroll.Y != 4096 || scroll.DocH != 120000 {
		t.Fatalf("scroll = %+v", scroll)
	}

	var resync protocol.Resync
	if err := decodeClientFrame(t, frames, "resync").DecodeBody(&resync); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if resync.HaveTo != 12 || resync.Reason != "gap" {
		t.Fatalf("resync = %+v", resync)
	}

	var nav protocol.Navigate
	if err := decodeClientFrame(t, frames, "navigate").DecodeBody(&nav); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if nav.URL != "https://example.test/" {
		t.Fatalf("navigate = %+v", nav)
	}

	var vp protocol.Viewport
	if err := decodeClientFrame(t, frames, "viewport").DecodeBody(&vp); err != nil {
		t.Fatalf("viewport: %v", err)
	}
	if vp.W != 1280 || vp.H != 800 {
		t.Fatalf("viewport = %+v", vp)
	}

	var want protocol.ImageWant
	if err := decodeClientFrame(t, frames, "imageWant").DecodeBody(&want); err != nil {
		t.Fatalf("imageWant: %v", err)
	}
	if len(want.Hashes) != 1 || want.Hashes[0] != "deadbeef" || len(want.Have) != 1 {
		t.Fatalf("imageWant = %+v", want)
	}

	var cmd protocol.AdapterCommand
	if err := decodeClientFrame(t, frames, "adapter").DecodeBody(&cmd); err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if cmd.Adapter != "googlechat" || cmd.Cmd != "send" || cmd.Text == "" {
		t.Fatalf("adapter command = %+v", cmd)
	}
}

func TestClientHelloDecodes(t *testing.T) {
	frames := loadClientFrames(t)
	var hello protocol.Hello
	if err := decodeClientFrame(t, frames, "hello").DecodeBody(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.Version != protocol.Version {
		t.Fatalf("version = %d", hello.Version)
	}
	if hello.Token != "conformance-token" {
		t.Fatalf("token = %q", hello.Token)
	}
	if len(hello.Resume) != 1 || hello.Resume[0].Seq != 9 || hello.Resume[0].Hash != 0xdeadbeef {
		t.Fatalf("resume = %+v", hello.Resume)
	}
	if hello.Viewport.W != 1280 {
		t.Fatalf("viewport = %+v", hello.Viewport)
	}
}

// TestEveryClientFixtureDecodes is the blunt version: whatever the client can
// produce, the server must be able to read.
func TestEveryClientFixtureDecodes(t *testing.T) {
	for name, data := range loadClientFrames(t) {
		var f protocol.Frame
		if err := protocol.Unmarshal(data, &f); err != nil {
			t.Errorf("fixture %q: %v", name, err)
			continue
		}
		if f.Type == 0 {
			t.Errorf("fixture %q decoded without a frame type", name)
		}
	}
}
