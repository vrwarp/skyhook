/**
 * The app shell: tab strip, URL bar, link-health HUD, chat panel, and the
 * routing between the network worker and each tab's sandboxed mirror frame.
 *
 * No framework. The whole client is a patcher and an input serialiser; a
 * runtime would be more bytes than the mirror protocol it exists to carry.
 */
import { MirrorHost, type MenuTarget, type MirrorFreeze } from '../mirror/host.js';
import { IMAGE_CACHE, imageCacheKey } from '../shared/caches.js';
import { closeMenu, menuIsOpen, showMenu, type MenuGroups } from './menu.js';
import { pairingFromFragment, transportUrls } from './pairing.js';
import { gather } from './capture.js';
import * as clientlog from './clientlog.js';
import { Progress, type Ask } from './progress.js';
import {
  BOOKMARK_LIMIT, Bookmarks, exportText, parseImport, search, type Bookmark,
} from './bookmarks.js';
import { BookmarkPanel, StartPage } from './bookmarkview.js';
import { Suggest } from './suggest.js';
import { Store, type Pairing } from '../store/store.js';
import type {
  AdapterRecord, CaptureDone, CaptureRequest, ImageMeta, Mutation, Refusal, Snapshot, Stats,
  TabState, Welcome,
} from '../shared/protocol.js';

const store = new Store();
const hosts = new Map<number, MirrorHost>();
const tabs = new Map<number, TabView>();
const archive: AdapterRecord[] = [];
const spaces = new Map<string, { name: string; unread: number }>();
/** Navigations asked for and not yet answered. See progress.ts. */
const progress = new Progress();
let worker: Worker | null = null;
let active = 0;
let currentSpace = '';
/**
 * The session this app was last reading, offered back to the server on the next
 * load. Tabs are landside and outlive the page that was showing them.
 */
let resumeSession = '';
/** Whether the link is up. Controls which chrome is usable, not what is shown. */
let connected = false;
/** The link's last round trip, which is what an ask's patience is measured in. */
let linkRtt = 0;
/** One timer for every outstanding ask, armed at the earliest deadline. */
let sweepTimer: ReturnType<typeof setTimeout> | null = null;
/** How many tabs have been asked for that the reader expects to land in. */
let wantForeground = 0;

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
  marks: byId<HTMLButtonElement>('marks'),
  chat: byId<HTMLButtonElement>('chat'),
  frames: byId<HTMLDivElement>('frames'),
  progress: byId<HTMLDivElement>('progress'),
  status: byId<HTMLDivElement>('status'),
  panel: byId<HTMLElement>('panel'),
  panelTitle: byId<HTMLElement>('panel-title'),
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
  capture: byId<HTMLDialogElement>('capture'),
  captureForm: byId<HTMLFormElement>('capture-form'),
  captureNote: byId<HTMLTextAreaElement>('capture-note'),
  captureCost: byId<HTMLParagraphElement>('capture-cost'),
  captureCancel: byId<HTMLButtonElement>('capture-cancel'),
  toast: byId<HTMLDivElement>('toast'),
};

function byId<T extends HTMLElement>(id: string): T {
  const node = document.getElementById(id);
  if (!node) throw new Error(`missing element #${id}`);
  return node as T;
}

// ------------------------------------------------------------------- startup

async function main(): Promise<void> {
  clientlog.install();
  await store.requestPersistence();
  registerServiceWorker();
  startWorker();
  // Draw the chrome before anything is connected. The "new tab" button lives in
  // the tab strip, so waiting for the first server message to render it left a
  // window — one round trip wide, and this link's round trips are seconds — in
  // which the app was up and offered no way to open anything.
  renderTabs();
  // The saved list is local, so the start page can be on screen with something
  // worth clicking before the transport has even been configured.
  await bookmarks.whenReady();
  renderBookmarks();

  const pairing = await pairingFromURL() ?? await store.readPairing();
  // Read before the dialog below can send us down the pairing path, so both
  // ways into configure() carry the session this app left behind.
  resumeSession = await store.readSessionId();
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
    // The session this app was reading when it was last closed. The tabs are
    // landside — real Chromium tabs, still loaded, still logged in — and they
    // outlive the page that was showing them. A load that did not name the
    // session it left behind was given a fresh one, and everything the reader
    // had open went on running on the VPS with nothing able to reach it: an
    // empty strip and a blank frame, over a link where every one of those pages
    // cost seconds.
    sessionId: resumeSession,
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
      resumeSession = w.sessionId;
      void store.writeSessionId(w.sessionId);
      const held = new Set<number>();
      for (const t of w.tabs ?? []) {
        held.add(t.tab);
        // Welcome carries a URL and a title and no history flags, so a tab this
        // side already knows keeps the ones it has rather than being told it
        // cannot go back. A tab arriving cold gets them from the state frame
        // the server sends behind this.
        const known = tabs.get(t.tab);
        upsertTab({
          id: t.tab, url: t.url, title: t.title, loading: t.loading,
          canBack: known?.canBack ?? false, canForward: known?.canForward ?? false,
        });
        void hostFor(t.tab);
        if (t.active) active = t.tab;
      }
      // Whatever this side holds and the session does not is gone: a server
      // restarted under a reconnect answers with a session that never had those
      // tabs, and a strip of tabs that cannot be reached is worse than a short
      // one — every click on them would go to a tab id the server will refuse.
      for (const id of Array.from(tabs.keys())) {
        if (!held.has(id)) closeTabLocally(id);
      }
      if (!tabs.has(active)) active = tabs.keys().next().value ?? 0;
      // The tab the session says is the active one is the tab to show. Each
      // frame above was laid out as it was created, so without this the visible
      // one is whichever tab happened to be first rather than the one the strip
      // is about to mark active.
      layout();
      // The server has just said what every tab is, which answers anything this
      // side was still waiting to hear about them.
      progress.clear();
      renderProgress();
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
      if (!known) {
        void hostFor(id);
        // The tab the reader asked for has arrived, so its placeholder in the
        // strip has a real tab to become.
        progress.appeared();
        // And if it was asked for by a gesture that means "go there" — the +
        // button, an address typed with no tab open — this is where the reader
        // lands. The server has no opinion about focus, so the answer has to be
        // remembered from the gesture, and it matters more now that an empty
        // tab has the saved list in it rather than nothing.
        if (wantForeground > 0) {
          wantForeground -= 1;
          active = id;
          layout();
        }
      }
      // Once the server says a tab is loading it is telling this side what it
      // had been guessing, and it is the better source.
      progress.serverLoading(id, st.loading);
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
      renderProgress();
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
      // Only when the panel is actually showing chat: a message arriving while
      // the reader is looking at the saved list used to replace the list.
      if (!el.panel.hidden && panelView === 'chat') renderChat();
      break;
    }
    case 'status': {
      const online = args.online === true;
      const changed = online !== connected;
      connected = online;
      // Nothing asked for is on its way during an outage: the worker drops
      // navigate frames while the link is down. Saying a page is coming would
      // be a promise the shell cannot keep, and the HUD's "offline" is the
      // honest affordance for that state.
      const stalled = !online && progress.clear();
      const refused = args.refused as Refusal | undefined;
      renderStatus(online, String(args.kind ?? ''), args.reason as string | undefined, refused);
      for (const h of hosts.values()) h.setOffline(!online);
      if (changed || stalled) renderProgress();
      if (refused) showRefusal(refused);
      break;
    }
    case 'stats':
      renderStats(args as unknown as Stats & { rttMs?: number });
      break;
    case 'capture':
      // Nothing between here and the freeze inside runCapture may await. See
      // MirrorHost.freeze for why.
      runCapture(args.request as unknown as CaptureRequest);
      break;
    case 'captureDone': {
      const done = args as unknown as CaptureDone;
      if (done.error) {
        log(`capture failed: ${done.error}`);
        toast(`Capture failed: ${done.error}`);
      } else {
        log(`capture written landside: ${done.path} (${done.bytes} bytes)`);
        toast(`Capture saved landside: ${basename(done.path)}`);
      }
      break;
    }
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
  // The page the reader asked for is on screen. Waiting for the tab-state frame
  // behind it to say so would leave the bar running over a page that has
  // already arrived.
  if (progress.arrived(tab, snap.url)) renderProgress();
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
    navigating: (t, url) => asking(t, 'Loading', url),
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
    const busy = isBusy(tab.id);
    const node = document.createElement('div');
    node.className = `tab${tab.id === active ? ' active' : ''}${busy ? ' loading' : ''}`;
    node.setAttribute('role', 'tab');
    // The landside tab this row stands for, for the same reason the frame
    // carries it: a row in the strip is otherwise identified only by a title it
    // shares with every other tab on the same site.
    node.dataset.tab = String(tab.id);
    // Before the title, where a favicon goes and where a browser puts this: a
    // background tab fetching a page is the case the bar over the mirror cannot
    // show, because the mirror it would sit over is another tab's.
    if (busy) node.appendChild(spinner());

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
      renderProgress();
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
  // A tab is opened by the server, so between the gesture and the tab there is
  // a round trip in which the strip is unchanged and the reader has no way to
  // tell a middle click that was heard from one that was missed. They press
  // again, and come back to two tabs.
  for (const open of progress.opening) {
    const ghost = document.createElement('div');
    ghost.className = 'tab ghost loading';
    ghost.title = 'Waiting for the server to open this tab';
    ghost.appendChild(spinner());
    const title = document.createElement('span');
    title.className = 'title';
    title.textContent = (open.url && hostOf(open.url)) || 'New tab';
    ghost.appendChild(title);
    el.strip.appendChild(ghost);
  }
  const add = document.createElement('button');
  add.id = 'newtab';
  add.textContent = '+';
  add.title = connected ? 'New tab' : 'Waiting for the link';
  // Opening a tab is a request to the server, so offline it would do nothing at
  // all. Better to look unavailable than to look broken.
  add.disabled = !connected;
  add.addEventListener('click', () => openTab('', 'focus'));
  el.strip.appendChild(add);
  syncToolbar();
}

/** The one moving thing in the chrome: a tab, or a tab-to-be, is waiting. */
function spinner(): HTMLElement {
  const spin = document.createElement('span');
  spin.className = 'spin';
  // Decoration to a screen reader otherwise, in a strip that is otherwise a
  // row of page names.
  spin.setAttribute('role', 'img');
  spin.setAttribute('aria-label', 'Loading');
  return spin;
}

function closeTab(id: number): void {
  send('closeTab', { tab: id });
  closeTabLocally(id);
}

function closeTabLocally(id: number): void {
  hosts.get(id)?.destroy();
  hosts.delete(id);
  tabs.delete(id);
  progress.forget(id);
  if (active === id) {
    const next = tabs.keys().next().value;
    active = typeof next === 'number' ? next : 0;
  }
  renderProgress();
  layout();
}

function syncToolbar(): void {
  const tab = tabs.get(active);
  const url = tab?.url ?? '';
  // A tab that has not been anywhere shows an empty address bar rather than
  // `about:blank`: the reader is about to type over it, and a placeholder that
  // has to be cleared first is a placeholder in the way.
  if (document.activeElement !== el.urlbar) el.urlbar.value = isBlank(url) ? '' : url;
  el.back.disabled = !tab?.canBack;
  el.forward.disabled = !tab?.canForward;
  // A back gesture let through earlier spent the trap. Now that there is a page
  // to go back to, it is worth keeping again.
  if (!centred && tab?.canBack) claimHistoryGestures();
  renderBookmarks();
}

/** A tab that has not been anywhere yet. `about:blank` is not a page. */
function isBlank(url: string): boolean {
  return !url || url.startsWith('about:');
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

// ------------------------------------------------------------------ progress
//
// Three affordances, for three questions a reader asks in the seconds after a
// click, and one timer under all of them.
//
//   the bar    — did that do anything? (the active tab, at the top of the page)
//   the strip  — which tab is it doing it in? (spinners, and tabs-to-be)
//   the line   — where am I going? (bottom left, the way browsers say it)
//
// All three are driven from the same two facts: what this side has asked for
// (progress.ts) and what the server last said about the tab. Nothing here
// invents a percentage: how far along a page is, is not knowable from this end,
// and a bar that fills at a rate nobody measured is a lie told at the exact
// moment the reader is deciding whether to trust the thing.

/** Whether a tab is waiting for a page: either half of the story counts. */
function isBusy(tab: number): boolean {
  return !!progress.waiting(tab) || !!tabs.get(tab)?.loading;
}

/**
 * Records a navigation this side has just asked for, and shows it at once.
 *
 * Offline it records nothing: the worker drops navigate frames while the link
 * is down, so there would be no page coming to wait for.
 */
function asking(tab: number, verb: string, url?: string): void {
  if (!connected || !tab) return;
  progress.ask(tab, { verb, url, from: tabs.get(tab)?.url ?? '' }, clock(), linkRtt);
  renderProgress();
}

/** Redraws everything that says a page is on its way. */
function renderProgress(): void {
  renderTabs();
  const busy = isBusy(active);
  el.progress.hidden = !busy;
  el.status.hidden = !busy;
  if (busy) el.status.textContent = busyLabel(active);
  for (const [id, host] of hosts) host.setBusy(isBusy(id));
  armSweep();
}

/** What the status line says: the verb the gesture meant, and where it points. */
function busyLabel(tab: number): string {
  const ask = progress.waiting(tab);
  // While there is an ask, only the ask's own destination. A gesture whose
  // destination this side does not know — back, forward, a form submitted —
  // would otherwise be labelled with the tab's current URL, which is the one
  // page the reader is definitely not going to. Once the server has taken over
  // the tab's URL is the newer truth, and by then it is the page arriving.
  const url = ask ? ask.url ?? '' : tabs.get(tab)?.url ?? '';
  const verb = ask?.verb ?? 'Loading';
  return url ? `${verb} ${plainUrl(url)}…` : `${verb}…`;
}

/**
 * A URL as a status line shows one: no scheme, no trailing slash on a bare
 * host. Overlong ones are left overlong and clipped by the element, so what is
 * dropped is the tail and not the host the reader is checking.
 */
function plainUrl(url: string): string {
  try {
    const u = new URL(url);
    const path = u.pathname === '/' ? '' : u.pathname;
    return u.host + path + u.search;
  } catch {
    return url;
  }
}

/** The clock asks are timed against: monotonic, so a sleeping laptop or a
 *  clock correction cannot expire one early. */
function clock(): number {
  return performance.now();
}

/**
 * Arms a single timer at the earliest deadline outstanding.
 *
 * Something has to fire, or an ask nothing ever answers — a link the page
 * treats as a button — leaves the bar running with no event coming to take it
 * down.
 */
function armSweep(): void {
  if (sweepTimer) {
    clearTimeout(sweepTimer);
    sweepTimer = null;
  }
  const until = progress.deadline();
  if (!Number.isFinite(until)) return;
  sweepTimer = setTimeout(() => {
    sweepTimer = null;
    if (progress.sweep(clock())) renderProgress();
    else armSweep();
  }, Math.max(50, until - clock()));
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
  openTab(url);
}

/** Asks the server for a tab, and puts a placeholder in the strip until it
 *  arrives. Every gesture that opens one comes through here. */
function openTab(url: string, where: 'background' | 'focus' = 'background'): void {
  send('openTab', { url });
  if (!connected) return;
  // A tab the reader means to be in, rather than one opened beside what they
  // are reading: the + button and the menus mean the first, a middle click on a
  // link means the second, and only the gesture knows which.
  if (where === 'focus') wantForeground += 1;
  progress.askOpen({ verb: 'Opening', url: url || undefined }, clock(), linkRtt);
  renderProgress();
}

function navigateTo(tab: number, url: string): void {
  if (!tab) {
    openTab(url, 'focus');
    return;
  }
  send('navigate', { tab, url });
  asking(tab, 'Loading', url);
}

/** Reloads a tab, from wherever the gesture came from. */
function reloadTab(tab: number): void {
  send('navigate', { tab, action: 'reload' });
  asking(tab, 'Reloading', tabs.get(tab)?.url);
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
  // Where it goes is the tab's history, which this side does not keep — only
  // that it was asked for, which is what the reader is waiting to see.
  asking(tab, action === 'back' ? 'Going back' : 'Going forward');
  return true;
}

// ------------------------------------------------------------------ bookmarks
//
// The saved list is the only way of getting somewhere that does not spend the
// link: reading it, searching it and rearranging it are free and work through
// an outage, and the single round trip at the end is the one the reader meant
// to spend. So it is wired into four places rather than one — the star, the
// side panel, the start page a tab shows before it has been anywhere, and the
// address bar's completions — and every one of them is drawn from the same
// list, plane-side, with no server involved.

const bookmarks = new Bookmarks({
  read: () => store.readBookmarks(),
  write: (marks) => store.writeBookmarks(marks),
  onError: (message) => {
    log(message);
    // A silent failure here is the worst kind: the reader believes a page is
    // kept, and finds out it is not on the flight where they needed it.
    toast(message);
  },
});

const marksPanel = new BookmarkPanel({
  open: (mark, where) => openBookmark(mark, where),
  remove: (mark) => removeBookmark(mark),
  rename: (mark, title) => bookmarks.rename(mark.id, title),
  menu: (mark, x, y) => bookmarkMenu(mark, x, y, 'panel'),
  exportAll: () => exportBookmarks(),
  importFrom: () => importBookmarks(),
});

const startPage = new StartPage({
  open: (mark, where) => openBookmark(mark, where),
  remove: (mark) => removeBookmark(mark),
  rename: (mark, title) => bookmarks.rename(mark.id, title),
  menu: (mark, x, y) => bookmarkMenu(mark, x, y, 'start'),
});
el.frames.appendChild(startPage.root);

const suggest = new Suggest(el.urlbar, {
  source: (query, limit) => search(bookmarks.all(), query, limit),
  pick: (mark) => openBookmark(mark, 'here'),
});

bookmarks.onChange(() => renderBookmarks());

/** Redraws everything that shows the saved list. Cheap: it is all local. */
function renderBookmarks(): void {
  const marks = bookmarks.all();
  syncBookmarkButton();
  if (!el.panel.hidden && panelView === 'marks') marksPanel.render(marks, connected);
  startPage.render(marks, { show: startShouldShow(), online: connected });
}

/**
 * Whether the tab on screen has nothing in it. A tab that has been asked to go
 * somewhere counts as occupied even before the answer arrives, and a session
 * with no tabs at all counts as empty — that is the moment after a cold start,
 * and the list is the most useful thing the app can put there.
 */
function startShouldShow(): boolean {
  // A tab with a page on the way is not an empty tab, even while it still looks
  // like one: `isBusy` is the same fact the bar and the tab spinner are drawn
  // from, so the start page cannot disagree with them about what is happening.
  if (isBusy(active)) return false;
  const tab = tabs.get(active);
  if (!tab) return true;
  return isBlank(tab.url);
}

function syncBookmarkButton(): void {
  const url = tabs.get(active)?.url ?? '';
  const savable = !isBlank(url);
  const saved = savable && bookmarks.has(url);
  el.bookmark.disabled = !savable;
  el.bookmark.textContent = saved ? '★' : '☆';
  el.bookmark.classList.toggle('on', saved);
  // The star is a toggle, so it has to say which way it is currently thrown —
  // the version that always read "Bookmark this page" was the reason a second
  // click seemed like the only way to find out whether the first had worked.
  el.bookmark.setAttribute('aria-pressed', String(saved));
  el.bookmark.title = saved
    ? 'Saved. Click to remove (Ctrl+D)'
    : 'Save this page (Ctrl+D)';
  el.marks.setAttribute('aria-expanded', String(!el.panel.hidden && panelView === 'marks'));
}

/** The star, Ctrl+D, and the page menu all land here. */
function toggleBookmark(id = active): void {
  const tab = tabs.get(id);
  const url = tab?.url ?? '';
  if (isBlank(url)) {
    toast('There is no page here to save yet.');
    return;
  }
  const existing = bookmarks.find(url);
  if (existing) {
    removeBookmark(existing);
    return;
  }
  addBookmark(tab?.title ?? '', url);
}

/**
 * Saves a page or a link. Idempotent, and it says so: a reader who cannot see
 * that a page is already kept will keep it again, and used to get a second
 * identical row for it.
 */
function addBookmark(title: string, url: string): void {
  if (!url) return;
  const result = bookmarks.add(title, url);
  if (result.full) {
    toast(`The saved list is full at ${BOOKMARK_LIMIT}. Remove one to make room.`);
    return;
  }
  if (!result.mark) return;
  const mark = result.mark;
  if (result.existed) {
    toast(`Already saved as “${mark.title}”.`, {
      label: 'Rename',
      run: () => {
        openPanel('marks');
        marksPanel.beginRename(mark.id);
      },
    });
    return;
  }
  toast(`Saved “${mark.title}”.`, { label: 'Undo', run: () => void bookmarks.remove(mark.id) });
}

/**
 * Removes one, with the undo alongside it. No confirmation: a confirmation on
 * every removal is a cost paid on every correct one, and this is the same
 * bargain the rest of the shell makes — say what happened, and offer the way
 * back.
 */
function removeBookmark(mark: Bookmark): void {
  const gone = bookmarks.remove(mark.id);
  if (!gone) return;
  toast(`Removed “${gone.mark.title}”.`, {
    label: 'Undo',
    run: () => bookmarks.restore(gone.mark, gone.index),
  });
}

/**
 * Opens a saved page. The only part of the feature that needs the link, which
 * is why it is the only part that goes quiet during an outage — and it says so
 * rather than doing nothing.
 */
function openBookmark(mark: Bookmark, where: 'here' | 'newTab'): void {
  if (!connected) {
    toast('Offline: opening a saved page needs the link. The list stays readable.');
    return;
  }
  bookmarks.touch(mark.url);
  if (where === 'newTab') {
    openInNewTab(mark.url);
    return;
  }
  navigateTo(active, mark.url);
}

/** The menu on a saved row, in either view. */
function bookmarkMenu(mark: Bookmark, x: number, y: number, from: 'panel' | 'start'): void {
  showMenu(x, y, [
    [
      { label: 'Open', disabled: !connected, run: () => openBookmark(mark, 'here') },
      {
        label: 'Open in new tab',
        hint: 'Middle click',
        disabled: !connected,
        run: () => openBookmark(mark, 'newTab'),
      },
      { label: 'Copy address', run: () => copyText(mark.url) },
    ],
    [
      {
        label: 'Rename…',
        run: () => {
          // The rename happens in the panel's list, so a rename asked for from
          // the start page brings the panel with it rather than failing quietly.
          if (from === 'start') openPanel('marks');
          marksPanel.beginRename(mark.id);
        },
      },
      { label: 'Remove', run: () => removeBookmark(mark) },
    ],
  ]);
}

/**
 * Writes the list out as JSON. Everything else on this client is recoverable
 * from the server; the saved list is not — it is the one thing that exists only
 * plane-side, and `Store.wipe()`, a cleared browser profile or a reinstalled
 * PWA all take it with them. So there is a way out of the box.
 */
function exportBookmarks(): void {
  const marks = bookmarks.all();
  if (!marks.length) {
    toast('Nothing saved yet.');
    return;
  }
  const blob = new Blob([exportText(marks)], { type: 'application/json' });
  const href = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = href;
  a.download = 'skyhook-bookmarks.json';
  a.click();
  setTimeout(() => URL.revokeObjectURL(href), 10000);
  toast(`Exported ${marks.length} saved page(s).`);
}

/** Reads one back in. Additive: an import never overwrites what is here. */
function importBookmarks(): void {
  const picker = document.createElement('input');
  picker.type = 'file';
  picker.accept = 'application/json,.json';
  picker.addEventListener('change', () => {
    const file = picker.files?.[0];
    if (!file) return;
    void file.text().then((text) => {
      const { added, skipped } = bookmarks.merge(parseImport(text));
      toast(skipped
        ? `Imported ${added}; skipped ${skipped} already saved.`
        : `Imported ${added} saved page(s).`);
    }).catch((err: unknown) => {
      log(`bookmark import failed: ${String(err)}`);
      toast(`Import failed: ${err instanceof Error ? err.message : String(err)}`);
    });
  });
  picker.click();
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
 * network worker wrote them into Cache Storage — so this costs nothing over the
 * link and works during an outage. The real remote URL is not knowable here,
 * which is why the file is named after the alt text.
 *
 * Read from the cache rather than fetched from the URL the service worker
 * serves it on: until that worker has claimed this page the same fetch reaches
 * the network, and the server answers an unknown path with the app shell — so
 * "save image" would hand the reader an index.html named after the picture.
 */
async function saveImage(hash: string, alt: string): Promise<void> {
  try {
    const cache = await caches.open(IMAGE_CACHE);
    const res = await cache.match(imageCacheKey(hash));
    if (!res) throw new Error(`image ${hash} is not cached`);
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
        run: () => goHistory(tab, 'back'),
      },
      {
        label: 'Forward',
        disabled: !connected || !view?.canForward,
        run: () => goHistory(tab, 'forward'),
      },
      {
        label: 'Reload',
        disabled: !connected || !tab,
        run: () => reloadTab(tab),
      },
    ],
    [
      { label: 'Copy page address', disabled: !url, run: () => copyText(url) },
      {
        // Says which way the toggle is thrown, like the star does. An entry
        // that reads "Bookmark page" on a page already bookmarked is an
        // invitation to make a second copy of it.
        label: bookmarks.has(url) ? 'Remove bookmark' : 'Bookmark page',
        hint: 'Ctrl+D',
        disabled: isBlank(url),
        run: () => toggleBookmark(tab),
      },
      { label: 'Saved pages…', hint: 'Ctrl+B', run: () => showPanel('marks') },
      {
        label: 'Duplicate tab',
        disabled: !url || !connected,
        run: () => openInNewTab(url),
      },
    ],
    [
      {
        // Deliberately the last entry everywhere it appears: it is the one
        // that costs real bytes, and it is only ever wanted when something has
        // already gone wrong.
        label: 'Report a rendering problem…',
        hint: 'sends a screenshot',
        disabled: !connected,
        run: () => askForCapture(),
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
      bookmarks.has(link)
        // A link already in the list is worth saying so about: on this link the
        // reason to bookmark rather than click is that the page has not been
        // paid for yet, and "already saved" is the answer to that question.
        ? {
          label: 'Remove saved link',
          run: () => {
            const mark = bookmarks.find(link);
            if (mark) removeBookmark(mark);
          },
        }
        : { label: 'Bookmark link', run: () => addBookmark(target.linkText ?? '', link) },
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
      { label: 'New tab', disabled: !connected, run: () => openTab('', 'focus') },
      {
        label: 'Duplicate tab',
        disabled: !view?.url || !connected,
        run: () => openInNewTab(view?.url ?? ''),
      },
      {
        label: 'Reload',
        disabled: !connected,
        run: () => reloadTab(id),
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
    [{ label: 'New tab', disabled: !connected, run: () => openTab('', 'focus') }],
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
  suggest.close();
  navigateTo(active, url);
});

el.back.addEventListener('click', () => goHistory(active, 'back'));
el.forward.addEventListener('click', () => goHistory(active, 'forward'));
el.reload.addEventListener('click', () => reloadTab(active));
el.bookmark.addEventListener('click', () => toggleBookmark());
el.marks.addEventListener('click', () => showPanel('marks'));

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
  // How long the shell waits for an answer is measured in round trips, so the
  // HUD's own number is where that comes from.
  if (rttMs) linkRtt = rttMs;
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

const REFUSAL_LABEL: Record<Refusal, string> = {
  unauthorized: 'unpaired',
  version: 'stale',
  replaced: 'taken over',
};

function renderStatus(
  online: boolean, kind: string, reason?: string, refused?: Refusal,
): void {
  el.hudState.className = online ? 'online' : 'offline';
  el.hudState.textContent = online ? kind.replace('web', '') : 'offline';
  if (refused) el.hudState.textContent = REFUSAL_LABEL[refused];
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
function showRefusal(refused: Refusal): void {
  if (refused === 'replaced') {
    // Not a fault, and not the pairing dialog's business: the session is fine,
    // it is just being read somewhere else now. Whichever copy of the app the
    // reader brings to the front takes it back, which is what they mean by
    // bringing it to the front.
    log('another Skyhook window took over this session');
    toast('This session is open in another window. Switch back to this one to take it over.');
    return;
  }
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
  // Also into the ring a capture carries up. The console is not somewhere
  // anybody is looking at 35,000 feet.
  clientlog.record('warn', message);
}

let toastTimer: ReturnType<typeof setTimeout> | null = null;

/**
 * A transient notice in the corner, for things the reader asked for.
 *
 * The optional action is what makes "Saved" and "Removed" safe to do without
 * asking first: the notice that says what happened is also the way to undo it,
 * so neither one needs a dialog in front of it.
 */
function toast(message: string, action?: { label: string; run(): void }): void {
  el.toast.textContent = '';
  const text = document.createElement('span');
  text.className = 'toast-text';
  text.textContent = message;
  el.toast.appendChild(text);
  if (action) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'toast-act';
    button.textContent = action.label;
    button.addEventListener('click', () => {
      hideToast();
      action.run();
    });
    el.toast.appendChild(button);
  }
  el.toast.hidden = false;
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(hideToast, 8000);
}

function hideToast(): void {
  el.toast.hidden = true;
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = null;
}

function basename(path: string): string {
  const cut = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'));
  return cut >= 0 ? path.slice(cut + 1) : path;
}

// ------------------------------------------------------------------ captures
//
// A capture is the one thing this client does that deliberately spends the
// link: the mirrored document and a screenshot of it go up to the server, which
// zips them together with the landside page, the frames it actually sent, and
// its own screenshot of the same tab. That bundle is the only artifact that can
// settle whether a page that looks wrong is wrong landside, wrong in the frames
// or wrong in the patcher — the three possibilities each look identical from
// here.
//
// It is never automatic on this side. The server takes one by itself when it
// catches a divergence, but a reader has to ask, because the reader is the one
// paying for the bytes.

/** Opens the "what looks wrong?" dialog. */
function askForCapture(): void {
  if (!connected) {
    toast('A capture needs the link: the bundle is written on your server.');
    return;
  }
  el.captureNote.value = '';
  // The server asks for every open tab, not just this one, because a mirror
  // bug is often visible in the tab beside the one being complained about. Say
  // so, since the reader is paying for it.
  const open = hosts.size;
  el.captureCost.textContent = open > 1
    ? `Sends all ${open} open tabs and a screenshot of each. This costs real bandwidth.`
    : 'Sends this page and a screenshot of it. This costs real bandwidth.';
  el.capture.showModal();
}

el.captureCancel.addEventListener('click', () => el.capture.close());
el.captureForm.addEventListener('submit', (ev) => {
  ev.preventDefault();
  const note = el.captureNote.value.trim();
  el.capture.close();
  send('capture', { reason: 'manual', note });
  toast('Capture requested. Gathering this side…');
});

/**
 * Answers the server's request for this side of a capture.
 *
 * The freeze below happens before anything is awaited, and that ordering is the
 * whole design. The server usually asks for a capture because it has just found
 * the two halves holding different documents — and the next thing it does about
 * that is send a resync, which replaces this side's document with a correct
 * one. The request rides the ctrl channel and the resync rides dom, so this
 * runs first; an `await` before the freeze would hand the evidence back.
 */
function runCapture(request: CaptureRequest): void {
  const wanted = request.tabs?.length ? request.tabs : Array.from(hosts.keys());
  const frozen: MirrorFreeze[] = [];
  for (const id of wanted) {
    const host = hosts.get(id);
    if (!host) continue;
    try {
      frozen.push(host.freeze());
    } catch (err) {
      log(`capture: tab ${id} could not be frozen: ${String(err)}`);
    }
  }
  clientlog.record('info', `capture ${request.id}: froze ${frozen.length} tab(s)`);

  void (async (): Promise<void> => {
    try {
      const artifacts = await gather({ request, frozen, shell: shellReport() });
      for (const artifact of artifacts) {
        send('capturePart', { id: request.id, name: artifact.name, data: artifact.data });
      }
      send('capturePart', { id: request.id, done: true });
      toast(`Capture sent: ${artifacts.length} file(s) on their way to your server.`);
    } catch (err) {
      log(`capture ${request.id} failed on this side: ${String(err)}`);
      // The server is holding a bundle open waiting for us. Telling it what
      // went wrong is better than letting it time out and record nothing.
      send('capturePart', { id: request.id, error: String(err), done: true });
    }
  })();
}

/** An outstanding ask as the bundle's client.json records it, with how long it
 *  has been outstanding — the number that says whether the reader was waiting. */
function waitReport(ask: Ask | undefined): Record<string, unknown> | null {
  if (!ask) return null;
  return { verb: ask.verb, url: ask.url ?? '', from: ask.from, forMs: Math.round(clock() - ask.at) };
}

/** What the shell itself knows, for the bundle's client.json. */
function shellReport(): Record<string, unknown> {
  return {
    connected,
    activeTab: active,
    tabs: Array.from(tabs.values()).map((t) => ({
      tab: t.id, url: t.url, title: t.title, loading: t.loading,
      canBack: t.canBack, canForward: t.canForward,
      hasFrame: hosts.has(t.id),
      nodes: hosts.get(t.id)?.nodes ?? 0,
      // A page the reader is still waiting for is the other half of "this looks
      // wrong": a capture taken mid-navigation is a picture of the old page.
      waitingFor: waitReport(progress.waiting(t.id)),
    })),
    openingTabs: progress.opening.map((o) => waitReport(o)),
    chatPanelOpen: !el.panel.hidden,
    archivedRecords: archive.length,
  };
}

// ------------------------------------------------------------------ the panel
//
// One panel, two views. They are the two things on this client worth reading
// while the link is down — the chat archive and the saved list — and both are
// read beside a page rather than instead of it, so they share the strip of
// screen that is already the cost of not being the page.

type PanelView = 'chat' | 'marks';
let panelView: PanelView = 'chat';

const PANEL_TITLES: Record<PanelView, string> = { chat: 'Chat', marks: 'Saved pages' };

/** Opens a view, or closes the panel if that view is already the one showing. */
function showPanel(view: PanelView): void {
  if (!el.panel.hidden && panelView === view) {
    closePanel();
    return;
  }
  openPanel(view);
}

/**
 * Opens a view without the toggle. Anything that needs the panel *there* —
 * a rename, which happens in the list — goes through this: asking for it while
 * it is already open must not be what closes it.
 */
function openPanel(view: PanelView): void {
  panelView = view;
  el.panel.hidden = false;
  el.panelTitle.textContent = PANEL_TITLES[view];
  el.panelBody.textContent = '';
  if (view === 'chat') {
    void openChat();
  } else {
    el.panelBody.appendChild(marksPanel.root);
    marksPanel.render(bookmarks.all(), connected);
    marksPanel.focusSearch();
  }
  syncBookmarkButton();
}

function closePanel(): void {
  el.panel.hidden = true;
  syncBookmarkButton();
}

el.chat.addEventListener('click', () => showPanel('chat'));
el.panelClose.addEventListener('click', () => closePanel());

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

document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && !ev.defaultPrevented && !el.panel.hidden) {
    // Not when a menu just took it: the menu handles Escape in the capture
    // phase and marks it handled, and closing the panel out from under a menu
    // dismissal is two things happening for one key.
    closePanel();
    return;
  }
  if (!(ev.ctrlKey || ev.metaKey) || ev.altKey) return;
  const key = ev.key.toLowerCase();
  // Ctrl/⌘+Shift+D. The browser's own devtools chord is F12 and Ctrl+Shift+I,
  // and this deliberately avoids both: on a mirrored page devtools show a
  // sandboxed frame full of inert nodes, which is the wrong answer to the
  // question somebody pressing them is asking.
  if (ev.shiftKey) {
    if (key !== 'd') return;
    ev.preventDefault();
    askForCapture();
    return;
  }
  // The two chords every browser already has for this, doing what they do
  // everywhere else. Both are claimed from the host browser deliberately: its
  // own bookmark would save the app shell's address, which is the one page in
  // this session nobody needs a way back to.
  if (key === 'd') {
    ev.preventDefault();
    toggleBookmark();
    return;
  }
  if (key === 'b') {
    ev.preventDefault();
    showPanel('marks');
  }
});

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState !== 'visible') return;
  // A backgrounded tab gets throttled: a connection it lost while hidden has a
  // reconnect pending on a timer the browser is running at once a minute, so
  // coming back is worth a nudge rather than waiting that out.
  //
  // Only when the link is actually down, though. A reconnect resyncs every open
  // tab, and this fires every single time the reader switches back to the app —
  // so nudging a healthy connection spends a snapshot per tab to replace a
  // connection that was working.
  if (!connected) send('reconnect', {});
});

void main();
