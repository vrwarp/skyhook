package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"
)

/*
Region shots: the one part of a page the mirror cannot describe.

Everything else the mirror ships is structure — nodes, text, attributes, the
CSS that was actually used — and structure survives the trip because both ends
agree what it means. A canvas has no structure. Its content is whatever
JavaScript painted into it, and page JavaScript is precisely what never runs
plane-side. The DOM of a WebGL map and the DOM of a blank one are identical.

So these are photographed instead. The landside browser screenshots the box,
the image pipeline transcodes it like any other picture, and the client paints
it onto the element that stood empty. It is a worse trade than the rest of the
mirror — a bitmap costs more than the markup for the same area, it does not
reflow, and it cannot be selected or read out — but the alternative on a site
whose content is a canvas is a page with nothing in it, which is what the
capture that prompted this showed for both a game and a map.

What decides when to take one is the interesting part. A canvas can repaint
sixty times a second and no mutation will ever say so, so there is no signal
to follow and a poll would spend the whole link on frames nobody asked for.
Input is the signal used instead: the reader pressed a key or dragged the map,
which is the moment something they caused might have changed. That keeps the
cost proportional to interaction and holds to the same promise as the rest of
the client — one round trip per interaction, and none when idle. An animation
running on its own is not followed, deliberately.
*/

const (
	// shotMax bounds how many regions one pass photographs. Pages with a dozen
	// tiny canvases exist (sparklines, spinners); the agent sorts by area so
	// this budget goes to the one the reader came for.
	shotMax = 4
	// shotAfterInput is how long to wait for the page to finish reacting. Long
	// enough for a tile animation to land, short enough to stay inside the
	// second the reader is already spending on the link.
	shotAfterInput = 350 * time.Millisecond
	// shotAfterLoad gives a page that draws on startup — which is most of
	// them — time to draw before its first photograph.
	shotAfterLoad = 900 * time.Millisecond
	// shotTimeout bounds one pass: a screenshot of a busy compositor can block.
	shotTimeout = 15 * time.Second
)

// shotBox is one region the agent says needs photographing. X and Y are page
// coordinates; OX and OY place the rectangle inside the element's own box.
type shotBox struct {
	N  int64   `json:"n"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	W  float64 `json:"w"`
	H  float64 `json:"h"`
	OX float64 `json:"ox"`
	OY float64 `json:"oy"`
}

// shotSoon schedules a refresh, coalescing a burst of input into one pass.
//
// Every keystroke of a held arrow key would otherwise queue its own
// screenshot, and the reader only ever sees the last of them.
func (t *Tab) shotSoon(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.shotTimer != nil {
		t.shotTimer.Stop()
	}
	t.shotTimer = time.AfterFunc(d, func() {
		ctx, cancel := context.WithTimeout(context.Background(), shotTimeout)
		defer cancel()
		t.refreshShots(ctx)
	})
}

// refreshShots photographs every canvas-like region the page is showing.
func (t *Tab) refreshShots(ctx context.Context) {
	raw, err := t.eval(ctx, fmt.Sprintf("__skyhook.shots(%d)", shotMax))
	if err != nil {
		t.log.Debug("region shot list failed", "tab", t.ID, "err", err)
		return
	}
	var boxes []shotBox
	if err := json.Unmarshal(raw, &boxes); err != nil {
		return
	}
	live := make(map[int64]bool, len(boxes))
	for _, b := range boxes {
		live[b.N] = true
		t.shootRegion(ctx, b)
	}
	// A canvas that scrolled out of view, or one the page removed, leaves a key
	// behind that would suppress the next shot of whatever takes its node id —
	// the mirror reuses ids. Runs on an empty list too, which is the case where
	// every canvas went away at once.
	t.mu.Lock()
	for id := range t.lastShot {
		if !live[id] {
			delete(t.lastShot, id)
		}
	}
	t.mu.Unlock()
}

func (t *Tab) shootRegion(ctx context.Context, b shotBox) {
	shot, err := t.captureClip(ctx, b)
	if err != nil {
		t.log.Debug("region shot failed", "tab", t.ID, "node", b.N, "err", err)
		return
	}
	// The key is the content hash of what the browser painted, so a canvas the
	// reader did not change costs nothing at all: same pixels, same key, and
	// the client already holds the bytes.
	sum := fnv.New64a()
	_, _ = sum.Write(shot)
	key := fmt.Sprintf("%016x", sum.Sum64())

	t.mu.Lock()
	if t.lastShot == nil {
		t.lastShot = map[int64]string{}
	}
	unchanged := t.lastShot[b.N] == key
	t.lastShot[b.N] = key
	t.mu.Unlock()
	if unchanged {
		return
	}

	t.out.WantImage(t.ID, ImageRequest{
		Key: key, Node: b.N, Src: shot,
		W: int(b.W + 0.5), H: int(b.H + 0.5),
		Box: []int{int(b.OX + 0.5), int(b.OY + 0.5), int(b.W + 0.5), int(b.H + 0.5)},
		// Priority 0: this is not a photograph beside the content, it is the
		// content. Waiting to be asked for it would cost the round trip the
		// whole client exists to avoid.
		Priority: 0,
	})
}

// captureClip screenshots one rectangle of the page.
//
// The clip is in page coordinates — a viewport-relative rectangle photographs
// whatever happens to sit that far down the document instead, which on any
// scrolled page is not the element that was asked for.
//
// PNG, though nothing here ships PNG: this is the transcoder's input, and
// choosing the codec is the transcoder's whole job. Handing it JPEG would cost
// twice — the artefacts of the first pass are baked into what the second pass
// compresses, and the ringing around flat UI edges reads as photographic
// detail to the palette heuristic, which then picks the lossy codec for
// exactly the content that wanted the lossless one. These bytes never cross
// the link, so their size is landside's problem and landside has the room.
func (t *Tab) captureClip(ctx context.Context, b shotBox) ([]byte, error) {
	if b.W < 1 || b.H < 1 {
		return nil, fmt.Errorf("mirror: node %d has no box", b.N)
	}
	var out struct {
		Data []byte `json:"data"`
	}
	err := t.sess.Do(ctx, "Page.captureScreenshot", map[string]any{
		"format": "png",
		"clip": map[string]any{
			"x": b.X, "y": b.Y, "width": b.W, "height": b.H, "scale": 1,
		},
		"captureBeyondViewport": false,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Data, nil
}
