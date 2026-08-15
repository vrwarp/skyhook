// Package protocol defines the Skyhook wire format: CBOR frames carried on a
// small number of logical channels, optionally zstd-compressed per message.
//
// Design constraints (see docs/PROTOCOL.md):
//   - Never JSON on the wire.
//   - Frames are individually compressed so the server's replay ring buffer can
//     re-send an old frame without a shared compression context.
//   - Integer keys everywhere; a chat mutation must fit in a few hundred bytes.
package protocol

import (
	"github.com/fxamacker/cbor/v2"
)

// Version is the protocol version negotiated in Hello/Welcome. Client and
// server must agree exactly; there is a single user and both ends ship together.
const Version = 1

// Close codes, sent when the server hangs up on a client. They exist so the
// client can tell a link that dropped from a link it will never be allowed on:
// the first is worth retrying immediately and forever, the second is worth
// stopping for and telling the user to pair again. Retrying the second is how a
// client ends up flapping between "offline" and "connected" indefinitely.
//
// The client mirrors these in client/src/shared/protocol.ts.
const (
	// CloseNormal is an orderly hang-up: session over, or replaced.
	CloseNormal uint32 = 0
	// CloseBadHello means the first frame was missing or undecodable.
	CloseBadHello uint32 = 1
	// CloseUnauthorized means the token was not this server's. Fatal: the
	// credential has to change before a reconnect can go any differently.
	CloseUnauthorized uint32 = 2
	// CloseVersionMismatch means the two halves are not the same build. Fatal:
	// the client has to be reloaded, not reconnected.
	CloseVersionMismatch uint32 = 3
	// CloseSetupFailed means the session could not be built. Retryable.
	CloseSetupFailed uint32 = 4
	// CloseReplaced means a newer connection took this session over. Fatal for
	// the connection that receives it: the session is not gone and the link is
	// not broken, but reconnecting would evict whichever connection replaced
	// this one, which would reconnect and evict the next — two clients trading
	// a session back and forth once a second, resyncing every tab each time.
	// Whoever is told this has lost, and has to stop rather than retry.
	CloseReplaced uint32 = 5
)

// Channel identifies a logical stream. Channels map onto real QUIC streams in
// the WebTransport transport and onto a length-prefixed mux in the WebSocket
// fallback.
type Channel uint8

const (
	// ChCtrl carries session management, acks, resync and stats. Highest priority.
	ChCtrl Channel = 0
	// ChInput carries semantic input events client->server. Highest priority.
	ChInput Channel = 1
	// ChDom carries snapshots, mutation batches and style updates.
	ChDom Channel = 2
	// ChMedia carries images and favicons; each object rides its own stream so
	// a stalled image never head-of-line-blocks a DOM diff.
	ChMedia Channel = 3
	// ChBulk carries dictionaries, file transfers and adapter backlog.
	ChBulk Channel = 4
	// ChTelemetry carries scroll/viewport telemetry over datagrams (latest wins).
	ChTelemetry Channel = 5
)

// Priority returns the scheduling priority of a channel; lower is more urgent.
func (c Channel) Priority() int {
	switch c {
	case ChCtrl, ChInput, ChTelemetry:
		return 0
	case ChDom:
		return 1
	case ChMedia:
		return 2
	default:
		return 3
	}
}

func (c Channel) String() string {
	switch c {
	case ChCtrl:
		return "ctrl"
	case ChInput:
		return "input"
	case ChDom:
		return "dom"
	case ChMedia:
		return "media"
	case ChBulk:
		return "bulk"
	case ChTelemetry:
		return "telemetry"
	}
	return "unknown"
}

// Type is the frame discriminator.
type Type uint8

const (
	TypeHello        Type = 1  // client -> server, first frame on ctrl
	TypeWelcome      Type = 2  // server -> client
	TypePing         Type = 3  // either direction, keepalive
	TypePong         Type = 4  //
	TypeAck          Type = 5  // client -> server, per-tab applied seq
	TypeResync       Type = 6  // client -> server, request replay or snapshot
	TypeTabOpen      Type = 7  // client -> server
	TypeTabClose     Type = 8  // client -> server
	TypeNavigate     Type = 9  // client -> server
	TypeTabState     Type = 10 // server -> client (url/title/loading/history)
	TypeSnapshot     Type = 11 // server -> client
	TypeMutation     Type = 12 // server -> client
	TypeInput        Type = 13 // client -> server
	TypeScroll       Type = 14 // client -> server (datagram)
	TypeImageMeta    Type = 15 // server -> client (blurhash + dims + hash)
	TypeImageData    Type = 16 // server -> client (bytes)
	TypeImageWant    Type = 17 // client -> server (cache miss / priority bump)
	TypeStats        Type = 18 // server -> client
	TypeError        Type = 19 // either direction
	TypeAdapterEvent Type = 20 // server -> client (append-log records)
	TypeAdapterCmd   Type = 21 // client -> server (send message, mark read, sync)
	TypeDict         Type = 22 // server -> client, zstd dictionary on bulk
	// 23 was TypeSpeculative, a prefetched snapshot. Speculation is gone: it
	// crawled links the user never opened, which is what a scraper looks like
	// from the origin's side. The number stays retired rather than reused.
	TypeKill        Type = 24 // client -> server, wipe landside session + profile
	TypeIntegrity   Type = 25 // server -> client subtree hashes; client replies on ctrl
	TypeViewport    Type = 26 // client -> server, window size / dpr changed
	TypeCapture     Type = 27 // both ways: ask for a diagnostic capture / for the client's half
	TypeCapturePart Type = 28 // client -> server, one plane-side artifact (or a chunk of one)
	TypeCaptureDone Type = 29 // server -> client, the bundle is written (or it failed)
)

// Frame is the envelope. Body is a CBOR-encoded, type-specific payload; keeping
// it opaque here lets the ring buffer store frames without re-encoding.
type Frame struct {
	Type  Type            `cbor:"1,keyasint"`
	Tab   uint32          `cbor:"2,keyasint,omitempty"`
	Seq   uint64          `cbor:"3,keyasint,omitempty"`
	Base  uint64          `cbor:"4,keyasint,omitempty"`
	Body  cbor.RawMessage `cbor:"5,keyasint,omitempty"`
	Cause uint64          `cbor:"6,keyasint,omitempty"` // causedByInput seq
}

// ---------------------------------------------------------------------------
// ctrl bodies

// Hello is the first client frame. Capabilities are advertised, not assumed.
type Hello struct {
	Version   int      `cbor:"1,keyasint"`
	Token     string   `cbor:"2,keyasint"`
	SessionID string   `cbor:"3,keyasint,omitempty"` // resume an existing session
	Caps      []string `cbor:"4,keyasint,omitempty"` // "zstd", "zstd-dict", "avif", "webp"
	Viewport  Viewport `cbor:"5,keyasint"`
	Resume    []TabAck `cbor:"6,keyasint,omitempty"` // per-tab last applied seq
	Queued    []Frame  `cbor:"7,keyasint,omitempty"` // input queued while offline
	Client    string   `cbor:"8,keyasint,omitempty"` // version string
}

// Welcome answers Hello.
type Welcome struct {
	Version   int      `cbor:"1,keyasint"`
	SessionID string   `cbor:"2,keyasint"`
	Resumed   bool     `cbor:"3,keyasint"`
	Tabs      []TabRef `cbor:"4,keyasint,omitempty"`
	Caps      []string `cbor:"5,keyasint,omitempty"`
	Server    string   `cbor:"6,keyasint,omitempty"`
	// KeepaliveMS is how often the client should ping.
	KeepaliveMS int      `cbor:"7,keyasint,omitempty"`
	Adapters    []string `cbor:"8,keyasint,omitempty"`
}

// TabRef is a tab summary sent on resume.
type TabRef struct {
	Tab     uint32 `cbor:"1,keyasint"`
	URL     string `cbor:"2,keyasint,omitempty"`
	Title   string `cbor:"3,keyasint,omitempty"`
	Seq     uint64 `cbor:"4,keyasint,omitempty"`
	Active  bool   `cbor:"5,keyasint,omitempty"`
	Loading bool   `cbor:"6,keyasint,omitempty"`
}

// TabAck reports the last mutation seq the client has applied for a tab.
type TabAck struct {
	Tab uint32 `cbor:"1,keyasint"`
	Seq uint64 `cbor:"2,keyasint"`
	// Hash is the client's whole-document hash, used for divergence detection.
	Hash uint64 `cbor:"3,keyasint,omitempty"`
}

// Viewport mirrors the client window so landside layout and media queries match.
type Viewport struct {
	W   int     `cbor:"1,keyasint"`
	H   int     `cbor:"2,keyasint"`
	DPR float64 `cbor:"3,keyasint,omitempty"`
	// Mobile toggles Chromium's mobile emulation.
	Mobile bool `cbor:"4,keyasint,omitempty"`
}

// Resync asks the server to close a gap.
type Resync struct {
	Tab    uint32 `cbor:"1,keyasint"`
	HaveTo uint64 `cbor:"2,keyasint"`
	Reason string `cbor:"3,keyasint,omitempty"` // "gap", "hash-mismatch", "cold"
}

// Navigate drives a tab.
type Navigate struct {
	URL    string `cbor:"1,keyasint,omitempty"`
	Action string `cbor:"2,keyasint,omitempty"` // "", "back", "forward", "reload", "stop"
}

// TabState reports chrome-UI-relevant tab state.
type TabState struct {
	URL        string `cbor:"1,keyasint,omitempty"`
	Title      string `cbor:"2,keyasint,omitempty"`
	Loading    bool   `cbor:"3,keyasint,omitempty"`
	CanBack    bool   `cbor:"4,keyasint,omitempty"`
	CanForward bool   `cbor:"5,keyasint,omitempty"`
	FaviconID  string `cbor:"6,keyasint,omitempty"`
	Closed     bool   `cbor:"7,keyasint,omitempty"`
	Error      string `cbor:"8,keyasint,omitempty"`
}

// Stats is the HUD payload.
type Stats struct {
	RTTMicros     int64   `cbor:"1,keyasint,omitempty"`
	SendRateBps   int64   `cbor:"2,keyasint,omitempty"`
	LossPct       float64 `cbor:"3,keyasint,omitempty"`
	QueueDepth    int     `cbor:"4,keyasint,omitempty"`
	BytesSent     int64   `cbor:"5,keyasint,omitempty"`
	BytesRecv     int64   `cbor:"6,keyasint,omitempty"`
	Tabs          int     `cbor:"7,keyasint,omitempty"`
	PendingImages int     `cbor:"8,keyasint,omitempty"`
	ServerCPU     float64 `cbor:"9,keyasint,omitempty"`
}

// ErrorBody is a human-readable failure notice.
type ErrorBody struct {
	Code    string `cbor:"1,keyasint"`
	Message string `cbor:"2,keyasint,omitempty"`
	Fatal   bool   `cbor:"3,keyasint,omitempty"`
}

// ---------------------------------------------------------------------------
// dom bodies

// Node kinds mirror the DOM node types we care about.
const (
	KindElement  uint8 = 1
	KindText     uint8 = 3
	KindComment  uint8 = 8
	KindDoctype  uint8 = 10
	KindFragment uint8 = 11
)

// Node is one mirrored node. Strings are indices into the per-tab intern table;
// -1 means "absent".
type Node struct {
	ID     int64   `cbor:"1,keyasint"`
	Parent int64   `cbor:"2,keyasint,omitempty"`
	Kind   uint8   `cbor:"3,keyasint"`
	Ref    int32   `cbor:"4,keyasint,omitempty"` // tag name ref, or text ref
	Attrs  []int32 `cbor:"5,keyasint,omitempty"` // flat (nameRef, valueRef) pairs
	// Flags marks nodes needing client-side special handling.
	Flags uint8 `cbor:"6,keyasint,omitempty"`
}

// Node flags.
const (
	FlagEditable  uint8 = 1 << 0 // input/textarea/contenteditable
	FlagImage     uint8 = 1 << 1 // has a mirrored image payload
	FlagScrollDiv uint8 = 1 << 2 // independently scrollable container
	FlagShadow    uint8 = 1 << 3 // node hosts a flattened shadow root
	FlagCanvas    uint8 = 1 << 4 // canvas/webgl/video placeholder
)

// Snapshot is a full document for a tab. It resets the client's intern table.
type Snapshot struct {
	Strings  []string    `cbor:"1,keyasint"`
	Nodes    []Node      `cbor:"2,keyasint"`
	CSS      []string    `cbor:"3,keyasint,omitempty"`
	URL      string      `cbor:"4,keyasint,omitempty"`
	Title    string      `cbor:"5,keyasint,omitempty"`
	Viewport Viewport    `cbor:"6,keyasint,omitempty"`
	Images   []ImageMeta `cbor:"7,keyasint,omitempty"`
	ScrollX  int         `cbor:"8,keyasint,omitempty"`
	ScrollY  int         `cbor:"9,keyasint,omitempty"`
	// 10 was Speculative, which marked a prefetched snapshot.
	DocHash uint64 `cbor:"11,keyasint,omitempty"`
	BaseURL string `cbor:"12,keyasint,omitempty"`
}

// Mutation op codes.
const (
	OpInsert  uint8 = 1
	OpRemove  uint8 = 2
	OpAttr    uint8 = 3
	OpText    uint8 = 4
	OpMove    uint8 = 5
	OpSplice  uint8 = 6
	OpStyle   uint8 = 7
	OpImage   uint8 = 8
	OpFocus   uint8 = 9
	OpScroll  uint8 = 10
	OpDocInfo uint8 = 11
)

// Op is one mutation operation.
type Op struct {
	Op     uint8      `cbor:"1,keyasint"`
	Node   int64      `cbor:"2,keyasint,omitempty"`
	Parent int64      `cbor:"3,keyasint,omitempty"`
	Before int64      `cbor:"4,keyasint,omitempty"`
	Ref    int32      `cbor:"5,keyasint,omitempty"`
	Ref2   int32      `cbor:"6,keyasint,omitempty"`
	Nodes  []Node     `cbor:"7,keyasint,omitempty"`  // OpInsert subtree, document order
	Off    int32      `cbor:"8,keyasint,omitempty"`  // OpSplice offset
	Del    int32      `cbor:"9,keyasint,omitempty"`  // OpSplice deleted count
	Add    []string   `cbor:"10,keyasint,omitempty"` // OpStyle: rule texts to add
	Drop   []int32    `cbor:"11,keyasint,omitempty"` // OpStyle: rule indices to drop
	Image  *ImageMeta `cbor:"12,keyasint,omitempty"`
	X      int        `cbor:"13,keyasint,omitempty"`
	Y      int        `cbor:"14,keyasint,omitempty"`
	Str    string     `cbor:"15,keyasint,omitempty"` // OpDocInfo url/title, OpSplice literal
}

// Mutation is a batch of ops for one tab. Strings extend the intern table.
type Mutation struct {
	Strings []string `cbor:"1,keyasint,omitempty"`
	Ops     []Op     `cbor:"2,keyasint"`
	DocHash uint64   `cbor:"3,keyasint,omitempty"`
	// Flush marks a batch emitted early because it was caused by user input.
	Flush bool `cbor:"4,keyasint,omitempty"`
}

// ---------------------------------------------------------------------------
// media bodies

// ImageMeta describes a mirrored image before (or without) its bytes.
type ImageMeta struct {
	Node     int64  `cbor:"1,keyasint,omitempty"`
	Hash     string `cbor:"2,keyasint"` // content hash, also the cache key
	W        int    `cbor:"3,keyasint,omitempty"`
	H        int    `cbor:"4,keyasint,omitempty"`
	Blur     string `cbor:"5,keyasint,omitempty"` // blurhash
	Mime     string `cbor:"6,keyasint,omitempty"`
	Bytes    int    `cbor:"7,keyasint,omitempty"`
	Priority int    `cbor:"8,keyasint,omitempty"` // 0 = above the fold
	Alt      string `cbor:"9,keyasint,omitempty"`
	// Box places a region shot inside the element it was taken from: x, y, w, h
	// in CSS pixels, relative to that element's border box. A canvas half off
	// the bottom of the viewport is photographed as the half that exists, and
	// this is what stops the client from stretching that half over the whole
	// box. Empty means the image covers the element, which is what an ordinary
	// <img> means and what a fully visible canvas reduces to.
	Box []int `cbor:"10,keyasint,omitempty"`
}

// ImageData carries the encoded bytes for a hash.
type ImageData struct {
	Hash string `cbor:"1,keyasint"`
	Mime string `cbor:"2,keyasint,omitempty"`
	Data []byte `cbor:"3,keyasint"`
}

// ImageWant is a client-side cache miss or a priority bump from scroll telemetry.
type ImageWant struct {
	Hashes []string `cbor:"1,keyasint"`
	Have   []string `cbor:"2,keyasint,omitempty"` // already cached, do not send
}

// ---------------------------------------------------------------------------
// input bodies

// Input kinds.
const (
	InClick    = "click"
	InDblClick = "dblclick"
	InContext  = "contextmenu"
	InKey      = "key"
	InText     = "text"
	InSubmit   = "submit"
	InFocus    = "focus"
	InBlur     = "blur"
	InSelect   = "select"
	InHover    = "hover"
	InPaste    = "paste"
	InSetValue = "setvalue"
	InWheel    = "wheel"
	InDrag     = "drag"
)

// InputEvent is a semantic input event. Coordinates are avoided wherever a node
// id will do: the server resolves the node and clicks its center, which is
// robust to layout drift between mirror and truth.
type InputEvent struct {
	Kind      string            `cbor:"1,keyasint"`
	Node      int64             `cbor:"2,keyasint,omitempty"`
	Seq       uint64            `cbor:"3,keyasint"`
	Text      string            `cbor:"4,keyasint,omitempty"`
	Key       string            `cbor:"5,keyasint,omitempty"` // control keys: Enter, Tab, Escape, Arrow*
	Modifiers int               `cbor:"6,keyasint,omitempty"` // 1 alt, 2 ctrl, 4 meta, 8 shift
	Button    int               `cbor:"7,keyasint,omitempty"`
	X         int               `cbor:"8,keyasint,omitempty"` // relative to node box, when needed
	Y         int               `cbor:"9,keyasint,omitempty"`
	Fields    map[string]string `cbor:"10,keyasint,omitempty"` // submit payload
	ExpectSeq uint64            `cbor:"11,keyasint,omitempty"`
	TS        int64             `cbor:"12,keyasint,omitempty"` // client monotonic ms
	Start     int32             `cbor:"13,keyasint,omitempty"` // selection/caret
	End       int32             `cbor:"14,keyasint,omitempty"`
	// 15 was URL, the anchor href a click landed on, which only speculation read.
	Repeat int `cbor:"16,keyasint,omitempty"`
	// Hold is how long the button was really down plane-side, in milliseconds.
	// The landside replay of a click is otherwise instantaneous, which no hand
	// produces; the reader's own timing is better than any number the server
	// could invent, and it costs two bytes on a frame that is already being sent.
	Hold int `cbor:"17,keyasint,omitempty"`
	// Point is where in the node's box the pointer really was, in permille of
	// its width and height — two elements, or absent. Permille rather than
	// pixels because the landside box is laid out with different fonts and is
	// rarely exactly the size the reader saw.
	Point []int32 `cbor:"18,keyasint,omitempty"`
	// Path is the pointer's approach to the click: triplets of (x, y, dt), x
	// and y in permille of the viewport, dt in milliseconds since the previous
	// sample. Real cursor movement, sampled plane-side, replayed landside.
	Path []int32 `cbor:"19,keyasint,omitempty"`
}

// ScrollEvent is telemetry: it drives image prioritisation and infinite-scroll
// synthesis landside. Sent on datagrams, latest wins.
type ScrollEvent struct {
	Tab     uint32  `cbor:"1,keyasint"`
	X       int     `cbor:"2,keyasint,omitempty"`
	Y       int     `cbor:"3,keyasint,omitempty"`
	H       int     `cbor:"4,keyasint,omitempty"` // viewport height
	DocH    int     `cbor:"5,keyasint,omitempty"` // mirrored document height
	Node    int64   `cbor:"6,keyasint,omitempty"` // scroll container, 0 = document
	Seq     uint64  `cbor:"7,keyasint,omitempty"`
	Visible []int64 `cbor:"8,keyasint,omitempty"` // node ids near the viewport
}

// ---------------------------------------------------------------------------
// adapter bodies

// AdapterRecord is one append-log record (space, message, membership, ...).
type AdapterRecord struct {
	Adapter string            `cbor:"1,keyasint"`
	Kind    string            `cbor:"2,keyasint"` // "space", "message", "unread", "sync", "presence"
	ID      string            `cbor:"3,keyasint,omitempty"`
	Space   string            `cbor:"4,keyasint,omitempty"`
	Author  string            `cbor:"5,keyasint,omitempty"`
	Text    string            `cbor:"6,keyasint,omitempty"`
	TS      int64             `cbor:"7,keyasint,omitempty"`
	Seq     uint64            `cbor:"8,keyasint,omitempty"`
	Unread  int               `cbor:"9,keyasint,omitempty"`
	Extra   map[string]string `cbor:"10,keyasint,omitempty"`
}

// AdapterBatch ships records; adapters are append-only logs by construction.
type AdapterBatch struct {
	Records []AdapterRecord `cbor:"1,keyasint"`
	// Backlog marks a "while you were gone" replay after a reconnect.
	Backlog bool `cbor:"2,keyasint,omitempty"`
}

// AdapterCommand is the client -> server direction (outbox).
type AdapterCommand struct {
	Adapter string `cbor:"1,keyasint"`
	Cmd     string `cbor:"2,keyasint"` // "send", "sync", "markread", "open"
	Space   string `cbor:"3,keyasint,omitempty"`
	Text    string `cbor:"4,keyasint,omitempty"`
	LocalID string `cbor:"5,keyasint,omitempty"` // echoes back for optimistic send
	Since   uint64 `cbor:"6,keyasint,omitempty"`
}

// DictUpdate ships a trained zstd dictionary on the bulk channel.
type DictUpdate struct {
	ID     uint32 `cbor:"1,keyasint"`
	Origin string `cbor:"2,keyasint,omitempty"`
	Data   []byte `cbor:"3,keyasint"`
}

// Integrity is the periodic Merkle-ish divergence check.
type Integrity struct {
	Roots map[int64]uint64 `cbor:"1,keyasint"` // subtree root node id -> hash
	Full  uint64           `cbor:"2,keyasint,omitempty"`
}

// ---------------------------------------------------------------------------
// capture bodies
//
// A capture is one diagnostic bundle: everything both halves know about a tab
// at one instant, zipped up landside. It is the only frame family that
// deliberately spends the link — a screenshot is worth more bytes than any
// mirror update ever is — so it happens when somebody asks for it, or when the
// server has caught the two halves disagreeing.

// Capture reasons. Free-form on the wire; these are the ones both ends know.
const (
	// CaptureManual is a reader saying "this looks wrong".
	CaptureManual = "manual"
	// CaptureDivergence is the integrity check finding two different documents.
	CaptureDivergence = "divergence"
	// CaptureResync is a resync the server could not close with diffs.
	CaptureResync = "resync"
)

// CaptureRequest travels in both directions, which is what makes one round of
// it a capture: the client asks for one with no ID, and the server answers with
// the ID it allocated and the list of tabs it wants the client's half of.
type CaptureRequest struct {
	ID     string `cbor:"1,keyasint,omitempty"` // set server -> client
	Reason string `cbor:"2,keyasint,omitempty"`
	// Note is whatever the reader typed about what looked wrong. It is the one
	// field in a bundle no amount of instrumentation can reconstruct.
	Note string   `cbor:"3,keyasint,omitempty"`
	Tabs []uint32 `cbor:"4,keyasint,omitempty"` // server -> client: gather these
	// MaxBytes caps what the client should send up, so a capture over a 250 kbps
	// link cannot turn into a multi-megabyte upload nobody asked for.
	MaxBytes int `cbor:"5,keyasint,omitempty"`
	// Screenshots is false when the server wants the cheap half only.
	Screenshots bool `cbor:"6,keyasint,omitempty"`
}

// CapturePart is one plane-side artifact on its way up, or a chunk of one.
// Chunking is by hand rather than by transport because the bulk channel is a
// message stream: a 900 kB screenshot as one message would sit in front of
// everything behind it for as long as it takes to clear the link.
type CapturePart struct {
	ID   string `cbor:"1,keyasint"`
	Name string `cbor:"2,keyasint,omitempty"` // path inside the zip
	Data []byte `cbor:"3,keyasint,omitempty"`
	// More marks a chunk with siblings following, appended to the same Name.
	More bool `cbor:"4,keyasint,omitempty"`
	// Done marks the last part of the capture: the server seals the zip.
	Done bool `cbor:"5,keyasint,omitempty"`
	// Error records something the client could not gather. It is written into
	// the bundle rather than dropped, because a missing artifact and an
	// artifact that failed to be produced are different diagnoses.
	Error string `cbor:"6,keyasint,omitempty"`
}

// CaptureDone reports where the bundle landed, so the reader who asked for one
// can quote a filename at whoever is going to read it.
type CaptureDone struct {
	ID    string `cbor:"1,keyasint"`
	Path  string `cbor:"2,keyasint,omitempty"`
	Bytes int64  `cbor:"3,keyasint,omitempty"`
	Error string `cbor:"4,keyasint,omitempty"`
}
