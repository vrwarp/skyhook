/**
 * The app shell: tab strip, URL bar, link-health HUD, chat panel, and the
 * routing between the network worker and each tab's sandboxed mirror frame.
 *
 * No framework. The whole client is a patcher and an input serialiser; a
 * runtime would be more bytes than the mirror protocol it exists to carry.
 */
import { MirrorHost, imageURL, type MenuTarget } from '../mirror/host.js';
import { closeMenu, menuIsOpen, showMenu, type MenuGroups } from './menu.js';
import { pairingFromFragment, transportUrls } from './pairing.js';
import { Store, type Pairing } from '../store/store.js';
import type {
  AdapterRecord, ImageMeta, Mutation, Snapshot, Stats, TabState, Welcome,
} from '../shared/protocol.js';

const store = new Store();
const hosts = new Map<number, MirrorHost>();
const tabs = new Map<number, TabView>();
const archive: AdapterRecord[] = [];
const spaces = new Map<string, { name: string; unread: number }>();
let worker: Worker | null = null;
let active = 0;
let currentSpace = '';
/** Whether the link is up. Controls which chrome is usable, not what is shown. */
let connected = false;

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
  // Draw the chrome before anything is connected. The "new tab" button lives in
  // the tab strip, so waiting for the first server message to render it left a
  // window — one round trip wide, and this link's round trips are seconds — in
  // which the app was up and offered no way to open anything.
  renderTabs();

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
  if (!location.hash) return undefined;
  const pairing = pairingFromFragment(location.hash, location);
  if (!pairing) return undefined;
  await store.writePairing(pairing as Pairing);
  history.replaceState(null, '', location.pathname + location.search);
  return pairing as Pairing;
}

function configure(pairing: Pairing): void {
  // After the pairing fragment has been stripped, so the entries the trap keeps
  // are the address the reader should be left on if they leave after all.
  claimHistoryGestures();
  const urls = transportUrls(pairing, location);
  send('configure', {
    pairing: {
      url: urls.url,
      fallbackUrl: urls.fallbackUrl,
      certHash: urls.certHash,
      token: pairing.token,
      preferFallback: urls.preferFallback,
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
      // Relative links in the mirror resolve against this; a tab that navigates
      // without a fresh snapshot would otherwise keep the old base.
      hosts.get(id)?.setPageUrl(tab.url);
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
    case 'status': {
      const online = args.online === true;
      const changed = online !== connected;
      connected = online;
      const refused = args.refused as 'unauthorized' | 'version' | undefined;
      renderStatus(online, String(args.kind ?? ''), args.reason as string | undefined, refused);
      for (const h of hosts.values()) h.setOffline(!online);
      if (changed) renderTabs();
      if (refused) showRefusal(refused);
      break;
    }
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
  // A whole new document arrived: whatever the open menu was pointing at is
  // gone, and half its entries would act on a node that no longer exists.
  if (tab === active) closeMenu();
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
    openLink: (_t, url) => openInNewTab(url),
    menu: (t, target) => showMenu(target.x, target.y, mirrorMenu(t, target)),
    dismiss: () => {
      const was = menuIsOpen();
      closeMenu();
      return was;
    },
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
      closeTab(tab.id);
    });
    node.appendChild(close);

    node.addEventListener('click', () => {
      active = tab.id;
      renderTabs();
      layout();
      syncToolbar();
    });
    node.addEventListener('contextmenu', (ev) => {
      ev.preventDefault();
      // The shell's own menu below would offer page actions; on a tab the
      // useful actions are about the tab.
      ev.stopPropagation();
      showMenu(ev.clientX, ev.clientY, tabMenu(tab.id));
    });
    el.strip.appendChild(node);
  }
  const add = document.createElement('button');
  add.id = 'newtab';
  add.textContent = '+';
  add.title = connected ? 'New tab' : 'Waiting for the link';
  // Opening a tab is a request to the server, so offline it would do nothing at
  // all. Better to look unavailable than to look broken.
  add.disabled = !connected;
  add.addEventListener('click', () => send('openTab', { url: '' }));
  el.strip.appendChild(add);
  syncToolbar();
}

function closeTab(id: number): void {
  send('closeTab', { tab: id });
  closeTabLocally(id);
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
  // A back gesture let through earlier spent the trap. Now that there is a page
  // to go back to, it is worth keeping again.
  if (!centred && tab?.canBack) claimHistoryGestures();
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

// -------------------------------------------------------------------- actions

/**
 * Opens a URL in a new tab. Both halves of the "open in a new tab" gesture end
 * up here: the middle click and the menu entry. The tab opens in the background,
 * which is what the gesture means everywhere else — and matters more here, where
 * the page behind it is the one already paid for.
 */
function openInNewTab(url: string): void {
  if (!connected) {
    log('offline: a new tab needs the link');
    return;
  }
  send('openTab', { url });
}

function navigateTo(tab: number, url: string): void {
  if (!tab) {
    send('openTab', { url });
    return;
  }
  send('navigate', { tab, url });
}

/**
 * Moves a tab through its own history, from wherever the gesture came from: the
 * toolbar, the mouse's side buttons, Alt+←, the browser's back button.
 *
 * Reports whether it asked for anything. A tab at the start of its history has
 * nowhere to go, and the answer matters to the caller — it is what tells the
 * back gesture below that this one is not ours to keep.
 */
function goHistory(tab: number, action: 'back' | 'forward'): boolean {
  const view = tabs.get(tab);
  if (!connected || !tab || !view) return false;
  if (!(action === 'back' ? view.canBack : view.canForward)) return false;
  send('navigate', { tab, action });
  return true;
}

function addBookmark(title: string, url: string): void {
  if (!url) return;
  void store.readBookmarks().then((marks) => {
    marks.push({ title: title || url, url });
    return store.writeBookmarks(marks);
  });
}

function copyText(text: string): void {
  if (!text) return;
  void navigator.clipboard?.writeText(text)
    .catch((err: unknown) => log(`clipboard write failed: ${String(err)}`));
}

async function pasteInto(tab: number, field: number): Promise<void> {
  try {
    const text = await navigator.clipboard.readText();
    if (text) hosts.get(tab)?.replaceSelection(field, text);
  } catch (err) {
    log(`clipboard read failed: ${String(err)}`);
  }
}

/**
 * Saves an image out of the mirror. The bytes are already plane-side — the
 * network worker wrote them into Cache Storage and the service worker serves
 * them — so this costs nothing over the link and works during an outage. The
 * real remote URL is not knowable here, which is why the file is named after
 * the alt text.
 */
async function saveImage(hash: string, alt: string): Promise<void> {
  try {
    const res = await fetch(imageURL(hash));
    if (!res.ok) throw new Error(`image ${hash} is not cached`);
    const blob = await res.blob();
    const ext = (blob.type.split('/')[1] ?? 'bin').replace(/[^a-z0-9]/gi, '');
    const stem = alt.replace(/[^\w-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60);
    const href = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = href;
    a.download = `${stem || `skyhook-${hash}`}.${ext}`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(href), 10000);
  } catch (err) {
    log(`save image failed: ${String(err)}`);
  }
}

// -------------------------------------------------------------- context menus

/** The entries that act on the tab itself, shared by every menu. */
function pageGroups(tab: number): MenuGroups {
  const view = tabs.get(tab);
  const url = view?.url ?? '';
  return [
    [
      // Navigation is a request to the server, so offline it would do nothing
      // at all. Better to look unavailable than to look broken — the same rule
      // the new-tab button follows.
      {
        label: 'Back',
        disabled: !connected || !view?.canBack,
        run: () => send('navigate', { tab, action: 'back' }),
      },
      {
        label: 'Forward',
        disabled: !connected || !view?.canForward,
        run: () => send('navigate', { tab, action: 'forward' }),
      },
      {
        label: 'Reload',
        disabled: !connected || !tab,
        run: () => send('navigate', { tab, action: 'reload' }),
      },
    ],
    [
      { label: 'Copy page address', disabled: !url, run: () => copyText(url) },
      {
        label: 'Bookmark page',
        disabled: !url,
        run: () => addBookmark(view?.title ?? '', url),
      },
      {
        label: 'Duplicate tab',
        disabled: !url || !connected,
        run: () => openInNewTab(url),
      },
    ],
  ];
}

/** The menu for a right click inside a mirrored page. */
function mirrorMenu(tab: number, target: MenuTarget): MenuGroups {
  const host = hosts.get(tab);
  const groups: MenuGroups = [];
  const link = target.link;
  if (link) {
    groups.push([
      {
        label: 'Open link in new tab',
        hint: 'Middle click',
        disabled: !connected,
        run: () => openInNewTab(link),
      },
      { label: 'Open link', disabled: !connected, run: () => navigateTo(tab, link) },
      { label: 'Copy link address', run: () => copyText(link) },
      { label: 'Bookmark link', run: () => addBookmark(target.linkText ?? '', link) },
    ]);
  }

  const image = target.image;
  if (image) {
    groups.push([
      { label: 'Save image', run: () => void saveImage(image, target.imageAlt ?? '') },
      {
        label: 'Copy image description',
        disabled: !target.imageAlt,
        run: () => copyText(target.imageAlt ?? ''),
      },
    ]);
  }

  const field = target.field;
  if (field) {
    groups.push([
      {
        label: 'Cut',
        disabled: !target.selection,
        run: () => {
          copyText(target.selection);
          host?.replaceSelection(field, '');
        },
      },
      { label: 'Copy', disabled: !target.selection, run: () => copyText(target.selection) },
      { label: 'Paste', run: () => void pasteInto(tab, field) },
      { label: 'Select all', run: () => host?.selectAll(field) },
    ]);
  } else if (target.selection) {
    groups.push([{ label: 'Copy', run: () => copyText(target.selection) }]);
  }

  groups.push(...pageGroups(tab));
  groups.push([
    {
      // Some pages answer a right click with a menu of their own, which arrives
      // in the mirror as ordinary DOM. That is a round trip, so it is a choice
      // rather than the default.
      label: 'Send right-click to the page',
      hint: 'one round trip',
      disabled: !target.node || !connected,
      run: () => host?.sendContextMenu(target.node),
    },
  ]);
  return groups;
}

/** The menu for a right click on a tab in the strip. */
function tabMenu(id: number): MenuGroups {
  const view = tabs.get(id);
  return [
    [
      { label: 'New tab', disabled: !connected, run: () => send('openTab', { url: '' }) },
      {
        label: 'Duplicate tab',
        disabled: !view?.url || !connected,
        run: () => openInNewTab(view?.url ?? ''),
      },
      {
        label: 'Reload',
        disabled: !connected,
        run: () => send('navigate', { tab: id, action: 'reload' }),
      },
    ],
    [
      { label: 'Close tab', run: () => closeTab(id) },
      {
        label: 'Close other tabs',
        disabled: tabs.size < 2,
        run: () => {
          for (const other of Array.from(tabs.keys())) {
            if (other !== id) closeTab(other);
          }
        },
      },
    ],
  ];
}

/** The menu for a right click on the chrome itself. */
function shellMenu(): MenuGroups {
  const groups: MenuGroups = [
    [{ label: 'New tab', disabled: !connected, run: () => send('openTab', { url: '' }) }],
  ];
  if (active) groups.push(...pageGroups(active));
  return groups;
}

document.addEventListener('contextmenu', (ev) => {
  const target = ev.target as HTMLElement | null;
  // Text fields and the chat panel keep the native menu. Those are real local
  // documents, so the browser's own clipboard entries do exactly the right
  // thing — and the URL bar is where people paste.
  if (target?.closest?.('input, textarea, #panel')) return;
  ev.preventDefault();
  showMenu(ev.clientX, ev.clientY, shellMenu());
});

// -------------------------------------------------------------------- toolbar

el.urlbar.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter') return;
  const url = el.urlbar.value.trim();
  if (!url) return;
  navigateTo(active, url);
});

el.back.addEventListener('click', () => goHistory(active, 'back'));
el.forward.addEventListener('click', () => goHistory(active, 'forward'));
el.reload.addEventListener('click', () => send('navigate', { tab: active, action: 'reload' }));
el.bookmark.addEventListener('click', () => {
  const tab = tabs.get(active);
  if (tab) addBookmark(tab.title, tab.url);
});

// ------------------------------------------- the browser's own back and forward
//
// Every gesture a reader has for "the page before this one" — the browser's own
// buttons, the mouse's side buttons, Alt+←, ⌘+[, the two-finger swipe, Android's
// system back — acts on the shell's history. The shell has one entry: the app.
// So the most ordinary instinct in browsing throws away the session and every
// page it paid for over a link where pages cost seconds, and the page the reader
// actually wanted is still a round trip away, never asked for.
//
// The browser resolves all of them to the same thing — a step through session
// history — long before any of them is an event a page can see, so that step is
// what gets caught, once, here. Recognising the gestures individually would mean
// re-implementing a keymap that differs per platform, and being wrong in the
// expensive direction: a chord the browser acts on *and* the shell answers goes
// back two pages, which on this link is a page fetched to be thrown away.
//
// Catching a step means having somewhere for it to go: the shell keeps an entry
// on either side of itself and lives in the middle one. A pop to either side is
// the gesture, spent on the tab's history instead, with the middle put back
// under the reader before they can see it move.
//
// A back gesture the tab cannot answer — the first page of a tab, or a session
// with no tab at all — is deliberately let through. It lands on the entry
// behind, where the app looks unchanged and a second press leaves for real: a
// browser that cannot be closed by the gesture that closes browsers is worse
// than one that has to be told twice.
const HISTORY_STATES = { back: 'skyhook:back', here: 'skyhook:here', forward: 'skyhook:forward' };
/** Whether the shell still occupies the middle entry, i.e. the trap is armed. */
let centred = false;

function claimHistoryGestures(): void {
  history.replaceState({ skyhook: HISTORY_STATES.back }, '');
  history.pushState({ skyhook: HISTORY_STATES.here }, '');
  history.pushState({ skyhook: HISTORY_STATES.forward }, '');
  centred = true;
  history.back();
}

window.addEventListener('popstate', (ev) => {
  const state = (ev.state as { skyhook?: string } | null)?.skyhook;
  if (state === HISTORY_STATES.back) {
    // Only a gesture let through spends the trap. Clearing it here as well
    // would rearm on the next tab state — in the window before the restore
    // lands — pushing a fresh pair of entries under a step already on its way
    // back to the middle.
    if (goHistory(active, 'back')) history.forward();
    else centred = false;
    return;
  }
  if (state === HISTORY_STATES.forward) {
    goHistory(active, 'forward');
    // Restored either way: an unanswerable forward gesture is inert, and
    // leaving the shell parked at the end would cost the next back gesture.
    history.back();
    return;
  }
  // The middle, arrived at from one side or the other: the restore landing.
  centred = true;
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

function renderStatus(
  online: boolean, kind: string, reason?: string,
  refused?: 'unauthorized' | 'version',
): void {
  el.hudState.className = online ? 'online' : 'offline';
  el.hudState.textContent = online ? kind.replace('web', '') : 'offline';
  if (refused) el.hudState.textContent = refused === 'version' ? 'stale' : 'unpaired';
  el.hudState.title = reason ?? '';
}

/**
 * A refusal is not an outage, and showing it as one is what makes it baffling:
 * the link is fine, the server is up, and it will keep saying no until the
 * credential changes. So say which it is, and put the way out of it on screen.
 *
 * The usual cause is a server that came back with a new token — a restart
 * without a persisted one used to do this — so the pairing dialog is the fix,
 * with a fresh pairing link or the server's pairing.json.
 */
function showRefusal(refused: 'unauthorized' | 'version'): void {
  if (refused === 'version') {
    log('server refused the connection: client and server are different builds');
    if (!el.pairing.open) {
      el.pairingError.textContent =
        'This client and the server are different builds. Reload the page to pick up the '
        + 'version the server is serving.';
    }
    return;
  }
  log('server refused the pairing token');
  el.pairingError.textContent =
    'The server no longer accepts this pairing. It has most likely been restarted with a '
    + 'new token: open a fresh pairing link, or paste the current pairing.json below.';
  if (!el.pairing.open) el.pairing.showModal();
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
