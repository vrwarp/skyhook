package imgproc

import (
	"container/list"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// Request is one image the mirror wants transcoded and shipped.
type Request struct {
	Tab      uint32
	Key      string
	URL      string
	W, H     int
	Alt      string
	Priority int // 0 = above the fold, ship immediately
	Node     int64
	Referer  string
	// Src carries the source bytes when there is nothing to fetch: a canvas or
	// a video frame exists only as pixels the landside browser was asked to
	// photograph, and no URL names it. Set, it replaces both fetch paths.
	Src []byte
	// Box places a region shot inside its element; see protocol.ImageMeta.Box.
	Box []int
}

// Fetcher retrieves image bytes through the landside browser, so an asset is
// fetched by the same client that rendered the page referencing it. The tab id
// says which browser tab to fetch on behalf of; limit bounds the read.
type Fetcher interface {
	FetchImage(ctx context.Context, tab uint32, url string, limit int) ([]byte, error)
}

// Delivery is how the pipeline hands results back to the session.
type Delivery interface {
	// ImageReady reports metadata (blurhash, dimensions) for a key.
	ImageReady(tab uint32, meta protocol.ImageMeta)
	// ImageBytes ships the encoded bytes.
	ImageBytes(tab uint32, data protocol.ImageData)
}

// Pipeline fetches, transcodes and caches images with a small worker pool.
// Above-the-fold images are pushed without being asked for; everything else
// waits for the client to say it does not already have that content hash, which
// is what makes a warm cross-flight cache pay off.
type Pipeline struct {
	tc        *Transcoder
	deliver   Delivery
	fetcher   Fetcher
	userAgent string
	log       *slog.Logger
	client    *http.Client
	cache     *diskCache

	hi   chan Request
	lo   chan Request
	stop chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	inFlight map[string]bool
	meta     map[string]protocol.ImageMeta
	metaAge  *list.List          // keys oldest-first, so meta can be bounded
	wanted   map[string][]uint32 // key -> tabs waiting for bytes
}

// metaMax bounds the metadata table.
//
// A page of ordinary images adds one entry per distinct asset and then stops.
// A canvas adds one per frame it was photographed at, for as long as the
// reader keeps playing — an afternoon of panning a map would otherwise grow
// this without any limit at all. Dropping the oldest entry costs a
// re-transcode if that exact image ever comes back, which for the entries
// this bound exists to drop — a board position from ten minutes ago — it will
// not.
const metaMax = 4096

// PipelineOptions configures the pipeline.
type PipelineOptions struct {
	Workers   int
	CacheDir  string
	CacheSize int64
	Transcode Options
	Logger    *slog.Logger
	// Client is the HTTP client used for the fallback fetch path; it must not
	// follow requests to the local network.
	Client *http.Client
	// Fetcher fetches through the landside browser. Nil means every fetch takes
	// the uncredentialed direct path, which is what the tests want.
	Fetcher Fetcher
	// UserAgent is sent on the direct path, so the fallback at least agrees
	// with the browser about who it is.
	UserAgent string
}

// NewPipeline starts the workers.
func NewPipeline(opts PipelineOptions, d Delivery) (*Pipeline, error) {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CacheSize == 0 {
		opts.CacheSize = 512 << 20
	}
	cl := opts.Client
	if cl == nil {
		cl = &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("imgproc: too many redirects")
				}
				return nil
			},
		}
	}
	cache, err := newDiskCache(opts.CacheDir, opts.CacheSize)
	if err != nil {
		return nil, err
	}
	p := &Pipeline{
		tc: New(opts.Transcode), deliver: d, fetcher: opts.Fetcher, userAgent: opts.UserAgent,
		log: opts.Logger, client: cl, cache: cache,
		hi: make(chan Request, 512), lo: make(chan Request, 4096),
		stop:     make(chan struct{}),
		inFlight: map[string]bool{},
		meta:     map[string]protocol.ImageMeta{},
		metaAge:  list.New(),
		wanted:   map[string][]uint32{},
	}
	for i := 0; i < opts.Workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p, nil
}

// Close stops the workers.
func (p *Pipeline) Close() {
	close(p.stop)
	p.wg.Wait()
}

// Submit queues a request, skipping duplicates and cache hits.
func (p *Pipeline) Submit(req Request) {
	if req.Key == "" || (req.URL == "" && len(req.Src) == 0) {
		return
	}
	p.mu.Lock()
	if meta, ok := p.meta[req.Key]; ok {
		p.mu.Unlock()
		// Already transcoded: the new node just needs to know the key exists.
		meta.Node = req.Node
		meta.Box = req.Box
		p.deliver.ImageReady(req.Tab, meta)
		if req.Priority == 0 {
			p.sendCached(req.Tab, req.Key)
		}
		return
	}
	if p.inFlight[req.Key] {
		p.mu.Unlock()
		return
	}
	p.inFlight[req.Key] = true
	p.mu.Unlock()

	q := p.lo
	if req.Priority == 0 {
		q = p.hi
	}
	select {
	case q <- req:
	default:
		// Queue full: drop the lowest-value work rather than block the mirror.
		p.mu.Lock()
		delete(p.inFlight, req.Key)
		p.mu.Unlock()
		p.log.Debug("image queue full, dropping", "key", req.Key)
	}
}

// Want handles a client cache miss: ship bytes for keys it does not have.
//
// Whether a key is finished is decided by the metadata table and not by
// looking in the cache, because the two questions race. A request that read
// the cache a moment before the bytes landed there, and joined the waiting
// list a moment after the worker had already read that list, would be answered
// by neither — and nothing here is ever asked for twice, so that asset would be
// missing from the page for the rest of the session.
//
// The window is microseconds wide and was found by reading rather than by
// reproducing it; what prompted the reading was one full-suite run where an
// icon font never arrived, on a machine running ten browsers, which has never
// happened again. That is evidence of nothing in particular and a good enough
// reason to close a hole that is free to close.
//
// Under the lock, meta and wanted are consistent with each other: the worker
// writes the cache before it publishes the metadata, so a key in meta is a key
// whose bytes are there to send.
func (p *Pipeline) Want(tab uint32, keys []string) {
	for _, k := range keys {
		p.mu.Lock()
		_, done := p.meta[k]
		if !done {
			p.wanted[k] = append(p.wanted[k], tab)
		}
		p.mu.Unlock()
		if done {
			p.sendCached(tab, k)
		}
	}
}

func (p *Pipeline) sendCached(tab uint32, key string) bool {
	data, mime, ok := p.cache.get(key)
	if !ok {
		return false
	}
	p.deliver.ImageBytes(tab, protocol.ImageData{Hash: key, Mime: mime, Data: data})
	return true
}

func (p *Pipeline) worker() {
	defer p.wg.Done()
	for {
		// Above-the-fold work always wins; a photo below the fold can wait for
		// a link that is measured in hundreds of kbps.
		select {
		case <-p.stop:
			return
		case req := <-p.hi:
			p.process(req)
			continue
		default:
		}
		select {
		case <-p.stop:
			return
		case req := <-p.hi:
			p.process(req)
		case req := <-p.lo:
			p.process(req)
		}
	}
}

func (p *Pipeline) process(req Request) {
	defer func() {
		p.mu.Lock()
		delete(p.inFlight, req.Key)
		p.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src, err := p.fetch(ctx, req)
	if err != nil {
		p.log.Debug("image fetch failed", "url", req.URL, "err", err)
		return
	}
	res, err := p.tc.Transcode(ctx, src, req.W, req.H)
	if err != nil {
		// A region shot has no URL to name it; the key is all there is to say
		// which image failed.
		p.log.Debug("image transcode failed", "url", req.URL, "key", req.Key, "err", err)
		return
	}
	meta := res.Meta(req.Key, req.Node, req.Priority)
	meta.Alt = req.Alt
	meta.Box = req.Box
	// The cache first, then the metadata, and both before the waiting list is
	// taken: a client asking for this key sees it as finished only once the
	// bytes it would be answered with are actually there. See Want.
	p.cache.put(req.Key, res.Data, res.Mime)
	p.mu.Lock()
	p.remember(req.Key, meta)
	waiting := p.wanted[req.Key]
	delete(p.wanted, req.Key)
	p.mu.Unlock()

	p.deliver.ImageReady(req.Tab, meta)
	if req.Priority == 0 {
		p.deliver.ImageBytes(req.Tab, protocol.ImageData{Hash: req.Key, Mime: res.Mime, Data: res.Data})
	}
	for _, tab := range waiting {
		p.deliver.ImageBytes(tab, protocol.ImageData{Hash: req.Key, Mime: res.Mime, Data: res.Data})
	}
}

// remember records a key's metadata, dropping the oldest once past metaMax.
// Callers hold p.mu.
func (p *Pipeline) remember(key string, meta protocol.ImageMeta) {
	if _, dup := p.meta[key]; !dup {
		p.metaAge.PushBack(key)
	}
	p.meta[key] = meta
	for p.metaAge.Len() > metaMax {
		old := p.metaAge.Front()
		p.metaAge.Remove(old)
		delete(p.meta, old.Value.(string))
	}
}

// dataURL decodes an inline image. The agent leaves small ones in the document
// and sends the large ones here to be transcoded, which is the whole point —
// but there is nothing to fetch, and an HTTP client asked to GET one only says
// it has never heard of the scheme.
func dataURL(raw string) ([]byte, bool) {
	if !strings.HasPrefix(raw, "data:") {
		return nil, false
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return nil, false
	}
	meta, payload := raw[len("data:"):comma], raw[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		// Percent-encoded, which is how inline SVG usually travels.
		s, err := url.PathUnescape(payload)
		if err != nil {
			return nil, false
		}
		return []byte(s), true
	}
	// Base64 in a URL may be padded or not, and may carry whitespace.
	b64 := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, payload)
	dec, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(strings.TrimRight(b64, "="))
	if err != nil {
		return nil, false
	}
	return dec, true
}

// fetch gets the source bytes, preferring the browser that asked for them.
//
// The browser is not just the more convenient client here, it is the only
// correct one: it already holds the connection, the cookie jar, the client
// hints and the TLS fingerprint the origin associates with this user. A
// second client fetching the same asset alongside it is a different visitor
// arriving with the same session, which is precisely the shape of a stolen
// cookie.
//
// The direct path remains for what the browser cannot be asked — an asset
// whose tab has closed, a build with no loadNetworkResource, and the tests.
// It sends no credentials, so an asset that needs a login simply fails there
// rather than leaking one.
func (p *Pipeline) fetch(ctx context.Context, req Request) ([]byte, error) {
	// A region shot arrives with its pixels: the landside browser has already
	// been asked to photograph a canvas that no URL names.
	if len(req.Src) > 0 {
		return req.Src, nil
	}
	// An inline image is already here. Neither path can do anything with one:
	// there is no request to make, and both would report that they have never
	// heard of the scheme.
	if data, ok := dataURL(req.URL); ok {
		if len(data) == 0 {
			return nil, errors.New("imgproc: empty data url")
		}
		return data, nil
	}
	limit := p.tc.opts.MaxBytes + 1
	if p.fetcher != nil {
		data, err := p.fetcher.FetchImage(ctx, req.Tab, req.URL, limit)
		if err == nil {
			return data, nil
		}
		p.log.Debug("browser image fetch failed, trying direct", "url", req.URL, "err", err)
	}
	return p.fetchDirect(ctx, req, limit)
}

func (p *Pipeline) fetchDirect(ctx context.Context, req Request, limit int) ([]byte, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, err
	}
	if req.Referer != "" {
		hreq.Header.Set("Referer", req.Referer)
	}
	hreq.Header.Set("Accept", "image/avif,image/webp,image/png,image/*;q=0.8,*/*;q=0.5")
	if p.userAgent != "" {
		hreq.Header.Set("User-Agent", p.userAgent)
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("imgproc: http " + resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
}

// diskCache is a bounded content-addressed store shared by all sessions.
type diskCache struct {
	dir   string
	limit int64

	mu    sync.Mutex
	size  int64
	order *list.List
	index map[string]*list.Element
}

type cacheEntry struct {
	key  string
	mime string
	size int64
}

func newDiskCache(dir string, limit int64) (*diskCache, error) {
	c := &diskCache{dir: dir, limit: limit, order: list.New(), index: map[string]*list.Element{}}
	if dir == "" {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		key := e.Name()
		el := c.order.PushBack(&cacheEntry{key: key, size: info.Size(), mime: mimeFor(key)})
		c.index[key] = el
		c.size += info.Size()
	}
	c.evict()
	return c, nil
}

func (c *diskCache) path(key string) string { return filepath.Join(c.dir, key) }

func (c *diskCache) get(key string) ([]byte, string, bool) {
	if c.dir == "" {
		return nil, "", false
	}
	c.mu.Lock()
	el, ok := c.index[key]
	if ok {
		c.order.MoveToBack(el)
	}
	c.mu.Unlock()
	if !ok {
		return nil, "", false
	}
	data, err := os.ReadFile(c.path(key)) //nolint:gosec // key is a hex hash
	if err != nil {
		return nil, "", false
	}
	mime := el.Value.(*cacheEntry).mime
	if mime == "" {
		// Recovered from disk at startup: the type lives in the bytes.
		mime = Sniff(data)
	}
	return data, mime, true
}

func (c *diskCache) put(key string, data []byte, mime string) {
	if c.dir == "" {
		return
	}
	if err := os.WriteFile(c.path(key), data, 0o600); err != nil { //nolint:gosec // key is a hex hash
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		c.size -= el.Value.(*cacheEntry).size
		c.order.Remove(el)
	}
	el := c.order.PushBack(&cacheEntry{key: key, size: int64(len(data)), mime: mime})
	c.index[key] = el
	c.size += int64(len(data))
	c.evictLocked()
}

func (c *diskCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictLocked()
}

func (c *diskCache) evictLocked() {
	for c.size > c.limit && c.order.Len() > 0 {
		el := c.order.Front()
		ent := el.Value.(*cacheEntry)
		c.order.Remove(el)
		delete(c.index, ent.key)
		c.size -= ent.size
		_ = os.Remove(c.path(ent.key))
	}
}

// mimeFor guesses a cached entry's type from its stored bytes' magic on read;
// the key itself carries no extension, so entries recovered at startup are
// re-sniffed lazily.
func mimeFor(string) string { return "" }

// Sniff reports the image type of a byte slice, used when a cache entry was
// recovered from disk without its metadata.
func Sniff(data []byte) string {
	switch {
	case len(data) > 12 && string(data[4:12]) == "ftypavif":
		return "image/avif"
	case len(data) > 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) > 3 && data[0] == 0xff && data[1] == 0xd8:
		return "image/jpeg"
	case len(data) > 8 && string(data[1:4]) == "PNG":
		return "image/png"
	case len(data) > 3 && string(data[0:3]) == "GIF":
		return "image/gif"
	}
	return "application/octet-stream"
}

// SniffFont names a font's container, or "" for anything that is not one.
//
// The magic numbers are the whole test: a font reaches the image pipeline
// because it was named by a url() in a stylesheet, exactly as a background
// image is, and by then nothing but the bytes says which it was. The content
// type is worth carrying because it rides all the way to the blob the mirror
// frame loads the font from.
func SniffFont(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	switch string(data[0:4]) {
	case "wOFF":
		return "font/woff"
	case "wOF2":
		return "font/woff2"
	case "OTTO":
		return "font/otf"
	case "ttcf":
		return "font/collection"
	case "true", "typ1":
		return "font/ttf"
	case "\x00\x01\x00\x00":
		return "font/ttf"
	}
	return ""
}
