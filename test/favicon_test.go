package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

/*
The strip identifies a tab by its icon as well as its title (P-104).

The favicon is chrome UI — the one part of a page the mirror's DOM never
carries — so the server fetches it landside and delivers it inside the tab
state, as a data URL the shell hands straight to an <img>. The fixture site
declares no icon, so what this measures is the whole default path: the agent
falling back to /favicon.ico, the landside fetch, the magic-byte sniff, and
the state merge that keeps the icon on while later states report their one
changed thing.
*/
func TestTheTabStateCarriesTheFavicon(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	tab := h.openFixture(ctx, cl)

	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if st, ok := cl.TabState(tab); ok &&
			strings.HasPrefix(st.FaviconID, "data:image/png;base64,") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	st, _ := cl.TabState(tab)
	t.Fatalf("no favicon reached the tab state (FaviconID %.40q)", st.FaviconID)
}
