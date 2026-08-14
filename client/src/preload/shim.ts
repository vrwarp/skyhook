/**
 * The client shim: the only script that runs in a mirror tab.
 *
 * It runs in Electron's isolated preload world, so the page's content security
 * policy (default-src 'none'; script-src 'none') can forbid every script in the
 * document itself. Page JavaScript therefore cannot exist plane-side, which is
 * the security property the whole design rests on.
 *
 * Responsibilities: apply frames through the patcher, echo input locally,
 * serialise semantic events, report scroll telemetry, and paint blurhash
 * placeholders until real images arrive.
 */
import { contextBridge, ipcRenderer } from 'electron';

import { decodeBlurhashToCSS } from '../shared/blurhash.js';
import {
  ImageMeta, InputKind, Mutation, MutationOp, OpCode, Snapshot,
} from '../shared/protocol.js';
import { EchoEngine, modifierMask, valueOf } from './echo.js';
import { Patcher } from './patcher.js';

/** Messages the shim receives from the main process. */
interface ShimFrame {
  kind: 'snapshot' | 'mutation' | 'imageMeta' | 'imageData' | 'reset' | 'offline';
  tab: number;
  seq?: number;
  cause?: number;
  snapshot?: Snapshot;
  mutation?: Mutation;
  meta?: ImageMeta;
  hash?: string;
  offline?: boolean;
}

let tabId = 0;
let inputSeq = 0;
let lastAppliedSeq = 0;

const doc = document;
const pending = new Map<string, HTMLImageElement[]>();

const patcher: Patcher = new Patcher(doc, {
  isOwned: (node: Node): boolean => echo.isOwned(node),
  onDeferred: () => undefined,
  onImage: (el, meta, hash) => applyImage(el, meta, hash),
  onFocus: (node) => {
    // Landside focus moved. Following it locally keeps keyboard interaction
    // aligned, but never while the user is typing into something else.
    if (echo.ownedId) return;
    (node as HTMLElement | null)?.focus?.();
  },
  onScroll: (node, x, y) => {
    if (!node) {
      window.scrollTo(x, y);
      return;
    }
    const el = node as HTMLElement;
    el.scrollLeft = x;
    el.scrollTop = y;
  },
  onApplied: (seq) => {
    lastAppliedSeq = seq;
    ipcRenderer.send('skyhook:applied', { tab: tabId, seq, hash: patcher.docHash() });
  },
});

const echo: EchoEngine = new EchoEngine(doc, {
  idOf: (node: Node | null): number => patcher.idOf(node),
  sendText: (node, text) => sendInput({ kind: InputKind.Text, node, text }),
  sendKey: (node, key, modifiers, repeat) =>
    sendInput({ kind: InputKind.Key, node, key, modifiers, repeat }),
  sendValue: (node, value, start, end) =>
    sendInput({ kind: InputKind.SetValue, node, text: value, start, end }),
  sendFocus: (node, focused) =>
    sendInput({ kind: focused ? InputKind.Focus : InputKind.Blur, node }),
  onChatSend: (node, text) => placeGhost(node, text),
});

function sendInput(ev: {
  kind: string;
  node?: number;
  text?: string;
  key?: string;
  modifiers?: number;
  button?: number;
  x?: number;
  y?: number;
  start?: number;
  end?: number;
  url?: string;
  repeat?: number;
}): void {
  inputSeq += 1;
  ipcRenderer.send('skyhook:input', {
    tab: tabId,
    seq: inputSeq,
    ts: Date.now(),
    expectSeq: lastAppliedSeq,
    ...ev,
  });
}

// ------------------------------------------------------------------ input

doc.addEventListener('click', (ev) => {
  const target = ev.target as HTMLElement | null;
  if (!target) return;
  const anchor = target.closest?.('a[href]') as HTMLAnchorElement | null;
  const node = patcher.idOf(anchor ?? target);
  if (!node) return;
  // The mirror never navigates itself: every click is a semantic event the
  // server replays into the real page.
  ev.preventDefault();
  sendInput({
    kind: InputKind.Click,
    node,
    modifiers: modifierMask(ev),
    button: ev.button,
    url: anchor?.getAttribute('href') ?? undefined,
  });
}, true);

doc.addEventListener('dblclick', (ev) => {
  const node = patcher.idOf(ev.target as Node);
  if (node) sendInput({ kind: InputKind.DblClick, node, modifiers: modifierMask(ev) });
}, true);

doc.addEventListener('contextmenu', (ev) => {
  const node = patcher.idOf(ev.target as Node);
  if (node) sendInput({ kind: InputKind.Context, node, modifiers: modifierMask(ev) });
}, true);

doc.addEventListener('submit', (ev) => {
  const form = ev.target as HTMLFormElement;
  ev.preventDefault();
  const node = patcher.idOf(form);
  if (!node) return;
  const fields: Record<string, string> = {};
  for (const el of Array.from(form.elements)) {
    const input = el as HTMLInputElement;
    if (input.name && typeof input.value === 'string') fields[input.name] = input.value;
  }
  // Form fills ship once, on submit, not per keystroke.
  ipcRenderer.send('skyhook:input', {
    tab: tabId, seq: ++inputSeq, ts: Date.now(),
    kind: InputKind.Submit, node, fields,
  });
}, true);

doc.addEventListener('focusin', (ev) => echo.focus(ev.target), true);
doc.addEventListener('focusout', () => {
  echo.blur((op) => applyDeferred(op));
}, true);
doc.addEventListener('input', (ev) => echo.input(ev as InputEvent), true);
doc.addEventListener('keydown', (ev) => {
  if (echo.key(ev as KeyboardEvent)) ev.preventDefault();
}, true);

// Scroll is entirely local — the whole document is here — but the server wants
// to know where we are so it can prioritise images and drive infinite lists.
let scrollTimer: ReturnType<typeof setTimeout> | null = null;
window.addEventListener('scroll', () => {
  if (scrollTimer) return;
  scrollTimer = setTimeout(() => {
    scrollTimer = null;
    ipcRenderer.send('skyhook:scroll', {
      tab: tabId,
      x: window.scrollX,
      y: window.scrollY,
      h: window.innerHeight,
      docH: doc.documentElement.scrollHeight,
    });
  }, 250);
}, { passive: true });

// ------------------------------------------------------------------ images

function applyImage(el: HTMLImageElement, meta: ImageMeta | undefined, hash: string): void {
  if (!hash) return;
  const cached = `skyhook://img/${hash}`;
  if (meta?.blur && !el.dataset.skyhookBlur) {
    el.dataset.skyhookBlur = '1';
    // Paint the blurhash immediately: a page of grey boxes is what a mirror
    // feels like without this, and the placeholder costs about 30 bytes.
    el.style.backgroundImage = decodeBlurhashToCSS(meta.blur, 8, 8);
    el.style.backgroundSize = 'cover';
  }
  if (el.getAttribute('src') !== cached) el.setAttribute('src', cached);
  const list = pending.get(hash) ?? [];
  if (!list.includes(el)) list.push(el);
  pending.set(hash, list);
  ipcRenderer.send('skyhook:want-image', { tab: tabId, hashes: [hash] });
}

function onImageBytes(hash: string): void {
  const list = pending.get(hash);
  if (!list) return;
  pending.delete(hash);
  for (const el of list) {
    // The local store now holds the bytes; forcing a re-fetch of the same URL
    // makes the protocol handler serve them.
    const src = el.getAttribute('src') ?? '';
    el.setAttribute('src', '');
    el.setAttribute('src', src || `skyhook://img/${hash}`);
    el.style.backgroundImage = '';
  }
}

// --------------------------------------------------------------- optimistic

function placeGhost(composer: Node, text: string): boolean {
  // Find the message list this composer belongs to and append a pending copy.
  // Getting this wrong is harmless: the ghost is removed when the server's
  // authoritative mutation arrives.
  const root = (composer as HTMLElement).closest?.('[data-skyhook-root], body');
  const list = root?.querySelector('[role="list"], ul, ol');
  if (!list) return false;
  const ghost = doc.createElement('div');
  ghost.className = 'skyhook-ghost';
  ghost.setAttribute('data-skyhook-ghost', text);
  ghost.textContent = text;
  ghost.style.opacity = '0.55';
  list.appendChild(ghost);
  return true;
}

function retireGhosts(): void {
  const body = doc.body.textContent ?? '';
  for (const el of Array.from(doc.querySelectorAll('[data-skyhook-ghost]'))) {
    const text = el.getAttribute('data-skyhook-ghost') ?? '';
    // The ghost stands in for a message that has not been confirmed. Once the
    // real one arrives the text appears twice, and the ghost can go.
    if (text && body.split(text).length > 2) el.remove();
  }
}

function applyDeferred(op: MutationOp): void {
  patcher.applyMutation({ strings: [], ops: [op], docHash: 0, flush: false }, lastAppliedSeq);
}

// ------------------------------------------------------------------ frames

ipcRenderer.on('skyhook:frame', (_evt, frame: ShimFrame) => {
  tabId = frame.tab || tabId;
  switch (frame.kind) {
    case 'snapshot':
      if (frame.snapshot) {
        echo.release();
        patcher.applySnapshot(frame.snapshot);
        window.scrollTo(frame.snapshot.scrollX, frame.snapshot.scrollY);
      }
      break;
    case 'mutation':
      if (frame.mutation) {
        const ops = frame.mutation.ops.filter((op: MutationOp) => {
          if (op.op === OpCode.Attr) reconcileAttr(op);
          return !echo.defer(op, (id: number) => patcher.nodeFor(id));
        });
        patcher.applyMutation({ ...frame.mutation, ops }, frame.seq ?? 0);
        retireGhosts();
      }
      break;
    case 'imageMeta':
      if (frame.meta) patcher.setImageMeta(frame.meta);
      break;
    case 'imageData':
      if (frame.hash) onImageBytes(frame.hash);
      break;
    case 'offline':
      doc.documentElement.classList.toggle('skyhook-offline', frame.offline === true);
      break;
    case 'reset':
      echo.release();
      break;
    default:
      break;
  }
});

function reconcileAttr(op: MutationOp): void {
  if (!echo.ownedId || op.node !== echo.ownedId) return;
  const node = patcher.nodeFor(op.node) as HTMLElement | undefined;
  if (!node) return;
  const name = op.ref;
  void name;
  // The server ships the live field value as data-sky-value; when it differs
  // from what we rendered locally, server truth wins and the caret is remapped.
  const server = op.str || undefined;
  if (server !== undefined && server !== valueOf(node)) echo.reconcile(op.node, server);
}

// A tiny surface for the chrome UI (find-in-page count, hash for diagnostics).
contextBridge.exposeInMainWorld('skyhook', {
  stats: () => ({ nodes: patcher.size, seq: lastAppliedSeq, hash: patcher.docHash() }),
  scrollToTop: () => window.scrollTo(0, 0),
});

ipcRenderer.send('skyhook:shim-ready', {});
