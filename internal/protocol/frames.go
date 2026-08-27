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
	// Downloads (P-108). A download lands on the server first — at datacenter
	// speed, safely — and crosses the link only when the reader asks, with
	// the size in front of them. See DESIGN.md's cost-labelled-ask grammar.
	TypeDownload     Type = 30 // server -> client, a download's state
	TypeDownloadCmd  Type = 31 // client -> server, fetch or discard one
	TypeDownloadPart Type = 32 // server -> client on bulk, one chunk of the bytes
	// A copy the page performed landside because of something the reader did,
	// relayed so the reader's own clipboard ends up holding what the page
	// told them it would (P-008).
	TypeClipboard Type = 33 // server -> client
	// File upload (P-007): a page's file chooser, intercepted landside and
	// asked across the link; the reader's files come back the other way.
	TypeFileAsk    Type = 34 // server -> client, the page wants files
	TypeUploadPart Type = 35 // client -> server on bulk, one chunk of them
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
	Client    string   `cbor:"8,keyasint,omitempty"` // "skyhook-pwa/0.1.0"
	// Build identifies the exact bytes of the plane-side app that is speaking:
	// the same id the client's service worker keys its cache on. It is what
	// makes "which client is that" answerable landside, where the browser
	// holding the answer is on the other side of the bad link and may be
	// running a shell it cached weeks ago.
	Build string `cbor:"9,keyasint,omitempty"`
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
	// ClientVersion and ClientBuild describe the plane-side app this server is
	// currently serving — not the one that is connected. A client compares them
	// against its own and knows whether the shell in its cache is the shell the
	// server would hand it today, which is the only way it can find out: the
	// service worker answers every request for the app out of that cache, so
	// the app cannot see the newer one merely by asking for it.
	ClientVersion string `cbor:"9,keyasint,omitempty"`
	ClientBuild   string `cbor:"10,keyasint,omitempty"`
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
	// Epoch names the document Hash is about: the Epoch of the snapshot the
	// client had applied when it hashed. A snapshot restarts Seq at zero, so
	// the pair (Tab, Seq) does not name a document, and a client acknowledging
	// frame zero of one document answered the server's question about frame
	// zero of another — which is a divergence report against a mirror that was
	// merely one document behind, and a whole document re-sent to repair it.
	// Zero from a client that predates this field, which is read as "unknown"
	// rather than as epoch zero.
	Epoch uint64 `cbor:"4,keyasint,omitempty"`
}

// Viewport mirrors the client window so landside layout and media queries match.
type Viewport struct {
	W   int     `cbor:"1,keyasint"`
	H   int     `cbor:"2,keyasint"`
	DPR float64 `cbor:"3,keyasint,omitempty"`
	// Mobile toggles Chromium's mobile emulation.
	Mobile bool `cbor:"4,keyasint,omitempty"`
	// Scheme is the colour scheme the reader wants the pages rendered in:
	// "light", "dark", or empty for whatever the landside browser is.
	//
	// It rides with the viewport because it is the same kind of fact and wants
	// the same treatment — something about the reader's window that the landside
	// tab is put into, so that both sides are laying out the same page. A mirror
	// cannot honour it plane-side: the palette is settled landside before the
	// bundle is written, along with every image the server fetched and
	// transcoded from that render. See IMPLEMENTATION.md §45.
	Scheme string `cbor:"5,keyasint,omitempty"`
	// Touch says the reader's device has a touchscreen. The landside browser
	// emulates one to match (P-006): a page that branches on maxTouchPoints
	// builds its touch interaction model only when the machine claims the
	// hardware, and the claim is honest exactly when input arrives to feed it
	// — which is what InputEvent.PT carries.
	Touch bool `cbor:"6,keyasint,omitempty"`
}

// Resync asks the server to close a gap.
type Resync struct {
	Tab    uint32 `cbor:"1,keyasint"`
	HaveTo uint64 `cbor:"2,keyasint"`
	Reason string `cbor:"3,keyasint,omitempty"` // "gap", "hash-mismatch", "cold"
}

// Navigate drives a tab. On TabOpen it also carries the two things only the
// client knows: which of its provisional tabs the answer belongs to, and
// whether the user asked for this one in front or behind.
type Navigate struct {
	URL    string `cbor:"1,keyasint,omitempty"`
	Action string `cbor:"2,keyasint,omitempty"` // "", "back", "forward", "reload", "stop"
	// Ref is an opaque client token echoed back on the opening TabState. The
	// client draws a tab the instant the user asks for one, a round trip before
	// the server can name it; the ref is how the drawn tab and the real one are
	// recognised as the same tab. TabOpen only.
	Ref string `cbor:"3,keyasint,omitempty"`
	// Background marks an open the user did not ask to look at — a middle click,
	// or "open link in a new tab". Such a tab must not take image priority away
	// from the page they are still reading. TabOpen only.
	Background bool `cbor:"4,keyasint,omitempty"`
}

// TabState reports chrome-UI-relevant tab state.
type TabState struct {
	URL        string `cbor:"1,keyasint,omitempty"`
	Title      string `cbor:"2,keyasint,omitempty"`
	Loading    bool   `cbor:"3,keyasint,omitempty"`
	CanBack    bool   `cbor:"4,keyasint,omitempty"`
	CanForward bool   `cbor:"5,keyasint,omitempty"`
	// FaviconID carries the page's icon itself, as a data: URL (P-104). An
	// icon is a few hundred bytes that wants to arrive with the tab state it
	// decorates, not a pipeline asset; the name is historical — the field was
	// wired into the client before anything set it, and nothing ever assigned
	// an id.
	FaviconID string `cbor:"6,keyasint,omitempty"`
	Closed    bool   `cbor:"7,keyasint,omitempty"`
	Error     string `cbor:"8,keyasint,omitempty"`
	// Ref echoes Navigate.Ref on the frame that announces an opened tab, and is
	// absent on every other TabState.
	Ref string `cbor:"9,keyasint,omitempty"`
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
	KindElement uint8 = 1
	KindText    uint8 = 3
	KindComment uint8 = 8
	KindDoctype uint8 = 10
	// KindFragment is a shadow root. A shadow root *is* a DocumentFragment, and
	// these kinds are DOM node types, so this needs no number of its own.
	//
	// It carries no name and no text: what it is for is to be a boundary. The
	// client attaches a real root to its parent and builds the children inside
	// it, which is what keeps a mirrored sub-document's stylesheet from
	// reaching the rest of the page. See §31.
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
	// Scoped holds the stylesheets that belong to a shadow root rather than to
	// the document. CSS above is the document's own; these are kept apart
	// because hoisting them into one sheet is exactly what the boundary exists
	// to prevent.
	Scoped []ScopedCSS `cbor:"13,keyasint,omitempty"`
	// Epoch counts the documents this tab has sent, and is echoed back in every
	// TabAck the client makes about this one. See TabAck.Epoch.
	Epoch uint64 `cbor:"14,keyasint,omitempty"`
	// Quirks carries the landside parser's verdict — document.compatMode ==
	// "BackCompat" — so the mirror can render under the same rules (P-125).
	// The doctype node's presence is not the same fact: an archaic doctype
	// still parses into quirks mode.
	Quirks bool `cbor:"15,keyasint,omitempty"`
	// Scrolls carries the containers the page had already scrolled when this
	// document was serialised. OpScroll reports a container when it moves, and
	// one that was where it belonged before the snapshot never moves again —
	// so without this a resync parks every inner scroller at the top and
	// nothing ever puts it back.
	Scrolls []NodeScroll `cbor:"16,keyasint,omitempty"`
}

// NodeScroll is one container's scroll position.
type NodeScroll struct {
	Node int64 `cbor:"1,keyasint"`
	X    int   `cbor:"2,keyasint,omitempty"`
	Y    int   `cbor:"3,keyasint,omitempty"`
}

// ScopedCSS is one shadow root's stylesheet.
type ScopedCSS struct {
	Root  int64    `cbor:"1,keyasint"`
	Rules []string `cbor:"2,keyasint,omitempty"`
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
	Op uint8 `cbor:"1,keyasint"`
	// Node is the node the op acts on. For OpStyle it names the shadow root
	// whose sheet is being added to, and zero means the document's own.
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
	// Missing says the bytes are not coming: the fetch or the decode failed
	// landside, and the key carries no size, no type and no blurhash because
	// nothing ever got far enough to measure one.
	//
	// Without it a failure is indistinguishable from slowness. The client asks
	// for a hash exactly once — a second request costs a round trip on a link
	// where round trips are the whole problem — so an asset the server quietly
	// gave up on is one the reader waits on for the rest of the session,
	// holding a placeholder that will never be replaced. Saying so costs a few
	// bytes and lets the element fall back to its alt text, which is the thing
	// the page's author wrote for exactly this.
	Missing bool `cbor:"11,keyasint,omitempty"`
	// Anim says the still was made from an animation (an animated GIF): the
	// client offers tap-to-play, which re-requests the original under this
	// hash plus imgproc.AnimSuffix (P-118).
	Anim bool `cbor:"12,keyasint,omitempty"`
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
	// PT is the pointer's kind: 0 mouse, 1 touch, 2 pen. The replay wants to
	// speak the modality the reader used — a page that branches on
	// pointerType gets the truth, and a touch-emulating landside browser can
	// deliver a finger's gesture as the touch events it really was.
	PT int `cbor:"20,keyasint,omitempty"`
	// Node2 names where a drag finished: the element under the pointer at
	// release. The path says how the gesture moved; Node2 says what it landed
	// on, which survives the two halves laying the page out differently —
	// permille of the viewport puts a drop near the right place, Node2 puts
	// it on the right element.
	Node2 int64 `cbor:"21,keyasint,omitempty"`
	// Point2 is where in Node2's box the drag finished, in permille of its
	// width and height — Point's twin, for the other end of the gesture.
	Point2 []int32 `cbor:"22,keyasint,omitempty"`
	// Path2 is a second finger's path, in the same (x, y, dt) triplets as
	// Path and sampled at the same instants, which is what makes a drag a
	// pinch: two pointers on one element and the distance between them.
	// Present only for a gesture the reader made with two fingers.
	Path2 []int32 `cbor:"23,keyasint,omitempty"`
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
	// Anchor names the mirrored element at the top of the plane's viewport,
	// and AnchorY where its border box sits relative to that top (P-020). A
	// document scroll lands exactly when the same element is put at the same
	// offset landside; the range fraction is the fallback for an anchor the
	// landside document no longer has.
	Anchor  int64 `cbor:"9,keyasint,omitempty"`
	AnchorY int   `cbor:"10,keyasint,omitempty"`
}

// ---------------------------------------------------------------------------
// download bodies (P-108)

// Download is one landside download's state, sent whenever it changes. The
// announcement is the point: today's alternative was a file appearing on the
// VPS with nobody told.
type Download struct {
	ID  string `cbor:"1,keyasint"`
	URL string `cbor:"2,keyasint,omitempty"`
	// Name is the filename the origin suggested, relayed for display and for
	// the eventual save. The server stores the bytes under the ID.
	Name string `cbor:"3,keyasint,omitempty"`
	// Total is the size when known; 0 with State "landing" means the origin
	// did not say, and the honest display counts Received instead.
	Total    int64 `cbor:"4,keyasint,omitempty"`
	Received int64 `cbor:"5,keyasint,omitempty"`
	// State: "landing" (arriving on the server), "ready" (safe landside,
	// fetchable), "failed", or "gone" (discarded or wiped).
	State string `cbor:"6,keyasint,omitempty"`
}

// Download states.
const (
	DownloadLanding = "landing" // arriving on the server
	DownloadReady   = "ready"   // safe landside, fetchable
	DownloadFailed  = "failed"  // the landside download was cancelled or broke
	DownloadGone    = "gone"    // discarded or wiped
)

// DownloadCmd asks for one download's bytes, for the stream to stop, or for
// the file to be deleted.
type DownloadCmd struct {
	ID  string `cbor:"1,keyasint"`
	Cmd string `cbor:"2,keyasint"` // "fetch" | "stop" | "discard"
	// Offset resumes a fetch partway: the client says how much it already
	// holds, and the stream starts there.
	Offset int64 `cbor:"3,keyasint,omitempty"`
}

// DownloadPart is one chunk of a fetched download, on the bulk channel where
// it cannot head-of-line-block a page.
type DownloadPart struct {
	ID   string `cbor:"1,keyasint"`
	Off  int64  `cbor:"2,keyasint,omitempty"`
	Data []byte `cbor:"3,keyasint,omitempty"`
	Done bool   `cbor:"4,keyasint,omitempty"`
	Size int64  `cbor:"5,keyasint,omitempty"`
	Err  string `cbor:"6,keyasint,omitempty"`
}

// Clipboard is text the landside page put on its clipboard because of
// something the reader did — a Copy button, a Ctrl+C the page handled —
// relayed so the reader's device holds it too. Cause is the input seq that
// provoked it, which is what makes "because of something the reader did"
// checkable plane-side. Text is capped landside at ClipboardCap.
type Clipboard struct {
	Text  string `cbor:"1,keyasint"`
	Cause uint64 `cbor:"2,keyasint,omitempty"`
}

// ClipboardCap bounds a relayed copy, in bytes. 64 kB of text is beyond any
// coordinates, share-link or code snippet a Copy button produces; past it the
// relay is more likely moving a document than helping a reader.
const ClipboardCap = 64 << 10

// FileAsk is a page's file chooser, intercepted landside (P-007). Node names
// the mirrored input when the server could resolve it, so the client can read
// its accept attribute; zero is still answerable.
type FileAsk struct {
	ID       uint32 `cbor:"1,keyasint"`
	Node     int64  `cbor:"2,keyasint,omitempty"`
	Multiple bool   `cbor:"3,keyasint,omitempty"`
}

// UploadPart is one piece of the reader's answer to a FileAsk, client to
// server on the bulk channel. A part opening a new file carries Name (and
// Mime/Size for display); Last closes the current file; Done closes the ask
// and hands everything to the input. Err ends the ask with nothing — the
// reader dismissed the picker — and the page sees a dismissed chooser.
type UploadPart struct {
	Ask  uint32 `cbor:"1,keyasint"`
	Name string `cbor:"2,keyasint,omitempty"`
	Mime string `cbor:"3,keyasint,omitempty"`
	Size int64  `cbor:"4,keyasint,omitempty"`
	Off  int64  `cbor:"5,keyasint,omitempty"`
	Data []byte `cbor:"6,keyasint,omitempty"`
	Last bool   `cbor:"7,keyasint,omitempty"`
	Done bool   `cbor:"8,keyasint,omitempty"`
	Err  string `cbor:"9,keyasint,omitempty"`
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
