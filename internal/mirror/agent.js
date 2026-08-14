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

  var KIND_ELEMENT = 1, KIND_TEXT = 3, KIND_DOCTYPE = 10;
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
  var SENSITIVE_AUTOCOMPLETE = /(^|\s)(current-password|new-password|one-time-code|cc-number|cc-csc)(\s|$)/i;

  var nextId = 1;
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
  var cssOrder = [];
  var lastText = new Map();     // id -> last text we reported
  var lastScroll = new Map();
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
    if (isEditable(el)) flags |= FLAG_EDITABLE;
    if (CANVAS_TAGS[tag]) flags |= FLAG_CANVAS;
    if (isScrollable(el)) flags |= FLAG_SCROLL;
    if (el.shadowRoot) flags |= FLAG_SHADOW;
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

    if (node.shadowRoot) {
      // Flattening the shadow tree in place keeps the client a plain patcher.
      var sk = node.shadowRoot.childNodes;
      for (var s = 0; s < sk.length; s++) n += serializeNode(sk[s], id2, rows);
      observeDocument(node.shadowRoot);
    }
    if (tag === 'IFRAME') {
      hookFrame(node);
      var idoc = null;
      try { idoc = node.contentDocument; } catch (e) { idoc = null; }
      if (idoc && idoc.documentElement) {
        n += serializeNode(idoc.documentElement, id2, rows);
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
      try { if (!el.contentDocument) return; } catch (e) { return; }
      if (reframeTimer) return;
      reframeTimer = setTimeout(function () {
        reframeTimer = null;
        api.snapshot();
      }, REFRAME_DEBOUNCE_MS);
    }, { passive: true });
  }

  function serializeSubtree(node) {
    var rows = [];
    var parent = node.parentNode ? (idOf.get(node.parentNode) || 0) : 0;
    serializeNode(node, parent, rows);
    return rows;
  }

  // ------------------------------------------------------------------ used CSS

  var PSEUDO_RE = /::?(?:hover|active|focus(?:-visible|-within)?|visited|link|target|checked|disabled|enabled|placeholder|before|after|first-line|first-letter|selection|marker|backdrop|-webkit-[a-z-]+|-moz-[a-z-]+)(?:\([^)]*\))?/gi;

  function testableSelector(sel) {
    var s = sel.replace(PSEUDO_RE, '');
    s = s.replace(/\s*,\s*/g, ',');
    // A selector that was nothing but a pseudo (":root::before") degrades to
    // an empty part; keep such rules rather than risk dropping layout.
    var parts = s.split(',').filter(function (p) { return p.trim().length > 0; });
    if (!parts.length) return null;
    return parts.join(',');
  }

  function selectorMatches(doc, sel) {
    var test = testableSelector(sel);
    if (test === null) return true;
    try { return doc.querySelector(test) !== null; } catch (e) { return true; }
  }

  function collectRules(doc, list, out, depth) {
    if (!list || depth > 8) return;
    for (var i = 0; i < list.length; i++) {
      var rule = list[i];
      try {
        switch (rule.type) {
          case 1: // style rule
            if (selectorMatches(doc, rule.selectorText)) out.push(rule.cssText);
            break;
          case 4: // media
          case 12: // supports
            var inner = [];
            collectRules(doc, rule.cssRules, inner, depth + 1);
            if (inner.length) {
              var cond = rule.type === 4 ? '@media ' + rule.conditionText
                : '@supports ' + rule.conditionText;
              out.push(cond + '{' + inner.join('') + '}');
            }
            break;
          case 7: // keyframes: small, and cheap insurance for CSS animations
            out.push(rule.cssText);
            break;
          case 5: // font-face: fonts are blocked landside; system substitution
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

  function collectSheets(doc, sheets, out) {
    if (!sheets) return;
    for (var i = 0; i < sheets.length; i++) {
      var rules = null;
      try { rules = sheets[i].cssRules; } catch (e) { rules = null; } // cross-origin
      if (!rules) continue;
      collectRules(doc, rules, out, 0);
    }
  }

  function collectUsedCSS(doc) {
    var out = [];
    try { collectSheets(doc, doc.styleSheets, out); } catch (e) { /* detached */ }
    // Constructed stylesheets are invisible to document.styleSheets, and they
    // are how every Lit-based web component ships its CSS. Without these a
    // component-heavy page arrives with its structure intact and no styling at
    // all, which looks far more broken than a missing rule.
    try { collectSheets(doc, doc.adoptedStyleSheets, out); } catch (e) { /* unsupported */ }
    return out;
  }

  function cssDelta() {
    var docs = [document];
    // Shadow roots and same-origin iframe documents both carry their own
    // styles; either kind may hold them in a constructed sheet instead.
    observedDocs.forEach(function (d) {
      if (d !== document && (d.styleSheets || d.adoptedStyleSheets)) docs.push(d);
    });
    var adds = [];
    for (var d = 0; d < docs.length; d++) {
      var rules = collectUsedCSS(docs[d]);
      for (var i = 0; i < rules.length; i++) {
        var text = rules[i];
        if (emittedCSS.has(text)) continue;
        emittedCSS.set(text, cssOrder.length);
        cssOrder.push(text);
        adds.push(text);
      }
    }
    return adds;
  }

  function scheduleCSS() {
    if (cssTimer) return;
    cssTimer = setTimeout(function () {
      cssTimer = null;
      var adds = cssDelta();
      if (adds.length) {
        pendingOps.push([7, adds]);
        scheduleFlush(false);
      }
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
    var css = cssDelta();
    var imgs = pendingImages; pendingImages = [];
    snapshotDone = true;
    seq = 0;
    send({
      t: 'snap', seq: 0, strings: strings.slice(), nodes: rows, css: css,
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
      var r = el.getBoundingClientRect();
      // Scroll a target into view before reporting: the host clicks by
      // coordinate, and an offscreen element would land on the wrong node.
      if (r.bottom < 0 || r.top > (globalThis.innerHeight || 0) ||
          r.right < 0 || r.left > (globalThis.innerWidth || 0)) {
        try { el.scrollIntoView({ block: 'center', inline: 'center' }); } catch (e) { /* older engines */ }
        r = el.getBoundingClientRect();
      }
      return {
        x: r.left, y: r.top, w: r.width, h: r.height,
        cx: r.left + r.width / 2, cy: r.top + r.height / 2,
        tag: el.tagName, editable: isEditable(el),
        href: el.tagName === 'A' ? (el.href || '') : ''
      };
    },
    focus: function (id) {
      var n = byId.get(id);
      if (!n) return false;
      var el = n.nodeType === KIND_TEXT ? n.parentElement : n;
      try { el.focus({ preventScroll: false }); } catch (e) { return false; }
      return document.activeElement === el;
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
    stats: function () {
      return { nodes: byId.size, strings: strings.length, css: cssOrder.length, seq: seq };
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
