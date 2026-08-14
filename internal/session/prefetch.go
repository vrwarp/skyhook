package session

import (
	"context"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// speculation is a prefetched snapshot for a URL the user is likely to open
// next. On a 1.2 s link, a link-follow that hits a speculation costs zero
// perceived round trips: the client already holds the document.
//
// Only same-origin <a href> navigations are speculated. Cloning arbitrary SPA
// state is unreliable, so JS-driven clicks are deliberately not attempted.
type speculation struct {
	mu      sync.Mutex
	byURL   map[string]time.Time
	pending bool
	last    time.Time
}

// prefetchBudget caps how much of the link speculation may consume.
const (
	maxSpeculations  = 5
	speculationTTL   = 3 * time.Minute
	prefetchCooldown = 4 * time.Second
)

func (s *Session) schedulePrefetch(tab uint32) {
	if !s.mgr.opts.Prefetch {
		return
	}
	s.mu.Lock()
	ts := s.tabs[tab]
	if ts == nil {
		s.mu.Unlock()
		return
	}
	if ts.spec == nil {
		ts.spec = &speculation{byURL: map[string]time.Time{}}
	}
	spec := ts.spec
	s.mu.Unlock()

	spec.mu.Lock()
	if spec.pending || time.Since(spec.last) < prefetchCooldown {
		spec.mu.Unlock()
		return
	}
	spec.pending = true
	spec.last = time.Now()
	spec.mu.Unlock()

	go func() {
		defer func() {
			spec.mu.Lock()
			spec.pending = false
			spec.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		s.runPrefetch(ctx, tab, spec)
	}()
}

func (s *Session) runPrefetch(ctx context.Context, tab uint32, spec *speculation) {
	t := s.Tab(tab)
	if t == nil {
		return
	}
	// Speculation is preemptible by definition: if the link is already busy
	// with real traffic, do not add to it.
	if s.queueDepth() > 4 {
		return
	}
	links, err := t.VisibleLinks(ctx, maxSpeculations)
	if err != nil || len(links) == 0 {
		return
	}
	spec.mu.Lock()
	fresh := make([]mirror.LinkHint, 0, len(links))
	for _, l := range links {
		if at, ok := spec.byURL[l.Href]; ok && time.Since(at) < speculationTTL {
			continue
		}
		fresh = append(fresh, l)
	}
	spec.mu.Unlock()
	if len(fresh) == 0 {
		return
	}

	// One hidden pooled tab does the work for all speculations in a round; the
	// alternative (a tab per candidate) costs far more landside RAM than the
	// bandwidth it saves.
	pooled, err := s.mgr.browser.NewPage(ctx, "about:blank")
	if err != nil {
		return
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.mgr.browser.CloseTarget(closeCtx, pooled.Target)
	}()

	collector := &specCollector{session: s, tab: tab}
	ptab, err := mirror.NewTab(ctx, tab, s.mgr.browser, pooled, collector, mirror.Options{
		Viewport: s.viewportCopy(), Logger: s.log,
	})
	if err != nil {
		return
	}
	for _, l := range fresh {
		if s.queueDepth() > 4 {
			return
		}
		collector.reset(l.Href)
		if err := ptab.Navigate(ctx, protocol.Navigate{URL: l.Href}); err != nil {
			continue
		}
		if !collector.wait(ctx, 20*time.Second) {
			continue
		}
		spec.mu.Lock()
		spec.byURL[l.Href] = time.Now()
		spec.mu.Unlock()
	}
}

func (s *Session) viewportCopy() protocol.Viewport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewport
}

func (s *Session) queueDepth() int {
	d := 0
	for _, q := range s.sendQ {
		d += len(q)
	}
	return d
}

// specCollector receives the pooled tab's frames and forwards only the first
// snapshot, tagged speculative and marked with the URL it belongs to.
type specCollector struct {
	session *Session
	tab     uint32

	mu   sync.Mutex
	url  string
	done chan struct{}
	sent bool
}

func (c *specCollector) reset(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.url = url
	c.sent = false
	c.done = make(chan struct{})
}

func (c *specCollector) wait(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	case <-ctx.Done():
		return false
	}
}

func (c *specCollector) EmitFrame(ch protocol.Channel, f *protocol.Frame) {
	if ch != protocol.ChDom || f.Type != protocol.TypeSnapshot {
		return
	}
	c.mu.Lock()
	if c.sent || c.url == "" {
		c.mu.Unlock()
		return
	}
	c.sent = true
	url := c.url
	done := c.done
	c.mu.Unlock()

	var snap protocol.Snapshot
	if err := f.DecodeBody(&snap); err != nil {
		close(done)
		return
	}
	snap.Speculative = true
	snap.URL = url
	// Speculations ride at bulk priority and are always preemptible by real
	// traffic; the budget guard is the queue-depth check in runPrefetch.
	c.session.Send(protocol.ChBulk, protocol.TypeSpeculative, c.tab, snap)
	close(done)
}

func (c *specCollector) WantImage(uint32, mirror.ImageRequest) {
	// Speculative pages do not get to spend the link on images.
}

// applySpeculation fast-forwards the real tab when the client navigates to a
// URL we already shipped. The client applies its cached snapshot immediately;
// the server still performs the real navigation so page state is authoritative.
func (s *Session) applySpeculation(ctx context.Context, tab uint32, url string) bool {
	if url == "" || !s.mgr.opts.Prefetch {
		return false
	}
	s.mu.Lock()
	ts := s.tabs[tab]
	s.mu.Unlock()
	if ts == nil || ts.spec == nil {
		return false
	}
	ts.spec.mu.Lock()
	at, ok := ts.spec.byURL[url]
	ts.spec.mu.Unlock()
	if !ok || time.Since(at) > speculationTTL {
		return false
	}
	// The client is already showing the speculated document; navigate the real
	// tab and let the resulting snapshot reconcile it.
	go func() {
		navCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if t := s.Tab(tab); t != nil {
			_ = t.Navigate(navCtx, protocol.Navigate{URL: url})
		}
	}()
	_ = ctx
	return true
}
