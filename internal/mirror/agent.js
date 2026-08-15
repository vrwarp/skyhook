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

  // Only the top-level document is mirrored. The host installs this script into
  // every frame's isolated world, so without this check a subframe runs its own
  // agent and sends a snapshot of the subframe's document on the tab's stream —
  // and the client, which has no idea a frame produced it, replaces the whole
  // page with the contents of the frame. Same-origin frames are inlined by the
  // top-level agent below; cross-origin frames cannot be reached at all, and a
  // frame that mirrors itself into the wrong document is worse than an empty
  // box.
  var isTop = true;
  try { isTop = globalThis.top === globalThis.self; } catch (e) { isTop = false; }
  if (!isTop) return;

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
  // How often a custom element that has not upgraded yet is looked at again,
  // and how far that interval backs off while nothing is happening.
  var UPGRADE_POLL_MS = 500;
  var UPGRADE_POLL_MAX_MS = 8000;
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
    SCRIPT: 1, NOSCRIPT: 1, STYLE: 1, LINK: 1, META: 1, BASE: 1, TEMPLATE: 1,
    OBJECT: 1, EMBED: 1, APPLET: 1
  };
  var CANVAS_TAGS = { CANVAS: 1, VIDEO: 1, AUDIO: 1 };
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
  var SENSITIVE_AUTOCOMPLETE = /(^|\s)(current-password|new-password|one-time-code|cc-number|cc-csc)(\s|$)/i;

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
  var reframeTimer = null;
  var pendingOps = [];
  var pendingImages = [];
  var flushTimer = null;
  var cssTimer = null;
  var emittedCSS = new Map();   // rule text -> index
  var scopedEmitted = new Map(); // shadow-root id -> its own emitted-rule set
  var cssOrder = [];
  var recoveredSheets = new Map(); // href -> constructed sheet the host supplied
  var blockedSheets = {};          // href -> 1, for sheets nothing can read yet
  var blockedNew = false;          // one of those is news the host has not heard
  var cssSeen = 0;                 // style rules the last pass considered
  var cssRejected = 0;             // of those, how many matched nothing
  var cssRejectedList = [];        // and which, up to CSS_REJECTED_MAX
  var fontsWanted = {};            // family -> 1, for fonts nothing can substitute
  var lastText = new Map();     // id -> last text we reported
  var lastScroll = new Map();
  var awaitingUpgrade = new Set(); // ids of custom elements not yet defined
  var upgradeTimer = null;
  var upgradePoll = UPGRADE_POLL_MS;
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
      }
      pairs.push(intern(name), intern(value));
    }
    // Live form state is a property, not an attribute; without this a mirrored
    // form loses everything the user (or the page) already typed.
    var tag = el.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA') {
      if (!isSensitive(el)) {
        pairs.push(intern('data-sky-value'), intern(el.value == null ? '' : el.value));
      }
      if (el.checked) pairs.push(intern('data-sky-checked'), intern('1'));
    } else if (tag === 'OPTION' && el.selected) {
      pairs.push(intern('data-sky-selected'), intern('1'));
    }
    // Whether a custom element has upgraded is a live question landside and a
    // settled one plane-side, where no definition will ever run: every custom
    // element there is undefined for ever. A site that dresses its placeholders
    // with `:not(:defined)` would get the placeholder styling on top of the
    // upgraded markup, and the upgraded styling — gated on `:defined` — would
    // match nothing. So the landside answer is recorded here, and the used-CSS
    // rules are rewritten against it (see rewriteDefined in css.go).
    if (isCustom(el) && !isDefined(el)) pairs.push(intern(UNDEFINED_ATTR), intern(''));
    flags |= flagsOf(el);
    if (tag === 'IFRAME') {
      // The client cannot materialise an iframe — it would be a browsing
      // context, and the whole point is that nothing plane-side fetches
      // anything — so it renders the inlined document into a plain box. That
      // box has to be told how big it is, because the CSS that sized the real
      // iframe selects on `iframe` and will not match the substitute.
      var fr = el.getBoundingClientRect();
      if (fr.width || fr.height) {
        pairs.push(intern('data-sky-box'),
          intern(Math.round(fr.width) + 'x' + Math.round(fr.height)));
      }
    }
    if (VOID_IMAGE_TAGS[tag]) {
      var img = describeImage(el, base);
      if (img) {
        flags |= FLAG_IMAGE;
        pairs.push(intern('src'), intern('skyhook://img/' + img.key));
        if (img.w) pairs.push(intern('width'), intern(String(img.w)));
        if (img.h) pairs.push(intern('height'), intern(String(img.h)));
        pendingImages.push(img);
      }
    }
    out.flags = flags;
    return pairs;
  }

  function describeImage(el, base) {
    var src = el.currentSrc || el.getAttribute('src') || '';
    if (!src) return null;
    src = absolute(base, src);
    if (/^data:/i.test(src)) {
      // Small inline images are cheaper left alone than round-tripped.
      if (src.length < 4096) return null;
    }
    if (/^blob:/i.test(src)) return null;
    var r = el.getBoundingClientRect();
    var w = Math.round(r.width) || el.naturalWidth || 0;
    var h = Math.round(r.height) || el.naturalHeight || 0;
    if (w > 4096) w = 4096;
    if (h > 4096) h = 4096;
    var key = imageKey(src, w, h);
    return {
      n: idFor(el), url: src, w: w, h: h, key: key,
      alt: el.getAttribute('alt') || '',
      pri: r.top < (globalThis.innerHeight || 900) * 1.5 && r.bottom > -200 ? 0 : 1
    };
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
    if (CANVAS_TAGS[tag]) return n;

    if (tag === 'SLOT') {
      // A slot renders what was assigned to it, so that is what goes here. The
      // nodes come from the host's light DOM and are skipped where they sit
      // (see childrenToSerialize), which is the whole of what "flattened" has
      // to mean: composed, not merely moved inside the host.
      //
      // Left as it was, a component's light DOM rendered next to the slot
      // instead of in it, and everything the slot was inside stopped applying
      // to it. Reddit hangs its tooltips off `<span slot="content">`, and the
      // slot they belong in sits inside a `hidden` box: the mirror drew "Open
      // navigation", "Go to Reddit Home" and "Log in to Reddit" across the top
      // of the page, permanently, because the box that hides them was no
      // longer an ancestor.
      var assigned = slotContent(node);
      for (var a = 0; a < assigned.length; a++) n += serializeNode(assigned[a], id2, rows);
      return n;
    }

    if (node.shadowRoot) {
      // Flattening the shadow tree in place keeps the client a plain patcher.
      var sk = node.shadowRoot.childNodes;
      for (var s = 0; s < sk.length; s++) n += serializeNode(sk[s], id2, rows);
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
    var kids = childrenToSerialize(node);
    for (var i = 0; i < kids.length; i++) n += serializeNode(kids[i], id2, rows);
    return n;
  }

  /*
   * slotContent gives what a slot actually renders.
   *
   * `{flatten: true}` is what makes this the rendered answer rather than the
   * assigned one: a slot with nothing assigned draws its own children as
   * fallback, and a slot assigned another slot resolves through it. Both are
   * ordinary in a component library, and both are what the reader sees.
   */
  function slotContent(slot) {
    try {
      return slot.assignedNodes ? slot.assignedNodes({ flatten: true }) : slot.childNodes;
    } catch (e) {
      return slot.childNodes;
    }
  }

  var NO_CHILDREN = [];

  /*
   * childrenToSerialize gives the children that render under this node.
   *
   * A node assigned to a slot renders there and not here, so it is left for the
   * slot to emit. A light-DOM child of a shadow host that no slot claimed
   * renders nowhere at all — the browser drops it — and mirroring it would put
   * content on the reader's screen that nobody else can see.
   */
  function childrenToSerialize(node) {
    // Nothing here distributes anything — including a host whose root is
    // closed, which reads as no root at all. Its light DOM goes on being
    // mirrored where it sits: the same guess as before, and the only one left.
    if (!node.shadowRoot) return node.childNodes;
    // An open host draws none of its light DOM where it sits: what a slot
    // claimed is drawn at that slot, and what no slot claimed is drawn nowhere.
    // The host's own subtree came from the shadow root above.
    return NO_CHILDREN;
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
      try { if (!el.contentDocument) return; } catch (e) { return; }
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
    upgradePoll = UPGRADE_POLL_MS;
    scheduleUpgradeCheck(upgradePoll);
  }

  function scheduleUpgradeCheck(delay) {
    if (upgradeTimer || !awaitingUpgrade.size) return;
    upgradeTimer = setTimeout(function () {
      upgradeTimer = null;
      var before = awaitingUpgrade.size;
      if (checkUpgrades()) {
        scheduleFlush(false);
        // The shadow root that just arrived brings its own stylesheet.
        scheduleCSS();
      }
      // Components load in waves: back off while nothing is happening, and
      // start over the moment something upgrades.
      upgradePoll = awaitingUpgrade.size < before
        ? UPGRADE_POLL_MS
        : Math.min(upgradePoll * 2, UPGRADE_POLL_MAX_MS);
      scheduleUpgradeCheck(upgradePoll);
    }, delay);
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

  // ------------------------------------------------- shadow-scoped selectors

  /*
   * The mirror flattens every shadow tree into its host (see serializeNode), so
   * plane-side there is no boundary left for a shadow-scoped selector to be
   * scoped by. `:host` matches nothing outside a shadow tree and `::part()`
   * names a part of a tree that is no longer there, so a component's own
   * stylesheet crosses the link intact and then does nothing at all.
   *
   * That is not a corner case on a site built out of web components. Reddit's
   * search field is a `faceplate-search-input` whose box padding, its font, and
   * the `white-space: pre` that keeps its placeholder on one line are all
   * `:host` rules; with every one of them inert the field spelled "Find
   * anything" down the screen a letter per line, on top of the header.
   *
   * So the selectors are re-pointed at the flattened tree as the sheet is read,
   * which is the one place that still knows which element hosts it:
   *
   *   :host             -> the host's tag name
   *   :host(S)          -> the host, carrying S's own conditions
   *   :host-context(S)  -> the host under an ancestor matching S, or matching it
   *   X::part(p)        -> [part~="p"] under X, which is where it now sits
   *
   * Two things are deliberately left undone. `::slotted()` goes on being
   * dropped: flattening lands slotted content beside the slot rather than in
   * it, so re-pointing it means rewriting the selector around the host, and no
   * page in any capture so far has a rule that would gain by it. And a part
   * renamed on the way out by `exportparts` is matched under the name it was
   * given inside, because the flattened tree keeps the inner name and nothing
   * else records the mapping.
   *
   * Specificity moves for `:host`, which counts as a pseudo-class (0,1,0) and
   * lands as a type selector (0,0,1). It moves by that same amount for every
   * rule in a component's sheet, so the order a component intends among its own
   * rules survives; only ties against a different sheet can turn over, and in a
   * document whose shadow styles are all hoisted into one sheet those were
   * approximate already.
   */

  // scanSelString returns the index just past the string literal opening at i.
  function scanSelString(s, i) {
    var quote = s.charAt(i);
    for (var j = i + 1; j < s.length; j++) {
      if (s.charAt(j) === '\\') { j++; continue; }
      if (s.charAt(j) === quote) return j + 1;
    }
    return s.length; // unterminated
  }

  // scanSelGroup takes the index of a '(' and returns the index of the ')' that
  // closes it, or -1. Nesting and quoted attribute values both count.
  function scanSelGroup(s, open) {
    var depth = 0;
    for (var i = open; i < s.length; i++) {
      var c = s.charAt(i);
      if (c === '\\') { i++; continue; }
      if (c === '"' || c === "'") { i = scanSelString(s, i) - 1; continue; }
      if (c === '(') depth++;
      else if (c === ')' && --depth === 0) return i;
    }
    return -1;
  }

  function isSelIdentChar(c) {
    return c === '-' || c === '_' || c === '\\' ||
      (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') ||
      (c >= 'A' && c <= 'Z') || c >= '\u0080';
  }

  // startsWith, case-folded: pseudo-class names are ASCII case-insensitive.
  function selAt(s, i, lit) {
    return s.substr(i, lit.length).toLowerCase() === lit;
  }

  /*
   * rewriteHost re-points a shadow sheet's host selectors at the flattened
   * tree. `host` is the tag name of the element the sheet's shadow root hangs
   * off; the empty string means the sheet is a document's, and there is nothing
   * to re-point.
   *
   * The argument of `:host(S)` is wrapped in `:is()` rather than pasted onto
   * the tag name, because a compound selector must lead with its type: the
   * paste spells `:host(:not([multiline]))` correctly and `:host-context(body)`
   * as the tag name "bodyfaceplate-search-input".
   */
  function rewriteHost(sel, host) {
    if (!host || sel.indexOf(':host') < 0) return sel;
    var out = '', i = 0, changed = false;
    while (i < sel.length) {
      var c = sel.charAt(i);
      if (c === '"' || c === "'") {
        var end = scanSelString(sel, i);
        out += sel.slice(i, end);
        i = end;
        continue;
      }
      // An escaped character stands for itself: `.\:host` is a class name.
      if (c === '\\') { out += sel.substr(i, 2); i += 2; continue; }
      if (c === ':' && !(i > 0 && sel.charAt(i - 1) === ':')) {
        if (selAt(sel, i, ':host-context(')) {
          var ctx = scanSelGroup(sel, i + ':host-context'.length);
          if (ctx > 0) {
            var outer = sel.slice(i + ':host-context('.length, ctx);
            // Matching the ancestor, or matching it itself — both are what
            // :host-context() means, and a descendant combinator is only half.
            out += ':is(' + host + ':is(' + outer + '),' + outer + ' ' + host + ')';
            i = ctx + 1;
            changed = true;
            continue;
          }
        } else if (selAt(sel, i, ':host(')) {
          var arg = scanSelGroup(sel, i + ':host'.length);
          if (arg > 0) {
            out += host + ':is(' + sel.slice(i + ':host('.length, arg) + ')';
            i = arg + 1;
            changed = true;
            continue;
          }
        } else if (selAt(sel, i, ':host') && !isSelIdentChar(sel.charAt(i + 5))) {
          out += host;
          i += ':host'.length;
          changed = true;
          continue;
        }
      }
      out += c;
      i++;
    }
    return changed ? out : sel;
  }

  // rewritePart turns `X::part(p)` into `X [part~="p"]`. The part attribute
  // survives flattening on the element itself, so the descendant it now is can
  // be named directly. A name is a plain identifier; anything else is left be.
  var PART_RE = /::part\(\s*([-\w\u00a0-\uffff]+)\s*\)/g;

  function rewritePart(sel) {
    if (sel.indexOf('::part(') < 0) return sel;
    return sel.replace(PART_RE, function (m, name) {
      return ' [part~="' + name + '"]';
    });
  }

  // rewriteScoped is the pair of them, applied to one rule's selector.
  function rewriteScoped(sel, host) {
    return rewritePart(rewriteHost(sel, host));
  }

  // ------------------------------------------------------------------ used CSS

  var PSEUDO_RE = /::?(?:hover|active|focus(?:-visible|-within)?|visited|link|target|checked|disabled|enabled|placeholder|before|after|first-line|first-letter|selection|marker|backdrop|-webkit-[a-z-]+|-moz-[a-z-]+)(?:\([^)]*\))?/gi;

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
   * afterHost reduces a selector to the part of it that can still be looked up.
   *
   * A shadow sheet's rules are tested against the shadow root, and there the
   * host is not a descendant: `:host .label-container` finds nothing, and so
   * would every rule a component writes about its own box. What can be asked is
   * whether the shadow root holds a `.label-container` — the host is the root's
   * own, so its half of the question is already answered yes.
   *
   * Returns '' when the selector says nothing beyond the host, which is a rule
   * that has to be kept without a test.
   */
  function afterHost(part) {
    var last = -1, i = 0;
    while (i < part.length) {
      var c = part.charAt(i);
      if (c === '\\') { i += 2; continue; }
      if (c === '"' || c === "'") { i = scanSelString(part, i); continue; }
      if (c === ':' && !(i > 0 && part.charAt(i - 1) === ':') && selAt(part, i, ':host')) {
        i += ':host'.length;
        if (part.charAt(i) === '-' && selAt(part, i, '-context')) i += '-context'.length;
        if (part.charAt(i) === '(') {
          var end = scanSelGroup(part, i);
          i = end < 0 ? part.length : end + 1;
        }
        last = i;
        continue;
      }
      i++;
    }
    if (last < 0) return part;
    // Everything up to the host compound goes, and so does the combinator that
    // separated it from what follows.
    return part.slice(last).replace(/^[\s>+~]+/, '');
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
      var p = list[i];
      if (p.indexOf(':host') >= 0) p = afterHost(p);
      var part = p.indexOf('::part(') >= 0 ? p.slice(0, p.indexOf('::part(')) : p;
      part = part.replace(PSEUDO_RE, '').trim();
      // A selector that was nothing but a pseudo (":root::before"), or nothing
      // but the host, degrades to an empty part; keep such rules rather than
      // risk dropping layout.
      if (!part) return null;
      parts.push(part);
    }
    if (!parts.length) return null;
    return parts.join(',');
  }

  function selectorMatches(doc, sel) {
    var test = testableSelector(sel);
    if (test === null) return true;
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

  /*
   * ruleText gives a style rule the text it should cross the link as: its own,
   * unless re-pointing its selector at the flattened tree changed it.
   *
   * The declarations are taken from rule.style rather than cut out of cssText,
   * which keeps a selector containing a brace-like character in a quoted
   * attribute value from being split in the wrong place.
   */
  function ruleText(rule, host) {
    var sel = rule.selectorText;
    var next = rewriteScoped(sel, host);
    if (next === sel) return rule.cssText;
    var body = '';
    try { body = rule.style.cssText; } catch (e) { body = ''; }
    return body ? next + '{' + body + '}' : rule.cssText;
  }

  // host is the tag name of the element a shadow root hangs off, or '' for a
  // document's own sheets. See rewriteHost.
  function collectRules(doc, list, out, depth, host) {
    if (!list || depth > 8) return;
    for (var i = 0; i < list.length; i++) {
      var rule = list[i];
      try {
        switch (rule.type) {
          case 1: // style rule
            cssSeen++;
            if (selectorMatches(doc, rule.selectorText)) {
              out.push(ruleText(rule, host));
            } else {
              noteRejected(rule.selectorText);
            }
            break;
          case 4: // media
          case 12: // supports
            var inner = [];
            collectRules(doc, rule.cssRules, inner, depth + 1, host);
            if (inner.length) {
              var cond = rule.type === 4 ? '@media ' + rule.conditionText
                : '@supports ' + rule.conditionText;
              out.push(cond + '{' + inner.join('') + '}');
            }
            break;
          case 7: // keyframes: small, and cheap insurance for CSS animations
            out.push(rule.cssText);
            break;
          case 5: // font-face
            // Substituting from the system is the right trade for a font that
            // carries text and the wrong one for a font that carries pictures.
            // See fontsWithoutSubstitute: only a family the page is drawing
            // private-use codepoints in is kept, and the server then ships the
            // file the way it ships any other url() in a stylesheet.
            var fam = firstFamily(rule.style && rule.style.fontFamily);
            if (fam && fontsWanted[fam]) {
              out.push(rule.cssText);
            } else {
              noteRejected('@font-face ' + (fam || '?'));
            }
            break;
          default:
            if (rule.cssText && rule.cssText.charAt(0) === '@' &&
                rule.cssText.indexOf('@import') !== 0) {
              out.push(rule.cssText);
            }
        }
      } catch (e) { /* cross-origin sheet, skip */ }
    }
  }

  function collectSheets(doc, sheets, out, host) {
    if (!sheets) return;
    for (var i = 0; i < sheets.length; i++) {
      var sheet = sheets[i];
      var rules = null;
      try { rules = sheet.cssRules; } catch (e) { rules = null; } // cross-origin
      if (!rules) {
        // A stylesheet served from another origin — which is where a CDN-backed
        // site keeps all of them — cannot be read through the CSSOM at all. The
        // host fetches the text over the protocol and hands it back through
        // addSheet; substituting it here, in the sheet's own place in the
        // cascade, is what keeps a later rule later.
        var href = null;
        try { href = sheet.href; } catch (e) { href = null; }
        if (href) {
          var sub = recoveredSheets.get(href);
          if (sub) {
            try { rules = sub.cssRules; } catch (e) { rules = null; }
          } else if (!blockedSheets[href]) {
            blockedSheets[href] = 1;
            blockedNew = true;
          }
        }
      }
      if (!rules) continue;
      collectRules(doc, rules, out, 0, host);
    }
  }

  function collectUsedCSS(doc) {
    var out = [];
    // A shadow root's sheets are written against a boundary the mirror does not
    // keep, and its host is the one thing that can re-point them. See
    // rewriteHost.
    var host = doc.host ? localNameOf(doc.host) : '';
    try { collectSheets(doc, doc.styleSheets, out, host); } catch (e) { /* detached */ }
    // Constructed stylesheets are invisible to document.styleSheets, and they
    // are how every Lit-based web component ships its CSS. Without these a
    // component-heavy page arrives with its structure intact and no styling at
    // all, which looks far more broken than a missing rule.
    try { collectSheets(doc, doc.adoptedStyleSheets, out, host); } catch (e) { /* unsupported */ }
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

  function fontsWithoutSubstitute(docs) {
    var want = {};
    for (var d = 0; d < docs.length; d++) {
      var doc = docs[d];
      var root = doc.body || doc.documentElement || doc;
      var walker;
      try {
        walker = doc.createTreeWalker(root, 4 /* SHOW_TEXT */);
      } catch (e) { continue; }
      var nodes = 0, hits = 0, n;
      while (hits < FONT_SCAN_HITS && nodes++ < FONT_SCAN_NODES) {
        try { n = walker.nextNode(); } catch (e) { break; }
        if (!n) break;
        if (!n.nodeValue || !PUA_RE.test(n.nodeValue)) continue;
        hits++;
        var el = n.parentElement;
        if (!el) continue;
        // From the element's own window: these documents include the inlined
        // same-origin iframes, and a style resolved against the wrong one is
        // not this element's style.
        var view = doc.defaultView || globalThis;
        var fam = '';
        try { fam = firstFamily(view.getComputedStyle(el).fontFamily); } catch (e) { fam = ''; }
        if (fam) want[fam] = 1;
      }
    }
    return want;
  }

  function cssDelta() {
    var docs = [document];
    // Shadow roots and same-origin iframe documents both carry their own
    // styles; either kind may hold them in a constructed sheet instead.
    observedDocs.forEach(function (d) {
      if (d !== document && (d.styleSheets || d.adoptedStyleSheets)) docs.push(d);
    });
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
      var rules = collectUsedCSS(doc);
      // A scoped sheet is deduped against itself, not against the document's:
      // the same rule text may be needed in two places and mean two different
      // things, which is the point of scoping it.
      var seen = root ? scopedEmitted.get(root) : emittedCSS;
      if (root && !seen) { seen = new Map(); scopedEmitted.set(root, seen); }
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
    // Where the node renders, which is where it was serialised. A node the host
    // handed to a slot belongs under that slot; asking parentNode instead would
    // put every later mutation back beside the component rather than inside it,
    // and undo the flattening one record at a time.
    var slot = null;
    try { slot = node.assignedSlot; } catch (e) { slot = null; }
    if (slot) return idOf.get(slot) || 0;
    var p = node.parentNode;
    if (!p) return 0;
    if (p.nodeType === 11 && p.host) return idOf.get(p.host) || 0; // shadow root
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
    }
  }

  function onInput(ev) {
    var el = ev.target;
    if (!el || !el.tagName) return;
    var id = idOf.get(el);
    if (id === undefined) return;
    if ((el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && !isSensitive(el)) {
      pendingOps.push([3, id, intern('data-sky-value'), intern(el.value == null ? '' : el.value)]);
      scheduleFlush(false);
    }
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
      return;
    }
    lastScroll.set(id, { x: el.scrollLeft | 0, y: el.scrollTop | 0 });
  }

  function onScroll(ev) {
    var t = ev.target;
    var id = 0;
    var x = 0, y = 0;
    if (t === document || t === document.documentElement || t === document.body || !t.tagName) {
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
    if (scrollTimer) return;
    scrollTimer = setTimeout(function () {
      scrollTimer = null;
      lastScroll.forEach(function (pos, nid) {
        pendingOps.push([10, nid, pos.x, pos.y]);
      });
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
    pendingOps = []; pendingImages = [];
    lastText = new Map();
    // The watch list is rebuilt by the walk below; its old ids may not even be
    // in the new document.
    awaitingUpgrade = new Set();
    upgradePoll = UPGRADE_POLL_MS;

    var rows = [];
    serializeNode(document.documentElement, 0, rows);
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
      images: imgs,
      docHeight: Math.max(
        document.documentElement ? document.documentElement.scrollHeight : 0,
        document.body ? document.body.scrollHeight : 0)
    });
    pendingStrings = [];
  }

  function start() {
    if (started) return;
    started = true;
    observeDocument(document);
    snapshot();
    // Late-loading webfont/CSS work and lazily-attached shadow roots settle
    // within a second or two; a follow-up CSS pass is cheaper than a resnapshot.
    setTimeout(scheduleCSS, 800);
    setTimeout(scheduleCSS, 2500);
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

  // --------------------------------------------------------------- host API

  var api = {
    version: 1,
    start: start,
    snapshot: function () { snapshotDone = false; started = false; start(); return true; },
    flush: function () { scheduleFlush(true); return true; },
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
        r = viewportRect(el);
      }
      return {
        x: r.left, y: r.top, w: r.width, h: r.height,
        cx: r.left + r.width / 2, cy: r.top + r.height / 2,
        tag: el.tagName, editable: isEditable(el),
        href: el.tagName === 'A' ? (el.href || '') : ''
      };
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
      if (el.isContentEditable) {
        el.textContent = value;
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
        cssPending: cssTimer !== null,
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
        out.push([id, node.nodeType, v.slice(0, 32),
          node.nodeType === KIND_ELEMENT ? flagsOf(node) : 0]);
      }
      return { total: ids.length, truncated: ids.length > out.length, nodes: out };
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
    checkpoint: function () {
      for (var i = 0; i < observers.length; i++) {
        var records = observers[i].takeRecords();
        if (records.length) handleMutations(records);
      }
      if (pendingOps.length || pendingImages.length) scheduleFlush(true);
      return { seq: seq, hash: api.docHash() };
    },
    docHash: function () {
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
      var h = 0x811c9dc5;
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

  Object.defineProperty(globalThis, '__skyhook', { value: api, configurable: true });

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }
  globalThis.addEventListener('load', function () {
    scheduleCSS();
    scheduleFlush(false);
  }, { once: true });
})();
