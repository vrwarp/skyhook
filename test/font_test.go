package e2e

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// Substituting a webfont from the system costs the page its typeface and
// nothing the reader came for — right up until the glyphs are private use
// codepoints, which every font on the reader's device is entitled to have
// nothing at. Then the substitution is a row of empty boxes where the toolbar
// was, which is what the Google Maps capture showed.
func TestIconFontsCrossAndProseFontsDoNot(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/fonts"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a heading set in a webfont", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// The @font-face pass runs on the CSS sweep after load, not on the first
	// snapshot: the sheet declaring an icon family routinely lands after the
	// glyphs that need it.
	var faces []string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		faces = fontFaces(cl.Model(tab).CSS)
		if len(faces) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(faces) == 0 {
		t.Fatal("no @font-face survived; the icons have nothing to render with")
	}

	joined := strings.ToLower(strings.Join(faces, "\n"))
	if !strings.Contains(joined, "test icons") {
		t.Errorf("the icon font was dropped, so its glyphs are empty boxes: %q", faces)
	}
	if strings.Contains(joined, "test prose") {
		t.Errorf("a prose webfont was shipped; the reader's own font would have done: %q", faces)
	}

	// And the bytes have to actually arrive, which for anything a stylesheet
	// names means being asked for: the server can see no viewport position for
	// a font, so nothing pushes one.
	key := skyhookKey(t, faces, "test icons")
	deadline = time.Now().Add(budget(30 * time.Second))
	for asked := 0; time.Now().Before(deadline); asked++ {
		// Asked again every couple of seconds rather than once. What is under
		// test here is that an icon font crosses the link at all; that a single
		// request is enough is a property of the image pipeline, and it is
		// tested there, where the timing can be controlled.
		if asked%20 == 0 {
			if err := cl.WantImages(tab, []string{key}); err != nil {
				t.Fatalf("ask for the font: %v", err)
			}
		}
		if data, ok := cl.ImageBytes(key); ok {
			if !bytes.HasPrefix(data, []byte("wOF2")) {
				t.Fatalf("the font arrived re-encoded as something else: %q", data[:4])
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the icon font's bytes never arrived (key %q)", key)
}

// fontFaces picks the @font-face rules out of the delivered stylesheet.
func fontFaces(rules []string) []string {
	var out []string
	for _, r := range rules {
		if strings.Contains(strings.ToLower(r), "@font-face") {
			out = append(out, r)
		}
	}
	return out
}

// skyhookKey finds the content hash the server rewrote a font's src() to.
func skyhookKey(t *testing.T, rules []string, family string) string {
	t.Helper()
	for _, r := range rules {
		if !strings.Contains(strings.ToLower(r), family) {
			continue
		}
		i := strings.Index(r, "skyhook://img/")
		if i < 0 {
			t.Fatalf("the font's src was not rewritten to a cache key: %q", r)
		}
		rest := r[i+len("skyhook://img/"):]
		end := strings.IndexAny(rest, `)"' `)
		if end < 0 {
			end = len(rest)
		}
		return rest[:end]
	}
	t.Fatalf("no @font-face for %q in %q", family, rules)
	return ""
}
