package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"
)

// Message header layout, identical on both transports:
//
//	byte 0      channel
//	byte 1      codec (0 = raw CBOR, 1 = zstd, 2 = zstd + dictionary)
//	bytes 2..5  dictionary id, little endian, only when codec == 2
//	rest        payload
//
// Length framing is the transport's job: WebSocket messages and WebTransport
// length-prefixed records both carry exact boundaries.
const (
	CodecRaw      uint8 = 0
	CodecZstd     uint8 = 1
	CodecZstdDict uint8 = 2

	headerLen     = 2
	headerLenDict = 6
	// MaxMessage caps a single decoded message. Snapshots of pathological pages
	// are rejected rather than allowed to exhaust client memory.
	MaxMessage = 64 << 20
)

var (
	// ErrShortMessage means the framing header was truncated.
	ErrShortMessage = errors.New("protocol: short message")
	// ErrUnknownCodec means the peer used a codec we did not advertise.
	ErrUnknownCodec = errors.New("protocol: unknown codec")
	// ErrTooLarge means a decoded message exceeded MaxMessage.
	ErrTooLarge = errors.New("protocol: message too large")
)

var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	var err error
	// Deterministic encoding keeps frame bytes stable, which matters for the
	// replay ring buffer and for content-hashing frames in tests.
	encMode, err = cbor.EncOptions{
		Sort:          cbor.SortCanonical,
		ShortestFloat: cbor.ShortestFloat16,
		NaNConvert:    cbor.NaNConvert7e00,
		InfConvert:    cbor.InfConvertFloat16,
		IndefLength:   cbor.IndefLengthForbidden,
	}.EncMode()
	if err != nil {
		panic(err)
	}
	decMode, err = cbor.DecOptions{
		MaxArrayElements: 20_000_000,
		MaxMapPairs:      1_000_000,
		MaxNestedLevels:  256,
	}.DecMode()
	if err != nil {
		panic(err)
	}
}

// Marshal encodes a value as canonical CBOR.
func Marshal(v any) ([]byte, error) { return encMode.Marshal(v) }

// Unmarshal decodes canonical CBOR.
func Unmarshal(b []byte, v any) error { return decMode.Unmarshal(b, v) }

// MustMarshal panics on encoding failure; used for statically-correct bodies.
func MustMarshal(v any) cbor.RawMessage {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return cbor.RawMessage(b)
}

// NewFrame builds a frame with an encoded body.
func NewFrame(t Type, tab uint32, body any) (*Frame, error) {
	f := &Frame{Type: t, Tab: tab}
	if body != nil {
		b, err := Marshal(body)
		if err != nil {
			return nil, err
		}
		f.Body = b
	}
	return f, nil
}

// DecodeBody decodes a frame body into v.
func (f *Frame) DecodeBody(v any) error {
	if len(f.Body) == 0 {
		return nil
	}
	return Unmarshal(f.Body, v)
}

// Codec compresses and decompresses wire messages. It is safe for concurrent
// use: the underlying zstd encoder and decoder are goroutine-safe for the
// EncodeAll/DecodeAll APIs.
type Codec struct {
	mu sync.RWMutex

	enc        *zstd.Encoder
	dec        *zstd.Decoder
	dicts      map[uint32]*dictPair
	useZstd    bool
	useDict    bool
	activeDict uint32
	// MinCompress is the payload size below which compression is skipped; tiny
	// frames get bigger with a zstd header.
	MinCompress int
}

type dictPair struct {
	id   uint32
	data []byte
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// NewCodec builds a codec. level 0 selects a latency-friendly default.
func NewCodec(useZstd bool, level int) (*Codec, error) {
	c := &Codec{useZstd: useZstd, dicts: map[uint32]*dictPair{}, MinCompress: 96}
	if !useZstd {
		return c, nil
	}
	el := zstd.SpeedDefault
	switch {
	case level < 0:
		el = zstd.SpeedFastest
	case level > 0:
		el = zstd.SpeedBetterCompression
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(el),
		zstd.WithEncoderConcurrency(2),
		zstd.WithWindowSize(1<<20),
	)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(2),
		zstd.WithDecoderMaxMemory(MaxMessage),
	)
	if err != nil {
		return nil, err
	}
	c.enc, c.dec = enc, dec
	return c, nil
}

// Close releases codec resources.
func (c *Codec) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc != nil {
		_ = c.enc.Close()
	}
	if c.dec != nil {
		c.dec.Close()
	}
	for _, d := range c.dicts {
		_ = d.enc.Close()
		d.dec.Close()
	}
	c.dicts = map[uint32]*dictPair{}
}

// AddDict registers a trained dictionary. Both ends must hold it before it is
// referenced on the wire; the server ships dictionaries on the bulk channel and
// only starts using one after the client acknowledges it.
func (c *Codec) AddDict(id uint32, data []byte) error {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderDict(data),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderDicts(data),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(MaxMessage),
	)
	if err != nil {
		_ = enc.Close()
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.dicts[id]; ok {
		_ = old.enc.Close()
		old.dec.Close()
	}
	c.dicts[id] = &dictPair{id: id, data: data, enc: enc, dec: dec}
	return nil
}

// EnableDict selects the dictionary used for outgoing messages. Zero disables.
func (c *Codec) EnableDict(id uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == 0 {
		c.useDict = false
		c.activeDict = 0
		return nil
	}
	if _, ok := c.dicts[id]; !ok {
		return fmt.Errorf("protocol: dictionary %d not loaded", id)
	}
	c.useDict = true
	c.activeDict = id
	return nil
}

// Encode wraps a CBOR payload in the channel header, compressing when it pays.
func (c *Codec) Encode(ch Channel, payload []byte) []byte {
	c.mu.RLock()
	useZstd := c.useZstd
	useDict := c.useDict
	active := c.activeDict
	var dp *dictPair
	if useDict {
		dp = c.dicts[active]
	}
	enc := c.enc
	minc := c.MinCompress
	c.mu.RUnlock()

	// Media payloads are already-compressed image bytes; zstd only adds work.
	if ch == ChMedia || !useZstd || len(payload) < minc {
		out := make([]byte, headerLen+len(payload))
		out[0] = byte(ch)
		out[1] = CodecRaw
		copy(out[headerLen:], payload)
		return out
	}
	if dp != nil {
		body := dp.enc.EncodeAll(payload, nil)
		out := make([]byte, headerLenDict+len(body))
		out[0] = byte(ch)
		out[1] = CodecZstdDict
		binary.LittleEndian.PutUint32(out[2:], dp.id)
		copy(out[headerLenDict:], body)
		return out
	}
	body := enc.EncodeAll(payload, nil)
	if len(body) >= len(payload) {
		out := make([]byte, headerLen+len(payload))
		out[0] = byte(ch)
		out[1] = CodecRaw
		copy(out[headerLen:], payload)
		return out
	}
	out := make([]byte, headerLen+len(body))
	out[0] = byte(ch)
	out[1] = CodecZstd
	copy(out[headerLen:], body)
	return out
}

// Decode reverses Encode.
func (c *Codec) Decode(msg []byte) (Channel, []byte, error) {
	if len(msg) < headerLen {
		return 0, nil, ErrShortMessage
	}
	ch := Channel(msg[0])
	switch msg[1] {
	case CodecRaw:
		return ch, msg[headerLen:], nil
	case CodecZstd:
		c.mu.RLock()
		dec := c.dec
		c.mu.RUnlock()
		if dec == nil {
			return 0, nil, ErrUnknownCodec
		}
		out, err := dec.DecodeAll(msg[headerLen:], nil)
		if err != nil {
			return 0, nil, err
		}
		if len(out) > MaxMessage {
			return 0, nil, ErrTooLarge
		}
		return ch, out, nil
	case CodecZstdDict:
		if len(msg) < headerLenDict {
			return 0, nil, ErrShortMessage
		}
		id := binary.LittleEndian.Uint32(msg[2:])
		c.mu.RLock()
		dp := c.dicts[id]
		c.mu.RUnlock()
		if dp == nil {
			return 0, nil, fmt.Errorf("protocol: dictionary %d unknown", id)
		}
		out, err := dp.dec.DecodeAll(msg[headerLenDict:], nil)
		if err != nil {
			return 0, nil, err
		}
		return ch, out, nil
	}
	return 0, nil, ErrUnknownCodec
}

// EncodeFrame is the common path: CBOR-encode then wrap.
func (c *Codec) EncodeFrame(ch Channel, f *Frame) ([]byte, error) {
	b, err := Marshal(f)
	if err != nil {
		return nil, err
	}
	return c.Encode(ch, b), nil
}

// DecodeFrame is the inverse of EncodeFrame.
func (c *Codec) DecodeFrame(msg []byte) (Channel, *Frame, error) {
	ch, body, err := c.Decode(msg)
	if err != nil {
		return 0, nil, err
	}
	var f Frame
	if err := Unmarshal(body, &f); err != nil {
		return 0, nil, err
	}
	return ch, &f, nil
}
