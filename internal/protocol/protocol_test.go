package protocol_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func TestFrameRoundTrip(t *testing.T) {
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	snap := protocol.Snapshot{
		Strings: []string{"html", "body", "div", "hello world"},
		Nodes: []protocol.Node{
			{ID: 1, Kind: protocol.KindElement, Ref: 0},
			{ID: 2, Parent: 1, Kind: protocol.KindElement, Ref: 1},
			{ID: 3, Parent: 2, Kind: protocol.KindText, Ref: 3},
		},
		CSS:   []string{"body{margin:0}"},
		URL:   "https://example.com/",
		Title: "Example",
	}
	f, err := protocol.NewFrame(protocol.TypeSnapshot, 7, snap)
	if err != nil {
		t.Fatal(err)
	}
	f.Seq = 0
	msg, err := c.EncodeFrame(protocol.ChDom, f)
	if err != nil {
		t.Fatal(err)
	}
	ch, got, err := c.DecodeFrame(msg)
	if err != nil {
		t.Fatal(err)
	}
	if ch != protocol.ChDom {
		t.Fatalf("channel = %v, want dom", ch)
	}
	if got.Type != protocol.TypeSnapshot || got.Tab != 7 {
		t.Fatalf("frame header mismatch: %+v", got)
	}
	var back protocol.Snapshot
	if err := got.DecodeBody(&back); err != nil {
		t.Fatal(err)
	}
	if back.Title != "Example" || len(back.Nodes) != 3 || back.Nodes[2].Ref != 3 {
		t.Fatalf("snapshot did not survive the round trip: %+v", back)
	}
}

func TestCompressionActuallyCompresses(t *testing.T) {
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A DOM-like payload: highly repetitive minified class names.
	payload := []byte(strings.Repeat(`{"class":"nH aHU zA","role":"listitem"}`, 400))
	enc := c.Encode(protocol.ChDom, payload)
	if len(enc) >= len(payload)/2 {
		t.Fatalf("expected at least 2x compression, got %d -> %d", len(payload), len(enc))
	}
	_, dec, err := c.Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, payload) {
		t.Fatal("decompressed payload differs")
	}
}

func TestSmallFramesSkipCompression(t *testing.T) {
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A 300-byte chat mutation must not grow: the steady-state budget is 5 kbps.
	small := []byte(`{"op":"setText"}`)
	enc := c.Encode(protocol.ChDom, small)
	if len(enc) > len(small)+2 {
		t.Fatalf("small frame grew: %d -> %d", len(small), len(enc))
	}
}

func TestMediaChannelIsNotRecompressed(t *testing.T) {
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	jpegish := bytes.Repeat([]byte{0xff, 0xd8, 0x12, 0x9a}, 500)
	enc := c.Encode(protocol.ChMedia, jpegish)
	if len(enc) != len(jpegish)+2 {
		t.Fatalf("media payload was recompressed: %d -> %d", len(jpegish), len(enc))
	}
}

func TestCodecWithoutZstdIsInteroperable(t *testing.T) {
	// A client that cannot decompress must still be understood: the server
	// negotiates this from Hello capabilities.
	server, err := protocol.NewCodec(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := protocol.NewCodec(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	f, err := protocol.NewFrame(protocol.TypePing, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := server.EncodeFrame(protocol.ChCtrl, f)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := client.DecodeFrame(msg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != protocol.TypePing {
		t.Fatalf("type = %d", got.Type)
	}
}

func TestDictionaryRoundTrip(t *testing.T) {
	tr := protocol.NewDictTrainer()
	// Samples must vary the way real frames do: identical inputs give the
	// dictionary builder nothing to learn from.
	for i := 0; i < 64; i++ {
		var b strings.Builder
		for j := 0; j < 40; j++ {
			fmt.Fprintf(&b, `<div class="ajA ajC" role="listitem" data-id="%d-%d"><span class="bog">message %d</span></div>`, i, j, i*j)
		}
		tr.Observe("https://mail.google.com", []byte(b.String()))
	}
	id, dict, err := tr.Train("https://mail.google.com")
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	if id == 0 || len(dict) == 0 {
		t.Fatal("empty dictionary")
	}

	enc, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if err := enc.AddDict(id, dict); err != nil {
		t.Fatal(err)
	}
	if err := dec.AddDict(id, dict); err != nil {
		t.Fatal(err)
	}
	if err := enc.EnableDict(id); err != nil {
		t.Fatal(err)
	}
	// Compare like with like: both codecs must attempt compression on a frame
	// this small, which is exactly the chat-mutation size the dictionary is for.
	enc.MinCompress = 0

	payload := []byte(`<div class="ajA ajC" role="listitem"><span class="bog">a new message arrived</span></div>`)
	withDict := enc.Encode(protocol.ChDom, payload)
	_, back, err := dec.Decode(withDict)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, payload) {
		t.Fatal("dictionary round trip corrupted the payload")
	}

	plain, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	plain.MinCompress = 0
	noDict := plain.Encode(protocol.ChDom, payload)
	if len(withDict) >= len(noDict) {
		t.Fatalf("dictionary did not help: %d with dict vs %d without", len(withDict), len(noDict))
	}
}

func TestUnknownDictionaryIsAnError(t *testing.T) {
	c, err := protocol.NewCodec(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte{byte(protocol.ChDom), 2, 9, 9, 9, 9, 0x28}
	if _, _, err := c.Decode(msg); err == nil {
		t.Fatal("expected an error for an unknown dictionary id")
	}
}

func TestChannelPriorityOrdering(t *testing.T) {
	if protocol.ChCtrl.Priority() != 0 || protocol.ChInput.Priority() != 0 {
		t.Fatal("control and input must be highest priority")
	}
	if protocol.ChDom.Priority() >= protocol.ChMedia.Priority() {
		t.Fatal("dom must outrank media")
	}
	if protocol.ChMedia.Priority() >= protocol.ChBulk.Priority() {
		t.Fatal("media must outrank bulk")
	}
}
