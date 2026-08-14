package protocol

import (
	"bytes"
	"errors"
	"hash/crc32"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// DictTrainer accumulates recent frame payloads per origin and trains zstd
// dictionaries from them. Minified-class-heavy DOMs (Google apps) compress
// several times better with an origin-trained dictionary than without.
//
// Dictionaries are only used on the wire once the peer has acknowledged the
// dictionary id, so training can run whenever it likes.
type DictTrainer struct {
	mu sync.Mutex
	// samples per origin, bounded in count and total bytes.
	samples   map[string][][]byte
	bytes     map[string]int
	MaxSample int // per-sample cap
	MaxBytes  int // per-origin cap
	MaxCount  int // per-origin cap
	DictSize  int // trained dictionary size
}

// NewDictTrainer returns a trainer with defaults sized for a personal VPS.
func NewDictTrainer() *DictTrainer {
	return &DictTrainer{
		samples:   map[string][][]byte{},
		bytes:     map[string]int{},
		MaxSample: 256 << 10,
		MaxBytes:  16 << 20,
		MaxCount:  4096,
		DictSize:  110 << 10,
	}
}

// Observe records a payload sample for an origin.
func (t *DictTrainer) Observe(origin string, payload []byte) {
	if origin == "" || len(payload) == 0 {
		return
	}
	if len(payload) > t.MaxSample {
		payload = payload[:t.MaxSample]
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples[origin] = append(t.samples[origin], cp)
	t.bytes[origin] += len(cp)
	// Drop oldest samples when over budget.
	for len(t.samples[origin]) > t.MaxCount || t.bytes[origin] > t.MaxBytes {
		drop := t.samples[origin][0]
		t.samples[origin] = t.samples[origin][1:]
		t.bytes[origin] -= len(drop)
	}
}

// ErrNotEnoughSamples means training would produce a useless dictionary.
var ErrNotEnoughSamples = errors.New("protocol: not enough samples to train")

// Train builds a dictionary for an origin. The returned id is a checksum of the
// dictionary bytes so both ends can identify it without a registry.
//
// The dictionary *content* is chosen with a recency heuristic rather than
// zstd's COVER algorithm (which the pure-Go encoder does not implement): the
// most recent samples are concatenated, newest last, and the tail is kept.
// For a mirror stream this is close to optimal anyway, because consecutive
// frames from one origin share their class names, attribute names and
// structural boilerplate almost verbatim.
func (t *DictTrainer) Train(origin string) (id uint32, dict []byte, err error) {
	t.mu.Lock()
	src := make([][]byte, len(t.samples[origin]))
	copy(src, t.samples[origin])
	t.mu.Unlock()

	if len(src) < 8 {
		return 0, nil, ErrNotEnoughSamples
	}
	var total int
	for _, s := range src {
		total += len(s)
	}
	if total < 64<<10 {
		return 0, nil, ErrNotEnoughSamples
	}

	var content bytes.Buffer
	content.Grow(t.DictSize)
	for _, s := range src {
		content.Write(s)
	}
	raw := content.Bytes()
	if len(raw) > t.DictSize {
		raw = raw[len(raw)-t.DictSize:]
	}

	dict, err = zstd.BuildDict(zstd.BuildDictOptions{
		ID:       crc32.ChecksumIEEE([]byte(origin)) | 1,
		Contents: src,
		History:  raw,
		Level:    zstd.SpeedDefault,
	})
	if err != nil {
		return 0, nil, err
	}
	return crc32.ChecksumIEEE(dict) | 1, dict, nil
}

// Origins lists origins with samples, most sampled first.
func (t *DictTrainer) Origins() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.samples))
	for o := range t.samples {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return t.bytes[out[i]] > t.bytes[out[j]] })
	return out
}

// Reset drops all samples for an origin (called after a successful train).
func (t *DictTrainer) Reset(origin string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.samples, origin)
	delete(t.bytes, origin)
}
