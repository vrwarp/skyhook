/* Skyhook Google Chat extractor.
 *
 * Runs in an isolated world inside a dedicated landside Chat tab. It reads the
 * space list and the visible message log out of the DOM and hands them to the
 * host as plain records; the host turns them into an append-log the client
 * archives locally.
 *
 * Selectors live in a config object rather than in the code because Chat's DOM
 * is minified and changes without notice. Everything degrades to "found
 * nothing" rather than throwing, and the generic mirror remains the fallback
 * path for anything this misses.
 */
(function (config) {
  'use strict';
  var CFG = config || {};

  function qsa(root, sel) {
    if (!sel) return [];
    try { return Array.prototype.slice.call(root.querySelectorAll(sel)); }
    catch (e) { return []; }
  }

  function qs(root, sel) {
    if (!sel) return null;
    try { return root.querySelector(sel); } catch (e) { return null; }
  }

  function attr(el, names) {
    if (!el) return '';
    for (var i = 0; i < names.length; i++) {
      var v = el.getAttribute(names[i]);
      if (v) return v;
    }
    return '';
  }

  function text(el) {
    if (!el) return '';
    return (el.textContent || '').replace(/\s+/g, ' ').trim();
  }

  /*
   * label reads a person's or a space's name off an element.
   *
   * The attributes come first because in Chat the name is usually only there.
   * A roster entry's `[data-name]` is the avatar, whose text is empty, and the
   * elements around it that do carry text carry "Active", "Unread" and
   * "1 Notification" in the same minified, role-less spans as the name — so
   * the text of the entry is not a name and no selector over it is. Taking the
   * element's text when no attribute has one keeps this working on a DOM that
   * writes the name where it can be read.
   */
  function label(el, names) {
    if (!el) return '';
    var fromAttr = attr(el, names || []);
    if (fromAttr) return fromAttr.replace(/\s+/g, ' ').trim();
    return text(el);
  }


  // stableID derives an identifier for a message when the DOM does not carry
  // one, so re-scans dedupe instead of duplicating the whole log.
  function stableID(space, author, body, ts) {
    var s = space + '|' + author + '|' + body + '|' + (ts || '');
    var h = 0x811c9dc5;
    for (var i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i) & 0xff;
      h = (h * 16777619) >>> 0;
    }
    return 'm' + ('0000000' + h.toString(16)).slice(-8);
  }

  function docs() {
    // Chat renders inside nested same-origin iframes in the Gmail shell.
    var out = [document];
    qsa(document, 'iframe').forEach(function (f) {
      try { if (f.contentDocument) out.push(f.contentDocument); } catch (e) { /* cross-origin */ }
    });
    return out;
  }

  function scanSpaces() {
    var seen = {};
    var out = [];
    docs().forEach(function (d) {
      qsa(d, CFG.spaceItem).forEach(function (el) {
        var name = label(qs(el, CFG.spaceName) || el, CFG.spaceNameAttrs);
        if (!name) return;
        var id = attr(el, CFG.spaceIdAttrs || ['data-group-id', 'data-member-id', 'id']) || name;
        if (seen[id]) return;
        seen[id] = 1;
        var unreadEl = qs(el, CFG.spaceUnread);
        var unread = 0;
        if (unreadEl) {
          var n = parseInt(text(unreadEl).replace(/[^0-9]/g, ''), 10);
          unread = isNaN(n) ? (text(unreadEl) ? 1 : 0) : n;
        } else if (CFG.spaceUnreadClass && el.className &&
                   String(el.className).indexOf(CFG.spaceUnreadClass) >= 0) {
          unread = 1;
        }
        out.push({ id: String(id), name: name, unread: unread });
      });
    });
    return out;
  }

  function currentSpace() {
    for (var i = 0; i < docs().length; i++) {
      var el = qs(docs()[i], CFG.activeSpace);
      if (el) {
        return {
          id: attr(el, CFG.spaceIdAttrs || ['data-group-id', 'id']) || text(el),
          name: label(qs(el, CFG.spaceName) || el, CFG.spaceNameAttrs)
        };
      }
    }
    return { id: '', name: '' };
  }

  function scanMessages() {
    var space = currentSpace();
    var out = [];
    docs().forEach(function (d) {
      qsa(d, CFG.messageItem).forEach(function (el) {
        var body = text(qs(el, CFG.messageText) || el);
        if (!body) return;
        var author = label(qs(el, CFG.messageAuthor), CFG.messageAuthorAttrs);
        var tsEl = qs(el, CFG.messageTime);
        var tsRaw = tsEl ? (attr(tsEl, ['data-absolute-timestamp', 'datetime', 'title']) || text(tsEl)) : '';
        var ts = 0;
        if (tsRaw) {
          var parsed = Date.parse(tsRaw);
          if (!isNaN(parsed)) ts = parsed;
          else if (/^\d{10,}$/.test(tsRaw)) ts = parseInt(tsRaw, 10);
        }
        var id = attr(el, CFG.messageIdAttrs || ['data-message-id', 'data-id', 'id']) ||
          stableID(space.id, author, body, tsRaw);
        out.push({
          id: String(id), space: space.id || space.name, author: author,
          text: body, ts: ts
        });
      });
    });
    // Keep only the tail: a long backlog is the archive's job, not the
    // scanner's, and re-sending history every poll would defeat the point.
    if (out.length > (CFG.maxMessages || 80)) {
      out = out.slice(out.length - (CFG.maxMessages || 80));
    }
    return out;
  }

  function openSpace(spaceID) {
    var found = null;
    docs().forEach(function (d) {
      if (found) return;
      qsa(d, CFG.spaceItem).forEach(function (el) {
        if (found) return;
        var id = attr(el, CFG.spaceIdAttrs || ['data-group-id', 'data-member-id', 'id']) || text(el);
        if (String(id) === String(spaceID) || text(el) === String(spaceID)) found = el;
      });
    });
    if (!found) return false;
    found.click();
    return true;
  }

  function composer() {
    for (var i = 0; i < docs().length; i++) {
      var el = qs(docs()[i], CFG.composer);
      if (el) return el;
    }
    return null;
  }

  var api = {
    version: 1,
    scan: function () {
      return {
        spaces: scanSpaces(),
        messages: scanMessages(),
        space: currentSpace(),
        ready: !!composer() || scanSpaces().length > 0,
        url: location.href
      };
    },
    open: openSpace,
    // focusComposer returns the composer's position so the host can type into
    // it with real key events rather than assigning to a property, which Chat
    // ignores.
    focusComposer: function () {
      var el = composer();
      if (!el) return null;
      el.focus();
      var r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, ok: document.activeElement === el };
    }
  };
  Object.defineProperty(globalThis, '__skyhookChat', { value: api, configurable: true });
  return true;
})(SKYHOOK_CHAT_CONFIG);
