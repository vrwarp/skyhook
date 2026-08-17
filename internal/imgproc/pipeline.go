package imgproc

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

/*
Rasterizer turns bytes this process cannot decode into bytes it can, by asking
the landside browser — which has already decoded them once to paint the page
that named them — to hand back the pixels as a PNG.

The alternative was a decoder per format, and it is the wrong trade twice over.
Every one of them is a C library behind cgo, so the price of reading AVIF would
be a build that no longer runs `go test ./...` on a machine without libavif —
the exact cost §7 of the implementation notes went out of its way not to pay for
*encoding*. And the format list never closes: JPEG XL is already shipping and
whatever follows it will arrive the same way, as a site that serves one format
and no fallback.

Chromium is here, it is attached, and it holds a decoder for every format the
web has ever agreed on. Sending it the bytes costs one round trip on a machine
that has nothing else to do, and it costs it once, because the result is
transcoded and cached under the same content hash as everything else.

w and h are the box the page lays the image out in; the browser scales into it
before handing the pixels back, which is what keeps a 4000px hero from crossing
the CDP socket as an uncompressed PNG. Zero means "natural size".
*/
type Rasterizer interface {
	RasterizeImage(ctx context.Context, tab uint32, src []byte, w, h int) ([]byte, error)
}

/*
ErrEmptyResource means a fetch reported success and returned nothing.

It is a fetch failure and used to be reported as a decode one. `loadNetworkResource`
answers a request it could not read a body for with success and no stream, and
`FetchResource` passes that on as the empty resource it honestly is — but the
zero bytes then reached the codecs, where "no bytes" and "bytes in a format I
do not know" are the same answer, and the log said `image: unknown format`.
That sentence sends an operator looking for a missing codec for an asset that
never arrived at all.
*/
var ErrEmptyResource = errors.New("imgproc: the fetch succeeded and returned no bytes")

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
	raster    Rasterizer
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
	// Rasterizer decodes what this process cannot, through the landside
	// browser. Nil means a format Go has no decoder for stays undecoded, which
	// is the behaviour every build had before there was one.
	Rasterizer Rasterizer
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
		tc: New(opts.Transcode), deliver: d, fetcher: opts.Fetcher, raster: opts.Rasterizer,
		userAgent: opts.UserAgent,
		log:       opts.Logger, client: cl, cache: cache,
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
	p.warm(req.Key)
	p.forgetEvicted(req.Key)
	p.mu.Lock()
	if meta, ok := p.meta[req.Key]; ok {
		p.mu.Unlock()
		// Already transcoded: the new node just needs to know the key exists.
		meta.Node = req.Node
		meta.Box = req.Box
		// Above the fold the bytes go now, and they have to actually go: an
		// announcement the cache cannot answer is the silence this whole change
		// is about, and here — unlike in Want — there is a request in hand to
		// do the work again with.
		if req.Priority == 0 && !p.sendCached(req.Tab, req.Key) {
			p.forget(req.Key)
			p.queue(req)
			return
		}
		p.deliver.ImageReady(req.Tab, meta)
		return
	}
	p.mu.Unlock()
	p.queue(req)
}

// queue puts one request in front of the workers, unless it is already there.
func (p *Pipeline) queue(req Request) {
	p.mu.Lock()
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
		// The client is told, because it is already holding a placeholder for
		// this key and will not ask a second time.
		p.mu.Lock()
		delete(p.inFlight, req.Key)
		p.mu.Unlock()
		p.abandon(req, errors.New("imgproc: the queue was full"))
	}
}

/*
warm publishes what the disk cache already knows about a key.

The cache holds bytes across restarts and the metadata table does not, so
without this the two disagree from the moment the process starts: every reader
of the cache asks the table first, the table is empty, and a directory full of
already-transcoded assets is re-fetched from the origin and re-encoded one by
one. Reading the header is one small file read, and only ever for a key the
index already names.
*/
func (p *Pipeline) warm(key string) {
	p.mu.Lock()
	_, known := p.meta[key]
	p.mu.Unlock()
	if known {
		return
	}
	head, n, ok := p.cache.header(key)
	if !ok {
		return
	}
	p.mu.Lock()
	// Checked again under the lock: a worker may have published the real thing
	// while the header was being read, and that one knows the request it came
	// from.
	if _, raced := p.meta[key]; !raced {
		p.remember(key, protocol.ImageMeta{
			Hash: key, W: head.W, H: head.H, Blur: head.Blur, Mime: head.Mime, Bytes: n,
		})
	}
	p.mu.Unlock()
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
		// Both of these settle before the lock is taken, so the decision made
		// under it is still the single atomic one this comment is about.
		p.warm(k)
		if p.forgetEvicted(k) {
			// The table called this finished and the bytes have since been
			// evicted. Unlike Submit there is no request here to do the work
			// again with, so the honest answer is that it is not coming; the
			// next snapshot submits the key afresh, and the client takes the
			// good metadata when it arrives.
			p.deliver.ImageReady(tab, protocol.ImageMeta{Hash: k, Missing: true})
			continue
		}
		p.mu.Lock()
		_, done := p.meta[k]
		if !done {
			p.wanted[k] = append(p.wanted[k], tab)
		}
		p.mu.Unlock()
		if !done {
			continue
		}
		if !p.sendCached(tab, k) {
			// The bytes went out from under the table between the two. Nothing
			// will submit this key again on its own, and the client has spent
			// its one question on it.
			p.forget(k)
			p.deliver.ImageReady(tab, protocol.ImageMeta{Hash: k, Missing: true})
		}
	}
}

// forgetEvicted drops metadata for a key whose bytes the cache has evicted, so
// it is re-transcoded rather than announced as an asset nothing can send, and
// reports whether it dropped one. The two are bounded differently — the table
// by a count, the cache by a size — so they part company on any page busy
// enough to fill either.
func (p *Pipeline) forgetEvicted(key string) bool {
	p.mu.Lock()
	_, known := p.meta[key]
	p.mu.Unlock()
	if !known || p.cache.has(key) {
		return false
	}
	p.forget(key)
	return true
}

func (p *Pipeline) forget(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.meta, key)
	for el := p.metaAge.Front(); el != nil; el = el.Next() {
		if el.Value.(string) == key {
			p.metaAge.Remove(el)
			break
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
		p.abandon(req, err)
		return
	}
	res, err := p.tc.Transcode(ctx, src, req.W, req.H)
	if errors.Is(err, ErrNoDecoder) {
		res, err = p.rasterize(ctx, req, src, err)
	}
	if err != nil {
		// A region shot has no URL to name it; the key is all there is to say
		// which image failed.
		p.log.Debug("image transcode failed", "url", req.URL, "key", req.Key, "err", err)
		p.abandon(req, err)
		return
	}
	meta := res.Meta(req.Key, req.Node, req.Priority)
	meta.Alt = req.Alt
	meta.Box = req.Box
	// The cache first, then the metadata, and both before the waiting list is
	// taken: a client asking for this key sees it as finished only once the
	// bytes it would be answered with are actually there. See Want.
	p.cache.put(req.Key, res.Data, cacheHeader{
		W: res.W, H: res.H, Blur: res.Blurhash, Mime: res.Mime,
	})
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

/*
abandon says an asset is not coming, to everyone still holding a space for it.

Every failure here used to end at a log line. The key was never announced, the
waiting list was never emptied, and the client — which asks for a hash exactly
once — went on holding a transparent pixel where the picture was, for as long
as the session lasted. One missing codec on a site that serves one format made
that the whole page, which is how it was finally noticed; but a 403, a redirect
to a login, an oversized font and a full queue had all been doing it quietly
for as long as there had been an image pipeline.

The metadata table is deliberately not written. A key with no entry is a key a
later snapshot will submit again, and re-fetching a failure on the next resync
is the only second chance anything here has; recording the failure would take
that away in exchange for nothing.
*/
func (p *Pipeline) abandon(req Request, cause error) {
	p.mu.Lock()
	waiting := p.wanted[req.Key]
	delete(p.wanted, req.Key)
	p.mu.Unlock()

	// Alt is the whole of what is left to show, so it travels even though
	// nothing else about the asset was ever learned.
	meta := protocol.ImageMeta{Node: req.Node, Hash: req.Key, Alt: req.Alt, Missing: true}
	told := map[uint32]bool{req.Tab: true}
	p.deliver.ImageReady(req.Tab, meta)
	for _, tab := range waiting {
		if told[tab] {
			continue
		}
		told[tab] = true
		p.deliver.ImageReady(tab, protocol.ImageMeta{Hash: req.Key, Missing: true})
	}
	p.log.Debug("image abandoned", "url", req.URL, "key", req.Key,
		"tabs", len(told), "err", cause)
}

// rasterize takes the second run at a format this process cannot read, through
// the browser that can, and then transcodes the pixels it gets back exactly as
// if they had arrived that way.
//
// The failure is carried through rather than replaced: a build with no
// rasterizer, and a browser that cannot decode it either, should both still say
// which format was the problem — that is the sentence in the log that tells an
// operator whether to expect this on every page of that site or only this one.
func (p *Pipeline) rasterize(ctx context.Context, req Request, src []byte, cause error) (*Result, error) {
	if p.raster == nil {
		return nil, cause
	}
	pixels, err := p.raster.RasterizeImage(ctx, req.Tab, src, req.W, req.H)
	if err != nil {
		return nil, fmt.Errorf("%w (the browser could not read it either: %v)", cause, err)
	}
	res, err := p.tc.Transcode(ctx, pixels, req.W, req.H)
	if err != nil {
		return nil, err
	}
	p.log.Debug("image decoded by the landside browser",
		"url", req.URL, "key", req.Key, "was", cause, "bytes", len(res.Data))
	return res, nil
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
		if err == nil && len(data) > 0 {
			return data, nil
		}
		if err == nil {
			err = ErrEmptyResource
		}
		p.log.Debug("browser image fetch failed, trying direct", "url", req.URL, "err", err)
	}
	data, err := p.fetchDirect(ctx, req, limit)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrEmptyResource
	}
	return data, nil
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

/*
diskCache is a bounded content-addressed store shared by all sessions.

Each entry carries its own description. That is the difference between a cache
that survives a restart and one that only looks like it does: bytes alone are
not enough to answer a request, because announcing an asset means naming its
size, its type and its blurhash, and those existed only in a map in memory. A
restarted server therefore indexed five hundred megabytes it could never quote
from, and re-fetched and re-transcoded every asset on every page — the whole
cache reduced to a ledger for its own eviction.

The description is a JSON header behind a magic prefix, ahead of the bytes in
the same file. One file per entry keeps eviction exactly as it was, and the
prefix means an entry written by an older build is recognisably not one of
these and is dropped rather than misread.
*/
type diskCache struct {
	dir   string
	limit int64

	mu    sync.Mutex
	size  int64
	order *list.List
	index map[string]*list.Element
}

// cacheMagic marks a file written with a header. Anything else in the
// directory is from a build that stored bare bytes and cannot be quoted.
const cacheMagic = "SKYC1"

// cacheHeader is what an entry knows about itself.
//
// Only what belongs to the asset. A node id, a placement box and a priority
// describe the request that happened to want it, not the bytes, and the same
// bytes serve a different element on the next page.
type cacheHeader struct {
	W    int    `json:"w,omitempty"`
	H    int    `json:"h,omitempty"`
	Blur string `json:"blur,omitempty"`
	Mime string `json:"mime,omitempty"`
}

type cacheEntry struct {
	key  string
	size int64
	// head is the parsed header, read on first use rather than at startup:
	// indexing ten thousand entries should cost a directory listing, not ten
	// thousand file reads.
	head   cacheHeader
	bytes  int
	loaded bool
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
		el := c.order.PushBack(&cacheEntry{key: key, size: info.Size()})
		c.index[key] = el
		c.size += info.Size()
	}
	c.evict()
	return c, nil
}

func (c *diskCache) path(key string) string { return filepath.Join(c.dir, key) }

// has reports whether the key is in the index, without touching the disk. It
// is what stops a key whose bytes have been evicted from being announced as
// though they were still there.
func (c *diskCache) has(key string) bool {
	if c.dir == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[key]
	return ok
}

// read returns an entry's header and bytes, parsing the header once.
func (c *diskCache) read(key string) (cacheHeader, []byte, bool) {
	if c.dir == "" {
		return cacheHeader{}, nil, false
	}
	c.mu.Lock()
	el, ok := c.index[key]
	if ok {
		c.order.MoveToBack(el)
	}
	c.mu.Unlock()
	if !ok {
		return cacheHeader{}, nil, false
	}
	raw, err := os.ReadFile(c.path(key)) //nolint:gosec // key is a hex hash
	if err != nil {
		c.drop(key)
		return cacheHeader{}, nil, false
	}
	head, data, ok := splitCacheFile(raw)
	if !ok {
		// Written by a build that stored bare bytes, or truncated by a full
		// disk. Either way it cannot be quoted, and leaving it costs the space
		// it occupies for as long as the directory lives.
		c.drop(key)
		return cacheHeader{}, nil, false
	}
	c.mu.Lock()
	if ent, still := c.index[key]; still {
		e := ent.Value.(*cacheEntry)
		e.head, e.bytes, e.loaded = head, len(data), true
	}
	c.mu.Unlock()
	return head, data, true
}

// header returns what an entry says about itself, reading the file only the
// first time and only for a key the index already holds.
func (c *diskCache) header(key string) (cacheHeader, int, bool) {
	if c.dir == "" {
		return cacheHeader{}, 0, false
	}
	c.mu.Lock()
	el, ok := c.index[key]
	if ok {
		if e := el.Value.(*cacheEntry); e.loaded {
			head, n := e.head, e.bytes
			c.mu.Unlock()
			return head, n, true
		}
	}
	c.mu.Unlock()
	if !ok {
		return cacheHeader{}, 0, false
	}
	head, data, ok := c.read(key)
	if !ok {
		return cacheHeader{}, 0, false
	}
	return head, len(data), true
}

func (c *diskCache) get(key string) ([]byte, string, bool) {
	head, data, ok := c.read(key)
	if !ok {
		return nil, "", false
	}
	mime := head.Mime
	if mime == "" {
		mime = Sniff(data)
	}
	return data, mime, true
}

// splitCacheFile separates the header from the bytes.
func splitCacheFile(raw []byte) (cacheHeader, []byte, bool) {
	if !bytes.HasPrefix(raw, []byte(cacheMagic)) {
		return cacheHeader{}, nil, false
	}
	rest := raw[len(cacheMagic):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return cacheHeader{}, nil, false
	}
	var head cacheHeader
	if err := json.Unmarshal(rest[:nl], &head); err != nil {
		return cacheHeader{}, nil, false
	}
	return head, rest[nl+1:], true
}

// drop forgets an entry and removes its file.
func (c *diskCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return
	}
	c.size -= el.Value.(*cacheEntry).size
	c.order.Remove(el)
	delete(c.index, key)
	_ = os.Remove(c.path(key))
}

func (c *diskCache) put(key string, data []byte, head cacheHeader) {
	if c.dir == "" {
		return
	}
	desc, err := json.Marshal(head)
	if err != nil {
		return
	}
	raw := make([]byte, 0, len(cacheMagic)+len(desc)+1+len(data))
	raw = append(raw, cacheMagic...)
	raw = append(raw, desc...)
	raw = append(raw, '\n')
	raw = append(raw, data...)
	if err := os.WriteFile(c.path(key), raw, 0o600); err != nil { //nolint:gosec // key is a hex hash
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		c.size -= el.Value.(*cacheEntry).size
		c.order.Remove(el)
	}
	el := c.order.PushBack(&cacheEntry{
		key: key, size: int64(len(raw)), head: head, bytes: len(data), loaded: true,
	})
	c.index[key] = el
	c.size += int64(len(raw))
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

/*
Sniff reports the type of a byte slice the transcoder produced.

It has to know every type Transcode can emit, and it did not: an SVG passes
through as markup and a webfont passes through as itself, and both came back
from here as `application/octet-stream`. That answer rides all the way to the
`content-type` the client stores the bytes under, and from there to the type of
the Blob it mints a URL from — where, for an SVG handed to an `<img>`, it is
load-bearing. A browser will not sniff its way past the wrong type on a blob
URL, so the picture is simply not drawn.

Nothing reached this until the disk cache became readable again, which is what
makes it worth stating plainly: the fallback that had never run was wrong, and
would have blanked every logo on the page the moment it did.
*/
func Sniff(data []byte) string {
	// A font is named by four exact bytes, which is cheaper and surer than
	// anything below, and it is not a picture at all.
	if mime := SniffFont(data); mime != "" {
		return mime
	}
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
	// Last, because it is the only one that reads rather than matches: a
	// vector image has no magic number, only a root element somewhere near the
	// front.
	if looksLikeSVG(data) {
		return "image/svg+xml"
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
