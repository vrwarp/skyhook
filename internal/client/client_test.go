package client

import (
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
A verdict about an image must not erase the description of it.

`{Hash, Missing: true}` is sent from several places in the pipeline that have
concluded the bytes are not coming and know nothing else about the key — no
size, no type, no blurhash, and not the alt text of the element that referenced
it. It is a different message from the one that describes an asset, sharing a
body with it.

Replacing the table entry with a verdict lost the description, which the
emulated link caught: a re-snapshot re-asks for a key the pipeline has already
given up on, the re-asking and the re-submission race, and whichever answers
last is what the entry becomes. That is
TestAnImageThatCannotBeFetchedIsReportedRatherThanLeftPending failing on the
alt text it asserts travels.
*/
func TestAVerdictAboutAnImageKeepsWhatIsKnownAboutIt(t *testing.T) {
	described := protocol.ImageMeta{
		Node: 12, Hash: "k", W: 640, H: 480, Blur: "LEHV6n", Mime: "image/webp",
		Bytes: 8192, Alt: "the missing diagram", Box: []int{0, 0, 640, 480},
		Anim: true,
	}

	// The bytes are not coming, said by something that only ever knew the key.
	got := mergeImageMeta(described, protocol.ImageMeta{Hash: "k", Missing: true})
	if !got.Missing {
		t.Error("the verdict did not land: the element waits forever")
	}
	if got.Alt != "the missing diagram" {
		t.Errorf("Alt = %q, want the element's alt text — the whole of what is left to show", got.Alt)
	}
	if got.Node != 12 || got.W != 640 || got.H != 480 || got.Blur != "LEHV6n" ||
		got.Mime != "image/webp" || got.Bytes != 8192 || len(got.Box) != 4 || !got.Anim {
		t.Errorf("the verdict erased the description: %+v", got)
	}

	// And the key's second chance: a re-snapshot describes it again, and the
	// entry has to stop saying the bytes are not coming.
	again := mergeImageMeta(got, protocol.ImageMeta{
		Node: 12, Hash: "k", Alt: "the missing diagram",
	})
	if again.Missing {
		t.Error("a key described afresh is still marked as one nothing is sending")
	}
	if again.W != 640 || again.Blur != "LEHV6n" {
		t.Errorf("a partial description blanked what it did not carry: %+v", again)
	}
}
