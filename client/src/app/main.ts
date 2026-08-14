/**
 * The app shell: tab strip, URL bar, link-health HUD, chat panel, and the
 * routing between the network worker and each tab's sandboxed mirror frame.
 *
 * No framework. The whole client is a patcher and an input serialiser; a
 * runtime would be more bytes than the mirror protocol it exists to carry.
 */
import { MirrorHost } from '../mirror/host.js';
import { Store, type Pairing } from '../store/store.js';
import type {
  AdapterRecord, ImageMeta, Mutation, Snapshot, Stats, TabState, Welcome,
} from '../shared/protocol.js';

const store = new Store();
const hosts = new Map<number, MirrorHost>();
const tabs = new Map<number, TabView>();
const archive: AdapterRecord[] = [];
const spaces = new Map<string, { name: string; unread: number }>();
const speculations = new Map<string, Snapshot>();
let worker: Worker | null = null;
let active = 0;
let currentSpace = '';

interface TabView {
  id: number;
  url: string;
  title: string;
  loading: boolean;
  canBack: boolean;
  canForward: boolean;
}

const el = {
  strip: byId<HTMLDivElement>('tabstrip'),
  urlbar: byId<HTMLInputElement>('urlbar'),
  back: byId<HTMLButtonElement>('back'),
  forward: byId<HTMLButtonElement>('forward'),
  reload: byId<HTMLButtonElement>('reload'),
  bookmark: byId<HTMLButtonElement>('bookmark'),
  chat: byId<HTMLButtonElement>('chat'),
  frames: byId<HTMLDivElement>('frames'),
  panel: byId<HTMLElement>('panel'),
  panelBody: byId<HTMLDivElement>('panel-body'),
  panelClose: byId<HTMLButtonElement>('panel-close'),
  hudState: byId<HTMLSpanElement>('hud-state'),
  hudRtt: byId<HTMLSpanElement>('hud-rtt'),
  hudQueue: byId<HTMLSpanElement>('hud-queue'),
  hudBytes: byId<HTMLSpanElement>('hud-bytes'),
  pairing: byId<HTMLDialogElement>('pairing'),
  pairingForm: byId<HTMLFormElement>('pairing-form'),
  pairingJSON: byId<HTMLTextAreaElement>('pairing-json'),
  pairingError: byId<HTMLParagraphElement>('pairing-error'),
};

function byId<T extends HTMLElement>(id: string): T {
  const node = document.getElementById(id);
  if (!node) throw new Error(`missing element #${id}`);
  return node as T;
}

// ------------------------------------------------------------------- startup

async function main(): Promise<void> {
  await store.requestPersistence();
  registerServiceWorker();
  startWorker();

  const pairing = await pairingFromURL() ?? await store.readPairing();
  if (!pairing) {
    el.pairing.showModal();
    return;
  }
  configure(pairing);
}

function registerServiceWorker(): void {
  if (!('serviceWorker' in navigator)) return;
  void navigator.serviceWorker.register('/sw.js', { type: 'module', scope: '/' })
    .catch((err: unknown) => log(`service worker registration failed: ${String(err)}`));
}

/**
 * A one-time pairing link (`#token=...&host=...`) is how the server hands over
 * its credential. The fragment never reaches a server, and it is stripped from
 * the address bar as soon as it is stored.
 */
async function pairingFromURL(): Promise<Pairing | undefined> {
  const hash = location.hash.replace(/^#/, '');
  if (!hash) return undefined;
  const params = new URLSearchParams(hash);
  const token = params.get('token');
  if (!token) return undefined;
  const pairing: Pairing = {
    host: params.get('host') || location.hostname,
    port: Number(params.get('port') || 4433),
    path: params.get('path') || '/skyhook',
    token,
    certSha256: params.get('cert') || undefined,
    fallbackUrl: params.get('fallback') || undefined,
  };
  await store.writePairing(pairing);
  history.replaceState(null, '', location.pathname + location.search);
  return pairing;
}

function configure(pairing: Pairing): void {
  const url = `https://${pairing.host}:${pairing.port}${pairing.path}`;
  send('configure', {
    pairing: {
      url,
      fallbackUrl: pairing.fallbackUrl,
      certHash: pairing.certSha256,
      token: pairing.token,
      // WebTransport requires a secure origin. Served over plain HTTP — which
      // only happens on localhost during development — go straight to the
      // fallback instead of waiting out a handshake that cannot succeed.
      preferFallback: location.protocol !== 'https:',
    },
    viewport: viewport(),
  });
}

function startWorker(): void {
  worker = new Worker('/net.worker.js', { type: 'module', name: 'skyhook-net' });
  worker.addEventListener('message', (event: MessageEvent) => {
    const msg = event.data as { kind: string; args: Record<string, unknown> };
    handle(msg.kind, msg.args);
  });
}

function send(name: string, args: Record<string, unknown>): void {
  worker?.postMessage({ name, args });
}

function viewport(): { w: number; h: number; dpr: number; mobile: boolean } {
  const rect = el.frames.getBoundingClientRect();
  return {
    w: Math.max(320, Math.round(rect.width)),
    h: Math.max(320, Math.round(rect.height)),
    dpr: window.devicePixelRatio || 1,
    mobile: false,
  };
}

// --------------------------------------------------------- worker -> app shell

function handle(kind: string, args: Record<string, unknown>): void {
  switch (kind) {
    case 'welcome': {
      const w = args as unknown as Welcome;
      void store.writeSessionId(w.sessionId);
      for (const t of w.tabs ?? []) {
        upsertTab({
          id: t.tab, url: t.url, title: t.title, loading: t.loading,
          canBack: false, canForward: false,
        });
        void hostFor(t.tab);
        if (t.active) active = t.tab;
      }
      renderTabs();
      break;
    }
    case 'snapshot':
      void applySnapshot(Number(args.tab), args.snapshot as Snapshot);
      break;
    case 'speculative': {
      const snap = args.snapshot as Snapshot;
      if (snap?.url) {
        speculations.set(snap.url, snap);
        // Bounded: speculation must never crowd out real state.
        if (speculations.size > 8) {
          const oldest = speculations.keys().next().value;
          if (oldest) speculations.delete(oldest);
        }
      }
      break;
    }
    case 'mutation':
      void applyMutation(Number(args.tab), args.mutation as Mutation, Number(args.seq));
      break;
    case 'tabState': {
      const id = Number(args.tab);
      const st = args.state as TabState;
      if (st.closed) {
        closeTabLocally(id);
        break;
      }
      const known = tabs.get(id);
      const tab = known ?? {
        id, url: '', title: '', loading: false, canBack: false, canForward: false,
      };
      // A tab we have never seen needs its frame now, not when its first
      // snapshot happens to arrive: a tab opened onto about:blank has nothing
      // to snapshot until it navigates somewhere.
      if (!known) void hostFor(id);
      tab.url = st.url || tab.url;
      tab.title = st.title || tab.title;
      tab.loading = st.loading;
      tab.canBack = st.canBack;
      tab.canForward = st.canForward;
      upsertTab(tab);
      if (!active) active = id;
      renderTabs();
      break;
    }
    case 'imageMeta':
      void hostFor(Number(args.tab)).then((h) => h.setImageMeta(args.meta as ImageMeta));
      break;
    case 'imageData':
      void hostFor(Number(args.tab)).then((h) => h.imageArrived(String(args.hash)));
      break;
    case 'adapter': {
      const records = (args.records ?? []) as AdapterRecord[];
      void store.appendArchive(records);
      for (const r of records) ingestRecord(r, true);
      if (!el.panel.hidden) renderChat();
      break;
    }
    case 'status':
      renderStatus(args.online === true, String(args.kind ?? ''), args.reason as string | undefined);
      for (const h of hosts.values()) h.setOffline(args.online !== true);
      break;
    case 'stats':
      renderStats(args as unknown as Stats & { rttMs?: number });
      break;
    case 'log':
      log(String(args.message ?? ''));
      break;
    default:
      break;
  }
}

async function applySnapshot(tab: number, snap: Snapshot): Promise<void> {
  const host = await hostFor(tab);
  host.applySnapshot(snap);
}

async function applyMutation(tab: number, m: Mutation, seq: number): Promise<void> {
  const host = await hostFor(tab);
  host.applyMutation(m, seq);
}

async function hostFor(tab: number): Promise<MirrorHost> {
  const existing = hosts.get(tab);
  if (existing) {
    await existing.whenReady();
    return existing;
  }
  const host = new MirrorHost(tab, {
    input: (t, ev) => send('input', { ...ev, tab: t }),
    scroll: (t, ev) => send('scroll', { ...ev, tab: t }),
    applied: (t, seq, hash) => send('ack', { tab: t, seq, hash }),
    wantImages: (t, hashes) => send('wantImage', { tab: t, hashes }),
  });
  hosts.set(tab, host);
  el.frames.appendChild(host.frame);
  if (!active) active = tab;
  layout();
  await host.whenReady();
  return host;
}

// ------------------------------------------------------------------ tab strip

function upsertTab(tab: TabView): void {
  tabs.set(tab.id, tab);
}

function renderTabs(): void {
  el.strip.textContent = '';
  for (const tab of tabs.values()) {
    const node = document.createElement('div');
    node.className = `tab${tab.id === active ? ' active' : ''}${tab.loading ? ' loading' : ''}`;
    node.setAttribute('role', 'tab');

    const title = document.createElement('span');
    title.className = 'title';
    title.textContent = tab.title || hostOf(tab.url) || 'New tab';
    node.appendChild(title);

    const close = document.createElement('span');
    close.className = 'close';
    close.textContent = '×';
    close.addEventListener('click', (ev) => {
      ev.stopPropagation();
      send('closeTab', { tab: tab.id });
      closeTabLocally(tab.id);
    });
    node.appendChild(close);

    node.addEventListener('click', () => {
      active = tab.id;
      renderTabs();
      layout();
      syncToolbar();
    });
    el.strip.appendChild(node);
  }
  const add = document.createElement('button');
  add.id = 'newtab';
  add.textContent = '+';
  add.title = 'New tab';
  add.addEventListener('click', () => send('openTab', { url: '' }));
  el.strip.appendChild(add);
  syncToolbar();
}

function closeTabLocally(id: number): void {
  hosts.get(id)?.destroy();
  hosts.delete(id);
  tabs.delete(id);
  if (active === id) {
    const next = tabs.keys().next().value;
    active = typeof next === 'number' ? next : 0;
  }
  renderTabs();
  layout();
}

function syncToolbar(): void {
  const tab = tabs.get(active);
  if (document.activeElement !== el.urlbar) el.urlbar.value = tab?.url ?? '';
  el.back.disabled = !tab?.canBack;
  el.forward.disabled = !tab?.canForward;
}

function layout(): void {
  for (const [id, host] of hosts) {
    host.frame.style.display = id === active ? 'block' : 'none';
  }
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return '';
  }
}

// -------------------------------------------------------------------- toolbar

el.urlbar.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter') return;
  const url = el.urlbar.value.trim();
  if (!url) return;
  if (!active) {
    send('openTab', { url });
    return;
  }
  const spec = speculations.get(url);
  if (spec) {
    // The speculation is already here: paint it now and let the real
    // navigation reconcile. This is the zero-round-trip link follow.
    speculations.delete(url);
    void applySnapshot(active, spec);
  }
  send('navigate', { tab: active, url });
});

el.back.addEventListener('click', () => send('navigate', { tab: active, action: 'back' }));
el.forward.addEventListener('click', () => send('navigate', { tab: active, action: 'forward' }));
el.reload.addEventListener('click', () => send('navigate', { tab: active, action: 'reload' }));
el.bookmark.addEventListener('click', () => {
  const tab = tabs.get(active);
  if (!tab) return;
  void store.readBookmarks().then((marks) => {
    marks.push({ title: tab.title || tab.url, url: tab.url });
    return store.writeBookmarks(marks);
  });
});

// ------------------------------------------------------------------- the HUD

function renderStats(s: Partial<Stats> & { rttMs?: number }): void {
  const rttMs = s.rttMs || Math.round((s.rttMicros ?? 0) / 1000);
  el.hudRtt.textContent = rttMs ? `${rttMs} ms` : '--';
  el.hudQueue.textContent = `q${s.queueDepth ?? 0}`;
  const bytes = (s.bytesRecv ?? 0) + (s.bytesSent ?? 0);
  el.hudBytes.textContent = bytes > 1024 * 1024
    ? `${(bytes / 1024 / 1024).toFixed(1)} MB`
    : `${Math.round(bytes / 1024)} KB`;
  // A deep queue on this link means the user is about to feel it; say so
  // before they have to guess.
  if ((s.queueDepth ?? 0) > 8 || rttMs > 2500) {
    el.hudState.className = 'degraded';
    el.hudState.textContent = 'slow';
  }
}

function renderStatus(online: boolean, kind: string, reason?: string): void {
  el.hudState.className = online ? 'online' : 'offline';
  el.hudState.textContent = online ? kind.replace('web', '') : 'offline';
  el.hudState.title = reason ?? '';
}

function log(message: string): void {
  console.warn('[skyhook]', message);
}

// -------------------------------------------------------------- the chat panel

el.chat.addEventListener('click', () => {
  el.panel.hidden = !el.panel.hidden;
  if (!el.panel.hidden) void openChat();
});
el.panelClose.addEventListener('click', () => {
  el.panel.hidden = true;
});

async function openChat(): Promise<void> {
  // Cold open comes from the local archive, not from the network: the
  // difference between a chat that opens in 300 ms and one that opens in 8 s.
  for (const r of await store.readArchive()) ingestRecord(r, false);
  renderChat();
  send('adapter', { adapter: 'googlechat', cmd: 'sync', since: lastSeq() });
}

function lastSeq(): number {
  let max = 0;
  for (const r of archive) max = Math.max(max, r.seq);
  return max;
}

function ingestRecord(r: AdapterRecord, live: boolean): void {
  if (r.kind === 'space') {
    spaces.set(r.id, { name: r.text, unread: r.unread });
    if (!currentSpace) currentSpace = r.id;
    return;
  }
  if (r.kind === 'message' || r.kind === 'sent') {
    if (archive.some((x) => x.id === r.id && x.kind === r.kind)) return;
    archive.push(r);
    if (live) retirePending(r.text);
  }
}

const pending = new Map<string, HTMLElement>();

function retirePending(text: string): void {
  const node = pending.get(text);
  if (!node) return;
  pending.delete(text);
  node.remove();
}

function renderChat(): void {
  el.panelBody.textContent = '';
  for (const [id, space] of spaces) {
    const row = document.createElement('div');
    row.className = 'space';
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = space.name || id;
    row.appendChild(name);
    if (space.unread > 0) {
      const unread = document.createElement('span');
      unread.className = 'unread';
      unread.textContent = ` ${space.unread}`;
      row.appendChild(unread);
    }
    row.addEventListener('click', () => {
      currentSpace = id;
      send('adapter', { adapter: 'googlechat', cmd: 'open', space: id });
      renderChat();
    });
    el.panelBody.appendChild(row);
  }

  for (const r of archive.filter((m) => !currentSpace || m.space === currentSpace).slice(-200)) {
    const row = document.createElement('div');
    row.className = 'msg';
    const author = document.createElement('span');
    author.className = 'author';
    author.textContent = r.author || 'me';
    row.appendChild(author);
    row.appendChild(document.createTextNode(r.text));
    el.panelBody.appendChild(row);
  }
  el.panelBody.appendChild(composer());
  el.panelBody.scrollTop = el.panelBody.scrollHeight;
}

function composer(): HTMLElement {
  const wrap = document.createElement('div');
  wrap.id = 'composer';
  const input = document.createElement('input');
  input.placeholder = currentSpace ? `Message ${spaces.get(currentSpace)?.name ?? ''}` : 'Message';
  const sendBtn = document.createElement('button');
  sendBtn.textContent = 'Send';

  const submit = (): void => {
    const text = input.value.trim();
    if (!text) return;
    input.value = '';
    // Optimistic: the message appears now and is retired when the server's
    // authoritative copy arrives. Sending feels instant despite the RTT.
    const ghost = document.createElement('div');
    ghost.className = 'msg pending';
    ghost.textContent = text;
    el.panelBody.insertBefore(ghost, wrap);
    pending.set(text, ghost);
    send('adapter', {
      adapter: 'googlechat', cmd: 'send', space: currentSpace, text,
      localId: `local-${Date.now()}`,
    });
  };

  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') submit();
  });
  sendBtn.addEventListener('click', submit);
  wrap.appendChild(input);
  wrap.appendChild(sendBtn);
  return wrap;
}

// -------------------------------------------------------------------- pairing

el.pairingForm.addEventListener('submit', (ev) => {
  ev.preventDefault();
  try {
    const pairing = JSON.parse(el.pairingJSON.value) as Pairing;
    if (!pairing.token || !pairing.host) throw new Error('needs at least host and token');
    void store.writePairing(pairing).then(() => {
      el.pairing.close();
      configure(pairing);
    });
  } catch (err) {
    el.pairingError.textContent = `That is not a pairing file: ${String(err)}`;
  }
});

// --------------------------------------------------------------------- events

window.addEventListener('resize', () => {
  layout();
  send('viewport', { viewport: viewport() });
});

document.addEventListener('visibilitychange', () => {
  // A backgrounded tab gets throttled and may lose the connection. Coming back
  // is exactly the outage case the reconnect path already handles, so nudge it
  // rather than waiting for a keepalive to notice.
  if (document.visibilityState === 'visible') send('reconnect', {});
});

void main();
