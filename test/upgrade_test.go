package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
)

/*
lateUpgradePage is the shape of a code-split site.

The elements are in the document from the first byte; their definitions arrive
with a bundle some time later. Until then the site keeps them out of the way
with `:not(:defined)`, and afterwards dresses them with `:defined` — which is
the pair the mirror has to answer from landside, because plane-side no
definition will ever run.

`late-card` gets its definition; `never-card` never does. The delayed upgrade
touches no observed node: attachShadow reports nothing to a MutationObserver,
so nothing but a deliberate re-read will ever notice it.
*/
const lateUpgradePage = `<!DOCTYPE html><html><head><title>Late</title>
<style>
  .placeholder:not(:defined) { visibility: hidden; }
  late-card:defined { display: block; color: rgb(7, 8, 9); }
</style></head>
<body>
  <h1>late upgrade</h1>
  <never-card id="never" class="placeholder">no definition is coming</never-card>
  <late-card id="card" class="placeholder">
    <div slot="items">menu item nobody asked to see</div>
  </late-card>
<script>
  setTimeout(function () {
    class LateCard extends HTMLElement {
      connectedCallback() {
        var root = this.attachShadow({ mode: 'open' });
        root.innerHTML = '<div class="face">the component finished loading</div>' +
          '<div hidden><slot name="items"></slot></div>';
      }
    }
    customElements.define('late-card', LateCard);
  }, 1500);
</script>
</body></html>`

/*
A custom element that upgrades after it was mirrored is the difference between
a page and a pile of it.

Reddit builds every menu this way: the light DOM holds the items, the upgrade
attaches a shadow root that tucks them into a collapsed popup, and until that
happens `:not(:defined)` hides the lot. Mirror the element before the upgrade
and never look again, and the client keeps the skeleton for ever — every menu
on the page rendered flat and in the open, over the content.
*/
func TestLateCustomElementUpgradeReachesTheMirror(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/late-upgrade", "late upgrade")

	// An element whose definition is never coming keeps the mark, and the
	// placeholder rule that reads it has to arrive with it: plane-side the
	// question `:not(:defined)` asks cannot be answered any other way.
	never, err := cl.FindNode(tab, "never-card", "id", "never")
	if err != nil {
		t.Fatalf("un-upgraded element missing from the mirror: %v", err)
	}
	if _, ok := never.Attrs["data-sky-undefined"]; !ok {
		t.Errorf("un-upgraded element is not marked as such: %+v", never.Attrs)
	}
	if css := strings.Join(cl.Model(tab).CSS, "\n"); !strings.Contains(css, ".placeholder[data-sky-undefined]") {
		t.Errorf("the :not(:defined) placeholder rule did not arrive rewritten: %q", css)
	}

	// The upgrade itself: the component's own markup has to follow it across.
	if err := cl.WaitForText(ctx, tab, "the component finished loading", budget(30*time.Second)); err != nil {
		t.Fatalf("the shadow root attached at upgrade never reached the client: %v", err)
	}

	card, err := cl.FindNode(tab, "late-card", "id", "card")
	if err != nil {
		t.Fatalf("upgraded element missing from the mirror: %v", err)
	}
	if _, ok := card.Attrs["data-sky-undefined"]; ok {
		t.Error("upgraded element is still marked undefined, so it keeps the placeholder styling")
	}
	if never, err = cl.FindNode(tab, "never-card", "id", "never"); err != nil {
		t.Fatalf("un-upgraded element went missing: %v", err)
	} else if _, ok := never.Attrs["data-sky-undefined"]; !ok {
		t.Error("the mark was cleared from an element that never upgraded")
	}

	// The slotted content is still there — exactly once. It is the component's
	// to place now, not the page's.
	if n := strings.Count(cl.Model(tab).Text(), "menu item nobody asked to see"); n != 1 {
		t.Errorf("slotted light DOM appears %d times in the mirror, want 1", n)
	}

	// And the styling that only applies once a component has upgraded, which
	// before this could match nothing plane-side at all.
	deadline := time.Now().Add(budget(20 * time.Second))
	for {
		css := strings.Join(cl.Model(tab).CSS, "\n")
		if strings.Contains(css, "late-card:not([data-sky-undefined])") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the :defined rule for the upgraded component never arrived: %q", css)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// openPage opens one of the harness's other fixtures and waits for it.
func (h *harness) openPage(ctx context.Context, cl *client.Client, path, want string) uint32 {
	h.t.Helper()
	if err := cl.OpenTab(h.site.URL + path); err != nil {
		h.t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		h.t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, want, budget(45*time.Second)); err != nil {
		h.t.Fatalf("mirror never delivered %s: %v", path, err)
	}
	return tab
}
