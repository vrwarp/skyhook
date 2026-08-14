package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// nodeRect is what the agent reports for a node id.
type nodeRect struct {
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	CX       float64 `json:"cx"`
	CY       float64 `json:"cy"`
	Tag      string  `json:"tag"`
	Editable bool    `json:"editable"`
	Href     string  `json:"href"`
}

// controlKeys maps the control keys the client forwards verbatim onto the
// windowsVirtualKeyCode / text pairs Chromium expects. Ordinary characters
// never appear here: they go through Input.insertText, which is one CDP call
// per keystroke instead of three.
var controlKeys = map[string]struct {
	Code  string
	Key   string
	VK    int
	Text  string
	Unmod string
}{
	"Enter":      {"Enter", "Enter", 13, "\r", "\r"},
	"Tab":        {"Tab", "Tab", 9, "\t", "\t"},
	"Backspace":  {"Backspace", "Backspace", 8, "", ""},
	"Delete":     {"Delete", "Delete", 46, "", ""},
	"Escape":     {"Escape", "Escape", 27, "", ""},
	"ArrowUp":    {"ArrowUp", "ArrowUp", 38, "", ""},
	"ArrowDown":  {"ArrowDown", "ArrowDown", 40, "", ""},
	"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37, "", ""},
	"ArrowRight": {"ArrowRight", "ArrowRight", 39, "", ""},
	"Home":       {"Home", "Home", 36, "", ""},
	"End":        {"End", "End", 35, "", ""},
	"PageUp":     {"PageUp", "PageUp", 33, "", ""},
	"PageDown":   {"PageDown", "PageDown", 34, "", ""},
	"Space":      {"Space", " ", 32, " ", " "},
}

// HandleInput replays one semantic input event into the landside page.
func (t *Tab) HandleInput(ctx context.Context, ev *protocol.InputEvent) error {
	t.mu.Lock()
	t.pendingInput = ev.Seq
	t.mu.Unlock()

	switch ev.Kind {
	case protocol.InClick, protocol.InDblClick, protocol.InContext:
		return t.click(ctx, ev)
	case protocol.InText:
		return t.insertText(ctx, ev)
	case protocol.InKey:
		return t.key(ctx, ev)
	case protocol.InSetValue:
		return t.setValue(ctx, ev)
	case protocol.InFocus:
		_, err := t.eval(ctx, fmt.Sprintf("__skyhook.focus(%d)", ev.Node))
		return err
	case protocol.InBlur:
		_, err := t.eval(ctx, "document.activeElement && document.activeElement.blur()")
		return err
	case protocol.InSubmit:
		return t.submit(ctx, ev)
	case protocol.InPaste:
		return t.insertText(ctx, ev)
	case protocol.InWheel:
		return t.wheel(ctx, ev)
	case protocol.InHover:
		return t.hover(ctx, ev)
	case protocol.InSelect:
		return nil // selection is native in the mirror; nothing to replay
	}
	return fmt.Errorf("mirror: unknown input kind %q", ev.Kind)
}

func (t *Tab) rect(ctx context.Context, node int64) (*nodeRect, error) {
	raw, err := t.eval(ctx, fmt.Sprintf("__skyhook.rect(%d)", node))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("mirror: node %d not found landside", node)
	}
	var r nodeRect
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (t *Tab) click(ctx context.Context, ev *protocol.InputEvent) error {
	r, err := t.rect(ctx, ev.Node)
	if err != nil {
		return err
	}
	x, y := r.CX, r.CY
	// A click offset inside the box matters for things like sliders and maps;
	// the client sends it only when it is meaningful.
	if ev.X != 0 || ev.Y != 0 {
		x, y = r.X+float64(ev.X), r.Y+float64(ev.Y)
	}
	button := "left"
	clicks := 1
	switch {
	case ev.Kind == protocol.InContext || ev.Button == 2:
		button = "right"
	case ev.Button == 1:
		button = "middle"
	}
	if ev.Kind == protocol.InDblClick {
		clicks = 2
	}
	base := map[string]any{
		"x": x, "y": y, "button": button, "clickCount": clicks,
		"modifiers": ev.Modifiers, "buttons": 1,
	}
	// A real pointer sequence: some SPAs bind to mousedown/mouseup or pointer
	// events rather than click.
	move := cloneMap(base)
	move["type"] = "mouseMoved"
	move["clickCount"] = 0
	move["buttons"] = 0
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", move, nil); err != nil {
		return err
	}
	down := cloneMap(base)
	down["type"] = "mousePressed"
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", down, nil); err != nil {
		return err
	}
	up := cloneMap(base)
	up["type"] = "mouseReleased"
	up["buttons"] = 0
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", up, nil); err != nil {
		return err
	}
	// Give the page a beat to react, then push whatever it changed. Without
	// this the batch waits the full 100 ms window, which is dead time the user
	// is already paying an RTT for.
	go t.flushSoon(60 * time.Millisecond)
	return nil
}

func (t *Tab) hover(ctx context.Context, ev *protocol.InputEvent) error {
	r, err := t.rect(ctx, ev.Node)
	if err != nil {
		return err
	}
	return t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": r.CX, "y": r.CY, "modifiers": ev.Modifiers,
	}, nil)
}

func (t *Tab) insertText(ctx context.Context, ev *protocol.InputEvent) error {
	if ev.Node != 0 {
		if _, err := t.eval(ctx, fmt.Sprintf("__skyhook.focus(%d)", ev.Node)); err != nil {
			return err
		}
	}
	if ev.Text == "" {
		return nil
	}
	if err := t.sess.Do(ctx, "Input.insertText", map[string]any{"text": ev.Text}, nil); err != nil {
		return err
	}
	go t.flushSoon(120 * time.Millisecond)
	return nil
}

func (t *Tab) key(ctx context.Context, ev *protocol.InputEvent) error {
	if ev.Node != 0 {
		if _, err := t.eval(ctx, fmt.Sprintf("__skyhook.focus(%d)", ev.Node)); err != nil {
			return err
		}
	}
	k, ok := controlKeys[ev.Key]
	if !ok {
		// Not a control key: treat it as text so we still do the right thing.
		return t.insertText(ctx, &protocol.InputEvent{Node: ev.Node, Text: ev.Key})
	}
	rep := ev.Repeat
	if rep <= 0 {
		rep = 1
	}
	for i := 0; i < rep && i < 32; i++ {
		down := map[string]any{
			"type": "keyDown", "key": k.Key, "code": k.Code,
			"windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK,
			"modifiers": ev.Modifiers,
		}
		if k.Text != "" {
			down["text"] = k.Text
			down["unmodifiedText"] = k.Unmod
		}
		if err := t.sess.Do(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
			return err
		}
		up := map[string]any{
			"type": "keyUp", "key": k.Key, "code": k.Code,
			"windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK,
			"modifiers": ev.Modifiers,
		}
		if err := t.sess.Do(ctx, "Input.dispatchKeyEvent", up, nil); err != nil {
			return err
		}
	}
	go t.flushSoon(120 * time.Millisecond)
	return nil
}

func (t *Tab) setValue(ctx context.Context, ev *protocol.InputEvent) error {
	expr := fmt.Sprintf("__skyhook.setValue(%d,%s,%d,%d)",
		ev.Node, jsString(ev.Text), ev.Start, ev.End)
	if _, err := t.eval(ctx, expr); err != nil {
		return err
	}
	go t.flushSoon(120 * time.Millisecond)
	return nil
}

func (t *Tab) submit(ctx context.Context, ev *protocol.InputEvent) error {
	fields, err := json.Marshal(ev.Fields)
	if err != nil {
		return err
	}
	_, err = t.eval(ctx, fmt.Sprintf("__skyhook.submit(%d,%s)", ev.Node, string(fields)))
	return err
}

func (t *Tab) wheel(ctx context.Context, ev *protocol.InputEvent) error {
	return t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": 10, "y": 10,
		"deltaX": ev.X, "deltaY": ev.Y, "modifiers": ev.Modifiers,
	}, nil)
}

// HandleScroll applies client scroll telemetry. Beyond keeping landside scroll
// position roughly in sync (which matters for lazy-loading pages), reaching the
// end of the mirrored document synthesises real scrolling so infinite lists
// keep producing content.
func (t *Tab) HandleScroll(ctx context.Context, ev *protocol.ScrollEvent) error {
	if ev.Node != 0 {
		_, err := t.eval(ctx, fmt.Sprintf("__skyhook.scrollTo(%d,%d,%d)", ev.Node, ev.X, ev.Y))
		return err
	}
	ratio := 0.0
	if ev.DocH > 0 {
		ratio = float64(ev.Y+ev.H) / float64(ev.DocH)
	}
	if ratio > 1 {
		ratio = 1
	}
	_, err := t.eval(ctx, fmt.Sprintf("__skyhook.scrollProbe(%f)", ratio))
	if err != nil {
		return err
	}
	if ratio > 0.85 {
		// Near the end: nudge the real page so its intersection observers fire.
		go t.flushSoon(400 * time.Millisecond)
	}
	return nil
}

// Navigate drives navigation for a tab.
func (t *Tab) Navigate(ctx context.Context, n protocol.Navigate) error {
	switch n.Action {
	case "back", "forward":
		var hist struct {
			CurrentIndex int `json:"currentIndex"`
			Entries      []struct {
				ID int64 `json:"id"`
			} `json:"entries"`
		}
		if err := t.sess.Do(ctx, "Page.getNavigationHistory", nil, &hist); err != nil {
			return err
		}
		idx := hist.CurrentIndex
		if n.Action == "back" {
			idx--
		} else {
			idx++
		}
		if idx < 0 || idx >= len(hist.Entries) {
			return nil
		}
		return t.sess.Do(ctx, "Page.navigateToHistoryEntry",
			map[string]any{"entryId": hist.Entries[idx].ID}, nil)
	case "reload":
		return t.sess.Do(ctx, "Page.reload", map[string]any{"ignoreCache": false}, nil)
	case "stop":
		return t.sess.Do(ctx, "Page.stopLoading", nil, nil)
	}
	if n.URL == "" {
		return nil
	}
	url := normalizeURL(n.URL)
	t.setLoading(true)
	return t.sess.Do(ctx, "Page.navigate", map[string]any{"url": url}, nil)
}

// CaptureRegion renders one JPEG of a node's box: the fallback for canvas,
// WebGL and video regions, which the mirror cannot represent.
func (t *Tab) CaptureRegion(ctx context.Context, node int64) ([]byte, error) {
	r, err := t.rect(ctx, node)
	if err != nil {
		return nil, err
	}
	if r.W < 1 || r.H < 1 {
		return nil, fmt.Errorf("mirror: node %d has no box", node)
	}
	var out struct {
		Data []byte `json:"data"`
	}
	err = t.sess.Do(ctx, "Page.captureScreenshot", map[string]any{
		"format":  "jpeg",
		"quality": 55,
		"clip": map[string]any{
			"x": r.X, "y": r.Y, "width": r.W, "height": r.H, "scale": 1,
		},
		"captureBeyondViewport": false,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Data, nil
}

// VisibleLinks reports same-origin links near the viewport, ranked in reading
// order. Speculative prefetch consumes this.
func (t *Tab) VisibleLinks(ctx context.Context, limit int) ([]LinkHint, error) {
	raw, err := t.eval(ctx, fmt.Sprintf("__skyhook.links(%d)", limit))
	if err != nil {
		return nil, err
	}
	var out []LinkHint
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LinkHint is a candidate for speculative prefetch.
type LinkHint struct {
	ID   int64   `json:"id"`
	Href string  `json:"href"`
	Y    float64 `json:"y"`
}

// DocHash asks the agent for a whole-document fingerprint, used by the periodic
// divergence check.
func (t *Tab) DocHash(ctx context.Context) (uint64, error) {
	raw, err := t.eval(ctx, "__skyhook.docHash()")
	if err != nil {
		return 0, err
	}
	var h uint64
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, err
	}
	return h, nil
}

func (t *Tab) flushSoon(d time.Duration) {
	time.Sleep(d)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = t.eval(ctx, "__skyhook.flush()")
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// jsString renders a Go string as a JavaScript literal safe for eval.
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	// JSON is a subset of JS except for two line separators that are legal in
	// JSON strings but terminate a JavaScript line.
	out := strings.ReplaceAll(string(b), "\u2028", `\u2028`)
	out = strings.ReplaceAll(out, "\u2029", `\u2029`)
	return out
}

// normalizeURL turns bare hostnames and search terms into something navigable.
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "about:blank"
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "about:") || strings.HasPrefix(s, "file://") {
		return s
	}
	if !strings.Contains(s, " ") && strings.Contains(s, ".") {
		return "https://" + s
	}
	return "https://duckduckgo.com/?q=" + urlQueryEscape(s)
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteByte(r)
		case r == ' ':
			b.WriteByte('+')
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}
