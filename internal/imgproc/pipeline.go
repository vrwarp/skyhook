package imgproc

import (
	"container/list"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	Cookies  string
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
	tc      *Transcoder
	deliver Delivery
	log     *slog.Logger
	client  *http.Client
	cache   *diskCache

	hi   chan Request
	lo   chan Request
	stop chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	inFlight map[string]bool
	meta     map[string]protocol.ImageMeta
	wanted   map[string][]uint32 // key -> tabs waiting for bytes
}

// PipelineOptions configures the pipeline.
type PipelineOptions struct {
	Workers   int
	CacheDir  string
	CacheSize int64
	Transcode Options
	Logger    *slog.Logger
	// Client is the HTTP client used for fetches; it must not follow requests
	// to the local network.
	Client *http.Client
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
		tc: New(opts.Transcode), deliver: d, log: opts.Logger, client: cl, cache: cache,
		hi: make(chan Request, 512), lo: make(chan Request, 4096),
		stop:     make(chan struct{}),
		inFlight: map[string]bool{},
		meta:     map[string]protocol.ImageMeta{},
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
	if req.Key == "" || req.URL == "" {
		return
	}
	p.mu.Lock()
	if meta, ok := p.meta[req.Key]; ok {
		p.mu.Unlock()
		// Already transcoded: the new node just needs to know the key exists.
		meta.Node = req.Node
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
func (p *Pipeline) Want(tab uint32, keys []string) {
	for _, k := range keys {
		if !p.sendCached(tab, k) {
			p.mu.Lock()
			p.wanted[k] = append(p.wanted[k], tab)
			p.mu.Unlock()
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
		p.log.Debug("image transcode failed", "url", req.URL, "err", err)
		return
	}
	meta := res.Meta(req.Key, req.Node, req.Priority)
	meta.Alt = req.Alt
	p.mu.Lock()
	p.meta[req.Key] = meta
	waiting := p.wanted[req.Key]
	delete(p.wanted, req.Key)
	p.mu.Unlock()

	p.cache.put(req.Key, res.Data, res.Mime)
	p.deliver.ImageReady(req.Tab, meta)
	if req.Priority == 0 {
		p.deliver.ImageBytes(req.Tab, protocol.ImageData{Hash: req.Key, Mime: res.Mime, Data: res.Data})
	}
	for _, tab := range waiting {
		p.deliver.ImageBytes(tab, protocol.ImageData{Hash: req.Key, Mime: res.Mime, Data: res.Data})
	}
}

func (p *Pipeline) fetch(ctx context.Context, req Request) ([]byte, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, err
	}
	// Fetch as the page would: same referer, same cookies, so authenticated
	// avatars and attachments resolve.
	if req.Referer != "" {
		hreq.Header.Set("Referer", req.Referer)
	}
	if req.Cookies != "" {
		hreq.Header.Set("Cookie", req.Cookies)
	}
	hreq.Header.Set("Accept", "image/avif,image/webp,image/png,image/*;q=0.8,*/*;q=0.5")
	hreq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("imgproc: http " + resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, int64(p.tc.opts.MaxBytes)+1))
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
