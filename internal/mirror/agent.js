/* Skyhook mirror agent.
 *
 * Runs landside, inside an isolated world of every mirrored page. Page
 * JavaScript can neither see nor break it, and it in turn never touches page
 * globals. Responsibilities:
 *
 *   - serialise the document into an interned snapshot,
 *   - observe mutations and emit compact ops (including moves and text splices,
 *     which is what keeps React reorders and chat-log appends cheap),
 *   - extract only the CSS rules that actually match something,
 *   - report images at their rendered layout size,
 *   - resolve node ids back to elements for input replay.
 *
 * Output goes through a CDP binding as chunked JSON. The host process converts
 * it to CBOR; JSON never reaches the wire.
 */
(function () {
  'use strict';
  if (globalThis.__skyhook && globalThis.__skyhook.version === 1) return;

  /*
   * Which documents mirror themselves, and which are mirrored for them.
   *
   * This script runs in the isolated world of every frame the host can reach,
   * so a rule is needed: without one, a subframe would send a snapshot of
   * itself on the tab's stream and the client — which cannot tell one document
   * from another — would replace the whole page with the contents of a frame.
   *
   * The rule is `frameElement`. A same-origin frame can see the element that
   * holds it, and so can the agent above it: that document is read and inlined
   * by the agent that owns the page, and this one has nothing to do. A frame
   * whose parent is another origin sees null, and nothing above it can read it
   * either — so it mirrors itself, and the host splices the result into the
   * parent's document at the element that stands for the frame.
   *
   * Such a frame does not know what it is called or where it hangs, so it says
   * hello and waits: the host answers with `adopt`, which hands it a *slot* —
   * the id space its nodes live in — and starts it. The two shapes of frame
   * this covers arrive by different roads and meet here. One is a frame in a
   * process of its own, which the host had to attach to as a target before this
   * script could run in it at all. The other is a frame that is cross-origin
   * but same-site — mail.google.com holding ogs.google.com, which is most of
   * the real ones — where Chromium keeps one process, no target is ever
   * created, and this script was already running with nothing to do.
   */
  var isTop = true;
  try { isTop = globalThis.top === globalThis.self; } catch (e) { isTop = false; }
  var isFrameRoot = true;
  try { isFrameRoot = !globalThis.frameElement; } catch (e) { isFrameRoot = true; }
  if (!isTop && !isFrameRoot) return;
  var SLOT = 0;
  var adopted = isTop;

  /*
   * Ids are namespaced by slot, so two agents feeding one client can never
   * collide. The client, the Go replica and the integrity check all sort ids
   * ascending and hash them in that order, which puts each frame's nodes in one
   * contiguous run — so the hash of the whole mirror is each agent's hash
   * chained into the next, in slot order, and nothing has to know how many
   * agents there were.
   *
   * Every id stays inside 32 bits, which is not an implementation detail: the
   * client encodes an id above 2^32-1 as a float, and the host's decoder
   * refuses to put a float into an integer field and drops the frame whole.
   * That is the bug `safeInt` exists for, and an id space wider than the wire
   * would have brought it back as "clicks inside a frame do nothing".
   *
   * So the page keeps everything below 2^31 — two billion ids, which is more
   * than a document that fits in memory will ever want — and the frames divide
   * what is above it: 8 million ids each, 254 frames to a tab.
   */
  var FRAME_BASE = 2147483648;
  var FRAME_SPAN = 8388608;

  var SEND = globalThis.__skyhookSend;
  if (typeof SEND !== 'function') {
    // Binding not installed yet; the host retries after Runtime.addBinding.
    return;
  }

  var CHUNK = 256 * 1024;
  var MUTATION_BATCH_MS = 100;
  var CSS_DEBOUNCE_MS = 400;
  var SCROLL_THROTTLE_MS = 250;
  var REFRAME_DEBOUNCE_MS = 250;
  // How often the sweep looks for the things no observer reports — an element
  // that upgraded, a shadow root attached by hand, a control the page filled in
  // — and how far that interval backs off while it keeps finding nothing.
  var SWEEP_MS = 500;
  var SWEEP_MAX_MS = 8000;
  // How many un-upgraded custom elements are worth watching at once. A page
  // that has more than this has something other than lazy components going on.
  var UPGRADE_WATCH_MAX = 4096;
  // How many rejected selectors a capture is worth. A utility-class bundle
  // rejects tens of thousands; the first few thousand answer the question.
  var CSS_REJECTED_MAX = 4000;
  // How far up a frame chain a coordinate is translated. Ad stacks nest three
  // or four deep; past this something is looping and a click is not worth it.
  var FRAME_DEPTH_MAX = 16;

  var KIND_ELEMENT = 1, KIND_TEXT = 3, KIND_DOCTYPE = 10, KIND_FRAGMENT = 11;
  var FLAG_EDITABLE = 1, FLAG_IMAGE = 2, FLAG_SCROLL = 4, FLAG_SHADOW = 8, FLAG_CANVAS = 16;

  // Tags never mirrored: they either carry code, or carry styling we ship
  // separately as used-CSS.
  var SKIP_TAGS = {
    SCRIPT: 1, NOSCRIPT: 1, STYLE: 1, LINK: 1, META: 1, BASE: 1, TEMPLATE: 1
  };
  var CANVAS_TAGS = { CANVAS: 1, VIDEO: 1, AUDIO: 1 };
  // Plugin containers cross as labelled boxes, the iframe bargain (P-106):
  // their content cannot be mirrored, but dropping them whole left an
  // unexplained hole where everything below sat a plugin's height too high.
  // Their children are fallback content a landside page with the resource
  // loaded never renders, so they are not descended into.
  var PLUGIN_TAGS = { OBJECT: 1, EMBED: 1, APPLET: 1 };
  var VOID_IMAGE_TAGS = { IMG: 1, IMAGE: 1 };
  // Attributes dropped: event handlers, integrity/nonce metadata, and the
  // responsive-image machinery we replace with one server-chosen rendition.
  var SKIP_ATTRS = {
    srcset: 1, sizes: 1, integrity: 1, nonce: 1, crossorigin: 1, ping: 1,
    'http-equiv': 1, manifest: 1
  };
  var URL_ATTRS = { href: 1, src: 1, action: 1, poster: 1, formaction: 1, cite: 1, data: 1 };
  // Attributes that would carry a secret's *contents* back to the client.
  var SENSITIVE_ATTRS = { value: 1, 'data-sky-value': 1 };
  // Marks a custom element that had not upgraded landside. See serializeAttrs.
  var UNDEFINED_ATTR = 'data-sky-undefined';
  // The element the landside URL points at, which plane-side is nothing at all:
  // the mirror is a frame with no fragment in its address, and the client jumps
  // to one by scrolling rather than by navigating. See rewriteLandsideState.
  var TARGET_ATTR = 'data-sky-target';
  // Names the origin of a frame whose document could not be read. See
  // watchBox: the client draws the box as an empty panel that says so, because
  // an unexplained hole is the one thing worse than missing content.
  var OPAQUE_ATTR = 'data-sky-frame';
  // How small an unreadable frame can be before saying what it is stops being
  // worth the ink. A tracking beacon is a frame too, and a page carries a dozen
  // of them; a widget the reader was looking for is never this small.
  var FRAME_LABEL_MIN_W = 64, FRAME_LABEL_MIN_H = 32;
  var SENSITIVE_AUTOCOMPLETE = /(^|\s)(current-password|new-password|one-time-code|cc-number|cc-csc)(\s|$)/i;

  // The computed properties the parity probe reports, in this order. One copy
  // here, one in client/src/mirror/patcher.ts, one in internal/parity/types.go
  // (StyleProps) — the same three-implementations bargain the document hash
  // makes. Change one, change all three, and regenerate the parity baselines.
  var STYLE_PROPS = [
    'display', 'position', 'float', 'visibility', 'opacity',
    'overflow-x', 'overflow-y',
    'color', 'background-color', 'background-image',
    'font-family', 'font-size', 'font-weight', 'font-style', 'line-height',
    'text-align', 'text-transform', 'text-decoration-line', 'white-space',
    'direction',
    'border-top-width', 'border-top-style', 'border-top-color',
    'margin-top', 'margin-left', 'padding-top', 'padding-left',
    'z-index', 'box-sizing', 'list-style-type'
  ];

  // Ids start inside this agent's slot; slot 0 is the top-level document, whose
  // ids are exactly what they always were. A frame's slot arrives with adopt,
  // before anything has been serialised.
  var nextId = 1;
  // A mirrored sub-document -> the shadow-root node id it was serialised into.
  // The CSS pass needs it to say which sheet a rule belongs to; without it the
  // rules go to the document and the boundary buys nothing.
  var docRoot = new WeakMap();
  var idOf = new WeakMap();     // node -> id
  var byId = new Map();         // id -> node
  var strings = [];             // intern table
  var stringIndex = new Map();
  var pendingStrings = [];      // interned since the last flush
  var seq = 0;

  var observers = [];
  var observedDocs = new Set();
  var hookedFrames = new WeakSet();
  // Frames whose stand-in box the client has been told about: element -> the
  // "WxH host" it was last sent as. A Map rather than a WeakMap because a
  // capture has to be able to count it; a frame that leaves the document is
  // dropped from it when it is forgotten, and the rest go at the next snapshot.
  var boxWatch = new Map();
  var boxDirty = new Set();      // frames to re-measure before the next flush
  var boxObservers = new Map();  // window -> its ResizeObserver
  var imgShipped = new WeakMap(); // img element -> the "WxH" the client was told
  var topLayerShipped = new WeakMap(); // element -> the top-layer state the client holds
  var reframeTimer = null;
  var pendingOps = [];
  var pendingImages = [];
  var flushTimer = null;
  var cssTimer = null;
  var syntheticFonts = new Map(); // family -> a synthesized @font-face (P-003)
  var emittedCSS = new Map();   // rule text -> index
  var scopedEmitted = new Map(); // shadow-root id -> its own emitted-rule set
  var cssOrder = [];
  var recoveredSheets = new Map(); // href -> constructed sheet the host supplied
  var sheetSources = new Map();    // href -> authored text, for cssText repair
  var sheetSourceFetches = new Map(); // href -> 'pending' | 'failed'
  var constructedSourceTexts = typeof WeakMap === 'function' ? new WeakMap() : null;
  var blockedSheets = {};          // href -> 1, for sheets nothing can read yet
  var blockedNew = false;          // one of those is news the host has not heard
  var cssSeen = 0;                 // style rules the last pass considered
  var cssRejected = 0;             // of those, how many matched nothing
  var cssRejectedList = [];        // and which, up to CSS_REJECTED_MAX
  var fontsWanted = {};            // family -> 1, for fonts nothing can substitute
  var lastText = new Map();     // id -> last text we reported
  var lastScroll = new Map();
  var dirtyScroll = new Set();  // ids whose position moved since the last flush
  // [id, x, y] for containers already scrolled when a snapshot is taken. Set
  // to an array for the length of the walk and null the rest of the time, so
  // that the mutation path — which shares serializeAttrs — collects nothing.
  var scrolled = null;
  var awaitingUpgrade = new Set(); // ids of custom elements not yet defined
  var markedTarget = null;         // the element currently wearing TARGET_ATTR
  var sweepTimer = null;
  var sweepEvery = SWEEP_MS;
  // Controls whose live properties the mirror carries: element -> the value,
  // ticked and chosen state it was last sent as. See emitLive.
  var liveWatch = new Map();
  // Elements the shadow sweep has already re-read once. See sweepShadowRoots.
  var shadowSwept = new WeakSet();
  // Frames the host is mirroring with an agent of their own. See opaqueHost.
  var mirroredFrames = new WeakSet();
  // The page's own name and address as the client last heard them. See
  // syncDocInfo: neither is a node, so neither has a mutation to ride on.
  var lastInfo = { url: '', title: '' };
  var scrollTimer = null;
  var focusedId = 0;
  var started = false;
  var snapshotDone = false;
  var msgSeq = 0;

  // ---------------------------------------------------------------- utilities

  function intern(s) {
    if (s === null || s === undefined) return -1;
    if (typeof s !== 'string') s = String(s);
    var i = stringIndex.get(s);
    if (i !== undefined) return i;
    i = strings.length;
    strings.push(s);
    stringIndex.set(s, i);
    pendingStrings.push(s);
    return i;
  }

  function idFor(node) {
    var id = idOf.get(node);
    if (id === undefined) {
      id = nextId++;
      idOf.set(node, id);
      byId.set(id, node);
    }
    return id;
  }

  function forget(node) {
    var id = idOf.get(node);
    if (id !== undefined) {
      byId.delete(id);
      idOf.delete(node);
      lastText.delete(id);
      lastScroll.delete(id);
    }
    if (node.tagName === 'IFRAME' || VOID_IMAGE_TAGS[node.tagName]) unwatchBox(node);
    liveWatch.delete(node);
    var kids = node.childNodes;
    if (kids) for (var i = 0; i < kids.length; i++) forget(kids[i]);
    if (node.shadowRoot) forget(node.shadowRoot);
  }

  function absolute(base, url) {
    if (!url) return url;
    try { return new URL(url, base).href; } catch (e) { return url; }
  }

  // fnv1a32 gives a stable image key the host can recompute in Go without a
  // round trip, and without paying for SubtleCrypto's async API here.
  function fnv1a(str) {
    var h = 0x811c9dc5;
    for (var i = 0; i < str.length; i++) {
      h ^= str.charCodeAt(i) & 0xff;
      h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
      if (str.charCodeAt(i) > 0xff) {
        h ^= (str.charCodeAt(i) >> 8) & 0xff;
        h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
      }
    }
    return ('0000000' + h.toString(16)).slice(-8);
  }

  function imageKey(url, w, h) {
    return fnv1a(url + '|' + w + 'x' + h);
  }

  function isEditable(el) {
    var t = el.tagName;
    if (t === 'INPUT') {
      var ty = (el.getAttribute('type') || 'text').toLowerCase();
      return ty !== 'hidden' && ty !== 'submit' && ty !== 'button' && ty !== 'image';
    }
    if (t === 'TEXTAREA' || t === 'SELECT') return true;
    return el.isContentEditable === true;
  }

  /*
   * caretPoint resolves a character offset inside an editing host to the
   * (text node, offset) pair a Range is built from, counting through the
   * host's text nodes the way the client counted when it measured the offset.
   *
   * An offset past the end lands after the last character, which is what a
   * caret at the end of a field means and what an offset measured against a
   * value this host has not been given yet decays to.
   */
  function caretPoint(el, offset) {
    var walk = el.ownerDocument.createTreeWalker(el, 4 /* SHOW_TEXT */);
    var seen = 0, node, last = null, lastLen = 0;
    while ((node = walk.nextNode())) {
      var len = node.nodeValue ? node.nodeValue.length : 0;
      if (seen + len >= offset) return { node: node, offset: offset - seen };
      seen += len;
      last = node; lastLen = len;
    }
    if (last) return { node: last, offset: lastLen };
    return { node: el, offset: el.childNodes.length };
  }

  /*
   * placeCaret puts the selection back where the client says it is.
   *
   * Assigning textContent destroys every text node the selection could point
   * into, and Blink answers that by collapsing the selection to the start of
   * the editing host. Nothing complains: the text is right, the field looks
   * right, and the next keystroke goes in at the front. A reader who typed
   * "the test message has gone through!" with one Backspace in the middle of
   * it sent "e through!the test message has gon" — the ten characters after
   * the correction, then the twenty-four before it (P-129).
   *
   * The input and textarea branches of setValue never had this bug, because
   * setSelectionRange is the only way to put a caret in a field and they
   * always called it. A contenteditable's caret is the document's selection,
   * which is easy to lose by accident and has to be restored on purpose.
   */
  function placeCaret(el, start, end) {
    var doc = el.ownerDocument;
    var view = doc.defaultView;
    var sel = view && view.getSelection ? view.getSelection() : null;
    if (!sel) return;
    var text = el.textContent || '';
    var a = Math.max(0, Math.min(start | 0, text.length));
    var b = Math.max(a, Math.min(typeof end === 'number' ? end | 0 : a, text.length));
    var from = caretPoint(el, a);
    var to = b === a ? from : caretPoint(el, b);
    try {
      var range = doc.createRange();
      range.setStart(from.node, from.offset);
      range.setEnd(to.node, to.offset);
      sel.removeAllRanges();
      sel.addRange(range);
    } catch (e) { /* a detached or hidden host: the caret is nobody's business */ }
  }

  /*
   * caretOffset is placeCaret's inverse: where the selection sits inside this
   * editing host, counted in characters. A selection somewhere else — or none
   * at all — reads as the end of the text, which is where typing goes when
   * nothing better is known.
   */
  function caretOffset(el) {
    var doc = el.ownerDocument;
    var view = doc.defaultView;
    var sel = view && view.getSelection ? view.getSelection() : null;
    var text = el.textContent || '';
    if (!sel || sel.rangeCount === 0) return text.length;
    var range;
    try { range = sel.getRangeAt(0); } catch (e) { return text.length; }
    if (!range || !el.contains(range.startContainer)) return text.length;
    if (range.startContainer === el) {
      var upto = 0;
      for (var i = 0; i < range.startOffset && i < el.childNodes.length; i++) {
        upto += (el.childNodes[i].textContent || '').length;
      }
      return upto;
    }
    var walk = doc.createTreeWalker(el, 4 /* SHOW_TEXT */);
    var seen = 0, node;
    while ((node = walk.nextNode())) {
      if (node === range.startContainer) return seen + range.startOffset;
      seen += node.nodeValue ? node.nodeValue.length : 0;
    }
    return text.length;
  }

  function isScrollable(el) {
    if (!el.scrollHeight) return false;
    if (el.scrollHeight <= el.clientHeight + 2 && el.scrollWidth <= el.clientWidth + 2) return false;
    // A cheap proxy for overflow:auto|scroll that avoids getComputedStyle on
    // every node in the document.
    return el.clientHeight > 0 && el.scrollHeight > el.clientHeight + 8;
  }

  // A custom element is one whose name has a dash in it. Only these can be
  // undefined, and only these upgrade later.
  function isCustom(el) {
    var n = el.localName;
    return typeof n === 'string' && n.indexOf('-') > 0;
  }

  // isDefined reports whether the page has registered and run this element's
  // definition. Errs towards "defined": the placeholder styling is the
  // exception, and claiming it for an element that does not want it is the
  // more visible mistake.
  function isDefined(el) {
    try { return el.matches(':defined'); } catch (e) { return true; }
  }

  // flagsOf reads the four flags that describe an element rather than its
  // contents. Serialising and fingerprinting share it so the two can never
  // drift: a capture comparing the flags an element has landside against the
  // ones the client was sent is only worth reading if both were computed the
  // same way. FLAG_IMAGE is not here — it belongs to the act of queueing an
  // image for transcoding, not to the element.
  function flagsOf(el) {
    var flags = 0;
    if (isEditable(el)) flags |= FLAG_EDITABLE;
    if (CANVAS_TAGS[el.tagName]) flags |= FLAG_CANVAS;
    if (isScrollable(el)) flags |= FLAG_SCROLL;
    if (el.shadowRoot) flags |= FLAG_SHADOW;
    return flags;
  }

  // localNameOf gives the name an element must be rebuilt under. For HTML this
  // is the lowercased tagName; for SVG and MathML, where names are
  // case-sensitive, it is the only correct spelling.
  function localNameOf(node) {
    return node.localName || (node.tagName ? node.tagName.toLowerCase() : 'div');
  }

  // isSkipped tests the name rather than tagName, because inside SVG a
  // <script> reports tagName "script" and the uppercase table misses it — the
  // one tag whose contents must never reach the wire.
  function isSkipped(node) {
    return !!SKIP_TAGS[localNameOf(node).toUpperCase()];
  }

  function ownerDoc(node) {
    return node.ownerDocument || document;
  }

  function docBase(node) {
    var d = ownerDoc(node);
    try { return d.baseURI || location.href; } catch (e) { return location.href; }
  }

  // ------------------------------------------------------------ serialisation

  /**
   * isSensitive marks fields whose contents must never travel to the client.
   *
   * The user already has these characters — they typed them — so mirroring the
   * value back buys nothing and costs a great deal: it would sit in the replay
   * ring, in every resync, and in whatever the client persists. rrweb learned
   * this as a privacy feature; here it is simply the only defensible default.
   *
   * The test is narrow and predictable on purpose. A field wrongly judged
   * sensitive would lose what the user typed on a resync, so guessing from
   * names like "pass" is worse than useless.
   */
  function isSensitive(el) {
    if (!el || el.tagName !== 'INPUT') return false;
    var type = (el.getAttribute('type') || '').toLowerCase();
    if (type === 'password') return true;
    var auto = el.getAttribute('autocomplete') || '';
    if (SENSITIVE_AUTOCOMPLETE.test(auto)) return true;
    return el.hasAttribute('data-sky-mask');
  }

  function serializeAttrs(el, out) {
    var attrs = el.attributes;
    var base = docBase(el);
    var flags = 0;
    var pairs = [];
    var spriteAttr = null;
    for (var i = 0; i < attrs.length; i++) {
      var a = attrs[i];
      var name = a.name;
      if (name.length > 2 && name.charCodeAt(0) === 111 && name.charCodeAt(1) === 110) {
        if (/^on[a-z]/.test(name)) continue; // inline handlers never cross
      }
      if (SKIP_ATTRS[name]) continue;
      if (SENSITIVE_ATTRS[name] && isSensitive(el)) continue;
      var value = a.value;
      if (URL_ATTRS[name]) {
        value = absolute(base, value);
        if (/^javascript:/i.test(value)) continue;
      } else if (name === 'style') {
        var shot = styleAttrImages(value, base, el);
        if (shot) {
          value = shot.text;
          for (var s = 0; s < shot.images.length; s++) pendingImages.push(shot.images[s]);
        }
        // A style attribute travels with the DOM rather than with the sheet,
        // so it needs the same answer the sheet's rules get. See
        // pinColorSchemes.
        value = pinColorSchemes(value);
      }
      if ((name === 'href' || name === 'xlink:href') && tagOf(el) === 'USE') {
        var spr = useSpriteRef(el);
        if (spr) {
          // Re-pointed at the copy the client will build from the carried
          // fragment (P-116); the sprite is fetched here, where it can be.
          // The fragment itself is appended after the loop: the client
          // materialises it by reading the reference this rewrite sets, so
          // the reference has to be applied first.
          value = '#' + spr.key;
          var sm = spriteMarkup(spr.url, spr.frag);
          if (sm) spriteAttr = sm;
          else requestSprite(spr.url, el);
        }
      }
      pairs.push(intern(name), intern(value));
    }
    if (spriteAttr) pairs.push(intern('data-sky-sprite'), intern(spriteAttr));
    // Live form state is a property, not an attribute; without this a mirrored
    // form loses everything the user (or the page) already typed. What is
    // recorded here is also what the sweep compares against, so a page that
    // changes any of it later is a difference rather than a re-send.
    var tag = el.tagName;
    if (tag !== 'INPUT' && tag !== 'TEXTAREA' && isLiveEditable(el)) {
      // An editing host. Its text mirrors as DOM like anything else; this is
      // the copy the client compares against while it owns the field and is
      // holding those mutations aside.
      var hostText = liveValue(el);
      if (hostText.length <= LIVE_TEXT_MAX) {
        pairs.push(intern('data-sky-value'), intern(hostText));
        watchLive(el, { value: hostText, checked: false });
      }
    } else if (tag === 'INPUT' || tag === 'TEXTAREA') {
      var checked = !!el.checked;
      if (!isSensitive(el)) {
        // A file input's value is a fake local path no script may write —
        // the plane's browser throws on anything but '' — and the filename
        // it leaks belongs to this machine. The chosen files' story is told
        // by the page's own DOM, so the value ships as empty (P-007).
        var value = liveValue(el);
        if (el.type !== 'file') pairs.push(intern('data-sky-value'), intern(value));
        watchLive(el, { value: value, checked: checked, indeterminate: !!el.indeterminate });
      }
      if (checked) pairs.push(intern('data-sky-checked'), intern('1'));
      // The third state of a checkbox, which is a property and nothing else:
      // no attribute reflects it, so a serializer sees a box that is merely
      // unchecked. Every "select all" over a partly-selected list is one —
      // a mail app's message list, a file manager, any table with a header
      // tick — and the reader was shown the wrong answer to "is this on?"
      // (P-135).
      if (el.indeterminate) pairs.push(intern('data-sky-indeterminate'), intern('1'));
    } else if (tag === 'OPTION') {
      var selected = !!el.selected;
      if (selected) pairs.push(intern('data-sky-selected'), intern('1'));
      watchLive(el, { selected: selected });
    }
    // Whether a custom element has upgraded is a live question landside and a
    // settled one plane-side, where no definition will ever run: every custom
    // element there is undefined for ever. A site that dresses its placeholders
    // with `:not(:defined)` would get the placeholder styling on top of the
    // upgraded markup, and the upgraded styling — gated on `:defined` — would
    // match nothing. So the landside answer is recorded here, and the used-CSS
    // rules are rewritten against it (see rewriteDefined in css.go).
    if (isCustom(el) && !isDefined(el)) pairs.push(intern(UNDEFINED_ATTR), intern(''));
    // And which element the document's own URL names, for the same reason: a
    // page that highlights the footnote the reader followed a link to is
    // answering a question the mirror's address cannot. See syncTarget.
    if (el === markedTarget) pairs.push(intern(TARGET_ATTR), intern(''));
    flags |= flagsOf(el);
    // A container the page has already scrolled says so here, because a
    // snapshot is the only chance it gets. onScroll reports a container when
    // it moves, and a container that was already where it belongs before this
    // document was built never moves again — so a conversation pinned to its
    // newest message arrived at the top and stayed there, with the message the
    // reader had just sent below the fold and nothing able to bring it back
    // (P-130).
    if (scrolled && (flags & FLAG_SCROLL) && (el.scrollTop || el.scrollLeft)) {
      var sid = idOf.get(el) || 0;
      if (sid) scrolled.push([sid, el.scrollLeft | 0, el.scrollTop | 0]);
    }
    if (tag === 'IFRAME') {
      // The client cannot materialise an iframe — it would be a browsing
      // context, and the whole point is that nothing plane-side fetches
      // anything — so it renders the inlined document into a plain box. That
      // box has to be told how big it is, because the CSS that sized the real
      // iframe selects on `iframe` and will not match the substitute.
      var box = frameBox(el);
      var host = opaqueHost(el, box);
      if (box !== '0x0') pairs.push(intern('data-sky-box'), intern(box));
      if (host) pairs.push(intern(OPAQUE_ATTR), intern(host));
      watchBox(el, box, host);
    }
    if (PLUGIN_TAGS[tag]) {
      // The iframe bargain for plugin content (P-106): a labelled box the
      // size the plugin had, instead of an unexplained hole. The computed
      // display travels too, because the CSS that made the real object a
      // block selects on a tag name the stand-in no longer answers to.
      var pbox = frameBox(el);
      var phost = pluginHost(el, pbox);
      if (pbox !== '0x0') pairs.push(intern('data-sky-box'), intern(pbox));
      if (phost) pairs.push(intern(OPAQUE_ATTR), intern(phost));
      var pdisp = pluginDisplay(el);
      if (pdisp) pairs.push(intern('data-sky-display'), intern(pdisp));
      watchBox(el, pbox, phost);
    }
    var tl = topLayerState(el);
    if (tl) {
      // A popover shown or a dialog made modal before this element was
      // serialised (P-122); later changes arrive through onTopLayer.
      pairs.push(intern('data-sky-open'), intern(tl));
      topLayerShipped.set(el, tl);
    }
    if (VOID_IMAGE_TAGS[tag]) {
      var img = describeImage(el, base);
      if (img) {
        flags |= FLAG_IMAGE;
        pairs.push(intern('src'), intern('skyhook://img/' + img.key));
        if (img.aw) pairs.push(intern('width'), intern(String(img.aw)));
        if (img.ah) pairs.push(intern('height'), intern(String(img.ah)));
        pendingImages.push(img);
        watchImg(el, img.aw, img.ah);
      }
    }
    out.flags = flags;
    return pairs;
  }

  // --------------------------------------------------------- frame stand-ins

  /*
   * A frame's stand-in is sized once when it is serialised, and a frame that
   * changes size after that used to keep the box it was born with for ever.
   *
   * That is not an edge case: it is how every popover on a Google property
   * opens. Gmail's app launcher is a cross-origin iframe inside a wrapper the
   * page animates from `height: 0` to its full height, and the frame is put in
   * the document at the moment the animation starts — so the box the agent
   * measured was 370x0, the wrapper's own height reached the client (it is an
   * inline style, and a style is just an attribute), and the panel underneath
   * it stayed exactly as tall as nothing. The reader clicks the grid of dots
   * and the mirror does not change, which reads as an input that was dropped
   * on the way and cannot be told apart from one.
   *
   * `getBoundingClientRect` cannot be polled cheaply on every frame of a
   * transition, and a MutationObserver never sees this: the style that changed
   * is on an ancestor, and layout is not a mutation. A ResizeObserver is the
   * one thing that reports exactly this, so the frames get one, and the boxes
   * it marks dirty are re-read on the way into the next flush — where a size
   * that moved ten times during a 300ms transition costs one op.
   */

  function frameBox(el) {
    var r = el.getBoundingClientRect();
    return Math.round(r.width) + 'x' + Math.round(r.height);
  }

  /**
   * opaqueHost is the origin of a frame whose document cannot be read, and ''
   * for one that can — or one too small for the answer to be worth showing.
   *
   * A cross-origin frame is a hole in the mirror that nothing can fill: no
   * agent runs in it, `contentDocument` throws, and the stand-in stays empty
   * however right its box is. Empty and unexplained, it is indistinguishable
   * from a bug — which is exactly how it was reported. Named, it is the same
   * failure the HUD makes elsewhere: this much did not come, and here is why.
   */
  function opaqueHost(el, box) {
    // A frame the host has attached to is mirrored by an agent of its own, and
    // its content is spliced in under this element. Naming it as missing would
    // put a "not mirrored" panel on top of the thing it is mirroring.
    if (mirroredFrames.has(el)) return '';
    try {
      if (el.contentDocument && el.contentDocument.documentElement) return '';
    } catch (e) { /* cross-origin: nothing to read, which is the point */ }
    var wh = box.split('x');
    if (+wh[0] < FRAME_LABEL_MIN_W || +wh[1] < FRAME_LABEL_MIN_H) return '';
    var src = el.getAttribute('src') || '';
    if (!src) return ''; // about:blank, and nothing to say about it
    try {
      var u = new URL(absolute(docBase(el), src));
      return u.protocol === 'http:' || u.protocol === 'https:' ? u.host : '';
    } catch (e) { return ''; }
  }

  // pluginDisplay is the display the landside plugin computed, for the
  // stand-in to reproduce. Read once at serialisation: a plugin that changes
  // display after that is rarer than one that never should have crossed.
  function pluginDisplay(el) {
    try {
      var win = el.ownerDocument.defaultView || globalThis;
      return String(win.getComputedStyle(el).display || '');
    } catch (e) { return ''; }
  }

  // pluginHost is opaqueHost for a plugin container, which names its resource
  // with `data` (object) or `src` (embed), and is opaque by nature rather
  // than by origin.
  function pluginHost(el, box) {
    var wh = box.split('x');
    if (+wh[0] < FRAME_LABEL_MIN_W || +wh[1] < FRAME_LABEL_MIN_H) return '';
    var src = el.getAttribute('data') || el.getAttribute('src') || '';
    if (!src) return '';
    try {
      var u = new URL(absolute(docBase(el), src));
      return u.protocol === 'http:' || u.protocol === 'https:' ? u.host : '';
    } catch (e) { return ''; }
  }

  function onBoxResize(entries) {
    for (var i = 0; i < entries.length; i++) boxDirty.add(entries[i].target);
    // The size is read at flush time, not here: during a transition this runs
    // on every animation frame, and the answer is only worth the wire once.
    scheduleFlush(false);
  }

  // watchBox records what the client has been told about a frame's stand-in and
  // arranges to hear about it changing. The observer belongs to the frame's own
  // window, because an inlined frame's elements are in that document.
  function watchBox(el, box, host) {
    boxWatch.set(el, box + ' ' + host);
    boxDirty.delete(el);
    observeResize(el);
  }

  // watchImg remembers the size an image was described at. The description is
  // a measurement, and it is routinely taken early: an image serialised before
  // the page's stylesheet has loaded ships the box it has *without* that CSS,
  // and nothing in a MutationObserver ever corrects it — the border that
  // arrives with the sheet changes layout, not the DOM. The client then holds
  // a width= the landside page stopped rendering half a second in.
  function watchImg(el, w, h) {
    imgShipped.set(el, w + 'x' + h);
    observeResize(el);
  }

  function observeResize(el) {
    var win = null;
    try { win = el.ownerDocument && el.ownerDocument.defaultView; } catch (e) { win = null; }
    if (!win || typeof win.ResizeObserver !== 'function') return;
    var obs = boxObservers.get(win);
    if (!obs) {
      try { obs = new win.ResizeObserver(onBoxResize); } catch (e) { return; }
      boxObservers.set(win, obs);
    }
    try { obs.observe(el); } catch (e) { /* gone already */ }
  }

  function unwatchBox(el) {
    if (!boxWatch.has(el) && !imgShipped.has(el)) return;
    boxWatch.delete(el);
    imgShipped.delete(el);
    boxDirty.delete(el);
    boxObservers.forEach(function (obs) {
      try { obs.unobserve(el); } catch (e) { /* not this one's */ }
    });
  }

  // syncBoxes turns the frames marked dirty into attribute ops. Called on the
  // way into a flush, so a resize that came with no mutation of its own still
  // reaches the client, and a resize that came with one rides the same frame.
  function syncBoxes() {
    if (!boxDirty.size) return;
    var els = [];
    boxDirty.forEach(function (el) { els.push(el); });
    boxDirty.clear();
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      var id = idOf.get(el);
      if (id === undefined || !el.isConnected) { unwatchBox(el); continue; }
      if (imgShipped.has(el)) { syncImg(el, id); continue; }
      var box = frameBox(el);
      var host = PLUGIN_TAGS[el.tagName] ? pluginHost(el, box) : opaqueHost(el, box);
      var was = (boxWatch.get(el) || ' ').split(' ');
      if (was[0] === box && was[1] === host) continue;
      boxWatch.set(el, box + ' ' + host);
      if (was[0] !== box) {
        pendingOps.push([3, id, intern('data-sky-box'), intern(box)]);
      }
      if (was[1] !== host) {
        pendingOps.push([3, id, intern(OPAQUE_ATTR), host ? intern(host) : -1]);
      }
    }
  }

  // syncImg re-states an image's size when the rendered box has drifted from
  // the one it was described with. Only the attributes move: the transcode
  // keyed to the old size is a few pixels soft at worst, which is invisible,
  // while a wrong width= is a wrong layout for every element beside it.
  function syncImg(el, id) {
    var img = describeImage(el, docBase(el));
    if (!img) return;
    var now = img.aw + 'x' + img.ah;
    if (imgShipped.get(el) === now) return;
    imgShipped.set(el, now);
    if (img.aw) pendingOps.push([3, id, intern('width'), intern(String(img.aw))]);
    if (img.ah) pendingOps.push([3, id, intern('height'), intern(String(img.ah))]);
  }

  // ------------------------------------------------------ live control state

  /*
   * What a control holds is a property, and properties are not in the DOM a
   * MutationObserver describes.
   *
   * `value`, `checked` and `selected` were read once, when the control was
   * serialised, and refreshed after that only by an `input` event landside —
   * which covers the reader's own typing and nothing else. Everything a *page*
   * does to its own form is silent: React re-rendering a controlled input, a
   * draft being restored, a search box cleared after submit, a "select all"
   * ticking every row, a dependent dropdown moving to its new choice. The
   * mirror went on showing the state before it, indefinitely, and the integrity
   * check cannot see any of it — the hash is over ids, kinds and names, so both
   * sides agree about a document they disagree about.
   *
   * So the controls the mirror carries state for are watched, and the sweep
   * reports the difference. Reading these three properties forces no layout, so
   * the pass costs a property read per control and reaches the wire only when
   * an answer has actually changed.
   */

  // watchLive records the state a control was serialised with.
  // The value the wire may carry for a form control. A file input's is
  // always '': its real value is a fake local path no script may write —
  // the mirror's browser throws on anything but the empty string — and the
  // filename in it belongs to this machine, not to the wire (P-007).
  /*
   * LIVE_TEXT_MAX bounds the text of an editing host that is worth watching.
   *
   * A watched field reports its whole text whenever it changes — which is what
   * an input and a textarea have always done, and what makes the report worth
   * anything: the client compares it against its own copy. A chat composer
   * holds a sentence and that is cheap. A document editor's editing host holds
   * the document, and re-sending all of it would spend the link on what the
   * reader can already see. Past the bound the field stops being watched: the
   * client keeps its own text, which is what every editing host did before
   * this existed, and loses only the correction.
   */
  var LIVE_TEXT_MAX = 8192;

  /*
   * isLiveEditable reports whether an element's text is state the client has
   * to be told about separately from the DOM.
   *
   * For an input or a textarea that is the whole of it: the value is a
   * property, invisible to a serializer, and without it a mirrored form loses
   * what was typed. An editing host is the odd one — its text *is* DOM and
   * arrives as mutations like any other — but while the client owns the field
   * those mutations are held aside on purpose, and the value is then the only
   * thing that can tell the client the page rewrote what it typed: an emoji
   * for a smiley, a mention for an @name, an Enter the page kept for its own
   * autocomplete instead of sending (P-132).
   */
  function isLiveEditable(el) {
    var tag = el.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA') return true;
    var mode = el.contentEditable;
    return mode === 'true' || mode === 'plaintext-only';
  }

  function liveValue(el) {
    if (el.type === 'file') return '';
    if (el.value == null) return String(el.textContent == null ? '' : el.textContent);
    return String(el.value);
  }

  function watchLive(el, state) {
    liveWatch.set(el, state);
  }

  // emitLive sends whatever has changed about one control, and says whether
  // anything had. Shared by the sweep and by the input listener, so the two can
  // never disagree about what the client has been told.
  function emitLive(el) {
    var id = idOf.get(el);
    if (id === undefined || !el.isConnected) {
      liveWatch.delete(el);
      return false;
    }
    var was = liveWatch.get(el) || {};
    var changed = false;
    if (el.tagName === 'OPTION') {
      var selected = !!el.selected;
      if (was.selected !== selected) {
        // Removed rather than set to "0": the attribute means chosen, and the
        // client turns its absence back into a deselected option.
        pendingOps.push([3, id, intern('data-sky-selected'), selected ? intern('1') : -1]);
        changed = true;
      }
      liveWatch.set(el, { selected: selected });
      return changed;
    }
    // A field holding a secret is not mirrored in either direction, and a field
    // that has become one since it was serialised stops being watched here.
    if (isSensitive(el)) {
      liveWatch.delete(el);
      return false;
    }
    var value = liveValue(el);
    var checked = !!el.checked;
    // An editing host that has grown past what is worth re-sending on every
    // change. See LIVE_TEXT_MAX: the client keeps its own text from here on.
    if (el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA' && value.length > LIVE_TEXT_MAX) {
      liveWatch.delete(el);
      return false;
    }
    if (was.value !== value) {
      pendingOps.push([3, id, intern('data-sky-value'), intern(value)]);
      changed = true;
    }
    if (was.checked !== checked) {
      pendingOps.push([3, id, intern('data-sky-checked'), checked ? intern('1') : -1]);
      changed = true;
    }
    var mixed = !!el.indeterminate;
    if (was.indeterminate !== mixed) {
      pendingOps.push([3, id, intern('data-sky-indeterminate'), mixed ? intern('1') : -1]);
      changed = true;
    }
    liveWatch.set(el, { value: value, checked: checked, indeterminate: mixed });
    return changed;
  }

  function syncLive() {
    if (!liveWatch.size) return false;
    var els = [];
    liveWatch.forEach(function (_, el) { els.push(el); });
    var changed = false;
    for (var i = 0; i < els.length; i++) {
      if (emitLive(els[i])) changed = true;
    }
    return changed;
  }

  /*
   * The page's own name and address, which are not nodes and have no mutation
   * to ride on.
   *
   * Both travel as fields on every mutation frame, and a frame is only sent
   * when there are ops to put in it — so a page whose *only* change is its
   * title never sent one. That is unread counts, "(2) Slack", a build finishing
   * in a CI tab: exactly the pages that change nothing else. The title's own
   * text node is not mirrored either (head content is replaced by used-CSS), so
   * its mutation record is dropped and nothing is left to notice.
   *
   * One op fixes it, and it has to be an op rather than an empty frame: the
   * host drops a mutation with nothing in it, and the agent's sequence numbers
   * are the ones the integrity check waits for the client to reach. A frame the
   * host discards would leave the two counting differently.
   */
  function syncDocInfo() {
    // An attached frame's address and title are its own, and the tab wears the
    // top-level document's. A frame reporting them would rename the tab to
    // whatever a widget calls itself.
    if (SLOT) return false;
    var url = location.href;
    var title = document.title || '';
    if (url === lastInfo.url && title === lastInfo.title) return false;
    lastInfo = { url: url, title: title };
    pendingOps.push([11, title]);
    return true;
  }

  function describeImage(el, base) {
    var src = el.currentSrc || el.getAttribute('src') || '';
    if (!src) return null;
    src = absolute(base, src);
    if (/^data:/i.test(src)) {
      // Small inline images are cheaper left alone than round-tripped.
      if (src.length < 4096) return null;
    }
    // blob: crosses described like anything else (P-103): serialised verbatim
    // it named bytes only this page's process holds — a broken image with no
    // fallback and no notice. The pipeline reads it from inside the page.
    var r = el.getBoundingClientRect();
    // Two sizes with two jobs. The transcode target falls back to the natural
    // size so an image serialised before layout still ships at a useful
    // resolution; the attribute size is the rendered box and nothing else —
    // an author's width="0" spacer resurrected to its natural pixel was a
    // spacer visible on one half only.
    var aw = Math.round(r.width), ah = Math.round(r.height);
    var w = aw || el.naturalWidth || 0;
    var h = ah || el.naturalHeight || 0;
    // The transcode ceiling is in device pixels, not CSS ones (P-113): the
    // tab emulates the reader's density, so devicePixelRatio here is theirs,
    // and a 2x reader gets a 2x rendition — fit() still never upscales past
    // the source, so a 1x source costs nothing new. Folding the density into
    // the dimensions also folds it into the key, so readers at different
    // densities never collide in the transcode cache. Capped at 3x.
    var dpr = globalThis.devicePixelRatio || 1;
    if (dpr > 3) dpr = 3;
    if (dpr > 1) { w = Math.round(w * dpr); h = Math.round(h * dpr); }
    if (w > 4096) w = 4096;
    if (h > 4096) h = 4096;
    var key = imageKey(src, w, h);
    return {
      n: idFor(el), url: src, w: w, h: h, aw: aw, ah: ah, key: key,
      alt: el.getAttribute('alt') || '',
      pri: r.top < (globalThis.innerHeight || 900) * 1.5 && r.bottom > -200 ? 0 : 1
    };
  }

  // ------------------------------------------------------------- parity probe

  /**
   * probeAttrs is serializeAttrs without the wire: the attributes this agent
   * would put on a node if it serialised it right now, as a plain object.
   *
   * Kept in step with serializeAttrs by hand — the two walk the same rules in
   * the same order — because the serialiser cannot be reused directly: it
   * interns strings, queues images for transcoding and registers observers,
   * and a probe that changed what the reader is looking at would be measuring
   * itself. What this returns is compared against the attributes the patcher
   * actually holds, so the pair only agrees when every mutation arrived.
   */
  function probeAttrs(el) {
    var out = {};
    var any = false;
    var attrs = el.attributes;
    var base = docBase(el);
    for (var i = 0; i < attrs.length; i++) {
      var a = attrs[i];
      var name = a.name;
      if (name.length > 2 && name.charCodeAt(0) === 111 && name.charCodeAt(1) === 110) {
        if (/^on[a-z]/.test(name)) continue;
      }
      if (SKIP_ATTRS[name]) continue;
      if (SENSITIVE_ATTRS[name] && isSensitive(el)) continue;
      var value = a.value;
      if (URL_ATTRS[name]) {
        value = absolute(base, value);
        if (/^javascript:/i.test(value)) continue;
      } else if (name === 'style') {
        var shot = styleAttrImages(value, base, el);
        if (shot) value = shot.text; // and the images stay unqueued
        value = pinColorSchemes(value);
      }
      if ((name === 'href' || name === 'xlink:href') && tagOf(el) === 'USE') {
        var spr = useSpriteRef(el);
        if (spr) {
          value = '#' + spr.key;
          // Read-only twin of the serializer's branch: report the fragment
          // when it is cached, and never start a fetch from a probe.
          var sm = spriteMarkup(spr.url, spr.frag);
          if (sm) { out['data-sky-sprite'] = sm; any = true; }
        }
      }
      out[name] = value;
      any = true;
    }
    var tag = el.tagName;
    if (tag !== 'INPUT' && tag !== 'TEXTAREA' && isLiveEditable(el)) {
      var hostText = liveValue(el);
      if (hostText.length <= LIVE_TEXT_MAX) { out['data-sky-value'] = hostText; any = true; }
    } else if (tag === 'INPUT' || tag === 'TEXTAREA') {
      if (!isSensitive(el) && el.type !== 'file') {
        out['data-sky-value'] = liveValue(el);
        any = true;
      }
      if (el.checked) { out['data-sky-checked'] = '1'; any = true; }
      if (el.indeterminate) { out['data-sky-indeterminate'] = '1'; any = true; }
    } else if (tag === 'OPTION') {
      if (el.selected) { out['data-sky-selected'] = '1'; any = true; }
    }
    if (isCustom(el) && !isDefined(el)) { out[UNDEFINED_ATTR] = ''; any = true; }
    if (el === markedTarget) { out[TARGET_ATTR] = ''; any = true; }
    if (tag === 'IFRAME') {
      var box = frameBox(el);
      var host = opaqueHost(el, box);
      if (box !== '0x0') { out['data-sky-box'] = box; any = true; }
      if (host) { out[OPAQUE_ATTR] = host; any = true; }
    }
    if (PLUGIN_TAGS[tag]) {
      var pbox = frameBox(el);
      var phost = pluginHost(el, pbox);
      if (pbox !== '0x0') { out['data-sky-box'] = pbox; any = true; }
      if (phost) { out[OPAQUE_ATTR] = phost; any = true; }
      var pdisp = pluginDisplay(el);
      if (pdisp) { out['data-sky-display'] = pdisp; any = true; }
    }
    var tl = topLayerState(el);
    if (tl) { out['data-sky-open'] = tl; any = true; }
    if (VOID_IMAGE_TAGS[tag]) {
      var img = describeImage(el, base);
      if (img) {
        out.src = 'skyhook://img/' + img.key;
        if (img.aw) out.width = String(img.aw);
        if (img.ah) out.height = String(img.ah);
        any = true;
      }
    }
    return any ? out : null;
  }

  /**
   * probeElement is one row of the parity probe: everything the comparison
   * engine wants to hold against the patcher's copy of the same node.
   *
   * The box is the raw viewport rectangle plus the id of the element's own
   * document root; the engine subtracts the root's box from the node's, which
   * cancels scroll and puts an inlined frame's content into the frame's own
   * coordinates — the same coordinates the plane side measures it in.
   */
  function probeElement(el, id, families) {
    var d = ownerDoc(el);
    var win = d.defaultView || globalThis;
    var cs = null;
    try { cs = win.getComputedStyle(el); } catch (e) { cs = null; }
    var style = [];
    for (var i = 0; i < STYLE_PROPS.length; i++) {
      style.push(cs ? String(cs.getPropertyValue(STYLE_PROPS[i])) : '');
    }
    var r = { left: 0, top: 0, width: 0, height: 0 };
    try { r = el.getBoundingClientRect(); } catch (e) { /* keep zeros */ }
    var display = cs ? cs.getPropertyValue('display') : '';
    var visibility = cs ? cs.getPropertyValue('visibility') : '';
    var visible = display !== 'none' && visibility !== 'hidden' && r.width > 0 && r.height > 0;
    // Own text only: the direct text children, collapsed the way
    // internal/parity's collapseText does. Deep text belongs to the
    // descendants that carry it.
    var text = '';
    var kids = el.childNodes;
    for (var c = 0; c < kids.length && text.length < 96; c++) {
      if (kids[c].nodeType === KIND_TEXT) text += kids[c].nodeValue || '';
    }
    text = text.replace(/\s+/g, ' ').replace(/^ | $/g, '').slice(0, 24);
    var rootEl = d.documentElement;
    var probe = {
      i: id,
      t: localNameOf(el).toLowerCase(),
      b: [r.left, r.top, r.width, r.height],
      s: style,
      v: visible,
      r: rootEl ? (idOf.get(rootEl) || 0) : 0
    };
    if (text) probe.x = text;
    var attrs = probeAttrs(el);
    if (attrs) probe.a = attrs;
    if (tagOf(el) === 'IMG') {
      probe.g = {
        ok: !!(el.complete && el.naturalWidth > 0),
        w: el.naturalWidth | 0,
        h: el.naturalHeight | 0
      };
    }
    if (cs) {
      var fam = String(cs.getPropertyValue('font-family'));
      probe.f = fam;
      var first = firstFamilyName(fam);
      if (first && !families.has(first)) families.set(first, d);
    }
    return probe;
  }

  function tagOf(el) {
    return el.tagName ? String(el.tagName).toUpperCase() : '';
  }

  function firstFamilyName(list) {
    var first = list;
    var comma = list.indexOf(',');
    if (comma >= 0) first = list.slice(0, comma);
    first = first.replace(/^\s+|\s+$/g, '');
    return first.replace(/^["']|["']$/g, '');
  }

  // serializeNode appends [id, parent, kind, ref, flags, attrs] rows in document
  // order. Returns the number of rows appended.
  function serializeNode(node, parentId, rows) {
    var kind = node.nodeType;
    if (kind === KIND_TEXT) {
      var text = node.nodeValue;
      if (!text) return 0;
      var id = idFor(node);
      lastText.set(id, text);
      rows.push([id, parentId, KIND_TEXT, intern(text), 0, null]);
      return 1;
    }
    if (kind === KIND_DOCTYPE) {
      rows.push([idFor(node), parentId, KIND_DOCTYPE, intern(node.name || 'html'), 0, null]);
      return 1;
    }
    if (kind !== KIND_ELEMENT) return 0;
    var tag = node.tagName;
    if (isSkipped(node)) return 0;

    var id2 = idFor(node);
    var out = { flags: 0 };
    var pairs = serializeAttrs(node, out);
    // localName, not the lowercased tagName: SVG element names are
    // case-sensitive, and `clippath` builds nothing. For HTML the two agree.
    rows.push([id2, parentId, KIND_ELEMENT, intern(localNameOf(node)), out.flags, pairs]);
    var n = 1;

    if (tag === 'HEAD') return n; // head content is replaced by used-CSS
    if (CANVAS_TAGS[tag] || PLUGIN_TAGS[tag]) return n;

    if (node.shadowRoot) {
      // The boundary is mirrored, not flattened away. A component's stylesheet
      // is written against it — `:host`, `::part()`, `::slotted()` all name it,
      // and the rules that name nothing still lean on it, because `label {}`
      // inside a text input means that input's label and no other. Rebuilt
      // plane-side, all of that means what it meant. See §31.
      //
      // Slot assignment comes free with it: the light DOM stays where it sits
      // and the browser composes it, which is what it is for.
      var rootId = idFor(node.shadowRoot);
      rows.push([rootId, id2, KIND_FRAGMENT, -1, 0, null]);
      n += 1;
      docRoot.set(node.shadowRoot, rootId);
      var sk = node.shadowRoot.childNodes;
      for (var s = 0; s < sk.length; s++) n += serializeNode(sk[s], rootId, rows);
      observeDocument(node.shadowRoot);
    } else if (isCustom(node) && !isDefined(node)) {
      watchUpgrade(id2);
    }
    if (tag === 'IFRAME') {
      hookFrame(node);
      var idoc = null;
      try { idoc = node.contentDocument; } catch (e) { idoc = null; }
      if (idoc && idoc.documentElement) {
        // The frame's document goes inside a shadow root rather than straight
        // into the stand-in box. A frame is a document, and a document's
        // stylesheet is written on the assumption that it governs a document —
        // `body { margin: 0 }` means this frame's body. Inlined flat, that
        // sheet joins the page's own and the rule reaches the page's body too.
        // The root is the boundary that makes it mean what it says. See §31.
        // Registered like any other node, and against the frame's document:
        // the hash is taken over what is registered, so a node the client is
        // sent and the agent does not count is a divergence every thirty
        // seconds. It also gives the document an id, which is what a later
        // mutation inside the frame needs to find its parent.
        var fragId = idFor(idoc);
        rows.push([fragId, id2, KIND_FRAGMENT, -1, 0, null]);
        n += 1;
        docRoot.set(idoc, fragId);
        n += serializeNode(idoc.documentElement, fragId, rows);
        observeDocument(idoc);
      }
      return n;
    }
    var kids = node.childNodes;
    for (var i = 0; i < kids.length; i++) n += serializeNode(kids[i], id2, rows);
    return n;
  }

  /**
   * Re-reads a same-origin frame when it navigates.
   *
   * A frame's document is replaced wholesale, and nothing about that reaches
   * the MutationObserver watching the old one: no records, no removals, no way
   * to notice. The mirror would go on showing the document the frame used to
   * hold. Re-snapshotting is blunt, but frame navigation is rare and a stale
   * frame is a lie. Cross-origin frames are skipped — there is nothing to read.
   */
  function hookFrame(el) {
    if (hookedFrames.has(el)) return;
    hookedFrames.add(el);
    el.addEventListener('load', function () {
      var readable = false;
      try { readable = !!el.contentDocument; } catch (e) { readable = false; }
      if (!readable) {
        // Nothing to re-read, but two things about this frame have just become
        // knowable: the origin it settled on — a frame starts at about:blank,
        // which is readable and says nothing — and the size it took there.
        if (boxWatch.has(el)) { boxDirty.add(el); scheduleFlush(false); }
        return;
      }
      if (reframeTimer) return;
      reframeTimer = setTimeout(function () {
        reframeTimer = null;
        api.snapshot();
      }, REFRAME_DEBOUNCE_MS);
    }, { passive: true });
  }

  // ------------------------------------------------------- custom elements

  /*
   * A custom element upgrades when its definition arrives, which on a
   * code-split site is long after the element itself was in the document. The
   * upgrade attaches a shadow root and distributes the light DOM into its
   * slots — and none of that reaches a MutationObserver, which reports child
   * lists, attributes and text and has nothing at all to say about
   * attachShadow. Whatever the mirror saw first, it would keep for ever.
   *
   * What the client keeps in that case is the pre-upgrade skeleton: the
   * component's own markup missing, and the light-DOM children that were
   * destined for a slot inside some collapsed popup rendered flat and in the
   * open. A mirrored Reddit showed every sort menu, country list and view
   * picker on the page at once, stacked over the feed.
   *
   * There is no event for this, so it is polled — landside, where cycles are
   * cheap and nothing reaches the wire unless something actually changed.
   */

  function watchUpgrade(id) {
    if (awaitingUpgrade.size >= UPGRADE_WATCH_MAX) return;
    awaitingUpgrade.add(id);
    // Something new to look at, so start the interval over: an element usually
    // upgrades soon after it lands, and a long-backed-off timer would sit on
    // the change for eight seconds.
    sweepEvery = SWEEP_MS;
    scheduleSweep(sweepEvery);
  }

  /*
   * The sweep: one pass over everything that changes without saying so.
   *
   * Three things share it, because they share a shape — a question only
   * landside can answer, no event to answer it, and an answer worth nothing
   * until something asks. Upgrades were the first (§19) and had the poll to
   * themselves; late shadow roots and live control state are the same problem
   * wearing different clothes, and a second and third timer would have cost
   * three wake-ups where one does.
   *
   * It backs off to eight seconds while it keeps finding nothing, and drops
   * back to half a second the moment anything moves — components load in
   * waves, and so do the forms a page fills in.
   */
  function scheduleSweep(delay) {
    if (sweepTimer || !started) return;
    sweepTimer = setTimeout(function () {
      sweepTimer = null;
      var roots = checkUpgrades();
      if (sweepShadowRoots()) roots = true;
      var changed = roots;
      if (syncLive()) changed = true;
      if (syncDocInfo()) changed = true;
      if (syncTarget()) changed = true;
      if (changed) scheduleFlush(false);
      // A root that arrived — by upgrade or by hand — brings its own sheet.
      if (roots) scheduleCSS();
      sweepEvery = changed ? SWEEP_MS : Math.min(sweepEvery * 2, SWEEP_MAX_MS);
      scheduleSweep(sweepEvery);
    }, delay);
  }

  /*
   * A shadow root attached after its element was serialised.
   *
   * The upgrade watch above covers the case a custom element makes: mirrored
   * undefined, watched from that moment, re-read when its definition lands. But
   * any element can be given a root at any time, and two common shapes fall
   * outside that watch — a plain <div> a widget takes over, and a component
   * already defined when it was serialised that attaches its root a tick later.
   * `attachShadow` reaches no observer, so what the mirror keeps is the light
   * DOM rendered flat: the Reddit failure of §19, through a door §19 does not
   * cover.
   *
   * The test is exact rather than a guess: an element wearing a shadow root the
   * agent has no id for is a root that was never mirrored.
   */
  function sweepShadowRoots() {
    var roots = [];
    observedDocs.forEach(function (r) { roots.push(r); });
    var found = false;
    for (var r = 0; r < roots.length; r++) {
      var els;
      try { els = roots[r].querySelectorAll('*'); } catch (e) { continue; }
      for (var i = 0; i < els.length; i++) {
        var el = els[i];
        var sr = el.shadowRoot;
        if (!sr || idOf.get(sr) !== undefined) continue;
        // Not mirrored itself: whatever it is, it is not the client's yet, and
        // re-reading it here would have nowhere to put the rows.
        if (idOf.get(el) === undefined) continue;
        // Once each, whatever comes of it. An element has one shadow root for
        // life, so a re-read that did not register it — the serialiser stops
        // at a canvas and at the head, and an element whose parent is not
        // mirrored has nowhere to go — will not register it next time either,
        // and a sweep that kept trying would re-send the subtree for ever.
        if (shadowSwept.has(el)) continue;
        shadowSwept.add(el);
        reserialize(el);
        found = true;
      }
    }
    return found;
  }

  // readTarget is whichever element the document's own URL points at, by the
  // browser's own reckoning rather than by parsing the fragment: an id, a named
  // anchor, `#top`, and percent-encoding, all settled by the one that knows.
  function readTarget() {
    try { return document.querySelector(':target'); } catch (e) { return null; }
  }

  /*
   * syncTarget moves the mark when the landside URL starts pointing somewhere
   * else.
   *
   * A fragment changes for two reasons and neither reaches a mutation observer:
   * the reader follows an in-page link, and the page pushes a new address as
   * they scroll. `hashchange` catches the first promptly; the sweep catches
   * both, which is what covers a `pushState` that a `hashchange` never fires
   * for.
   *
   * The mark moves even where the elements are not mirrored, so that a target
   * that arrives later is marked by the walk that serialises it rather than
   * left behind by a sync that thought it had already done the work.
   */
  function syncTarget() {
    var now = readTarget();
    if (now === markedTarget) return false;
    var changed = false;
    var was = markedTarget ? idOf.get(markedTarget) : undefined;
    markedTarget = now;
    if (was !== undefined) {
      pendingOps.push([3, was, intern(TARGET_ATTR), -1]);
      changed = true;
    }
    var id = now ? idOf.get(now) : undefined;
    if (id !== undefined) {
      pendingOps.push([3, id, intern(TARGET_ATTR), intern('')]);
      changed = true;
    }
    return changed;
  }

  // checkUpgrades re-reads every watched element that has since upgraded, and
  // reports whether anything came of it.
  function checkUpgrades() {
    if (!awaitingUpgrade.size) return false;
    var ids = [];
    awaitingUpgrade.forEach(function (id) { ids.push(id); });
    var changed = false;
    for (var i = 0; i < ids.length; i++) {
      var id = ids[i];
      var el = byId.get(id);
      if (!el || !el.isConnected) {
        awaitingUpgrade.delete(id);
        continue;
      }
      if (!isDefined(el)) continue;
      awaitingUpgrade.delete(id);
      if (el.shadowRoot) {
        reserialize(el);
      } else {
        // Upgraded into its own light DOM, or behind a closed shadow root
        // nothing can read. The markup stands; only the placeholder styling
        // has to stop applying to it.
        pendingOps.push([3, id, intern(UNDEFINED_ATTR), -1]);
      }
      changed = true;
    }
    return changed;
  }

  // reserialize replaces one mirrored element with a fresh reading of it. The
  // element gets a new id, which is what makes this safe: the client drops the
  // old subtree whole rather than trying to reconcile a document it was never
  // sent the middle of.
  function reserialize(el) {
    var id = idOf.get(el);
    var pid = knownParentId(el);
    if (id === undefined || !pid) return;
    var beforeId = 0;
    var sib = el.nextSibling;
    while (sib && idOf.get(sib) === undefined) sib = sib.nextSibling;
    if (sib) beforeId = idOf.get(sib);
    pendingOps.push([2, id]);
    forget(el);
    var rows = [];
    serializeNode(el, pid, rows);
    if (rows.length) pendingOps.push([1, pid, beforeId, rows]);
  }

  function serializeSubtree(node) {
    var rows = [];
    var parent = node.parentNode ? (idOf.get(node.parentNode) || 0) : 0;
    serializeNode(node, parent, rows);
    return rows;
  }

  // ------------------------------------------------------------------ used CSS

  /*
   * What has to come out of a selector before the document can be asked about
   * it, in two kinds.
   *
   * A pseudo-*element* is not an element: `querySelector` parses one and then
   * matches nothing, for every selector that has one, for ever. So the question
   * that decides the rule is whether the element it hangs off is on the page,
   * and the pseudo-element itself has to go — whichever one it is. Naming them
   * instead of recognising the `::` was the older way and it aged badly: the
   * platform kept adding them, and each new one arrived as a rule that matched
   * nothing landside and was dropped. `::view-transition-old(root)`,
   * `input::file-selector-button` and `p::spelling-error` were all being thrown
   * away for saying no to a question they cannot answer yes to.
   *
   * A pseudo-*class* is a live state, and only the ones whose answer differs on
   * the two sides come out. Plane-side the reader has their own pointer, their
   * own focus and their own text in the fields, so `:hover`, `:focus` and
   * `:placeholder-shown` are all questions the landside document answers for
   * itself and not for the reader. The rest stay in and do their job of
   * rejecting rules nothing wants.
   *
   * Two guards keep the names straight, and both were paid for.
   *
   * `(?![-\w])` keeps a name from matching the front of a longer one. Without
   * it `:placeholder-shown` matched `:placeholder` and left `-shown` behind, so
   * `input:placeholder-shown` went to the document as `input-shown` — an
   * element type nothing is — and a float-label form lost every rule that
   * positions its labels.
   *
   * `(?<!\\)` keeps a colon that is part of a name from being read as the colon
   * that introduces one. A class may be called `field:hover`, and is written
   * `.field\:hover`; stripping the tail off that leaves `.field\`, which is not
   * a selector at all and which the presence index reads as a class named
   * `field` that nothing on the page carries. See readIdent, which resolves the
   * same escapes for the same reason.
   */
  var PSEUDO_ELEMENT_RE = /(?<!\\)::[-\w]+(?:\([^)]*\))?/g;
  var PSEUDO_CLASS_RE = /(?<!\\):(?:hover|active|focus-visible|focus-within|focus|visited|link|target|checked|disabled|enabled|placeholder-shown|placeholder|autofill|before|after|first-line|first-letter|selection|marker|backdrop|-webkit-[-\w]+|-moz-[-\w]+)(?![-\w])(?:\([^)]*\))?/gi;

  // scanSelString returns the index just past the string literal opening at i.
  function scanSelString(str, i) {
    var quote = str.charAt(i);
    for (var j = i + 1; j < str.length; j++) {
      if (str.charAt(j) === '\\') { j++; continue; }
      if (str.charAt(j) === quote) return j + 1;
    }
    return str.length; // unterminated
  }

  /*
   * splitSelectorList splits on the commas that separate selectors, and not on
   * the ones inside `:is(a, b)`, `:not([x], [y])` or a quoted attribute value.
   * Splitting on all of them builds fragments that are not selectors, and a
   * fragment that fails to parse takes its whole rule down the throwing path
   * into "keep" — which is safe, but keeps rules nothing on the page wants.
   */
  function splitSelectorList(sel) {
    var parts = [], depth = 0, start = 0;
    for (var i = 0; i < sel.length; i++) {
      var c = sel.charAt(i);
      if (c === '\\') { i++; continue; }
      if (c === '"' || c === "'") { i = scanSelString(sel, i) - 1; continue; }
      if (c === '(' || c === '[') depth++;
      else if (c === ')' || c === ']') { if (depth > 0) depth--; }
      else if (c === ',' && depth === 0) { parts.push(sel.slice(start, i)); start = i + 1; }
    }
    parts.push(sel.slice(start));
    return parts;
  }

  /*
   * testableSelector turns a selector into something querySelector can answer,
   * or null for "no honest test exists — keep the rule".
   *
   * `::part()` is reduced to the element whose parts are being styled. The part
   * itself is inside that element's shadow root landside, out of reach of the
   * document doing the asking, but whether the element is on the page at all is
   * the question that decides the rule — and a site ships parts for drawers and
   * modals it is not currently showing.
   */
  function testableSelector(sel) {
    var parts = [];
    var list = splitSelectorList(sel);
    for (var i = 0; i < list.length; i++) {
      // `:host` and `::part()` cannot be looked up: the first names the
      // element on the other side of the root, the second a pseudo-element, and
      // querySelector matches neither. They are kept rather than tested, which
      // is a handful of rules per component and the only honest answer.
      var p = list[i];
      if (p.indexOf(':host') >= 0 || p.indexOf('::part(') >= 0
          || p.indexOf('::slotted(') >= 0) {
        return null;
      }
      var part = p.replace(PSEUDO_ELEMENT_RE, '').replace(PSEUDO_CLASS_RE, '').trim();
      // A selector that was nothing but a pseudo (":root::before",
      // "::view-transition-old(root)") degrades to an empty part; keep such
      // rules rather than risk dropping layout.
      if (!part) return null;
      parts.push(part);
    }
    if (!parts.length) return null;
    return parts.join(',');
  }

  /*
   * The presence index: which tag names, class names and ids actually occur
   * under one root.
   *
   * Asking `querySelector` per rule is what this replaces, and the reason is
   * that a *failing* selector is the expensive one. A selector that matches
   * can stop at the first hit; one that matches nothing has to visit every
   * element under the root to prove it. A used-CSS pass is mostly failures by
   * design — that is the whole point of the filter — so the pass costs
   * O(rules x nodes). On a 12,000-rule utility bundle over 9,000 elements that
   * measured 1.24 s, and a pass is scheduled after every batch of DOM records:
   * appending one <div> per second held the renderer's main thread at 91%
   * busy, in 1.5 s blocks. Everything the mirror does — serialising mutations,
   * answering the host, laying the page out — waits behind those blocks, and
   * the tab the reader is waiting on is the one paying for them.
   *
   * One walk of the root builds the sets below; a rule is then rejected on a
   * set lookup instead of a document scan. The same 12,000 rules cost 46 ms
   * that way — a walk of 21 ms and 13 ms of lookups — and reach the identical
   * verdict on every rule, because the index only ever *rejects*: a selector
   * whose rightmost compound needs a class nothing on the page carries cannot
   * match, and anything the index cannot answer for still goes to
   * querySelector.
   */
  var presenceCache = new Map(); // root -> {tags, classes, ids}, one pass long

  function presenceFor(root) {
    var idx = presenceCache.get(root);
    if (idx !== undefined) return idx;
    idx = null;
    try {
      // getElementsByTagName is a live collection and cheaper to build than a
      // static NodeList; a shadow root has only querySelectorAll.
      var all = root.getElementsByTagName
        ? root.getElementsByTagName('*') : root.querySelectorAll('*');
      var tags = new Set(), classes = new Set(), ids = new Set();
      for (var i = 0; i < all.length; i++) {
        var el = all[i];
        tags.add(el.tagName.toLowerCase());
        if (el.id) ids.add(el.id);
        var cl = el.classList;
        if (cl) for (var c = 0; c < cl.length; c++) classes.add(cl[c]);
      }
      idx = { tags: tags, classes: classes, ids: ids };
    } catch (e) {
      idx = null; // detached, or a root that answers neither call: ask the DOM
    }
    presenceCache.set(root, idx);
    return idx;
  }

  // readIdent reads the identifier at i, resolving the `\` escapes that let a
  // class name hold punctuation — Tailwind's `.md\:flex` is the class named
  // `md:flex`, and that is the name classList reports.
  function readIdent(s, i) {
    var out = '';
    while (i < s.length) {
      var c = s.charAt(i);
      if (c === '\\') {
        if (i + 1 < s.length) { out += s.charAt(i + 1); i += 2; continue; }
        break;
      }
      if (c === '-' || c === '_' || c >= '\u0080' ||
          (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
        out += c; i++; continue;
      }
      break;
    }
    return { name: out, next: i };
  }

  // skipBalanced returns the index just past the bracket or paren opening at i.
  function skipBalanced(s, i) {
    var open = s.charAt(i), close = open === '(' ? ')' : ']';
    var depth = 0;
    for (var j = i; j < s.length; j++) {
      var c = s.charAt(j);
      if (c === '\\') { j++; continue; }
      if (c === '"' || c === "'") { j = scanSelString(s, j) - 1; continue; }
      if (c === open) depth++;
      else if (c === close) { depth--; if (depth === 0) return j + 1; }
    }
    return s.length;
  }

  // rightmostCompound returns the last compound of a complex selector — the
  // part that names the element the rule actually styles, and so the only part
  // whose absence proves the rule matches nothing.
  function rightmostCompound(part) {
    var start = 0;
    for (var i = 0; i < part.length; i++) {
      var c = part.charAt(i);
      if (c === '\\') { i++; continue; }
      if (c === '"' || c === "'") { i = scanSelString(part, i) - 1; continue; }
      if (c === '[' || c === '(') { i = skipBalanced(part, i) - 1; continue; }
      if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '\f' ||
          c === '>' || c === '+' || c === '~') {
        start = i + 1;
      }
    }
    return part.slice(start);
  }

  /*
   * compoundCanMatch reports whether an element answering this compound could
   * exist under the indexed root.
   *
   * A compound is a conjunction — `a.b#c` needs all three at once — so one
   * missing name is enough to reject it. Anything the index does not describe
   * (an attribute selector, a pseudo-class, `*`) leaves the compound unproven,
   * and unproven means keep: this function may only ever be sure of a no.
   */
  function compoundCanMatch(compound, idx) {
    var i = 0;
    while (i < compound.length) {
      var c = compound.charAt(i);
      if (c === '[' || c === '(') { i = skipBalanced(compound, i); continue; }
      if (c === ':') {
        // A pseudo-class, with its argument list if it has one. Its contents
        // are somebody else's element, not this compound's.
        i++;
        if (compound.charAt(i) === ':') i++;
        i = readIdent(compound, i).next;
        if (compound.charAt(i) === '(') i = skipBalanced(compound, i);
        continue;
      }
      if (c === '.' || c === '#') {
        var got = readIdent(compound, i + 1);
        if (!got.name) { i++; continue; }
        var set = c === '.' ? idx.classes : idx.ids;
        if (!set.has(got.name)) return false;
        i = got.next;
        continue;
      }
      if (c === '*') { i++; continue; }
      // A namespace prefix: `svg|rect` names rect in the SVG namespace, and the
      // index holds local names with no namespace to check them against. Rare
      // enough not to be worth modelling and cheap to decline.
      if (c === '|') return true;
      var tag = readIdent(compound, i);
      if (!tag.name) { i++; continue; }
      // A type selector, which is only a type selector at the front; anywhere
      // else the scan above has already consumed what it belongs to.
      if (!idx.tags.has(tag.name.toLowerCase())) return false;
      i = tag.next;
    }
    return true; // nothing the index describes ruled this compound out
  }

  // couldMatch reports whether any selector in the list has a chance. A list is
  // a disjunction, so one plausible member keeps the whole rule.
  function couldMatch(root, testable) {
    var idx = presenceFor(root);
    if (!idx) return true;
    var list = splitSelectorList(testable);
    for (var i = 0; i < list.length; i++) {
      var compound = rightmostCompound(list[i].trim());
      if (!compound) return true;
      if (compoundCanMatch(compound, idx)) return true;
    }
    return false;
  }

  function selectorMatches(doc, sel) {
    var test = testableSelector(sel);
    if (test === null) return true;
    // The index can only prove a no, and proving it here saves the document
    // scan that proving it by query would cost. See presenceFor.
    if (!couldMatch(doc, test)) return false;
    try { return doc.querySelector(test) !== null; } catch (e) { return true; }
  }

  /*
   * noteRejected keeps what the filter threw away.
   *
   * A bundle carries the rules that passed this test and no trace of the ones
   * that did not, which makes a filter bug indistinguishable from a rule the
   * site never wrote — the difference between "Skyhook dropped the rule that
   * hides this menu" and "the site has no such rule", from the outside, is
   * nothing at all. Selectors are cheap and the page's own structure is
   * already in the bundle, so the ones that matched nothing are kept: capped,
   * because a utility-class bundle rejects tens of thousands of them.
   */
  function noteRejected(sel) {
    cssRejected++;
    if (cssRejectedList.length < CSS_REJECTED_MAX && typeof sel === 'string') {
      cssRejectedList.push(sel);
    }
  }

  // ------------------------------------------------------------ media queries

  /*
   * A media query asks about the page's box, about the reader, or about the
   * palette the page was painted in, and the three do not cross the link alike.
   *
   * The box is the same question on both sides by construction. The client
   * reports its window and `Tab.SetViewport` puts the landside tab in exactly
   * that box, device pixel ratio included, so a rule that applies at 1628px
   * landside applies at 1628px plane-side. `width`, `height`, `orientation`
   * and `resolution` therefore stay live, and have to: the reader can turn a
   * phone sideways, and the mirror should reflow at the turn rather than at the
   * next round trip.
   *
   * The reader stays live too, and deliberately. `prefers-reduced-motion`,
   * `prefers-contrast`, `forced-colors`, `hover`, `pointer` — every one of them
   * is a fact about a person, and the person is plane-side. This browser is
   * headless with nobody at it: it answers `no-preference` and `pointer: none`
   * because nothing was ever asked of it, and freezing that would hand a reader
   * who did ask a page that ignores them. It is the same answer `:hover` and
   * `:focus` already get a few hundred lines up — the reader's own state is the
   * reader's.
   *
   * `prefers-color-scheme` is the one that cannot be either. It does not
   * describe the reader so much as decide what the page *is*, and by the time
   * the mirror is asked, the page has already been painted: the images were
   * fetched, chosen and transcoded from the landside render, the canvases were
   * rasterised there, the capture's landside screenshot is in that palette, and
   * the mirror's own chrome around the document is a flat `#fff`. The
   * stylesheet is the only part of that able to change its mind plane-side, and
   * a stylesheet that changes its mind alone does not produce the other theme.
   * It produces half of each.
   *
   * Which is exactly what a capture of GitHub showed. The document is
   * `data-color-mode="auto"`, so its whole palette hangs off
   * `@media (prefers-color-scheme: dark)`; this browser was light and the
   * reader's was dark. The file tree came out with the dark theme's controls
   * and the dark theme's near-white filenames, on the white the mirror paints
   * behind every page — folder names invisible against the background, over a
   * sidebar whose chrome was still light. Read as "the navigation on the left
   * is missing its CSS", which is how it was reported. Nothing in the capture
   * accused anything: the DOM agreed node for node, the filter reported no rule
   * dropped, and both halves were doing exactly as they were told.
   *
   * So that one question is answered here, once, by the browser that actually
   * painted the page, and what crosses is what is left of the query: one that
   * cannot match is not sent at all, one that always matches is sent without
   * its wrapper, and one that still turns on something plane-side keeps
   * precisely the part that does.
   */
  var MEDIA_ENV_FEATURE = { 'prefers-color-scheme': 1 };

  // What this browser says, one pass long — for the same reason presenceFor's
  // index is: the answer is a fact about now, and a bundle asks for it a
  // thousand times in a row. A landside browser whose reader changes the system
  // theme gets the new answer on the next pass.
  var mediaAnswers = null;
  var mediaResolved = null;

  /*
   * mediaAnswer asks this browser one feature query, or reports null where
   * there is nothing to ask: a document with no `matchMedia` is one whose
   * answers stay where they were written.
   */
  function mediaAnswer(query) {
    if (!mediaAnswers) mediaAnswers = new Map();
    var got = mediaAnswers.get(query);
    if (got !== undefined) return got;
    var v = null;
    try {
      var mq = globalThis.matchMedia(query);
      v = mq ? !!mq.matches : null;
    } catch (e) { v = null; }
    mediaAnswers.set(query, v);
    return v;
  }

  /*
   * mediaEnvFeature reports whether a `(...)` names one of the features above.
   *
   * The name is whichever identifier in it names a feature this knows, rather
   * than the first identifier in it, and that is the reading that cannot go
   * wrong in the direction that matters: an unrecognised feature costs a
   * wrapper, a misread one costs the rules under it. A feature's name comes
   * first in every form the syntax has — `(hover)`, `(prefers-color-scheme:
   * dark)`, `(width > 40em)` — so a *value* that happens to spell a feature
   * name is only ever reached after the name itself has been.
   *
   * The prefixes come off because a feature may arrive wearing them:
   * `-webkit-min-device-pixel-ratio` is `device-pixel-ratio` under two.
   */
  function mediaEnvFeature(inner) {
    var ids = inner.match(/[-a-zA-Z][-\w]*/g);
    if (!ids) return '';
    for (var i = 0; i < ids.length; i++) {
      var name = ids[i].toLowerCase()
        .replace(/^-(?:webkit|moz|ms|o)-/, '')
        .replace(/^(?:min|max)-/, '');
      if (MEDIA_ENV_FEATURE[name]) return name;
    }
    return '';
  }

  function mediaSpace(c) {
    return c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '\f';
  }

  function mediaSkipSpace(s, i) {
    while (i < s.length && mediaSpace(s.charAt(i))) i++;
    return i;
  }

  // mediaKeyword returns the index just past `word` at i, or -1. The word has
  // to end where it says it does: `and` is an operator, `android` is not.
  function mediaKeyword(s, i, word) {
    if (s.substr(i, word.length).toLowerCase() !== word) return -1;
    var j = i + word.length;
    var c = s.charAt(j);
    if (c && (c === '-' || c === '_' || /[0-9a-zA-Z]/.test(c))) return -1;
    return j;
  }

  // A parsed condition: a settled answer, an opaque `(...)` this side must not
  // settle, or a combination of them.
  function mediaConst(v) { return { k: 'const', v: v }; }

  function mediaNot(a) {
    if (a.k === 'const') return mediaConst(!a.v);
    return { k: 'not', a: a };
  }

  // mediaCombine folds the constants out of an `and`/`or` chain: an `and` with
  // a false in it is false whatever else it says, and a true in it carries no
  // information at all.
  function mediaCombine(op, list) {
    var kept = [];
    for (var i = 0; i < list.length; i++) {
      var n = list[i];
      if (n.k !== 'const') { kept.push(n); continue; }
      if (n.v === (op === 'or')) return mediaConst(op === 'or');
    }
    if (!kept.length) return mediaConst(op === 'and');
    if (kept.length === 1) return kept[0];
    return { k: op, list: kept };
  }

  // mediaAtom reads what one pair of brackets holds: another condition, a
  // feature this browser can answer, or something to leave exactly as written.
  function mediaAtom(inner) {
    var t = inner.trim();
    if (t.charAt(0) === '(' || mediaKeyword(t, 0, 'not') >= 0) {
      var r = parseMediaCondition(t, 0);
      if (r && mediaSkipSpace(t, r.next) >= t.length) return r.node;
      return { k: 'opaque', t: '(' + inner + ')' };
    }
    if (!mediaEnvFeature(t)) return { k: 'opaque', t: '(' + inner + ')' };
    var v = mediaAnswer('(' + t + ')');
    if (v === null) return { k: 'opaque', t: '(' + inner + ')' };
    return mediaConst(v);
  }

  function parseMediaInParens(s, i) {
    i = mediaSkipSpace(s, i);
    if (s.charAt(i) !== '(') return null;
    var end = skipBalanced(s, i);
    if (end > s.length || s.charAt(end - 1) !== ')') return null;
    return { node: mediaAtom(s.slice(i + 1, end - 1)), next: end };
  }

  // parseMediaCondition reads `not (a)`, `(a)`, `(a) and (b)`, `(a) or (b)` and
  // anything nested inside those. A shape it cannot read returns null, and the
  // query it came from is then shipped as written.
  function parseMediaCondition(s, i) {
    i = mediaSkipSpace(s, i);
    var k = mediaKeyword(s, i, 'not');
    if (k >= 0) {
      var inner = parseMediaInParens(s, k);
      if (!inner) return null;
      return { node: mediaNot(inner.node), next: inner.next };
    }
    var first = parseMediaInParens(s, i);
    if (!first) return null;
    var list = [first.node], op = '', j = first.next;
    for (;;) {
      j = mediaSkipSpace(s, j);
      var a = mediaKeyword(s, j, 'and'), o = mediaKeyword(s, j, 'or');
      var kind = a >= 0 ? 'and' : (o >= 0 ? 'or' : '');
      if (!kind) break;
      // `(a) and (b) or (c)` has no meaning without brackets and is not a query
      // this may guess at.
      if (op && op !== kind) return null;
      op = kind;
      var more = parseMediaInParens(s, a >= 0 ? a : o);
      if (!more) return null;
      list.push(more.node);
      j = more.next;
    }
    return { node: list.length === 1 ? list[0] : mediaCombine(op, list), next: j };
  }

  // mediaText writes a folded condition back out. `wrap` is for a position that
  // needs one term: `and` and `not` bind loosely enough to need the brackets.
  function mediaText(node, wrap) {
    if (node.k === 'opaque') return node.t;
    if (node.k === 'not') {
      var n = 'not ' + mediaText(node.a, true);
      return wrap ? '(' + n + ')' : n;
    }
    var parts = [];
    for (var i = 0; i < node.list.length; i++) parts.push(mediaText(node.list[i], true));
    var s = parts.join(node.k === 'and' ? ' and ' : ' or ');
    return wrap ? '(' + s + ')' : s;
  }

  /*
   * resolveMediaQuery answers one query of a list and says what is left of it:
   * null for "cannot match here", '' for "applies always, so drop the wrapper",
   * or the text that still has a question in it.
   *
   * The `not` in front of a media type negates the whole query rather than the
   * condition — `not screen and (prefers-color-scheme: dark)` is
   * `not (screen and dark)` — which is why the two are folded separately.
   */
  function resolveMediaQuery(text) {
    var s = text.trim();
    if (!s) return null;
    var head = /^(?:(not|only)\s+)?([-a-zA-Z][-\w]*)(?![-\w(])/.exec(s);
    // A bare `not (...)` is a condition, and its `not` is not a type prefix.
    if (head && !head[1] && head[2].toLowerCase() === 'not') head = null;
    var type = '', prefix = '', neg = false, i = 0;
    if (head) {
      neg = !!head[1] && head[1].toLowerCase() === 'not';
      prefix = head[1] ? head[1] + ' ' : '';
      type = head[2];
      i = head[0].length;
    }
    var rest = mediaSkipSpace(s, i);
    if (rest >= s.length) return s; // a media type on its own: nothing to fold
    var at = rest;
    if (type) {
      at = mediaKeyword(s, rest, 'and');
      if (at < 0) return s;
    }
    var parsed = parseMediaCondition(s, at);
    if (!parsed || mediaSkipSpace(s, parsed.next) < s.length) return s;
    var cond = parsed.node;
    if (cond.k !== 'const') {
      var left = mediaText(cond, false);
      return type ? prefix + type + ' and ' + left : left;
    }
    if (neg) {
      // Negated, so a condition that cannot hold makes the query match instead.
      if (!cond.v) return '';
      return type.toLowerCase() === 'all' ? null : 'not ' + type;
    }
    if (!cond.v) return null;
    if (!type || type.toLowerCase() === 'all') return '';
    return prefix + type;
  }

  /*
   * resolveMediaInText answers the media queries inside a block that crosses as
   * text rather than as rules.
   *
   * Almost everything is walked: collectRules goes into a `@media` and asks
   * this browser about its condition. Two things are not, and cannot be — a
   * `@scope` body, whose rules are written against a root the document cannot
   * be asked about, and a grouping at-rule this build has no name for, which
   * has to be shipped whole because guessing at its prelude is worse than
   * keeping it. Both hand over `cssText`, and a `@media (prefers-color-scheme:
   * dark)` inside one reached the reader with its question intact — the whole
   * fault of §45, in the two places §45's fix could not see.
   *
   * So the text is scanned for them. It is a small parser and it is deliberately
   * a nervous one: a shape it cannot read is left exactly as written, which
   * costs a wrapper and never a rule. Strings and comments are stepped over for
   * the reason they always are — `content: "@media print"` is text a page means
   * to display.
   */
  function resolveMediaInText(text) {
    if (!text || text.toLowerCase().indexOf('@media') < 0) return text;
    var out = '', i = 0;
    while (i < text.length) {
      var c = text.charAt(i);
      if (c === '"' || c === "'") {
        var q = scanSelString(text, i);
        out += text.slice(i, q);
        i = q;
        continue;
      }
      if (c === '\\' && i + 1 < text.length) {
        out += text.slice(i, i + 2);
        i += 2;
        continue;
      }
      if (c === '/' && text.charAt(i + 1) === '*') {
        var end = text.indexOf('*/', i + 2);
        if (end < 0) { out += text.slice(i); break; }
        out += text.slice(i, end + 2);
        i = end + 2;
        continue;
      }
      if (c === '@' && matchesAtRule(text, i, 'media')) {
        var open = findBlockStart(text, i);
        var close = open < 0 ? -1 : matchBlockEnd(text, open);
        if (close < 0) { out += c; i++; continue; }
        var cond = text.slice(i + 6, open).trim();
        var left = resolveMediaList(cond);
        if (left !== null) {
          // Recurse first: a block may hold another, and the inner question is
          // this browser's whether or not the outer one survives.
          var body = resolveMediaInText(text.slice(open + 1, close));
          out += left ? '@media ' + left + '{' + body + '}' : body;
        } else {
          noteRejected('@media ' + cond);
        }
        i = close + 1;
        continue;
      }
      out += c;
      i++;
    }
    return out;
  }

  // matchesAtRule reports whether `@name` stands at i and stops there: `@media`
  // is one at-rule and `@media-something` would be another.
  function matchesAtRule(text, i, name) {
    if (text.charAt(i) !== '@') return false;
    if (text.substr(i + 1, name.length).toLowerCase() !== name) return false;
    var c = text.charAt(i + 1 + name.length);
    return c !== '' && c !== '-' && c !== '_' && !/[0-9a-zA-Z]/.test(c);
  }

  // findBlockStart returns the index of the `{` that opens the block an at-rule
  // at i introduces, or -1 for one that ends at a semicolon and has no block.
  function findBlockStart(text, i) {
    for (var j = i; j < text.length; j++) {
      var c = text.charAt(j);
      if (c === '\\') { j++; continue; }
      if (c === '"' || c === "'") { j = scanSelString(text, j) - 1; continue; }
      if (c === '{') return j;
      if (c === ';') return -1;
    }
    return -1;
  }

  // matchBlockEnd returns the index of the `}` closing the block opened at i.
  function matchBlockEnd(text, i) {
    var depth = 0;
    for (var j = i; j < text.length; j++) {
      var c = text.charAt(j);
      if (c === '\\') { j++; continue; }
      if (c === '"' || c === "'") { j = scanSelString(text, j) - 1; continue; }
      if (c === '/' && text.charAt(j + 1) === '*') {
        var end = text.indexOf('*/', j + 2);
        if (end < 0) return -1;
        j = end + 1;
        continue;
      }
      if (c === '{') depth++;
      else if (c === '}' && --depth === 0) return j;
    }
    return -1;
  }

  /*
   * ------------------------------------------------------------ color-scheme
   *
   * `color-scheme` is the other half of the same disagreement, and the half a
   * media query cannot reach.
   *
   * A page that writes `color-scheme: light dark` is not choosing; it is saying
   * it can be either and letting the browser choose, and what the browser then
   * paints in the chosen scheme is everything the page does not paint itself:
   * form controls, scrollbars, the canvas behind the document, the default
   * text colour. `light-dark()` resolves against the same choice. So a mirror
   * that ships the declaration as written hands the choice to the reader's
   * browser — and gets a light page with dark checkboxes, dark dropdowns and a
   * dark scrollbar, or the reverse. Nothing in the stylesheet is wrong; the
   * page simply never chose, and the two browsers chose differently.
   *
   * A value that names one scheme is already an answer and is left alone.
   * A value that names both is collapsed to the one this browser picked, which
   * is the same answer `@media (prefers-color-scheme)` now gets, arrived at the
   * same way and for the same reason.
   */
  function pinnedSchemeValue(value) {
    var words = value.trim().split(/\s+/);
    var light = false, dark = false, only = false;
    for (var i = 0; i < words.length; i++) {
      switch (words[i].toLowerCase()) {
        case 'light': light = true; break;
        case 'dark': dark = true; break;
        case 'only': only = true; break;
        case '': break;
        default:
          // `normal`, or a scheme named for a browser that may not be this one.
          // Either way there is no ambiguity here this can honestly settle.
          return '';
      }
    }
    if (!light || !dark) return ''; // already an answer, or says nothing
    var wantDark = mediaAnswer('(prefers-color-scheme: dark)');
    if (wantDark === null) return '';
    return (only ? 'only ' : '') + (wantDark ? 'dark' : 'light');
  }

  /*
   * pinColorSchemes rewrites every `color-scheme` declaration in a piece of CSS
   * — a rule, a block of them, an inline style attribute.
   *
   * Scanned rather than matched, for the reason replaceCSSURLs is: `content:
   * "color-scheme: light dark"` is text a page means to display, and a pattern
   * cannot tell it from a declaration. A declaration starts a run — at the
   * beginning, after a `{`, or after a `;` — and that is what is looked for.
   */
  function pinColorSchemes(text) {
    if (text.indexOf('color-scheme') < 0) return text;
    var out = '', i = 0, start = true;
    while (i < text.length) {
      var c = text.charAt(i);
      if (c === '"' || c === "'") {
        var j = scanSelString(text, i);
        out += text.slice(i, j);
        i = j;
        continue;
      }
      if (c === '\\' && i + 1 < text.length) {
        out += text.slice(i, i + 2);
        i += 2;
        continue;
      }
      if (start && matchesProperty(text, i, 'color-scheme')) {
        var colon = text.indexOf(':', i);
        var end = i;
        while (end < text.length && text.charAt(end) !== ';' && text.charAt(end) !== '}') end++;
        var pinned = colon >= 0 && colon < end
          ? pinnedSchemeValue(text.slice(colon + 1, end)) : '';
        if (pinned) {
          out += 'color-scheme:' + pinned;
          i = end;
          continue;
        }
      }
      // A run ends where the next one begins. Whitespace between them is not
      // the start of anything, so the flag survives it.
      if (c === '{' || c === ';' || c === '}') start = true;
      else if (!mediaSpace(c)) start = false;
      out += c;
      i++;
    }
    return out;
  }

  // matchesProperty reports whether `name` stands at i as a property, which
  // means the next thing that is not whitespace is the colon of a declaration.
  function matchesProperty(text, i, name) {
    if (text.substr(i, name.length).toLowerCase() !== name) return false;
    var j = mediaSkipSpace(text, i + name.length);
    return text.charAt(j) === ':';
  }

  /*
   * resolveMediaList does the same for a whole comma-separated list. A list is
   * a disjunction: one query that always matches makes the wrapper pointless,
   * and a list with nothing left in it is a block that cannot apply here.
   */
  function resolveMediaList(text) {
    var cond = (text || '').trim();
    if (!cond || cond.toLowerCase() === 'all') return '';
    if (!mediaResolved) mediaResolved = new Map();
    var got = mediaResolved.get(cond);
    if (got !== undefined) return got;
    // Media queries are separated by the same commas selectors are, under the
    // same rules about brackets and strings.
    var queries = splitSelectorList(cond);
    var out = [], all = false;
    for (var i = 0; i < queries.length; i++) {
      var q = resolveMediaQuery(queries[i]);
      if (q === null) continue;
      if (q === '') { all = true; break; }
      out.push(q);
    }
    var res = all ? '' : (out.length ? out.join(',') : null);
    mediaResolved.set(cond, res);
    return res;
  }

  /*
   * Every url() a rule carries, resolved against the sheet that wrote it.
   *
   * `cssText` hands back the reference as authored — `url(../images/logo.svg)` —
   * and carries no trace of which sheet authored it. Downstream there is nothing
   * left to resolve that against but the document, and for any sheet not sitting
   * beside the page that is the wrong base. A WordPress theme keeps its
   * stylesheet at /blog/wp-content/themes/fem-v3/dist/style.css and its logo one
   * directory above that; resolved against the article's own address instead,
   * `../images/logo-dark.svg` asked for /blog/images/logo-dark.svg, which is the
   * site's 404 page. HTML decodes as no image, so the bytes were never shipped,
   * and the mirror drew the masthead as an empty 320px box — the styling was all
   * there, and the picture it named was somebody else's.
   *
   * Doing it here is what makes it possible at all: this is the last place the
   * sheet and its rules are in hand at the same time. It also settles the two
   * quieter cases with the same answer — a `<base href>`, and a stylesheet
   * inside an inlined same-origin frame, both of which resolve against something
   * that is not the top-level document's URL.
   *
   * The scanning is deliberately the server's, in another language: see
   * replaceCSSURLs in internal/mirror/css.go. A `url(` inside a string is text
   * the page means to display, and a quoted token may hold the `)` that a
   * pattern would stop at — one of those cost a mirrored Gmail four fifths of
   * its stylesheet.
   */
  var URL_CALL_RE = /url\(/i;

  function isCSSSpaceChar(c) {
    return c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '\f';
  }

  function isURLIdentChar(c) {
    return c === '-' || c === '_' ||
      (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z');
  }

  function isHexChar(c) {
    return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
  }

  // unescapeCSSURL resolves the `\x` escapes that let a URL carry a quote or a
  // bracket. A hex escape is left as written: reading one means knowing where
  // the digits stop, and no address in any capture has ever used one.
  function unescapeCSSURL(s) {
    if (s.indexOf('\\') < 0) return s;
    var out = '';
    for (var i = 0; i < s.length; i++) {
      if (s.charAt(i) !== '\\' || i + 1 >= s.length || isHexChar(s.charAt(i + 1))) {
        out += s.charAt(i);
        continue;
      }
      i++;
      out += s.charAt(i);
    }
    return out;
  }

  // scanURLToken reads the url token starting at i, which must be at its
  // `url(`, and returns the address it names with the index just past its `)`.
  // Null means the token does not end, which is left alone rather than guessed
  // at.
  function scanURLToken(s, i) {
    var j = i + 4; // past `url(`
    while (j < s.length && isCSSSpaceChar(s.charAt(j))) j++;
    if (j >= s.length) return null;
    var q = s.charAt(j);
    if (q === '"' || q === "'") {
      var k = scanSelString(s, j);
      if (k - 1 <= j || s.charAt(k - 1) !== q) return null; // unterminated
      var quoted = unescapeCSSURL(s.slice(j + 1, k - 1));
      while (k < s.length && isCSSSpaceChar(s.charAt(k))) k++;
      if (k >= s.length || s.charAt(k) !== ')') return null;
      return { raw: quoted, end: k + 1 };
    }
    // Unquoted, so it runs to the first `)` that is not escaped. A token of
    // this kind may hold neither whitespace nor brackets unescaped, which is
    // what makes reading it this way safe.
    var start = j;
    while (j < s.length) {
      if (s.charAt(j) === '\\' && j + 1 < s.length) { j += 2; continue; }
      if (s.charAt(j) === ')') break;
      j++;
    }
    if (j >= s.length) return null;
    return {
      raw: unescapeCSSURL(s.slice(start, j).replace(/[ \t\n\r\f]+$/, '')),
      end: j + 1
    };
  }

  // cssURLToken writes an address back out as a url token. The address decides
  // the quoting, not the form the site happened to use: one holding a bracket,
  // a quote or a space is only a url token at all inside quotes.
  function cssURLToken(raw) {
    if (!/["'()\\ \t\n\r\f]/.test(raw)) return 'url(' + raw + ')';
    return 'url("' + raw.replace(/["\\]/g, '\\$&') + '")';
  }

  // resolveCSSURL returns the absolute form of one reference, or '' for one to
  // leave exactly as it stands. A fragment names an SVG filter, clip path or
  // gradient in the document rather than a file, and giving it a path is how it
  // stops resolving; a data: URL is already the bytes.
  // `always` asks for the address even when the reference was already absolute.
  // Rewriting it to itself is pointless when all that is wanted is an absolute
  // form; it is the whole answer when what is wanted is something to key on.
  function resolveCSSURL(ref, base, always) {
    // Trimmed first, and empty means leave it: `new URL('  ', base)` is the
    // sheet's own address, so a blank reference would be rewritten into a
    // request for the stylesheet as an image.
    ref = ref.replace(/^[ \t\n\r\f]+|[ \t\n\r\f]+$/g, '');
    if (!ref || ref.charAt(0) === '#') return '';
    var head = ref.slice(0, 8).toLowerCase();
    if (head.indexOf('data:') === 0 || head.indexOf('skyhook:') === 0) return '';
    if (head.indexOf('blob:') === 0) return ''; // names an object in a realm we are not
    try {
      var abs = new URL(ref, base).href;
      return abs === ref && !always ? '' : abs;
    } catch (e) {
      return '';
    }
  }

  // mapCSSURLs hands every url() token in text to fn and puts back what it
  // returns — the whole token, `url(` and `)` included — or leaves the token
  // alone for an empty string. The scanning is the only subtle part, so it
  // lives here once and the callers differ only in what they do with an
  // address.
  function mapCSSURLs(text, fn) {
    if (typeof text !== 'string' || !URL_CALL_RE.test(text)) return text;
    var out = '';
    for (var i = 0; i < text.length;) {
      var c = text.charAt(i);
      if (c === '\\' && i + 1 < text.length) {
        out += text.slice(i, i + 2);
        i += 2;
        continue;
      }
      if (c === '"' || c === "'") {
        var j = scanSelString(text, i);
        out += text.slice(i, j);
        i = j;
        continue;
      }
      // `url(` is only a url token where an identifier does not run into it:
      // `-webkit-url(` would be some other function.
      if (text.substr(i, 4).toLowerCase() !== 'url(' ||
          (i > 0 && isURLIdentChar(text.charAt(i - 1)))) {
        out += c;
        i++;
        continue;
      }
      var tok = scanURLToken(text, i);
      if (!tok) {
        out += c;
        i++;
        continue;
      }
      var rep = fn(tok.raw);
      out += rep || text.slice(i, tok.end);
      i = tok.end;
    }
    return out;
  }

  function absolutizeCSSURLs(text, base) {
    if (!base) return text;
    return mapCSSURLs(text, function (raw) {
      var abs = resolveCSSURL(raw, base);
      return abs ? cssURLToken(abs) : '';
    });
  }

  /*
   * The pictures an inline `style` attribute names.
   *
   * Everything a stylesheet names is fetched, transcoded and shipped as a cache
   * key; a `style="background-image:url(hero.jpg)"` was shipped as written. The
   * client is a sandboxed frame with no network and a base address that is not
   * the page's, so the reference resolved to nothing and the element rendered
   * as an empty box — the same failure as a mis-based stylesheet url(), on the
   * construct sites reach for most: heroes, avatars, cards, and every
   * background a script assigns as it lazy-loads.
   *
   * Rewritten here rather than landside because this is where the intern table
   * is built. The value is one string among thousands the table shares, and the
   * table is addressed by position: editing an entry in place edits it for
   * every other use, and adding one means renumbering. Interning the rewritten
   * text costs neither.
   *
   * The size asked for is the element's own laid-out box, which is a better
   * answer than the blanket cap a stylesheet's images get — a background on a
   * 48px avatar is transcoded to 48px rather than to 512.
   */
  function styleAttrImages(text, base, el) {
    if (!URL_CALL_RE.test(text)) return null;
    var found = [];
    var box = null;
    try { box = el.getBoundingClientRect(); } catch (e) { box = null; }
    var w = box ? Math.round(box.width) : 0;
    var h = box ? Math.round(box.height) : 0;
    if (w > 4096) w = 4096;
    if (h > 4096) h = 4096;
    var rewritten = mapCSSURLs(text, function (raw) {
      var abs = resolveCSSURL(raw, base, true);
      if (!abs) return '';
      var key = imageKey(abs, w, h);
      found.push({
        n: idFor(el), url: abs, w: w, h: h, key: key, alt: '',
        // A background is worth the same urgency as the box it fills: the
        // element is on screen or it is not, and the rectangle says which.
        pri: box && box.top < (globalThis.innerHeight || 900) * 1.5 &&
          box.bottom > -200 ? 0 : 1
      });
      return 'url(skyhook://img/' + key + ')';
    });
    if (!found.length) return null;
    return { text: rewritten, images: found };
  }

  // sheetBase is the address a sheet's references resolve against: its own, or
  // the document's for a sheet that has none — an inline <style>, or the
  // constructed sheet a web component ships.
  function sheetBase(doc, sheet) {
    var href = null;
    try { href = sheet.href; } catch (e) { href = null; } // cross-origin
    if (href) return href;
    try { return doc.baseURI || null; } catch (e) { return null; }
  }

  /*
   * cssText is lossy, and the loss ships broken styles (P-126).
   *
   * When a shorthand set with var() is partially overridden by a later
   * declaration — Vector 2022's thumbnails write `border: 1px solid
   * var(--border-color-subtle,#c8ccd1); border-bottom: 0` — Chromium's
   * serialiser cannot reconstitute the shorthand and emits its longhands
   * with *empty values*: `border-top-color: ;`. The CSSOM keeps no way
   * back — getPropertyValue answers "" for shorthand and longhands alike,
   * the typed OM serialises the same nothing — so the only true copy is the
   * sheet's authored text, and the repair is to find this rule in it.
   *
   * Detection is the signature itself: an empty declaration value, which no
   * parser produces from real CSS (an empty *custom property* value is
   * legal and deliberately excluded). Recovery re-reads the rule from the
   * sheet source — a <style>'s text synchronously; a <link>'s by an in-page
   * fetch that reschedules the pass when it lands — locating it by
   * normalised selector and ordinal, exactly the walk the CSSOM makes. A
   * repair that cannot be validated ships the broken text it started with.
   */
  function hasLostShorthand(text) {
    var i = 0, n = text.length;
    while (i < n) {
      var c = text.charAt(i);
      if (c === '"' || c === "'") { i = scanSelString(text, i); continue; }
      if (c === '\\') { i += 2; continue; }
      if (c !== ':') { i++; continue; }
      var p = i - 1;
      while (p >= 0 && ' \t\n\r\f'.indexOf(text.charAt(p)) >= 0) p--;
      var e = p;
      while (p >= 0 && /[-A-Za-z0-9_]/.test(text.charAt(p))) p--;
      var prop = text.slice(p + 1, e + 1);
      if (prop && prop.slice(0, 2) !== '--') {
        var j = i + 1;
        while (j < n && ' \t\n\r\f'.indexOf(text.charAt(j)) >= 0) j++;
        var v = text.charAt(j);
        if (v === ';' || v === '}' || j >= n) return true;
      }
      i++;
    }
    return false;
  }

  function requestSheetSource(href) {
    if (sheetSources.has(href) || sheetSourceFetches.has(href)) return;
    sheetSourceFetches.set(href, 'pending');
    try {
      fetch(href, { credentials: 'include' }).then(function (r) {
        return r && r.ok ? r.text() : null;
      }).then(function (t) {
        if (t) { sheetSources.set(href, t); sheetSourceFetches.set(href, 'ok'); scheduleCSS(); }
        else { sheetSourceFetches.set(href, 'failed'); }
      }, function () { sheetSourceFetches.set(href, 'failed'); });
    } catch (e) { sheetSourceFetches.set(href, 'failed'); }
  }

  function sheetSourceFor(sheet) {
    if (constructedSourceTexts) {
      var t0 = constructedSourceTexts.get(sheet);
      if (t0) return t0;
    }
    var node = null;
    try { node = sheet.ownerNode; } catch (e) { node = null; }
    if (node && node.tagName === 'STYLE') return node.textContent || null;
    var href = null;
    try { href = sheet.href; } catch (e) { href = null; }
    if (!href) return null;
    var t = sheetSources.get(href);
    if (t) return t;
    requestSheetSource(href);
    return null;
  }

  // ruleOrdinal counts, in CSSOM order, style rules sharing this rule's
  // selector text, so the source scan can find the same one. Style-rule
  // children (CSS nesting) are not walked — collectRules ships them inside
  // their parent's text, and the source scanner skips them the same way.
  function ruleOrdinal(sheet, target, sel) {
    var n = -1, done = false;
    function walk(list) {
      for (var i = 0; i < list.length && !done; i++) {
        var r = list[i], ty = 0, kids = null;
        try { ty = r.type; } catch (e) { ty = 0; }
        if (ty === 1) {
          var s = null;
          try { s = r.selectorText; } catch (e) { s = null; }
          if (s === sel) {
            n++;
            if (r === target) { done = true; return; }
          }
          continue;
        }
        try { kids = r.cssRules; } catch (e) { kids = null; }
        if (kids) walk(kids);
      }
    }
    try { walk(sheet.cssRules); } catch (e) { return -1; }
    return done ? n : -1;
  }

  var scratchSelSheet = null;
  var selNormCache = new Map();
  function normalizeSelector(sel) {
    if (!sel) return null;
    var hit = selNormCache.get(sel);
    if (hit !== undefined) return hit;
    var out = null;
    try {
      if (!scratchSelSheet) scratchSelSheet = new CSSStyleSheet();
      scratchSelSheet.replaceSync(sel + '{}');
      var r = scratchSelSheet.cssRules[0];
      out = (r && r.selectorText) || null;
    } catch (e) { out = null; }
    if (selNormCache.size > 4096) selNormCache.clear();
    selNormCache.set(sel, out);
    return out;
  }

  function findRuleBlockInSource(src, wantSel, ordinal) {
    var found = null, count = -1;
    function pastComment(i) {
      var e = src.indexOf('*/', i + 2);
      return e < 0 ? src.length : e + 2;
    }
    function matchBrace(i) {
      var depth = 0;
      for (; i < src.length; i++) {
        var c = src.charAt(i);
        if (c === '"' || c === "'") { i = scanSelString(src, i) - 1; continue; }
        if (c === '\\') { i++; continue; }
        if (c === '/' && src.charAt(i + 1) === '*') { i = pastComment(i) - 1; continue; }
        if (c === '{') depth++;
        else if (c === '}') { depth--; if (depth === 0) return i; }
      }
      return -1;
    }
    function scan(i, end) {
      while (i < end && found === null) {
        while (i < end) {
          var w = src.charAt(i);
          if (w === ' ' || w === '\t' || w === '\n' || w === '\r' || w === '\f') { i++; continue; }
          if (w === '/' && src.charAt(i + 1) === '*') { i = pastComment(i); continue; }
          break;
        }
        if (i >= end) return;
        var start = i, brace = -1, term = -1;
        for (var j = i; j < end; j++) {
          var c = src.charAt(j);
          if (c === '"' || c === "'") { j = scanSelString(src, j) - 1; continue; }
          if (c === '\\') { j++; continue; }
          if (c === '/' && src.charAt(j + 1) === '*') { j = pastComment(j) - 1; continue; }
          if (c === '{') { brace = j; break; }
          if (c === ';' || c === '}') { term = j; break; }
        }
        if (brace < 0) { i = term >= 0 ? term + 1 : end; continue; }
        var close = matchBrace(brace);
        if (close < 0) return;
        var prelude = src.slice(start, brace).replace(/^\s+|\s+$/g, '');
        if (prelude.charAt(0) === '@') {
          // Group at-rules hold rules; the rest hold declarations, which the
          // inner scan reads as statements and walks past harmlessly.
          scan(brace + 1, close);
        } else if (normalizeSelector(prelude) === wantSel) {
          count++;
          if (count === ordinal) { found = src.slice(brace + 1, close); return; }
        }
        i = close + 1;
      }
    }
    scan(0, src.length);
    return found;
  }

  // A heal is only believed when the candidate reproduces every declaration
  // the broken serialisation still carried: the selector match alone cannot
  // tell two same-selector rules apart, and grafting a neighbour's
  // declarations under the right selector is a subtler bug than the one
  // being fixed.
  function validRepairedRule(rule, sel, block) {
    try {
      if (!scratchSelSheet) scratchSelSheet = new CSSStyleSheet();
      scratchSelSheet.replaceSync(sel + '{' + block + '}');
      if (scratchSelSheet.cssRules.length < 1) return false;
      var cand = scratchSelSheet.cssRules[0];
      if (cand.selectorText !== sel) return false;
      var have = rule.style, want = cand.style;
      for (var i = 0; i < have.length; i++) {
        var p = have.item(i);
        var v = have.getPropertyValue(p);
        if (v === '') continue; // the loss being healed
        if (want.getPropertyValue(p) !== v ||
            want.getPropertyPriority(p) !== have.getPropertyPriority(p)) {
          return false;
        }
      }
      return true;
    } catch (e) { return false; }
  }

  function healedRuleText(rule) {
    var text = rule.cssText;
    if (!hasLostShorthand(text)) return text;
    var sheet = null;
    try { sheet = rule.parentStyleSheet; } catch (e) { sheet = null; }
    if (!sheet) return text;
    var src = sheetSourceFor(sheet);
    if (!src) return text;
    var sel = rule.selectorText;
    var ord = ruleOrdinal(sheet, rule, sel);
    if (ord < 0) return text;
    var block = findRuleBlockInSource(src, sel, ord);
    if (block === null || !validRepairedRule(rule, sel, block)) return text;
    return sel + '{' + block + '}';
  }

  function collectRules(doc, list, out, depth, base, seen) {
    if (!list || depth > 8) return;
    for (var i = 0; i < list.length; i++) {
      var rule = list[i];
      try {
        switch (rule.type) {
          case 1: // style rule
            cssSeen++;
            if (selectorMatches(doc, rule.selectorText)) {
              // shippedWhole rather than the two bare passes: a nested
              // @media inside this rule's own text deserves the same
              // landside answer a top-level one gets (P-114), and the text
              // itself may first need its lost var() shorthands healed
              // from the sheet source (P-126).
              out.push(shippedWhole(healedRuleText(rule), base));
            } else {
              noteRejected(rule.selectorText);
            }
            break;
          case 4: // media
            collectMedia(doc, rule, out, depth, base, seen);
            break;
          case 12: // supports
            collectGroup(doc, rule, '@supports ' + rule.conditionText,
              null, out, depth, base, seen);
            break;
          case 7: // keyframes: small, and cheap insurance for CSS animations
            out.push(absolutizeCSSURLs(rule.cssText, base));
            break;
          case 5: // font-face
            // Substituting from the system is the right trade for a font that
            // carries text and the wrong one for a font that carries pictures.
            // See fontsWithoutSubstitute: only a family the page is drawing
            // private-use codepoints in is kept, and the server then ships the
            // file the way it ships any other url() in a stylesheet.
            var fam = firstFamily(rule.style && rule.style.fontFamily);
            if (fam && fontsWanted[fam]) {
              out.push(withIconNames(absolutizeCSSURLs(rule.cssText, base), fam));
            } else {
              noteRejected('@font-face ' + (fam || '?'));
            }
            break;
          case 3: // import
            collectImport(doc, rule, out, depth, base, seen);
            break;
          default:
            // A grouping at-rule the legacy `type` has no number for. These are
            // the ones the language grew after `type` was frozen, so they all
            // arrive as 0 and are told apart by what they hold.
            if (rule.cssRules && groupPrelude(rule)) {
              collectGroup(doc, rule, groupPrelude(rule), layerPlaceholder(rule),
                out, depth, base, seen);
            } else if (rule.cssRules && isScopeRule(rule)) {
              collectScope(doc, rule, out, base);
            } else if (rule.cssText && rule.cssText.charAt(0) === '@') {
              out.push(shippedWhole(rule.cssText, base));
            }
        }
      } catch (e) { /* cross-origin sheet, skip */ }
    }
  }

  /*
   * A group at-rule crosses as a rule that holds rules, and until now it
   * crossed whole.
   *
   * `@media` and `@supports` were walked into and the rest were not, because
   * the rest have no number: `type` was frozen before `@layer`, `@container`,
   * `@scope` and `@starting-style` existed, so every one of them arrives as 0
   * and the only branch left was the one that ships the text as it stands. That
   * was correct and it was expensive. Tailwind v4 writes its whole output
   * inside `@layer`, so on a captured page 93% of a 142 kB bundle — 131,799
   * bytes — crossed without the filter ever being asked about it, and the tally
   * the capture reports read "29 of 7 style rules", because seven was every
   * rule the filter had seen.
   *
   * So a grouping rule this can name the prelude of is walked into like any
   * other, and written back inside what it came in. The ones it cannot are
   * still shipped whole, which is the answer that cannot be wrong.
   */
  function groupPrelude(rule) {
    var kind = ruleKind(rule);
    if (kind === 'CSSLayerBlockRule') {
      // An anonymous layer is its own layer, and a second `@layer{}` is a
      // second one: re-emitted it would not merge with the first the way a
      // named layer does, and where it landed in the order would depend on
      // which pass a rule in it first matched on.
      return rule.name ? '@layer ' + rule.name : null;
    }
    if (kind === 'CSSContainerRule') {
      // conditionText carries the container name with the query when there is
      // one: `sidebar (min-width: 40rem)`.
      return '@container ' + rule.conditionText;
    }
    return null;
  }

  // layerPlaceholder is what to write when a named layer's every rule was
  // filtered out. A layer's place in the cascade is fixed by where its name
  // first appears, so a layer that turns up empty now and full later would
  // otherwise be ordered by the moment its first rule started matching.
  function layerPlaceholder(rule) {
    if (ruleKind(rule) !== 'CSSLayerBlockRule' || !rule.name) return null;
    return '@layer ' + rule.name + ';';
  }

  function isScopeRule(rule) {
    return ruleKind(rule) === 'CSSScopeRule';
  }

  function ruleKind(rule) {
    try {
      return (rule.constructor && rule.constructor.name) || '';
    } catch (e) {
      return '';
    }
  }

  /*
   * collectGroup filters a group at-rule's contents and writes back what
   * survived, inside the prelude it came in.
   *
   * The dedupe is the part worth explaining. A bundle is a delta: what has been
   * sent once is not sent again, and that was decided per top-level rule — for
   * a group, per *block*. One rule inside a 12,000-rule utility layer starting
   * to match therefore made the block's text different from the block already
   * sent, and the whole thing went again. On the link this exists for, a page
   * that mutates would have paid for its stylesheet over and over. So the
   * contents are deduped one rule at a time and the wrapper is written around
   * whatever is left, which is what a group at-rule allows: two `@media
   * (hover)` blocks are the one condition asked twice, and two `@layer
   * utilities` blocks are the one layer.
   */
  function collectGroup(doc, rule, prelude, placeholder, out, depth, base, seen) {
    collectInto(doc, rule.cssRules, prelude, placeholder, out, depth + 1, base, seen);
  }

  // collectInto is the body of that, taken out so a whole sheet can be walked
  // the same way: a `<link media>` is a wrapper around a sheet exactly as
  // `@media` is a wrapper around a block. See collectSheet.
  function collectInto(doc, rules, prelude, placeholder, out, depth, base, seen) {
    var inner = [];
    collectRules(doc, rules, inner, depth, base, seen);
    var fresh = [];
    for (var i = 0; i < inner.length; i++) {
      if (!seen || !seen.has(inner[i])) fresh.push(inner[i]);
    }
    if (!fresh.length) {
      if (placeholder) out.push(placeholder);
      return;
    }
    // No prelude left to write: the condition was one this side settled and it
    // held, so the rules apply unconditionally and go out on their own — and
    // are deduped by whoever receives them, exactly as a rule that was never in
    // a group at all is. Marking them here instead would mark the very text
    // about to be emitted, and the caller would then drop every one of them as
    // already sent. See resolveMediaList.
    if (!prelude) {
      for (var k = 0; k < fresh.length; k++) out.push(fresh[k]);
      return;
    }
    // The wrapper is what goes out, so the contents are what is remembered: it
    // is one rule inside starting to match that must not resend the block.
    if (seen) {
      for (var j = 0; j < fresh.length; j++) seen.set(fresh[j], 1);
    }
    out.push(prelude + '{' + fresh.join('') + '}');
  }

  /*
   * collectMedia walks a `@media` block under whatever is left of its condition
   * once this browser has answered the half that is about this browser.
   */
  function collectMedia(doc, rule, out, depth, base, seen) {
    var cond = '';
    try { cond = rule.conditionText || ''; } catch (e) { cond = ''; }
    var left = resolveMediaList(cond);
    if (left === null) {
      noteRejected('@media ' + cond);
      return;
    }
    collectGroup(doc, rule, left ? '@media ' + left : '',
      null, out, depth, base, seen);
  }

  /*
   * collectScope keeps or drops a `@scope` block whole.
   *
   * The rules inside one are written against the scope root — `:scope .row`,
   * and an implicit `:scope ` in front of everything else — so asking the
   * document about them as they stand is asking the wrong question, and it can
   * be wrong in the direction that drops a rule the page wants. What can be
   * asked honestly is whether the root itself is here: no `.card` on the page
   * means nothing in `@scope (.card)` can match, whatever it says inside.
   */
  function collectScope(doc, rule, out, base) {
    var start = null;
    try { start = rule.start; } catch (e) { start = null; }
    if (start && !selectorMatches(doc, String(start))) {
      noteRejected('@scope ' + start);
      return;
    }
    out.push(shippedWhole(rule.cssText, base));
  }

  // shippedWhole is what a block that crosses as text still has to have done to
  // it: the same three passes a walked rule gets, in the same order.
  function shippedWhole(text, base) {
    return resolveMediaInText(pinColorSchemes(absolutizeCSSURLs(text, base)));
  }

  /*
   * collectImport follows an `@import` into the sheet it names.
   *
   * An imported sheet is nowhere else. `document.styleSheets` lists the sheets
   * the document owns — a <link>, a <style> — and an imported one has no owner
   * node, so it is not in that list and no walk of it will ever reach the
   * rules. The import rule itself carried nothing but the address, and the
   * address was skipped as an at-rule the client could not act on, so a site
   * that imports its design system shipped the import and lost the sheet: all
   * of it, silently, with the filter reporting nothing rejected because it was
   * never asked.
   *
   * `styleSheet` is the way in, and it is a real sheet with a real href, so the
   * rules resolve their url() against their own address rather than against the
   * importer's. A cross-origin import cannot be read, which is the same
   * position a cross-origin <link> is in and gets the same answer: name it to
   * the host, which can read it over the protocol and hand the text back.
   *
   * The conditions an import may carry — `@import url(x) layer(a) supports(b)
   * print` — are exactly the group at-rules the sheet would have been wrapped
   * in had it been written inline, so that is what they become, innermost
   * first.
   */
  function collectImport(doc, rule, out, depth, base, seen) {
    var href = null;
    try { href = rule.href; } catch (e) { href = null; }
    var abs = href;
    if (href) {
      try { abs = new URL(href, base).href; } catch (e) { abs = href; }
    }
    var sheet = null;
    try { sheet = rule.styleSheet; } catch (e) { sheet = null; }
    var rules = null;
    if (sheet) {
      try { rules = sheet.cssRules; } catch (e) { rules = null; } // cross-origin
    }
    if (!rules && abs) {
      var sub = recoveredSheets.get(abs);
      if (sub) {
        try { rules = sub.cssRules; } catch (e) { rules = null; }
      } else if (!blockedSheets[abs]) {
        blockedSheets[abs] = 1;
        blockedNew = true;
      }
    }
    if (!rules) return;
    var inner = [];
    var sheetHref = null;
    try { sheetHref = sheet && sheet.href; } catch (e) { sheetHref = null; }
    collectRules(doc, rules, inner, depth + 1, sheetHref || abs || base, seen);
    if (!inner.length) return;
    var text = inner.join('');
    var media = '';
    try { media = (rule.media && rule.media.mediaText) || ''; } catch (e) { media = ''; }
    if (media) {
      // The same question a `@media` block asks, asked about the import that
      // stands in for one: a sheet imported only for a browser this is not
      // never applies here, and one imported for the browser this is applies
      // without saying so.
      var left = resolveMediaList(media);
      if (left === null) {
        noteRejected('@import ' + (abs || href || '?') + ' ' + media);
        return;
      }
      if (left) text = '@media ' + left + '{' + text + '}';
    }
    var supports = null;
    try { supports = rule.supportsText; } catch (e) { supports = null; }
    if (supports) text = '@supports ' + supports + '{' + text + '}';
    // A layer name of '' is the anonymous layer, which is not the same as no
    // layer at all: `@layer{…}` opens one, and where the import asked for one
    // the rules have to stay in it or they outrank everything in the cascade.
    var layer = null;
    try { layer = rule.layerName; } catch (e) { layer = null; }
    if (typeof layer === 'string') {
      text = '@layer ' + (layer ? layer + '{' : '{') + text + '}';
    }
    out.push(text);
  }

  /*
   * readableRules is a sheet's rules as this side can see them.
   *
   * A stylesheet served from another origin — which is where a CDN-backed site
   * keeps all of them — cannot be read through the CSSOM at all. The host
   * fetches the text over the protocol and hands it back through addSheet, and
   * the copy stands in for the original wherever the original is asked for: in
   * the sheet's own place in the cascade, which is what keeps a later rule
   * later. Both walks over the sheets go through here, so both see the same
   * document — a rule recovered for one and invisible to the other would have
   * the two disagreeing about a page neither can read twice.
   */
  function readableRules(sheet) {
    if (!sheet) return null;
    try { if (sheet.cssRules) return sheet.cssRules; } catch (e) { /* cross-origin */ }
    var href = null;
    try { href = sheet.href; } catch (e) { href = null; }
    var sub = href ? recoveredSheets.get(href) : null;
    if (!sub) return null;
    try { return sub.cssRules || null; } catch (e) { return null; }
  }

  function collectSheets(doc, sheets, out, seen) {
    if (!sheets) return;
    for (var i = 0; i < sheets.length; i++) {
      var sheet = sheets[i];
      var rules = readableRules(sheet);
      if (!rules) {
        // Nothing to read and nothing recovered yet: tell the host, which can
        // fetch what this side cannot.
        var missing = null;
        try { missing = sheet.href; } catch (e) { missing = null; }
        if (missing && !blockedSheets[missing]) {
          blockedSheets[missing] = 1;
          blockedNew = true;
        }
        continue;
      }
      collectSheet(doc, sheet, rules, out, seen);
    }
  }

  /*
   * collectSheet walks one sheet under the `media` its own tag carries.
   *
   * That attribute had been read by nobody. `document.styleSheets` lists every
   * <link> and <style> whatever their media says, and a browser parses a sheet
   * it is not currently applying — so a walk that goes straight to `cssRules`
   * collects the rules of a sheet the page is not using and hands them over
   * with nothing left to say they are conditional. The sheet stops being
   * conditional at all.
   *
   * It is the oldest way to split a site's themes and still the common one:
   *
   *     <link rel="stylesheet" media="(prefers-color-scheme: dark)" href="dark.css">
   *
   * Both sheets crossed, unwrapped, one after the other, and which theme the
   * reader got was decided by which <link> the page happened to write second.
   * `media="print"` is the same bug with a plainer symptom — a page's print
   * rules, applied to the screen.
   *
   * So the sheet's media is resolved exactly as a `@media` block's condition
   * is, and its rules are written back inside whatever is left of it.
   */
  function collectSheet(doc, sheet, rules, out, seen) {
    var media = '';
    try { media = (sheet.media && sheet.media.mediaText) || ''; } catch (e) { media = ''; }
    var left = resolveMediaList(media);
    if (left === null) {
      noteRejected('<sheet media> ' + media);
      return;
    }
    collectInto(doc, rules, left ? '@media ' + left : '', null, out, 0,
      sheetBase(doc, sheet), seen);
  }

  function collectUsedCSS(doc, seen) {
    var out = [];
    try { collectSheets(doc, doc.styleSheets, out, seen); } catch (e) { /* detached */ }
    // Constructed stylesheets are invisible to document.styleSheets, and they
    // are how every Lit-based web component ships its CSS. Without these a
    // component-heavy page arrives with its structure intact and no styling at
    // all, which looks far more broken than a missing rule.
    try { collectSheets(doc, doc.adoptedStyleSheets, out, seen); } catch (e) { /* unsupported */ }
    return out;
  }

  /*
  Which webfonts a page cannot be read without.

  Dropping @font-face and letting the system substitute is the right trade for
  a font that carries text: the reader gets the page in a font they already
  have, loses the typeface, and loses nothing they came for. It is the wrong
  trade for an icon font. Those glyphs live in the Unicode private use area —
  codepoints deliberately assigned no meaning, so every font on the reader's
  device is entitled to have nothing there, and the substitute draws a row of
  empty boxes where the toolbar was. The Google Maps capture that prompted
  this has thirty of them: U+E8B6, U+E52E, U+E56C, one per control.

  So a private-use codepoint on screen is the signal, and a precise one: it
  says this font is the only copy of what it draws, and no substitution can
  stand in. Nothing else is kept, which is what holds the cost to the pages
  that have no alternative — a page whose body font is a webfont still gets
  the reader's own, and still pays nothing for it.

  It is not the only signal, because it is not the only way to write an icon
  font. Material — so Google's own properties, and a large fraction of every
  other site built this decade — puts its glyphs at the ligature instead: the
  markup says `<i class="google-material-icons">mark_chat_unread</i>` and the
  font substitutes that whole run for one picture. Nothing in that text is
  private use, so the scan above sees nothing to keep, the family is dropped,
  and the substitute draws exactly what the markup says. The Google Chat
  capture is a nav with the word `star` where the star was and `spool` growing
  out of the side of a chip — worse than the empty boxes, because empty boxes
  read as absence and a word reads as the page.

  The signal for that kind is the declaration that makes it work.
  `font-feature-settings: "liga"` asks for a ligature substitution that would
  not otherwise happen, which is a thing to ask for only when the substitution
  is the content. Prose does not ask: ligatures are already on, so the one
  thing a text stylesheet ever writes here is the negation, `"liga" 0`, and on
  the Chat capture that is precisely how the two sorts divide — four rules
  asking for ligatures, all four naming an icon family, and every rule turning
  them off naming Google Sans.
  */

  // The private use areas: the BMP block, and the two supplementary planes as
  // the surrogate pairs a JavaScript string actually holds.
  var PUA_RE = /[\uE000-\uF8FF]|[\uDB80-\uDBBF][\uDC00-\uDFFF]/;

  // Bounds on the walk. Reading a computed style forces style resolution, so
  // the second is the one that matters; a page has a handful of icon families
  // at most, and sixty hits have found all of them long before this stops.
  var FONT_SCAN_NODES = 20000, FONT_SCAN_HITS = 64;

  // firstFamily takes the name a font-family list leads with, unquoted and
  // lowercased. The first entry is the one being asked for; the rest are what
  // to fall back to, which for an icon font is the substitution being avoided.
  function firstFamily(list) {
    if (!list) return '';
    var first = String(list).split(',')[0].trim();
    var q = first.charAt(0);
    if ((q === '"' || q === "'") && first.charAt(first.length - 1) === q) {
      first = first.slice(1, -1);
    }
    return first.toLowerCase();
  }

  /*
   * The shape of an icon name: the whole text run, lowercase letters, digits
   * and underscores, nothing else.
   *
   * This is what a ligature icon font is drawn from — "mark_chat_unread" is
   * both the markup and the glyph — and the point of recognising it is that
   * the server can then ship only the icons a page draws instead of the whole
   * family. Ordinary words match too, which costs a handful of glyphs in the
   * subset and nothing else; the family check below is what keeps prose out.
   */
  var ICON_NAME_RE = /^[a-z0-9_]{2,48}$/;

  // How many icon names one family's subset is built from. A page has dozens;
  // this is the point past which something is wrong and the whole font is the
  // better answer.
  var FONT_TEXT_MAX = 512;

  // And how many computed styles the icon hunt is allowed to read. The same
  // budget FONT_SCAN_HITS is: reading a computed style forces style resolution,
  // and a lowercase one-word text node is not rare enough on a page of prose to
  // let this run unbounded. Generous, because the icons in a toolbar are behind
  // however much text happens to be serialised before them.
  var FONT_ICON_LOOKS = 2000;

  function fontsWithoutSubstitute(docs) {
    var want = {};
    // Which families are drawn from ligatures, before the walk rather than
    // after it: the walk needs the answer to know whose icon names it is
    // collecting.
    var liga = {};
    for (var l = 0; l < docs.length; l++) ligatureFamilies(docs[l], liga);
    for (var lf in liga) want[lf] = 1;

    for (var d = 0; d < docs.length; d++) {
      var doc = docs[d];
      var root = doc.body || doc.documentElement || doc;
      var walker;
      try {
        walker = doc.createTreeWalker(root, 4 /* SHOW_TEXT */);
      } catch (e) { continue; }
      var nodes = 0, hits = 0, looks = 0, n;
      // Either budget still unspent keeps the walk going: a page with sixty
      // private-use icons would otherwise stop before it reached the ligature
      // ones, and a page with neither stops at the first of the two anyway.
      while (nodes++ < FONT_SCAN_NODES &&
             (hits < FONT_SCAN_HITS || looks < FONT_ICON_LOOKS)) {
        try { n = walker.nextNode(); } catch (e) { break; }
        if (!n) break;
        var value = n.nodeValue;
        if (!value) continue;
        var pua = PUA_RE.test(value);
        var name = pua ? '' : value.trim();
        var iconish = !pua && ICON_NAME_RE.test(name) && looks < FONT_ICON_LOOKS;
        if (!pua && !iconish) continue;
        if (pua && hits >= FONT_SCAN_HITS) continue;
        if (iconish) looks++;
        var el = n.parentElement;
        if (!el) continue;
        // From the element's own window: these documents include the inlined
        // same-origin iframes, and a style resolved against the wrong one is
        // not this element's style.
        var view = doc.defaultView || globalThis;
        var fam = '';
        try { fam = firstFamily(view.getComputedStyle(el).fontFamily); } catch (e) { fam = ''; }
        if (!fam) continue;
        if (pua) {
          hits++;
          want[fam] = 1;
          continue;
        }
        // An icon name only counts against a family already known to draw
        // ligatures. Every other lowercase word on the page resolves to some
        // prose family and is dropped here.
        if (liga[fam]) noteIconName(fam, name);
      }
    }
    return want;
  }

  /*
   * The icon names each family draws, which is what a subset is cut to.
   *
   * Kept across snapshots on purpose. A page that renders its toolbar, then
   * opens a menu with six more icons in it, asks for a font that covers both:
   * dropping the earlier names would cut a subset that no longer draws the
   * toolbar. The set only grows, so a key built from it only changes when
   * there is genuinely something new to draw.
   */
  var fontsText = {};

  function noteIconName(family, name) {
    var set = fontsText[family];
    if (!set) { set = fontsText[family] = {}; }
    if (!set[name] && Object.keys(set).length < FONT_TEXT_MAX) set[name] = 1;
  }

  // iconNamesFor lists a family's icon names in a fixed order, so that the same
  // page asking twice produces the same string and so the same cache key.
  function iconNamesFor(family) {
    var set = fontsText[family];
    if (!set) return '';
    return Object.keys(set).sort().join(' ');
  }

  /*
   * withIconNames writes a family's icon names into the @font-face rule that
   * ships its file.
   *
   * The server is what decides a font is too big to send, and what it needs to
   * cut a subset instead is the list of icons the page draws — which only this
   * side can see, because it is a fact about the document rather than about
   * the file. The rule that names the file is where that list belongs: it
   * travels the path the file travels, it is re-sent whenever the sheet is
   * re-collected, and nothing else has to grow a field for it.
   *
   * The descriptor is stripped again before the rule reaches the client, which
   * never has any use for it. See fontIconNames in css.go.
   */
  function withIconNames(text, family) {
    var names = iconNamesFor(family);
    if (!names || text.charAt(text.length - 1) !== '}') return text;
    return text.slice(0, -1) + ';' + ICON_NAMES_DESC + ':"' + names + '"}';
  }

  var ICON_NAMES_DESC = '-sky-icons';

  /*
   * One entry of a font-feature-settings list: a tag, optionally followed by
   * how much of it is wanted. `"liga"`, `"liga" 1` and `"liga" on` all ask for
   * ligatures; `"liga" 0` and `"liga" off` are a prose sheet turning them off,
   * which is the opposite of what is being looked for here.
   */
  var LIGA_ENTRY_RE = /^["']?(?:liga|clig|dlig)["']?(?:\s+(\S+))?$/;

  function asksForLigatures(style) {
    var value = '';
    try {
      // Blink aliases the prefixed property onto the standard one, so the
      // first read answers for both; the second is for a browser that does not,
      // since Material's own stylesheet still writes the prefix.
      value = style.fontFeatureSettings ||
        style.getPropertyValue('-webkit-font-feature-settings') || '';
    } catch (e) { return false; }
    if (!value || value === 'normal') return false;
    var parts = String(value).split(',');
    for (var i = 0; i < parts.length; i++) {
      var m = LIGA_ENTRY_RE.exec(parts[i].trim());
      if (!m) continue;
      if (m[1] === undefined || (m[1] !== '0' && m[1] !== 'off')) return true;
    }
    return false;
  }

  /*
   * ligatureFamilies reads the icon families out of a document's stylesheets.
   *
   * This walks the sheets rather than the DOM because the declaration is where
   * the evidence is, and reading it costs one property access per style rule
   * against the style resolution a computed read would force. `selectorMatches`
   * is asked only about the handful of rules that got that far — eleven of
   * twenty thousand on the Chat capture — so a family is kept only when the
   * page is actually wearing it, which is what stops a stylesheet that declares
   * `.material-icons` and never uses it from buying a font nothing draws.
   *
   * It runs before the collecting walk rather than inside it because the two
   * halves of the evidence are in different sheets: Google ships the icon
   * classes with the app and the `@font-face` from fonts.googleapis.com, and
   * which of those the walk reaches first is not something to depend on.
   */
  function ligatureFamilies(doc, want) {
    scanSheetsForLigatures(doc, doc.styleSheets, want);
    scanSheetsForLigatures(doc, doc.adoptedStyleSheets, want);
  }

  function scanSheetsForLigatures(doc, sheets, want) {
    if (!sheets) return;
    for (var i = 0; i < sheets.length; i++) {
      scanForLigatures(doc, readableRules(sheets[i]), want, 0);
    }
  }

  function scanForLigatures(doc, list, want, depth) {
    if (!list || depth > 8) return;
    for (var i = 0; i < list.length; i++) {
      var rule = list[i];
      try {
        if (rule.type === 1 && rule.style && asksForLigatures(rule.style)) {
          var fam = firstFamily(rule.style.fontFamily);
          if (fam && !want[fam] && selectorMatches(doc, rule.selectorText)) want[fam] = 1;
        }
        // A group at-rule holds rules, an `@import` holds a whole sheet, and a
        // nested rule holds both its own declarations and more rules.
        if (rule.cssRules) scanForLigatures(doc, rule.cssRules, want, depth + 1);
        else if (rule.styleSheet) {
          scanForLigatures(doc, readableRules(rule.styleSheet), want, depth + 1);
        }
      } catch (e) { /* cross-origin sheet, skip */ }
    }
  }

  /*
   * ------------------------------------------------- what the frame is told
   *
   * Two facts about the landside document that no rule in the page can carry,
   * because both are about being a document's root — and plane-side the page's
   * root is not one. It is an ordinary element inside the mirror's own document
   * (see §30), and the surface behind it, and the scheme the browser paints its
   * furniture in, belong to the frame.
   *
   * **The canvas.** A page's background does not paint its root box. It paints
   * the canvas — the whole surface behind the document, however short the
   * document is — and the value is taken from <html>, or from <body> where
   * <html> has none. A page that paints its <html> gets the right answer
   * plane-side by accident, because `html` is a type selector and matches the
   * mirror's own root as well as the page's copy of one. A page that paints
   * only its <body> — the ordinary way to write it — does not: a dark site
   * arrives as a dark document on a white field, white below the fold, white in
   * the margins, white wherever the document does not reach.
   *
   * **The colour scheme.** A browser that decides a page is light and its
   * reader is not may repaint it: Chrome for Android's "Dark theme" inverts a
   * page that has not said which scheme it is in, algorithmically, at paint
   * time. Applied to a mirror it repaints half of one — the DOM half — over
   * images the server fetched and transcoded from a light render, which is the
   * same half-a-theme this whole section exists to stop, arriving through a
   * door no stylesheet passes through. Measured: a mirrored light page comes
   * out rgb(18,18,18) under it, and rgb(255,255,255) with the one declaration
   * that turns it off. So the frame is told which scheme this document was
   * actually painted in, and told with `only`, which is the keyword that means
   * "and do not second-guess it".
   *
   * Both are sent as one rule about the mirror's own root. `:root` is the one
   * selector that cannot be confused about which document it means: plane-side
   * it is the frame's html element and never the page's. It is `!important`
   * because it is not part of the page's cascade at all — it is this side
   * reporting facts the other side cannot work out for itself, and a page rule
   * that lands in a later delta must not overturn them. `color-scheme` is
   * inherited rather than imposed: an element inside the page that declares its
   * own still wins for itself, which is what a dark card on a light page needs.
   *
   * Only the top-level agent says anything. A frame's document paints its own
   * box, and a frame repainting the reader's whole page would be a worse bug
   * than the one this fixes.
   */
  var rootRuleSent = '';

  function landsideRootRule() {
    if (!isTop) return '';
    var view, root;
    try {
      view = document.defaultView || globalThis;
      root = document.documentElement;
    } catch (e) { return ''; }
    if (!root) return '';
    var decls = [];
    var scheme = usedColorScheme(view, root);
    if (scheme) decls.push('color-scheme:' + scheme + ' !important');
    canvasBackground(view, root, decls);
    if (!decls.length) return '';
    // [data-sky-ground] marks this as the one :root that really means the
    // frame's own root — the host stamps its documentElement with it — so
    // the server's rewriteRootSelectors leaves it alone while re-pointing
    // every :root the page wrote at the mirrored document (P-119).
    return ':root[data-sky-ground]{' + decls.join(';') + ';}';
  }

  // The background properties that travel together. A background is a set, not
  // a colour: sending half of one puts a tile at the wrong size or repeats a
  // hero across the page.
  var CANVAS_BG_PROPS = [
    'background-image', 'background-position', 'background-size',
    'background-repeat', 'background-attachment', 'background-origin',
    'background-clip'
  ];

  /*
   * canvasBackground reads what paints the landside surface behind the document
   * and appends it to the rule the frame is given.
   *
   * Which element it comes from is the rule the propagation itself follows: the
   * root's, unless the root has neither an image nor a colour, in which case
   * the body's — and if neither has anything, nothing is said and the mirror's
   * own ground stands.
   *
   * A colour on its own is sent on its own, which is the common case and one
   * declaration. Anything with an image in it — a gradient, a tiled texture, a
   * hero photograph — brings the whole set, because a `background-image`
   * landing on top of the mirror's plain `background: #fff` would otherwise be
   * sized, positioned and repeated by that rule's defaults rather than by the
   * page's. The url() inside it is left absolute for the server to rewrite into
   * an image key, exactly as it does for the url() in any other rule
   * (rewriteCSSImages, css.go), so the picture crosses the link the same way
   * and at the same cost as every other background on the page.
   */
  function canvasBackground(view, root, decls) {
    var cs, src = root;
    try {
      cs = view.getComputedStyle(root);
      if (isTransparentColor(cs.backgroundColor) && isNoImage(cs.backgroundImage) &&
          document.body) {
        src = document.body;
        cs = view.getComputedStyle(src);
      }
    } catch (e) { return; }
    var color = cs.backgroundColor;
    var image = cs.backgroundImage;
    if (isNoImage(image)) {
      if (!isTransparentColor(color)) {
        decls.push('background-color:' + color + ' !important');
      }
      return;
    }
    decls.push('background-color:' +
      (isTransparentColor(color) ? 'transparent' : color) + ' !important');
    for (var i = 0; i < CANVAS_BG_PROPS.length; i++) {
      var prop = CANVAS_BG_PROPS[i];
      var v = '';
      try { v = cs.getPropertyValue(prop); } catch (e) { v = ''; }
      if (v) decls.push(prop + ':' + v + ' !important');
    }
  }

  function isNoImage(v) {
    return !v || v === 'none';
  }

  /*
   * usedColorScheme is which scheme this document was painted in, as a value
   * the other side can be given.
   *
   * The computed value is what was *declared*, so a document that named both
   * still has to be asked the same question the media query is asked. A
   * document that named neither was painted light, because that is what a
   * browser does with `normal` when nothing is forcing its hand — and this one
   * is a headless browser with nothing forcing its hand. A document that named
   * something else entirely says nothing: a scheme this build has no name for
   * is not one it can claim to have painted.
   */
  function usedColorScheme(view, root) {
    var declared = '';
    try {
      declared = String(view.getComputedStyle(root).colorScheme || '');
    } catch (e) { return ''; }
    var words = declared.toLowerCase().split(/\s+/);
    var light = false, dark = false;
    for (var i = 0; i < words.length; i++) {
      switch (words[i]) {
        case 'light': light = true; break;
        case 'dark': dark = true; break;
        case 'only': case 'normal': case '': break;
        default: return '';
      }
    }
    if (light && dark) {
      var wantDark = mediaAnswer('(prefers-color-scheme: dark)');
      if (wantDark === null) return '';
      return wantDark ? 'only dark' : 'only light';
    }
    return dark ? 'only dark' : 'only light';
  }

  // A computed background of nothing at all. Chromium spells it one way, but
  // the keyword is worth knowing too: a colour this cannot read is treated as
  // absent, which leaves the mirror's own white where it was.
  function isTransparentColor(c) {
    if (!c) return true;
    c = c.replace(/\s+/g, '');
    return c === 'transparent' || c === 'rgba(0,0,0,0)';
  }

  function cssDelta() {
    var docs = [document];
    // Shadow roots and same-origin iframe documents both carry their own
    // styles; either kind may hold them in a constructed sheet instead.
    observedDocs.forEach(function (d) {
      if (d !== document && (d.styleSheets || d.adoptedStyleSheets)) docs.push(d);
    });
    // One pass, one walk of each root. Held no longer than that: a rule
    // rejected against a stale index is a rule that stays rejected for the life
    // of the document, and the page the reader is looking at is the one that
    // just changed.
    presenceCache = new Map();
    // And what this browser answers about itself, for the same reason and the
    // same length of time. See mediaAnswer.
    mediaAnswers = null;
    mediaResolved = null;
    // Recomputed per pass, because a font arriving late is the ordinary case:
    // the icons are private-use codepoints from the first paint, but the sheet
    // that declares the family they need often lands after it.
    fontsWanted = fontsWithoutSubstitute(docs);
    // The rejection tally describes one pass, not the session: a selector that
    // matched nothing an hour ago may match now, and a list that only ever grew
    // would accuse the filter of dropping rules it has since shipped.
    cssSeen = 0;
    cssRejected = 0;
    cssRejectedList = [];
    var adds = [];
    var scoped = [];
    for (var d = 0; d < docs.length; d++) {
      var doc = docs[d];
      var root = docRoot.get(doc) || 0;
      // A scoped sheet is deduped against itself, not against the document's:
      // the same rule text may be needed in two places and mean two different
      // things, which is the point of scoping it.
      var seen = root ? scopedEmitted.get(root) : emittedCSS;
      if (root && !seen) { seen = new Map(); scopedEmitted.set(root, seen); }
      // The walk is handed the same record, because a rule inside a group
      // at-rule is deduped where it is found: only the walk knows which block a
      // rule came in, and re-sending a whole layer to add one rule to it is
      // what this costs otherwise. See collectGroup.
      var rules = collectUsedCSS(doc, seen);
      var mine = [];
      for (var i = 0; i < rules.length; i++) {
        var text = rules[i];
        if (seen.has(text)) continue;
        seen.set(text, 1);
        if (root) {
          mine.push(text);
        } else {
          cssOrder.push(text);
          adds.push(text);
        }
      }
      if (root && mine.length) scoped.push([root, mine]);
    }
    var rootRule = landsideRootRule();
    if (rootRule && rootRule !== rootRuleSent) {
      rootRuleSent = rootRule;
      cssOrder.push(rootRule);
      adds.push(rootRule);
    }
    // Faces the page registered through the FontFace API: no stylesheet
    // declares them, so the walk above can never find them (P-003).
    syntheticFonts.forEach(function (text) {
      if (emittedCSS.has(text)) return;
      emittedCSS.set(text, 1);
      cssOrder.push(text);
      adds.push(text);
    });
    return { adds: adds, scoped: scoped };
  }

  /*
   * announceBlocked tells the host there is a stylesheet here it could open.
   *
   * The host recovers cross-origin sheets on the main frame's load event, which
   * catches the ones the document was parsed with and none of the ones that
   * matter most: a widget that inserts its iframe after load brings a whole
   * stylesheet with it, and so does the next chunk of a client-side route. The
   * agent cannot read them and never asks again, so they stay blocked for the
   * life of the document — Google's "unusual traffic" interstitial arrives with
   * a reCAPTCHA whose checkbox has no styling at all, which renders as an
   * invisible zero-size span with nothing to click.
   *
   * The nudge carries no payload: the host answers it by calling blockedSheets,
   * which is the list, freshly walked.
   */
  function announceBlocked() {
    if (!blockedNew) return;
    blockedNew = false;
    send({ t: 'sheets' });
  }

  // emitCSSDelta collects and *sends* what it collected.
  //
  // Calling cssDelta and dropping the result is not a read: a rule it returns
  // has already been recorded as emitted, so discarding the return value drops
  // that rule from the page for good. Everything that walks the sheets goes
  // through here.
  //
  // `quiet` is for the host's own walk, which is already holding the answer
  // announceBlocked would be asking it for.
  function emitCSSDelta(quiet) {
    var delta = cssDelta();
    if (!quiet) announceBlocked();
    if (!delta.adds.length && !delta.scoped.length) return false;
    // Node 0 is the document's own sheet; anything else names the shadow root
    // whose sheet this is.
    if (delta.adds.length) pendingOps.push([7, delta.adds, 0]);
    for (var s2 = 0; s2 < delta.scoped.length; s2++) {
      pendingOps.push([7, delta.scoped[s2][1], delta.scoped[s2][0]]);
    }
    scheduleFlush(false);
    return true;
  }

  // ------------------------------------------------------------- svg sprites

  /*
   * An external <use> reference names a sprite file the sandboxed mirror can
   * never fetch: its CSP is default-src 'none', so the absolutised URL the
   * reference becomes draws nothing at all (P-116). Browsers require these
   * references to be same-origin, so the agent can always fetch the sprite
   * from inside the page. The fragment the reference names is carried on the
   * use element itself (data-sky-sprite), the reference is re-pointed at the
   * copy the client will build, and the client materialises the symbol into
   * a sprite holder of the mirror's own.
   */
  var spriteSources = new Map();  // sprite url -> its text
  var spriteFetches = new Map();  // sprite url -> 'pending' | 'ok' | 'failed'
  var spriteSymbols = new Map();  // url#frag -> extracted markup ('' = none)
  var spriteWaiters = new Map();  // sprite url -> use elements awaiting it

  function useSpriteRef(el) {
    var raw = el.getAttribute('href') || el.getAttribute('xlink:href') || '';
    if (!raw || raw.charAt(0) === '#') return null;
    var abs = absolute(docBase(el), raw);
    var hash = abs.indexOf('#');
    if (hash < 0) return null;
    var url = abs.slice(0, hash);
    var frag = abs.slice(hash + 1);
    if (!frag || !/^https?:/i.test(url)) return null;
    // A reference into the document's own URL is same-document already.
    if (url === String(location.href).split('#')[0]) return null;
    return { url: url, frag: frag, key: spriteKey(abs) };
  }

  function spriteKey(abs) {
    var h = 5381;
    for (var i = 0; i < abs.length; i++) h = ((h * 33) ^ abs.charCodeAt(i)) >>> 0;
    return 'sky-sprite-' + h.toString(36);
  }

  function spriteMarkup(url, frag) {
    var key = url + '#' + frag;
    if (spriteSymbols.has(key)) return spriteSymbols.get(key);
    var text = spriteSources.get(url);
    if (!text) return '';
    var markup = '';
    try {
      var sdoc = new DOMParser().parseFromString(text, 'image/svg+xml');
      var frel = sdoc.getElementById(frag);
      if (frel && frel.outerHTML && frel.outerHTML.length <= 65536) markup = frel.outerHTML;
    } catch (e) { markup = ''; }
    spriteSymbols.set(key, markup);
    return markup;
  }

  function requestSprite(url, el) {
    var els = spriteWaiters.get(url);
    if (!els) { els = []; spriteWaiters.set(url, els); }
    if (els.indexOf(el) < 0) els.push(el);
    if (spriteFetches.has(url)) return;
    spriteFetches.set(url, 'pending');
    try {
      fetch(url, { credentials: 'include' }).then(function (r) {
        return r && r.ok ? r.text() : null;
      }).then(function (t) {
        if (t) { spriteSources.set(url, t); spriteFetches.set(url, 'ok'); spriteLanded(url); }
        else { spriteFetches.set(url, 'failed'); spriteWaiters.delete(url); }
      }, function () { spriteFetches.set(url, 'failed'); spriteWaiters.delete(url); });
    } catch (e) { spriteFetches.set(url, 'failed'); spriteWaiters.delete(url); }
  }

  function pendingSpriteFetches() {
    var n = 0;
    spriteFetches.forEach(function (v) { if (v === 'pending') n++; });
    return n;
  }

  // spriteLanded hands each waiting use element its fragment, as an ordinary
  // attribute op: a sprite that arrives late rides the same train as one that
  // was cached at serialisation.
  function spriteLanded(url) {
    var els = spriteWaiters.get(url) || [];
    spriteWaiters.delete(url);
    var queued = false;
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      var id = idOf.get(el);
      if (id === undefined || !el.isConnected) continue;
      var ref = useSpriteRef(el);
      if (!ref || ref.url !== url) continue;
      var markup = spriteMarkup(ref.url, ref.frag);
      if (!markup) continue;
      pendingOps.push([3, id, intern('data-sky-sprite'), intern(markup)]);
      queued = true;
    }
    if (queued) scheduleFlush(false);
  }

  // familyHasFaceRule reports whether any readable stylesheet declares a
  // @font-face for this family: such a face ships through the ordinary walk,
  // and synthesizing a twin would be a second copy of the font.
  function familyHasFaceRule(family) {
    var want = String(family).toLowerCase().replace(/^["']|["']$/g, '');
    var docs = [document];
    observedDocs.forEach(function (d) { if (d !== document) docs.push(d); });
    for (var d = 0; d < docs.length; d++) {
      var sheets = null;
      try { sheets = docs[d].styleSheets; } catch (e) { continue; }
      if (!sheets) continue;
      for (var i = 0; i < sheets.length; i++) {
        var rules = null;
        try { rules = sheets[i].cssRules; } catch (e) { continue; }
        if (rules && faceRuleIn(rules, want)) return true;
      }
    }
    return false;
  }

  function faceRuleIn(rules, want) {
    for (var i = 0; i < rules.length; i++) {
      var r = rules[i];
      if (r.type === 5) { // CSSRule.FONT_FACE_RULE
        var fam = '';
        try { fam = String(r.style.getPropertyValue('font-family')); } catch (e) { fam = ''; }
        if (fam.toLowerCase().replace(/^["']|["']$/g, '') === want) return true;
      } else if (r.cssRules && faceRuleIn(r.cssRules, want)) {
        return true;
      }
    }
    return false;
  }

  function scheduleCSS() {
    if (cssTimer) return;
    cssTimer = setTimeout(function () {
      cssTimer = null;
      // Before the sweep, not after: an element that upgraded brings a shadow
      // root whose stylesheet this very pass is meant to collect.
      var upgraded = checkUpgrades();
      if (!emitCSSDelta() && upgraded) scheduleFlush(false);
    }, CSS_DEBOUNCE_MS);
  }

  // ------------------------------------------------------------------ mutations

  function isMirrored(node) {
    if (!node) return false;
    if (node.nodeType === KIND_ELEMENT && isSkipped(node)) return false;
    return true;
  }

  function knownParentId(node) {
    var p = node.parentNode;
    if (!p) return 0;
    // A shadow root is a node here in its own right, so it answers for its
    // children the way any parent does.
    return idOf.get(p) || 0;
  }

  // hasIn walks up the flat tree looking for an ancestor in a set. Shadow hosts
  // count as parents, because the mirror flattens shadow trees.
  function hasIn(node, set) {
    var p = node.parentNode;
    for (var depth = 0; p && depth < 256; depth++) {
      if (set.has(p)) return true;
      p = p.nodeType === 11 && p.host ? p.host : p.parentNode;
    }
    return false;
  }

  /**
   * handleMutations decides what a batch of records means *after* reading all
   * of them, not while reading them.
   *
   * MutationObserver reports history, not outcome. A node moved between two
   * parents arrives as a removal and an addition; a node added and dropped
   * within the same task arrives as an addition and a removal. Acting on each
   * record in turn gets both wrong: the move re-sends a subtree that the client
   * already has, and the dropped node is serialised, sent and immediately
   * deleted. rrweb hit both and solved them by deferring the decision to the
   * end of the batch, which is what this does.
   *
   * The state of the DOM at this point is the outcome, so `isConnected` is the
   * arbiter: still attached means it moved, detached means it is gone.
   */
  function handleMutations(records) {
    var removed = new Set();
    var addedSet = new Set();
    var added = [];
    var attrNames = new Map(); // node -> Set of attribute names
    var texts = new Set();

    for (var i = 0; i < records.length; i++) {
      var m = records[i];
      if (m.type === 'attributes') {
        if (/^on[a-z]/.test(m.attributeName) || SKIP_ATTRS[m.attributeName]) continue;
        var names = attrNames.get(m.target);
        if (!names) attrNames.set(m.target, names = new Set());
        // The name, not the value: the live value at flush time is the only one
        // worth sending, and a class toggled ten times a frame becomes one op.
        names.add(m.attributeName);
        continue;
      }
      if (m.type === 'characterData') {
        texts.add(m.target);
        continue;
      }
      var rm = m.removedNodes;
      for (var r = 0; r < rm.length; r++) removed.add(rm[r]);
      var ad = m.addedNodes;
      for (var a = 0; a < ad.length; a++) {
        if (!addedSet.has(ad[a])) {
          addedSet.add(ad[a]);
          added.push(ad[a]);
        }
      }
    }

    removed.forEach(function (node) {
      if (node.isConnected) return; // it moved; the addition below carries it
      var id = idOf.get(node);
      if (id === undefined) return;
      // A removed subtree costs one op: the client drops descendants with it.
      if (!hasIn(node, removed)) pendingOps.push([2, id]);
      forget(node);
    });

    for (var j = 0; j < added.length; j++) {
      var node = added[j];
      // Never mirrored, or gone again before we looked: rrweb calls these
      // dropped nodes, and they are pure waste on a link like this one.
      if (!node.isConnected || !isMirrored(node)) continue;
      if (hasIn(node, addedSet)) continue; // an ancestor's rows carry it
      var pid = knownParentId(node);
      if (!pid) continue; // parent not mirrored
      var existing = idOf.get(node);
      var beforeId = 0;
      var sib = node.nextSibling;
      while (sib && idOf.get(sib) === undefined) sib = sib.nextSibling;
      if (sib) beforeId = idOf.get(sib);
      if (existing !== undefined && byId.has(existing)) {
        // Keyed-list reorders arrive as remove+insert of the same node;
        // emitting a move keeps React-driven lists from re-sending subtrees.
        pendingOps.push([5, existing, pid, beforeId]);
        continue;
      }
      var rows = [];
      serializeNode(node, pid, rows);
      if (rows.length) pendingOps.push([1, pid, beforeId, rows]);
    }

    attrNames.forEach(function (names, el) {
      var id = idOf.get(el);
      if (id === undefined || !el.isConnected) return;
      names.forEach(function (name) {
        var val = el.getAttribute(name);
        if (val === null) {
          pendingOps.push([3, id, intern(name), -1]);
          return;
        }
        if (isSensitive(el) && SENSITIVE_ATTRS[name]) return;
        if (URL_ATTRS[name]) val = absolute(docBase(el), val);
        if (name === 'src' && VOID_IMAGE_TAGS[el.tagName]) {
          var img = describeImage(el, docBase(el));
          if (img) {
            pendingImages.push(img);
            val = 'skyhook://img/' + img.key;
            // The size the description was made at, the way a snapshot ships
            // it — and watched, so a box that settles later is re-stated.
            if (img.aw) pendingOps.push([3, id, intern('width'), intern(String(img.aw))]);
            if (img.ah) pendingOps.push([3, id, intern('height'), intern(String(img.ah))]);
            watchImg(el, img.aw, img.ah);
          }
        } else if ((name === 'href' || name === 'xlink:href') && tagOf(el) === 'USE') {
          var spr = useSpriteRef(el);
          if (spr) {
            val = '#' + spr.key;
            var sm = spriteMarkup(spr.url, spr.frag);
            if (sm) pendingOps.push([3, id, intern('data-sky-sprite'), intern(sm)]);
            else requestSprite(spr.url, el);
          }
        } else if (name === 'style') {
          // A background a script assigns as it scrolls arrives here rather
          // than in a snapshot, and needs the same rewrite. See styleAttrImages.
          var shot = styleAttrImages(val, docBase(el), el);
          if (shot) {
            val = shot.text;
            for (var s = 0; s < shot.images.length; s++) pendingImages.push(shot.images[s]);
          }
        }
        pendingOps.push([3, id, intern(name), intern(val)]);
      });
    });

    texts.forEach(function (node) {
      var tid = idOf.get(node);
      if (tid === undefined || !node.isConnected) return;
      if (addedSet.has(node) || hasIn(node, addedSet)) return; // just serialised
      var next = node.nodeValue || '';
      var prev = lastText.get(tid);
      lastText.set(tid, next);
      if (prev === next) return;
      if (prev === undefined) {
        pendingOps.push([4, tid, intern(next)]);
        return;
      }
      var sp = spliceOf(prev, next);
      if (sp && sp.ins.length + 8 < next.length) {
        pendingOps.push([6, tid, sp.off, sp.del, intern(sp.ins)]);
      } else {
        pendingOps.push([4, tid, intern(next)]);
      }
    });

    // The page's name and address move with a batch as often as not — a
    // client-side route change is a subtree swap and a pushState — and asking
    // here costs two string compares against what the client was last told.
    syncDocInfo();

    if (pendingOps.length) {
      scheduleFlush(false);
      scheduleCSS();
    }
  }

  // spliceOf finds the minimal middle edit between two strings. Chat logs and
  // typing both produce single-region edits, so this collapses a 40 KB text
  // node update into a few bytes.
  function spliceOf(prev, next) {
    var p = 0;
    var maxP = Math.min(prev.length, next.length);
    while (p < maxP && prev.charCodeAt(p) === next.charCodeAt(p)) p++;
    var s = 0;
    while (s < maxP - p && prev.charCodeAt(prev.length - 1 - s) === next.charCodeAt(next.length - 1 - s)) s++;
    var del = prev.length - p - s;
    var ins = next.slice(p, next.length - s);
    if (del < 0 || p + s > prev.length) return null;
    return { off: p, del: del, ins: ins };
  }

  function observeDocument(root) {
    if (observedDocs.has(root)) return;
    observedDocs.add(root);
    var obs = new MutationObserver(handleMutations);
    obs.observe(root, {
      subtree: true, childList: true, attributes: true,
      characterData: true, characterDataOldValue: false, attributeOldValue: false
    });
    observers.push(obs);
    if (root.addEventListener) {
      root.addEventListener('scroll', onScroll, { capture: true, passive: true });
      root.addEventListener('focusin', onFocus, { capture: true, passive: true });
      root.addEventListener('input', onInput, { capture: true, passive: true });
      // Top-layer membership is rendering state no attribute carries (P-122):
      // showPopover() and showModal() change what the reader sees without
      // touching anything a MutationObserver reports. The events do not
      // bubble, but capture reaches a non-bubbling event's target fine.
      root.addEventListener('toggle', onTopLayer, { capture: true, passive: true });
      root.addEventListener('close', onTopLayer, { capture: true, passive: true });
    }
  }

  // topLayerState names the top-layer membership an element holds: a shown
  // popover, a modal dialog, or nothing. A non-modal open dialog is not here
  // — its `open` attribute crosses like any attribute and renders in place.
  function topLayerState(el) {
    try { if (el.matches(':popover-open')) return 'popover'; } catch (e) { /* old engine */ }
    try { if (el.matches(':modal')) return 'modal'; } catch (e) { /* old engine */ }
    return '';
  }

  function onTopLayer(ev) {
    var el = ev.target;
    if (!el || el.nodeType !== KIND_ELEMENT) return;
    var id = idOf.get(el);
    if (id === undefined) return;
    var state = topLayerState(el);
    if ((topLayerShipped.get(el) || '') === state) return;
    if (state) topLayerShipped.set(el, state);
    else topLayerShipped.delete(el);
    pendingOps.push([3, id, intern('data-sky-open'), state ? intern(state) : -1]);
    scheduleFlush(false);
  }

  /*
   * An input event is the one moment a control's state is known to have moved,
   * and reporting it here rather than waiting for the sweep is what keeps a
   * reader's own typing off the half-second clock.
   *
   * A `select` reports on itself and not on the option that changed, so its
   * options are read instead — that is where `selected` lives.
   */
  function onInput(ev) {
    var el = ev.target;
    if (!el || !el.tagName) return;
    var changed = false;
    if (el.tagName === 'SELECT') {
      var opts = el.options || [];
      for (var i = 0; i < opts.length; i++) {
        if (emitLive(opts[i])) changed = true;
      }
    } else if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
      changed = emitLive(el);
    }
    if (changed) scheduleFlush(false);
  }

  function onFocus() {
    var el = document.activeElement;
    var id = el ? (idOf.get(el) || 0) : 0;
    if (id !== focusedId) {
      focusedId = id;
      pendingOps.push([9, id]);
      scheduleFlush(false);
    }
  }

  /**
   * Records a scroll position this agent just produced, so onScroll sees no
   * change and reports nothing.
   *
   * Without this the host's own nudges come straight back to the client as
   * scroll ops. The client is the authority on where the reader is looking, so
   * that report is never useful, and applying it moved the reader by most of a
   * viewport every round trip — a page being read scrolled itself away.
   */
  function ownScroll(id, el) {
    if (!id) {
      lastScroll.set(0, { x: globalThis.scrollX | 0, y: globalThis.scrollY | 0 });
      dirtyScroll.delete(0);
      return;
    }
    lastScroll.set(id, { x: el.scrollLeft | 0, y: el.scrollTop | 0 });
    // A position this side produced supersedes any report the reader's own
    // scroll had queued for the same box: what would go out now is the
    // host's nudge, which is exactly what must never echo.
    dirtyScroll.delete(id);
  }

  function onScroll(ev) {
    var t = ev.target;
    var id = 0;
    var x = 0, y = 0;
    if (t === document || t === document.documentElement || t === document.body || !t.tagName) {
      // An attached frame scrolling its own document is not the page scrolling:
      // the client would take it for the mirror's own scroll position and throw
      // the reader somewhere they never asked to be. What scrolls inside the
      // frame is the frame's root, which the client scrolls on its own.
      if (SLOT) return;
      x = globalThis.scrollX | 0; y = globalThis.scrollY | 0;
    } else {
      id = idOf.get(t) || 0;
      if (!id) return;
      x = t.scrollLeft | 0; y = t.scrollTop | 0;
    }
    var key = id;
    var prev = lastScroll.get(key);
    if (prev && prev.x === x && prev.y === y) return;
    lastScroll.set(key, { x: x, y: y });
    // Only the scrollers that actually moved go out. lastScroll is the
    // ledger of every position ever seen — the flush used to walk all of
    // it, so one container scrolling re-announced every container that had
    // ever scrolled, every 250 ms, forever, and re-announced the host's own
    // ownScroll nudges with them.
    dirtyScroll.add(key);
    if (scrollTimer) return;
    scrollTimer = setTimeout(function () {
      scrollTimer = null;
      dirtyScroll.forEach(function (nid) {
        var pos = lastScroll.get(nid);
        if (pos) pendingOps.push([10, nid, pos.x, pos.y]);
      });
      dirtyScroll.clear();
      scheduleFlush(false);
    }, SCROLL_THROTTLE_MS);
  }

  // -------------------------------------------------------------------- output

  function send(obj) {
    var json = JSON.stringify(obj);
    if (json.length <= CHUNK) { SEND(json); return; }
    var id = ++msgSeq;
    var total = Math.ceil(json.length / CHUNK);
    for (var i = 0; i < total; i++) {
      SEND(JSON.stringify({
        t: 'chunk', id: id, i: i, n: total,
        d: json.slice(i * CHUNK, (i + 1) * CHUNK)
      }));
    }
  }

  function takeStrings() {
    var s = pendingStrings;
    pendingStrings = [];
    return s;
  }

  function scheduleFlush(immediate) {
    if (immediate) {
      if (flushTimer) { clearTimeout(flushTimer); flushTimer = null; }
      flush(true);
      return;
    }
    if (flushTimer) return;
    flushTimer = setTimeout(function () { flushTimer = null; flush(false); }, MUTATION_BATCH_MS);
  }

  function flush(isFlush) {
    if (!snapshotDone) return;
    syncBoxes();
    if (!pendingOps.length && !pendingImages.length) return;
    var ops = pendingOps; pendingOps = [];
    var imgs = pendingImages; pendingImages = [];
    send({
      t: 'mut', seq: ++seq, strings: takeStrings(), ops: ops,
      images: imgs, flush: !!isFlush,
      url: location.href, title: document.title
    });
  }

  function snapshot() {
    // A snapshot resets both sides: intern table, ids stay (they are stable
    // handles for input replay), CSS set is rebuilt.
    strings = []; stringIndex = new Map(); pendingStrings = [];
    emittedCSS = new Map(); cssOrder = [];
    // Which includes the one rule the agent writes rather than finds: what has
    // been sent is a fact about a sheet that no longer exists. See
    // landsideRootRule.
    rootRuleSent = '';
    pendingOps = []; pendingImages = [];
    lastText = new Map();
    // The watch list is rebuilt by the walk below; its old ids may not even be
    // in the new document.
    awaitingUpgrade = new Set();
    // Read before the walk rather than synced after it: serializeAttrs marks
    // the element as it passes, and a mark applied afterwards would be an
    // attribute op about a node the client is being sent in the same breath.
    // On a first load this reads null however deep the link was — see the load
    // handler at the foot of this file — and on every snapshot after one it is
    // the answer.
    markedTarget = readTarget();
    sweepEvery = SWEEP_MS;
    // Same for the frames and the controls: the walk re-measures and re-reads
    // every one of them and says so in the snapshot, so what the client had
    // been told before is not about this document.
    boxWatch = new Map();
    boxDirty = new Set();
    boxObservers.forEach(function (obs) { obs.disconnect(); });
    boxObservers = new Map();
    liveWatch = new Map();
    // The snapshot carries both of these itself.
    lastInfo = { url: location.href, title: document.title || '' };

    var rows = [];
    // Collected by serializeAttrs while this walk runs, and by nothing else:
    // a mutation's inserts go through the same function, and a container that
    // arrives already scrolled is reported by onScroll when the page scrolls
    // it, not by the insert that built it.
    scrolled = [];
    if (SLOT) {
      /*
       * An attached frame mirrors itself into a root of its own, exactly as an
       * inlined same-origin frame is mirrored into one: a document's stylesheet
       * is written on the assumption that it governs a document, and flattened
       * into the page's it would dress the page.
       *
       * The row says parent 0 — this agent has no idea what it hangs from — and
       * the host rewrites that to the element in the parent's document that
       * stands for this frame. Neither hash covers parents, so rewriting one on
       * the wire is invisible to both ends; what matters is that the root is
       * registered *here*, and so is counted by this agent's hash exactly as
       * the client counts it.
       */
      var fragId = idFor(document);
      rows.push([fragId, 0, KIND_FRAGMENT, -1, 0, null]);
      docRoot.set(document, fragId);
      serializeNode(document.documentElement, fragId, rows);
    } else {
      serializeNode(document.documentElement, 0, rows);
    }
    // Handles to nodes this snapshot did not reach are dead. An iframe that
    // navigated is the case that matters: its old document is discarded whole,
    // without a mutation record, so nothing ever called forget() on its nodes.
    // They would then be hashed on this side and be absent on the client's,
    // which reads as a divergence that no resync can ever fix.
    var live = new Map();
    for (var r = 0; r < rows.length; r++) live.set(rows[r][0], byId.get(rows[r][0]));
    byId = live;
    // Root ids are new after a snapshot, so what each root had already been
    // sent is not about these roots.
    scopedEmitted = new Map();
    var delta = cssDelta();
    var css = delta.adds;
    var scopedCSS = [];
    for (var sc = 0; sc < delta.scoped.length; sc++) {
      scopedCSS.push({ root: delta.scoped[sc][0], rules: delta.scoped[sc][1] });
    }
    var imgs = pendingImages; pendingImages = [];
    snapshotDone = true;
    seq = 0;
    send({
      t: 'snap', seq: 0, strings: strings.slice(), nodes: rows, css: css,
      scoped: scopedCSS,
      url: location.href, title: document.title,
      scrollX: globalThis.scrollX | 0, scrollY: globalThis.scrollY | 0,
      vw: globalThis.innerWidth | 0, vh: globalThis.innerHeight | 0,
      dpr: globalThis.devicePixelRatio || 1,
      // The parser's own verdict, not the doctype's presence: an archaic
      // doctype still parses into quirks mode, and the mirror has to render
      // under the same rules the landside page really got (P-125).
      quirks: document.compatMode === 'BackCompat',
      icon: pageIconURL(),
      images: imgs,
      scrolls: scrolled || [],
      docHeight: Math.max(
        document.documentElement ? document.documentElement.scrollHeight : 0,
        document.body ? document.body.scrollHeight : 0)
    });
    scrolled = null;
    pendingStrings = [];
  }

  /*
   * pageIconURL names the page's favicon, for the tab strip (P-104). The
   * smallest raster the page declares wins — the strip draws it at 14px, so
   * a 16px icon beats the 512px maskable one — and a page that declares
   * nothing gets the /favicon.ico every browser would have asked for.
   */
  function pageIconURL() {
    if (!/^https?:$/.test(location.protocol)) return '';
    var links = document.querySelectorAll('link[rel~="icon" i], link[rel="shortcut icon" i]');
    var best = '', bestScore = -1;
    for (var i = 0; i < links.length; i++) {
      var href = links[i].getAttribute('href');
      if (!href) continue;
      var sizes = String(links[i].getAttribute('sizes') || '').toLowerCase();
      var score = 2;
      if (sizes.indexOf('16x16') >= 0) score = 5;
      else if (sizes.indexOf('32x32') >= 0) score = 4;
      else if (sizes === 'any') score = 3;
      else if (sizes) score = 1; // declared, and declared big
      if (score > bestScore) { bestScore = score; best = href; }
    }
    if (!best) best = '/favicon.ico';
    return absolute(location.href, best);
  }

  function start() {
    if (started) return;
    started = true;
    observeDocument(document);
    // A fragment changing reaches no mutation observer, and the sweep is on a
    // clock that backs off to eight seconds — which is a long time to look at
    // an unhighlighted footnote. The sweep still covers the `pushState` this
    // never fires for.
    if (globalThis.addEventListener) {
      globalThis.addEventListener('hashchange', function () {
        if (syncTarget()) scheduleFlush(false);
      }, { passive: true });
    }
    // Sheet sources up front, not on demand: healedRuleText needs a linked
    // sheet's authored text the moment a lost var() shorthand shows up in
    // the first CSS collect, and a repair that arrives as a follow-up update
    // is a layout that changes shape once — measurably, and visibly when the
    // shorthand was a border a paragraph wraps around. Prefetching alongside
    // the page's own load usually wins the race; when it loses, the repair
    // still ships as an ordinary update.
    prefetchSheetSources();
    snapshot();
    // Late-loading webfont/CSS work and lazily-attached shadow roots settle
    // within a second or two; a follow-up CSS pass is cheaper than a resnapshot.
    setTimeout(scheduleCSS, 800);
    setTimeout(scheduleCSS, 2500);
    scheduleSweep(sweepEvery);
  }

  function pendingSheetFetches() {
    var n = 0;
    sheetSourceFetches.forEach(function (v) { if (v === 'pending') n++; });
    return n;
  }

  function prefetchSheetSources() {
    var sheets;
    try { sheets = document.styleSheets; } catch (e) { return; }
    for (var i = 0; i < sheets.length; i++) {
      var href = null;
      try { href = sheets[i].href; } catch (e) { href = null; }
      if (!href) continue;
      // Only sheets whose rules are readable can present a broken cssText;
      // a cross-origin sheet throws before the question arises.
      try { void sheets[i].cssRules; } catch (e) { continue; }
      requestSheetSource(href);
    }
  }

  // ------------------------------------------------------------- coordinates

  /*
   * frameOffset is where a document's viewport sits inside the top-level one.
   *
   * Same-origin frames are inlined into the mirror, so the ids the client sends
   * back for input replay routinely belong to a frame's document rather than to
   * the page's — and `getBoundingClientRect` answers in the coordinates of
   * whichever document the element is in, measured from that frame's own top
   * left. The host replays a click by dispatching it at a point in the
   * top-level viewport, so without this every click inside a frame lands short
   * by exactly where the frame sits: on the page behind it, or on nothing.
   *
   * That failure is invisible from both ends. The mirror is correct, the click
   * is delivered, the page is fine, and the control simply never responds —
   * which is what a reCAPTCHA checkbox does when you cannot tick it.
   */
  function frameOffset(doc) {
    var dx = 0, dy = 0;
    var win = null;
    try { win = doc && doc.defaultView; } catch (e) { win = null; }
    for (var depth = 0; win && win !== globalThis && depth < FRAME_DEPTH_MAX; depth++) {
      var fe = null;
      // A cross-origin frame throws here, and there is nothing to correct for:
      // its document was never serialised, so no id inside it exists.
      try { fe = win.frameElement; } catch (e) { break; }
      if (!fe) break;
      var r = fe.getBoundingClientRect();
      dx += r.left + frameEdge(fe, 'Left');
      dy += r.top + frameEdge(fe, 'Top');
      try { win = fe.ownerDocument.defaultView; } catch (e) { break; }
    }
    return { x: dx, y: dy };
  }

  // frameEdge is the border plus padding on one side of a frame element: the
  // frame's viewport begins inside them, not at its border box.
  function frameEdge(el, side) {
    var cs = null;
    try { cs = el.ownerDocument.defaultView.getComputedStyle(el); } catch (e) { return 0; }
    if (!cs) return 0;
    return (parseFloat(cs['border' + side + 'Width']) || 0) + (parseFloat(cs['padding' + side]) || 0);
  }

  // viewportRect is an element's box in the top-level viewport, whichever
  // document it belongs to. Same shape as a DOMRect, in the parts read here.
  function viewportRect(el) {
    var r = el.getBoundingClientRect();
    var off = frameOffset(el.ownerDocument);
    if (!off.x && !off.y) return r;
    return {
      left: r.left + off.x, top: r.top + off.y,
      right: r.right + off.x, bottom: r.bottom + off.y,
      width: r.width, height: r.height
    };
  }

  /*
   * The 32-code-unit window every fingerprint reports — exactly what docHash
   * folds, except that a cut landing inside a surrogate pair backs off one
   * unit: the Go writers cannot hold a lone surrogate in a string, and a
   * list is only diffable if everyone cuts the same way. Twin of
   * mirror.HashValueWindow (model.go) and the patcher's fingerprintWindow.
   */
  function fingerprintWindow(v) {
    if (v.length <= 32) return v;
    var end = 32;
    var c = v.charCodeAt(31);
    if (c >= 0xD800 && c <= 0xDBFF) end = 31;
    return v.slice(0, end);
  }

  // --------------------------------------------------------------- host API

  // What the clipboard held last time anyone looked, so a probe can tell a
  // fresh copy from what was already there (P-008). Unseeded until one read
  // succeeds; the seeding read never relays, so whatever was on the OS
  // clipboard before this document existed stays where it is. clipInputSeen
  // marks the first replayed input: past it, only the input-boundary paths
  // may seed — a background seeder landing after an input could seed with
  // the very copy that input caused, and swallow it.
  var clipLast = null;
  var clipSeeded = false;
  var clipInputSeen = false;

  var api = {
    version: 1,
    start: start,
    snapshot: function () { snapshotDone = false; started = false; start(); return true; },
    flush: function () { scheduleFlush(true); return true; },
    /*
     * clipProbe answers "did the page just put something new on the
     * clipboard?" — a promise of { t: freshText } with t empty when nothing
     * is fresh, or { e: reason } when the clipboard cannot be read at all.
     * The host asks after replaying a click or a key, which is the only time
     * the answer is the reader's business: a copy nobody caused is not
     * relayed, and neither is anything predating the first successful read.
     * The refusal's name travels because a machine that denies
     * clipboard-read looks exactly like a timing miss otherwise; a failure
     * never unseeds.
     */
    clipProbe: function () {
      clipInputSeen = true;
      var clip = navigator.clipboard;
      if (!clip || !clip.readText) return Promise.resolve({ e: 'unavailable' });
      return clip.readText().then(function (text) {
        var fresh = clipSeeded && typeof text === 'string' &&
          text !== '' && text !== clipLast;
        clipSeeded = true;
        clipLast = text;
        return { t: fresh ? text.slice(0, 65536) : '' };
      }, function (err) { return { e: (err && err.name) || 'rejected' }; });
    },
    /*
     * clipSeed pins the relay's baseline at the input boundary: the host
     * calls it just before replaying a click or a key, so "fresh" afterwards
     * can only mean that input's own doing. This is what makes the baseline
     * correct on builds where document-start reads are refused — the seeding
     * used to happen whenever a read first succeeded, and on those builds
     * that moment was the first probe after the click, which then swallowed
     * the page's copy as its own baseline. Instant once seeded: one promise,
     * no clipboard read.
     */
    clipSeed: function () {
      clipInputSeen = true;
      if (clipSeeded) return Promise.resolve(true);
      var clip = navigator.clipboard;
      if (!clip || !clip.readText) return Promise.resolve(false);
      return clip.readText().then(function (text) {
        if (!clipSeeded) { clipSeeded = true; clipLast = text; }
        return true;
      }, function () { return false; });
    },
    node: function (id) { return byId.get(id) || null; },
    rect: function (id) {
      var n = byId.get(id);
      if (!n) return null;
      var el = n.nodeType === KIND_TEXT ? n.parentElement : n;
      if (!el || !el.getBoundingClientRect) return null;
      var r = viewportRect(el);
      // Scroll a target into view before reporting: the host clicks by
      // coordinate, and an offscreen element would land on the wrong node.
      if (r.bottom < 0 || r.top > (globalThis.innerHeight || 0) ||
          r.right < 0 || r.left > (globalThis.innerWidth || 0)) {
        try { el.scrollIntoView({ block: 'center', inline: 'center' }); } catch (e) { /* older engines */ }
        // The scroll that just happened is the host's own nudge, recorded so
        // onScroll reports nothing — the same discipline as scrollProbe, and
        // for the stakes see ownScroll's comment: this one echoed back as a
        // scroll op and threw a reader to the bottom of the page they were
        // reading, because a click on a below-the-fold element is exactly a
        // scroll the client did not make. Ancestor scrollers too:
        // scrollIntoView walks every scrollable box above the target.
        ownScroll(0);
        for (var sc = el.parentElement; sc; sc = sc.parentElement) {
          var scid = idOf.get(sc);
          if (scid) ownScroll(scid, sc);
        }
        r = viewportRect(el);
      }
      return {
        x: r.left, y: r.top, w: r.width, h: r.height,
        cx: r.left + r.width / 2, cy: r.top + r.height / 2,
        tag: el.tagName, editable: isEditable(el),
        href: el.tagName === 'A' ? (el.href || '') : '',
        // Whether a press here starts the browser's own drag-and-drop, which
        // a drag replay has to perform through drag interception rather than
        // as mouse moves. See Tab.drag.
        drag: !!(el.closest && el.closest('[draggable="true"]')),
        // Whether the page claimed touch gestures over this element: a
        // touch-action on it or an ancestor. A widget that pans under a real
        // finger must declare this or the browser takes the swipe for a
        // scroll, so it is the honest test for "would touch events reach the
        // page at all" — and with it, for which modality a finger's drag
        // should replay in. See Tab.drag.
        touchy: (function () {
          for (var a = el; a && a.style !== undefined; a = a.parentElement) {
            try {
              var ta = getComputedStyle(a).touchAction;
              if (ta && ta !== 'auto') return true;
            } catch (e) { break; }
          }
          return false;
        })()
      };
    },
    /*
     * The id this agent has for a node it is handed, and 0 for one it has never
     * serialised.
     *
     * The host calls this on the element that owns a cross-origin frame, by
     * resolving the frame's owner into this world and asking. That id is what
     * the frame's own document is spliced under, and there is no other way to
     * learn it: CDP knows the element, and only the agent knows what the client
     * calls it.
     */
    idOfNode: function (node) {
      var id = idOf.get(node);
      return id === undefined ? 0 : id;
    },
    /*
     * Where a frame element's content begins, in this document's own viewport
     * coordinates, and whether it is worth clicking into.
     *
     * A frame mirrored by its own agent measures everything against its own
     * viewport, and the host replays input at a point in the top-level one. The
     * difference is exactly this: the frame element's border-box origin plus
     * its border and padding, added up the chain of documents above it.
     */
    frameOrigin: function (id) {
      var el = byId.get(id);
      if (!el || !el.getBoundingClientRect) return null;
      var r = viewportRect(el);
      return {
        x: r.left + frameEdge(el, 'Left'),
        y: r.top + frameEdge(el, 'Top'),
        w: r.width, h: r.height
      };
    },
    /*
     * Marks a frame element as mirrored by an agent of its own, so the box for
     * it stops saying that its content did not come. Reported straight away:
     * the label is already on the client, and the panel it draws sits on top of
     * the document about to be spliced in behind it.
     */
    mirroredFrame: function (id, yes) {
      var el = byId.get(id);
      if (!el) return false;
      if (yes === false) {
        mirroredFrames.delete(el);
        boxDirty.add(el);
        scheduleFlush(false);
        return true;
      }
      mirroredFrames.add(el);
      var state = boxWatch.get(el);
      if (state !== undefined) boxWatch.set(el, state.split(' ')[0] + ' ');
      pendingOps.push([3, id, intern(OPAQUE_ATTR), -1]);
      scheduleFlush(false);
      return true;
    },
    // shots lists the boxes the host has to photograph, because their content
    // is pixels rather than DOM: canvas, WebGL and video. Sorted largest
    // first, so a budget spent on one region is spent on the one the reader
    // came for rather than on a 32px sparkline.
    //
    // Deliberately not rect(): that scrolls an offscreen target into view,
    // which is right for a click and wrong here. Moving the page in order to
    // photograph a corner of it would show the reader somewhere they are not.
    shots: function (max) {
      var vw = globalThis.innerWidth || 0, vh = globalThis.innerHeight || 0;
      var sx = globalThis.scrollX || 0, sy = globalThis.scrollY || 0;
      var out = [];
      byId.forEach(function (node, id) {
        if (!node || node.nodeType !== KIND_ELEMENT || !CANVAS_TAGS[node.tagName]) return;
        // viewportRect, not getBoundingClientRect: a canvas inside an inlined
        // same-origin frame measures against that frame's own viewport, and
        // the screenshot is of the top-level page — so the raw rectangle names
        // a place in the wrong document and photographs whatever is there.
        var r;
        try { r = viewportRect(node); } catch (e) { return; }
        // Clipped to the viewport: the host screenshots what the landside
        // browser has painted, and it has painted nothing outside it.
        var left = r.left, top = r.top;
        var x = Math.max(0, left), y = Math.max(0, top);
        var x2 = Math.min(vw, left + r.width), y2 = Math.min(vh, top + r.height);
        if (x2 - x < 8 || y2 - y < 8) return;
        out.push({
          // x and y are page coordinates, because that is what the screenshot
          // clip is measured in — a viewport-relative rectangle photographs
          // whatever happens to be that far down the document instead.
          n: id, x: x + sx, y: y + sy, w: x2 - x, h: y2 - y,
          // Where that clipped rectangle sits inside the element's own box, so
          // the client can put it back exactly where it came from.
          ox: x - left, oy: y - top
        });
      });
      out.sort(function (a, b) { return b.w * b.h - a.w * a.h; });
      return out.slice(0, max || 4);
    },
    focus: function (id) {
      var n = byId.get(id);
      if (!n) return false;
      var el = n.nodeType === KIND_TEXT ? n.parentElement : n;
      try { el.focus({ preventScroll: false }); } catch (e) { return false; }
      // Against the element's own document: focus inside a frame leaves the top
      // document pointing at the frame element, so asking there of an inlined
      // frame's field reports a failure that did not happen.
      return el.ownerDocument.activeElement === el;
    },
    /*
     * insertText types into a node from inside its own document, for the
     * frames CDP typing cannot reach: Input.insertText goes to the
     * browser's focused frame, and focusing an element from within an
     * inlined frame's agent does not make that frame the focused one — so
     * a keystroke aimed into a frame landed in the top document's body and
     * vanished (the other half of P-102). Contenteditable goes through
     * execCommand, which fires the input events a framework listens for;
     * fields splice at the caret through the native setter.
     */
    /*
     * fontFaceSeen is the landside browser telling the agent a web font
     * finished loading (CSS.fontsUpdated), with the one fact JavaScript can
     * never read back out of a FontFace: where it came from. A face the
     * page registered through the FontFace API has no @font-face rule for
     * the used-CSS walk to ship (P-003), so one is synthesized here and
     * rides the ordinary pipeline — rewrite, bytes, blob resolution and
     * all. A family a stylesheet already declares is left to that rule.
     */
    fontFaceSeen: function (family, src) {
      if (!family || !src) return false;
      if (syntheticFonts.has(family)) return true;
      if (familyHasFaceRule(family)) return false;
      syntheticFonts.set(family,
        '@font-face{font-family:' + JSON.stringify(String(family)) +
        ';src:url(' + JSON.stringify(String(src)) + ')}');
      scheduleCSS();
      return true;
    },
    insertText: function (id, text) {
      var el = byId.get(id);
      if (!el || !text) return false;
      try { el.focus({ preventScroll: true }); } catch (e) { /* still typed below */ }
      if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
        var proto = el.tagName === 'INPUT' ? HTMLInputElement.prototype : HTMLTextAreaElement.prototype;
        var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
        var cur = el.value || '';
        var s = typeof el.selectionStart === 'number' ? el.selectionStart : cur.length;
        var e2 = typeof el.selectionEnd === 'number' ? el.selectionEnd : s;
        setter.call(el, cur.slice(0, s) + text + cur.slice(e2));
        try { el.setSelectionRange(s + text.length, s + text.length); } catch (e) { /* number inputs */ }
        el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
        return true;
      }
      if (el.isContentEditable) {
        // execCommand reports failure by returning false, not by throwing —
        // and it does fail, in a frame whose document does not hold the
        // browser's focus. The fallback types the crude way: append and say so.
        var done = false;
        try { done = el.ownerDocument.execCommand('insertText', false, text); } catch (e) { done = false; }
        if (done) return true;
        // Spliced at the caret rather than appended, for the same reason
        // setValue puts the caret back: the reader's caret is not always at
        // the end, and typing that ignores it rewrites the sentence.
        var at = caretOffset(el);
        var was = el.textContent || '';
        el.textContent = was.slice(0, at) + text + was.slice(at);
        placeCaret(el, at + text.length, at + text.length);
        el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true, inputType: 'insertText', data: text }));
        return true;
      }
      return false;
    },
    setValue: function (id, value, start, end) {
      var el = byId.get(id);
      if (!el) return false;
      if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
        var proto = el.tagName === 'INPUT' ? HTMLInputElement.prototype : HTMLTextAreaElement.prototype;
        var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
        setter.call(el, value);
        if (typeof start === 'number') {
          try { el.setSelectionRange(start, typeof end === 'number' ? end : start); } catch (e) { /* number inputs */ }
        }
        // Frameworks listen for input/change, not for value assignment.
        el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
        el.dispatchEvent(new Event('change', { bubbles: true, composed: true }));
        return true;
      }
      if (el.tagName === 'SELECT') {
        // The reader chose from the mirror's own popup; here the choice is
        // applied as a value, exactly as typing is (P-101 — a select's
        // change used to reach this page only inside a form submit). The
        // native setter sidesteps any page-patched accessor, and a value
        // no option answers to leaves the control unchanged, which is what
        // the real popup would have done too. A multiple select crosses as
        // its selected values joined with newlines, which no option value
        // can contain.
        if (el.multiple) {
          var want = {};
          var parts = String(value).split('\n');
          for (var pi = 0; pi < parts.length; pi++) want[parts[pi]] = true;
          for (var oi = 0; oi < el.options.length; oi++) {
            el.options[oi].selected = !!want[el.options[oi].value];
          }
        } else {
          var sel = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value').set;
          sel.call(el, value);
        }
        el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
        el.dispatchEvent(new Event('change', { bubbles: true, composed: true }));
        return true;
      }
      if (el.isContentEditable) {
        el.textContent = value;
        // The caret the client measured, put back before anything else can be
        // typed. Without this every keystroke after a Backspace lands at the
        // front of the message (P-129).
        placeCaret(el, typeof start === 'number' ? start : value.length,
          typeof end === 'number' ? end : start);
        el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true }));
        return true;
      }
      return false;
    },
    submit: function (id, fields) {
      var form = byId.get(id);
      if (!form || form.tagName !== 'FORM') return false;
      if (fields) {
        Object.keys(fields).forEach(function (name) {
          var el = form.elements[name];
          if (el && 'value' in el) el.value = fields[name];
        });
      }
      if (typeof form.requestSubmit === 'function') form.requestSubmit();
      else form.submit();
      return true;
    },
    scrollTo: function (id, x, y) {
      if (!id) { globalThis.scrollTo(x, y); ownScroll(0); return true; }
      var el = byId.get(id);
      if (!el) return false;
      el.scrollLeft = x; el.scrollTop = y;
      ownScroll(id, el);
      return true;
    },
    // scrollProbe drives lazy loading and infinite scroll landside: the client
    // reports how far through the mirrored document the reader is, and the host
    // puts the real page at the same fraction of its own scrollable range. The
    // mapping is by range rather than by document height so that the top stays
    // the top and, more importantly, the bottom stays the bottom — an infinite
    // list only fetches more when the page is genuinely at its end.
    /*
     * scrollAnchor is the exact version of scrollProbe: put the element the
     * reader has at their viewport top at the same offset here (P-020). The
     * two documents differ in height — substituted fonts alone see to that —
     * so a fraction lands a lazy-load sentinel a viewport early or late; the
     * shared element lands it exactly. The fraction still travels, for the
     * element a mutation has since removed.
     */
    scrollAnchor: function (id, anchorY, fraction) {
      var el = byId.get(id);
      if (el && el.getBoundingClientRect && el.isConnected) {
        try {
          var top = el.getBoundingClientRect().top + (globalThis.scrollY || 0);
          globalThis.scrollTo({ top: Math.max(0, Math.round(top - anchorY)), behavior: 'instant' });
          ownScroll(0);
          return true;
        } catch (e) { /* fall through to the fraction */ }
      }
      return api.scrollProbe(fraction);
    },
    scrollProbe: function (fraction) {
      var h = Math.max(document.documentElement.scrollHeight, document.body ? document.body.scrollHeight : 0);
      var range = Math.max(0, h - (globalThis.innerHeight || 800));
      var target = Math.max(0, Math.min(range, Math.round(range * fraction)));
      globalThis.scrollTo({ top: target, behavior: 'instant' });
      ownScroll(0);
      return { height: h, top: globalThis.scrollY | 0 };
    },
    /**
     * Reports stylesheets the CSSOM will not open.
     *
     * Cross-origin sheets throw on `cssRules`, and a site that serves its CSS
     * from a CDN — which is most of them — has every stylesheet in that state.
     * The page then arrives with its whole structure and none of its design.
     * The host can read them over the protocol; this is how it learns which.
     */
    blockedSheets: function () {
      // Walking the sheets is what discovers them, and a caller may ask before
      // any CSS pass has run. It has to be the emitting walk: anything the
      // walk collects counts as sent from that moment on, so a bare cssDelta
      // here would quietly cost the page every rule this pass was the first to
      // see — which is the late-arriving stylesheets, every time.
      emitCSSDelta(true);
      // Whatever the walk just found is in the list being returned, so there is
      // nothing left to announce.
      blockedNew = false;
      return Object.keys(blockedSheets);
    },
    /**
     * Reports the rules the used-CSS filter turned down on its last pass.
     *
     * The filter is the part of this system most likely to be wrong about a
     * page, and it is invisible from the outside: a bundle holds the rules
     * that passed and nothing about the rest, so a rule dropped in error and a
     * rule the site never wrote look exactly alike. This is the other half of
     * that record.
     */
    rejectedCSS: function () {
      return {
        seen: cssSeen,
        rejected: cssRejected,
        truncated: cssRejected > cssRejectedList.length,
        selectors: cssRejectedList
      };
    },
    // sheets reports what became of the page's stylesheets, without walking
    // them: walking counts as sending what it finds, and the caller here is a
    // capture, which must not change what the reader is looking at.
    sheets: function () {
      return { blocked: Object.keys(blockedSheets), recovered: recoveredSheets.size };
    },
    /**
     * Supplies the text of a sheet blockedSheets named.
     *
     * It becomes a constructed stylesheet, which is same-origin whatever it was
     * built from, so the used-rule filter applies to it exactly as to the rest.
     * The sheet is never adopted into the document: the page must go on looking
     * the way it did, or the rules extracted from it would stop matching.
     */
    addSheet: function (href, text) {
      if (!href || !text) return false;
      try {
        var s = new CSSStyleSheet();
        s.replaceSync(text);
        recoveredSheets.set(href, s);
        // The authored text doubles as the repair source for cssText's
        // lost var() shorthands (healedRuleText).
        sheetSources.set(href, text);
        if (constructedSourceTexts) constructedSourceTexts.set(s, text);
        delete blockedSheets[href];
        scheduleCSS();
        return true;
      } catch (e) {
        return false;
      }
    },
    stats: function () {
      return { nodes: byId.size, strings: strings.length, css: cssOrder.length, seq: seq };
    },
    // diag reports everything the agent knows about itself, for a capture. It
    // is the landside half of "why do the two documents disagree": the client
    // reports the same shape, and the two are read side by side.
    diag: function () {
      var doc = document.documentElement;
      return {
        version: 1,
        nodes: byId.size,
        nextId: nextId,
        strings: strings.length,
        pendingStrings: pendingStrings.length,
        css: cssOrder.length,
        seq: seq,
        started: started,
        snapshotDone: snapshotDone,
        pendingOps: pendingOps.length,
        pendingImages: pendingImages.length,
        flushPending: flushTimer !== null,
        // Pending covers the timer and any sheet-source fetch the var()
        // shorthand repair kicked off: either one means the stylesheet the
        // client holds is about to change.
        cssPending: cssTimer !== null || pendingSheetFetches() > 0 || pendingSpriteFetches() > 0,
        // What the used-CSS filter did on its last pass. A page missing its
        // styling and a page whose rules were all rejected look the same from
        // the client; these two numbers tell them apart.
        cssSeen: cssSeen,
        cssRejected: cssRejected,
        blockedSheets: Object.keys(blockedSheets).length,
        observers: observers.length,
        observedDocs: observedDocs.size,
        // Custom elements still waiting for their definition. A capture taken
        // while this is large is a capture of a half-built page.
        awaitingUpgrade: awaitingUpgrade.size,
        focusedId: focusedId,
        messages: msgSeq,
        readyState: document.readyState,
        url: location.href,
        title: document.title,
        scrollX: globalThis.scrollX | 0,
        scrollY: globalThis.scrollY | 0,
        vw: globalThis.innerWidth | 0,
        vh: globalThis.innerHeight | 0,
        dpr: globalThis.devicePixelRatio || 1,
        docHeight: Math.max(doc ? doc.scrollHeight : 0, document.body ? document.body.scrollHeight : 0),
        // Nodes the page holds that the mirror never serialised. A number that
        // climbs while the mirror looks stale is the whole diagnosis.
        liveElements: document.getElementsByTagName('*').length,
        frames: document.getElementsByTagName('iframe').length,
        // Frames whose stand-in box is being kept up to date, and how many of
        // them are waiting to be re-read. A frame missing from the first number
        // is a stand-in frozen at whatever size it was born with.
        framesWatched: boxWatch.size,
        framesDirty: boxDirty.size,
        // Controls whose live state the sweep is keeping current, and how often
        // it is looking. A page where the sweep has backed off all the way is a
        // page nothing has changed for eight seconds.
        liveWatched: liveWatch.size,
        sweepEvery: sweepEvery,
        docHash: api.docHash()
      };
    },
    /**
     * fingerprint lists exactly what docHash is computed over, node by node,
     * so a hash mismatch can be turned into a list of the nodes responsible.
     *
     * The fourth column is the flags, which the hash does *not* cover — and
     * they are here because the hash agreeing is not the same as the mirror
     * being right. A custom element that upgraded after it was serialised has
     * the same id, kind and name on both sides and a shadow root on only one;
     * that difference is invisible in the hash, invisible in the HTML, and the
     * whole explanation for a page rendered inside out. Landside these are
     * read live, so they are what the element *is* now; the client's are what
     * it was sent, so a difference means its copy is stale.
     *
     * Values are truncated to the 32 characters the hash itself looks at, and
     * the whole list is capped: a pathological document must not turn a
     * capture into an out-of-memory.
     */
    fingerprint: function (limit) {
      var max = limit || 20000;
      var ids = Array.from(byId.keys()).sort(function (a, b) { return a - b; });
      var out = [];
      for (var k = 0; k < ids.length && out.length < max; k++) {
        var id = ids[k];
        var node = byId.get(id);
        if (!node) continue;
        var isText = node.nodeType === KIND_TEXT;
        var v = isText ? (node.nodeValue || '')
          : (node.tagName ? node.tagName.toLowerCase() : '');
        // An adopted sub-frame's document serialises as a fragment (the
        // wire's KindFragment, 11); reporting its raw nodeType (9) made the
        // three fingerprint lists disagree about the same node (P-128). The
        // truncation backs off a split surrogate pair for the same reason:
        // every writer must cut the same string the same way.
        var kind = node.nodeType === 9 ? 11 : node.nodeType;
        out.push([id, kind, fingerprintWindow(v),
          node.nodeType === KIND_ELEMENT ? flagsOf(node) : 0]);
      }
      return { total: ids.length, truncated: ids.length > out.length, nodes: out };
    },
    /**
     * parityProbe reports what the fingerprint cannot: attributes, computed
     * styles, boxes, text and image state, per element, for the comparison
     * against the patcher's copy (internal/parity). The hash agreeing means
     * the two documents have the same shape; this is for the bugs that ship
     * anyway — a stylesheet that never arrived, a value a mutation lost, a
     * picture that is a placeholder on the other side.
     *
     * Reads only. Like fingerprint, this must not change what the reader is
     * looking at, which is why serializeAttrs — which queues images and
     * registers observers — has a pure twin above instead of being called.
     *
     * Elements only, ascending id, sampled evenly past `limit` — except the
     * document roots, which are always included because every other element's
     * box is expressed relative to its root's.
     */
    parityProbe: function (limit) {
      var max = limit || 4096;
      var ids = [];
      byId.forEach(function (node, id) {
        if (node.nodeType === KIND_ELEMENT) ids.push(id);
      });
      ids.sort(function (a, b) { return a - b; });
      var stride = ids.length > max ? Math.ceil(ids.length / max) : 1;
      var families = new Map(); // first font family -> the document that uses it
      var out = [];
      var seen = {};
      function push(id) {
        if (seen[id]) return;
        var el = byId.get(id);
        if (!el || el.nodeType !== KIND_ELEMENT || !el.isConnected) return;
        seen[id] = 1;
        out.push(probeElement(el, id, families));
      }
      for (var k = 0; k < ids.length; k += stride) push(ids[k]);
      for (var j = 0; j < out.length; j++) {
        var root = out[j].r;
        if (root && !seen[root]) push(root);
      }
      var fonts = [];
      var seenFam = {};
      // Registered faces first: their load state is the truthful answer, and
      // a family registered on one half that never crossed is exactly what
      // check() cannot see — it answers true for any family the system can
      // fall back for, known or not.
      var fontDocs = [document];
      families.forEach(function (d) {
        if (fontDocs.indexOf(d) < 0) fontDocs.push(d);
      });
      for (var fd = 0; fd < fontDocs.length; fd++) {
        try {
          fontDocs[fd].fonts.forEach(function (face) {
            var fam = firstFamilyName(String(face.family || ''));
            if (!fam || seenFam[fam]) return;
            seenFam[fam] = 1;
            fonts.push({ family: fam, loaded: face.status === 'loaded', reg: true });
          });
        } catch (e) { /* no FontFaceSet here */ }
      }
      families.forEach(function (d, fam) {
        if (seenFam[fam]) return;
        seenFam[fam] = 1;
        var loaded = false;
        try { loaded = d.fonts.check('12px "' + fam.replace(/"/g, '') + '"'); } catch (e) { loaded = false; }
        fonts.push({ family: fam, loaded: loaded });
      });
      fonts.sort(function (a, b) { return a.family < b.family ? -1 : a.family > b.family ? 1 : 0; });
      var doc = document.documentElement;
      return {
        docs: [{
          slot: SLOT,
          url: location.href,
          title: document.title || '',
          compat: document.compatMode,
          scrollW: Math.max(doc ? doc.scrollWidth : 0, document.body ? document.body.scrollWidth : 0),
          scrollH: Math.max(doc ? doc.scrollHeight : 0, document.body ? document.body.scrollHeight : 0),
          vw: globalThis.innerWidth | 0,
          vh: globalThis.innerHeight | 0,
          dpr: globalThis.devicePixelRatio || 1,
          nodes: byId.size,
          fonts: fonts
        }],
        nodes: out,
        truncated: stride > 1
      };
    },
    /**
     * checkpoint anchors a divergence check to one frame.
     *
     * The hash on its own says nothing: it describes the document *now*, and
     * the client's document is whatever the last frame it acknowledged made
     * it. Comparing the two compares two different instants, and on any page
     * that changes faster than the link's round trip they will differ for ever
     * — which is a resync every thirty seconds on a link that cannot afford
     * one, and the resync makes the lag it is blamed on worse.
     *
     * So the pair travels together: the sequence number a client would have to
     * reach, and the hash of the document at exactly that point. Pending
     * records are drained and pending ops sent first, so the ops the client is
     * about to apply and the hash reported here describe the same document —
     * JavaScript is single-threaded, so nothing can change between the drain
     * and the hash.
     */
    checkpoint: function (seed) {
      for (var i = 0; i < observers.length; i++) {
        var records = observers[i].takeRecords();
        if (records.length) handleMutations(records);
      }
      if (pendingOps.length || pendingImages.length) scheduleFlush(true);
      return { seq: seq, hash: api.docHash(seed) };
    },
    /*
     * The hash of what this agent holds, continuing from `seed`.
     *
     * One tab can be fed by several agents — the page and every cross-origin
     * frame the host attached to — and the client hashes the one document they
     * add up to, in ascending id order. Ids are namespaced by slot, so that
     * order visits each frame's nodes in one run, and the whole hash is each
     * agent's chained into the next. Passing nothing starts from the FNV basis,
     * which is what a page with no attached frames has always computed.
     */
    docHash: function (seed) {
      // Cheap whole-document fingerprint for divergence checks. Two details
      // decide whether this is worth anything at all, because the Go replica
      // and the TypeScript patcher have to reproduce it exactly:
      //
      //   - ids are visited in order. A Map iterates by insertion, which is
      //     document order at first and is not after the first node is removed
      //     and another added.
      //   - the multiply is Math.imul. `h * 16777619` is a double multiply, and
      //     once h passes 2^29 the product needs more than 53 bits and the low
      //     ones are silently rounded away — so this returned a number no exact
      //     uint32 implementation could ever produce, and the integrity check
      //     re-snapshotted every document on the planet every thirty seconds.
      var h = seed === undefined || seed === null ? 0x811c9dc5 : (seed >>> 0);
      var ids = Array.from(byId.keys()).sort(function (a, b) { return a - b; });
      for (var k = 0; k < ids.length; k++) {
        var id = ids[k];
        var node = byId.get(id);
        if (!node) continue;
        // Lowercased tag names keep this identical to the Go replica and the
        // TypeScript patcher, so a hash mismatch means real divergence.
        var v = node.nodeType === KIND_TEXT ? (node.nodeValue || '')
          : (node.tagName ? node.tagName.toLowerCase() : '');
        h ^= id & 0xff;
        h = Math.imul(h, 16777619) >>> 0;
        for (var i = 0; i < v.length && i < 32; i++) {
          h ^= v.charCodeAt(i) & 0xff;
          h = Math.imul(h, 16777619) >>> 0;
        }
      }
      return h >>> 0;
    }
  };

  /*
   * adopt hands a frame the id space its nodes live in, and starts it.
   *
   * Ids are namespaced by slot so that two agents feeding one client can never
   * collide. Every hash on both sides visits ids in ascending order, which puts
   * each frame's nodes in one contiguous run — so the hash of the whole mirror
   * is each agent's chained into the next, in slot order, and nothing has to
   * know how many agents there were. 2^32 ids per frame, and a JavaScript
   * number holds every one of them exactly up to slot 2^21.
   */
  api.adopt = function (slot) {
    if (adopted || !(slot > 0)) return false;
    adopted = true;
    SLOT = slot;
    nextId = FRAME_BASE + (SLOT - 1) * FRAME_SPAN + 1;
    startWhenReady();
    return true;
  };

  Object.defineProperty(globalThis, '__skyhook', { value: api, configurable: true });

  // Baseline the clipboard now, so the first probe after an input cannot
  // mistake what was already there for a copy the page just made (P-008). A
  // failed read leaves it unseeded, and then the first probe that can read
  // seeds instead of relaying — the invariant lives in clipProbe.
  //
  // Retried, because the first read can lose for reasons that pass: the
  // per-origin permission grant lands moments around the document on some
  // Chrome builds, and focus arrives when the tab does. A seeder that gave up
  // after one refusal left the first probe after the reader's click to
  // swallow the page's first copy as its baseline — the relay's own
  // bookkeeping eating the one thing it exists to deliver.
  (function seedClipboard(tries) {
    try {
      if (!navigator.clipboard || !navigator.clipboard.readText) return;
      navigator.clipboard.readText().then(function (text) {
        // Never over an input's head: past the first replayed input, a late
        // success here could be reading the very copy that input caused.
        if (!clipSeeded && !clipInputSeen) { clipSeeded = true; clipLast = text; }
      }, function () {
        if (tries > 0 && !clipSeeded && !clipInputSeen) {
          setTimeout(function () { seedClipboard(tries - 1); }, 700);
        }
      });
    } catch (e) { /* no clipboard in this context */ }
  })(6);

  function startWhenReady() {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', start, { once: true });
    } else {
      start();
    }
  }

  if (adopted) {
    startWhenReady();
  } else {
    // Nothing above this frame can read it, and it does not know what the
    // client calls the box it sits in. The host does; this says where to look.
    SEND(JSON.stringify({ t: 'frame', url: location.href }));
  }
  globalThis.addEventListener('load', function () {
    if (!adopted) return;
    scheduleCSS();
    // `:target` is not settled at DOMContentLoaded, which is when the snapshot
    // is taken. Chromium scrolls to the indicated part of a document once it
    // has loaded, and sets `:target` at that moment — measured: null and
    // readyState `interactive` at DOMContentLoaded, the element and `complete`
    // at load, every time. So the one event that has the answer is this one,
    // and without it a deep link's highlight waited on a sweep.
    syncTarget();
    scheduleFlush(false);
  }, { once: true });
})();
