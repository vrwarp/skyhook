/**
 * The app shell: tab strip, URL bar, link-health HUD, chat panel, and the
 * routing between the network worker and each tab's sandboxed mirror frame.
 *
 * No framework. The whole client is a patcher and an input serialiser; a
 * runtime would be more bytes than the mirror protocol it exists to carry.
 */
import { MirrorHost, type MenuTarget, type MirrorFreeze } from '../mirror/host.js';
import { IMAGE_CACHE, imageCacheKey } from '../shared/caches.js';
import { closeMenu, menuIsOpen, showMenu, type MenuGroups, type MenuItem } from './menu.js';
import { pairingFromFragment, transportUrls } from './pairing.js';
import { gather } from './capture.js';
import * as clientlog from './clientlog.js';
import { Progress, type Ask } from './progress.js';
import { TabModel, type TabView } from './tabs.js';
import {
  BOOKMARK_LIMIT, Bookmarks, exportText, normalizeUrl, parseImport, type Bookmark,
} from './bookmarks.js';
import { BookmarkPanel, StartPage } from './bookmarkview.js';
import { completions, History, type Completion, type HistoryEntry } from './history.js';
import { TabList } from './tabsview.js';
import { isPhone, isTouch, prefersDark } from './layout.js';
import { Suggest } from './suggest.js';
import { Store, type Pairing } from '../store/store.js';
import { BUILD, VERSION } from '../shared/build.js';
import {
  browserEnv, detail as versionDetail, headline as versionHeadline, installUpdate, needsUpdate,
  verdict,
} from './upgrade.js';
import type {
  AdapterRecord, CaptureDone, CaptureRequest, Download, ImageMeta, Mutation, Refusal, Snapshot,
  Stats, TabState, Viewport, Welcome,
} from '../shared/protocol.js';
import { PROTOCOL_VERSION } from '../shared/protocol.js';
import * as transfers from './transfers.js';

const store = new Store();
const hosts = new Map<number, MirrorHost>();
const archive: AdapterRecord[] = [];
const spaces = new Map<string, { name: string; unread: number }>();
/** Navigations asked for and not yet answered. See progress.ts. */
const progress = new Progress();
let worker: Worker | null = null;
let currentSpace = '';
/**
 * The session this app was last reading, offered back to the server on the next
 * load. Tabs are landside and outlive the page that was showing them.
 */
let resumeSession = '';
/**
 * Which colour scheme the reader wants pages rendered in: 'light', 'dark', or
 * '' for whatever this device is set to.
 *
 * Answered landside, which is the whole of §45 — the palette is settled before
 * the stylesheet is written, along with every image the server transcoded from
 * that render, so a mirror that repainted itself over here would produce half of
 * each theme. This is the reader's say in what the answer is, and it travels
 * with the viewport for the same reason the width does.
 */
let schemePref = '';
/** Whether the link is up. Controls which chrome is usable, not what is shown. */
let connected = false;
/** The link's last round trip, which is what an ask's patience is measured in. */
let linkRtt = 0;
/** One timer for every outstanding ask, armed at the earliest deadline. */
let sweepTimer: ReturnType<typeof setTimeout> | null = null;
/**
 * Addresses typed into the address bar and not yet answered, by the tab they
 * were typed for — 0 meaning "the tab this is about to open".
 *
 * History records where a tab *arrived*, never what was typed at it (see
 * history.ts), so the one thing the arrival cannot tell us — that the reader
 * named this page from memory rather than following a link to it — has to be
 * carried across the round trip from the gesture that started it. Same shape as
 * `wantForeground` above, and for the same reason.
 */
const typedAsks = new Map<number, number>();
/**
 * How long such an ask stays believable. Long enough for a page over a bad
 * link, short enough that an ask whose navigation was dropped during an outage
 * cannot attach itself to whatever the tab does next.
 */
const TYPED_TTL_MS = 120_000;

/**
 * The tabs, including the ones drawn before the server has named them. Every
 * command aimed at a tab goes through this rather than straight to the worker,
 * because a tab the user opened half a round trip ago has no id to send under
 * yet — the model holds those until it does.
 *
 * Which tab the reader meant to land in is settled here too, at the moment of
 * the gesture: a foreground open is the front tab from the instant it is drawn,
 * so nothing has to be counted or guessed when the server answers.
 */
const tabs = new TabModel({
  send,
  adopted: (from, to) => {
    // The tab kept its frame and whatever was asked of it; both were filed
    // under a name it has now outgrown.
    const host = hosts.get(from);
    if (host) {
      hosts.delete(from);
      host.adopt(to);
      hosts.set(to, host);
    }
    progress.rekey(from, to);
    // An address typed into a tab before the server named it was filed under
    // the name it had then. History is written from where the tab arrives, and
    // the arrival will be under this one.
    const typed = typedAsks.get(from);
    if (typed !== undefined) {
      typedAsks.delete(from);
      typedAsks.set(to, typed);
    }
  },
  dropped: (id) => {
    hosts.get(id)?.destroy();
    hosts.delete(id);
    progress.forget(id);
    renderTabs();
    layout();
  },
});

const el = {
  strip: byId<HTMLDivElement>('tabstrip'),
  urlbar: byId<HTMLInputElement>('urlbar'),
  back: byId<HTMLButtonElement>('back'),
  forward: byId<HTMLButtonElement>('forward'),
  reload: byId<HTMLButtonElement>('reload'),
  bookmark: byId<HTMLButtonElement>('bookmark'),
  marks: byId<HTMLButtonElement>('marks'),
  chat: byId<HTMLButtonElement>('chat'),
  tabs: byId<HTMLButtonElement>('tabs'),
  more: byId<HTMLButtonElement>('more'),
  hud: byId<HTMLButtonElement>('hud'),
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
  about: byId<HTMLDialogElement>('about'),
  aboutRows: byId<HTMLElement>('about-rows'),
  aboutVerdict: byId<HTMLParagraphElement>('about-verdict'),
  aboutDetail: byId<HTMLParagraphElement>('about-detail'),
  aboutUpdate: byId<HTMLButtonElement>('about-update'),
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
  // Alongside it, and for the same reason: both lists are what the address bar
  // can offer before the link exists, and a completion that only starts working
  // once the transport is up would be missing at the one moment — a cold start
  // on a bad link — when typing an address is all there is to do.
  await visits.whenReady();

  // What the server said last time. It is the only answer available until the
  // link comes back, and on this link that can be the whole flight.
  serverVersions = await store.readVersions() ?? serverVersions;

  const pairing = await pairingFromURL() ?? await store.readPairing();
  // Read before the dialog below can send us down the pairing path, so both
  // ways into configure() carry the session this app left behind.
  resumeSession = await store.readSessionId();
  // Before the first Hello, because a scheme that arrived after a tab was built
  // costs that tab a re-snapshot to apply. See Tab.setColorScheme.
  schemePref = await store.readScheme();
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

/**
 * What the landside tab is laid out against.
 *
 * The `mobile` flag is the difference between a site's phone layout and its
 * desktop one squeezed into 393 pixels, and it is not a small difference: it is
 * whether Chromium honours the page's own `<meta name=viewport>` at all. Left
 * false — as it was — a phone reader got a 1000-pixel-wide desktop page
 * rendered into a 393-pixel window and mirrored at that scale, which is the
 * one failure this client cannot pinch-zoom its way out of.
 *
 * Both halves of the question have to be true. A touch laptop is a real device
 * with a real desktop screen, and telling the server it is a phone would trade
 * a good layout for a narrow one; a desktop window dragged narrow is still
 * being read by somebody who asked for the desktop web.
 */
function viewport(): Viewport {
  const rect = el.frames.getBoundingClientRect();
  return {
    w: Math.max(320, Math.round(rect.width)),
    h: Math.max(320, Math.round(rect.height)),
    dpr: window.devicePixelRatio || 1,
    mobile: isPhone() && isTouch(),
    scheme: schemePref || deviceScheme(),
  };
}

/**
 * What this device is set to, which is what "match this device" means.
 *
 * Sent rather than left blank so that the landside browser is put in the
 * reader's own scheme and paints the page there — the one arrangement in which
 * the reader gets the theme they prefer *and* the two sides agree about it,
 * because the server rendered the theme it sent. Left blank the server would
 * paint in its own scheme, which is nobody's.
 */
function deviceScheme(): string {
  return prefersDark() ? 'dark' : 'light';
}

// --------------------------------------------------------- worker -> app shell

function handle(kind: string, args: Record<string, unknown>): void {
  switch (kind) {
    case 'welcome': {
      const w = args as unknown as Welcome;
      // A different session hands out tab ids from one again, so everything
      // this side had learned to ignore is about tabs that no longer exist
      // anywhere — and the ids are about to be handed out afresh.
      const sameSession = w.sessionId === resumeSession;
      resumeSession = w.sessionId;
      void store.writeSessionId(w.sessionId);
      // Before the tabs, because it is the one thing in this frame that is not
      // about tabs: which build each half is, and whether they are the same.
      noteVersions(w);
      // The session's list is the truth for every tab but the ones asked for
      // on this connection, whose answers are still in flight. Anything else
      // this side holds is gone: a server restarted under a reconnect answers
      // with a session that never had those tabs, and a strip of tabs that
      // cannot be reached is worse than a short one — every click on them would
      // go to a tab id the server will refuse.
      tabs.reset(w.tabs ?? [], sameSession);
      for (const t of w.tabs ?? []) void hostFor(t.tab);
      renderTabs();
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
      void applyMutation(Number(args.tab), args.mutation as Mutation, Number(args.seq), Number(args.cause ?? 0));
      break;
    case 'tabState': {
      const st = args.state as TabState;
      if (st.error) log(`tab ${String(args.tab)}: ${st.error}`);
      // Adoption happens in here: a state carrying a ref renames the tab this
      // side already drew — frame, place in the strip and whether the reader
      // was sent to it — rather than adding a second one beside it.
      const before = tabs.get(Number(args.tab))?.url ?? '';
      const id = tabs.applyState(Number(args.tab), st);
      if (id === undefined) {
        renderTabs();
        layout();
        renderProgress();
        break;
      }
      // A tab we have never seen needs its frame now, not when its first
      // snapshot happens to arrive: a tab opened onto about:blank has nothing
      // to snapshot until it navigates somewhere.
      void hostFor(id);
      // Once the server says a tab is loading it is telling this side what it
      // had been guessing, and it is the better source.
      progress.serverLoading(id, st.loading);
      // Relative links in the mirror resolve against this; a tab that navigates
      // without a fresh snapshot would otherwise keep the old base.
      const view = tabs.get(id);
      if (view?.url) hosts.get(id)?.setPageUrl(view.url);
      // After the tab is complete, so the entry carries the title the strip is
      // about to show rather than the one it had a moment ago.
      if (view) recordArrival(view, before);
      renderTabs();
      layout();
      renderProgress();
      break;
    }
    case 'imageMeta':
      void hostFor(Number(args.tab)).then((h) => h?.setImageMeta(args.meta as ImageMeta));
      break;
    case 'imageData':
      void hostFor(Number(args.tab)).then((h) => h?.imageArrived(String(args.hash)));
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
      // One of these per transport that comes up, and the welcome for it is a
      // round trip behind. Tabs opened in that window belong to this
      // connection and must survive the welcome that follows.
      if (online) tabs.connectionUp();
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
    case 'download': {
      const d = args.download as unknown as Download;
      const before = transfers.ingest(d);
      if (d.state !== before) announceTransfer(d, before);
      renderTransfers();
      break;
    }
    case 'downloadProgress':
      transfers.progressed(String(args.id), Number(args.received));
      renderTransfers();
      break;
    case 'downloadDone': {
      const t = transfers.landed(String(args.id), args.data as Uint8Array, Number(args.size));
      if (t) {
        toast(`“${t.info.name}” is on this device.`,
          { label: 'Save', run: () => saveTransfer(t.info.id) });
      }
      renderTransfers();
      break;
    }
    case 'downloadError': {
      const t = transfers.failed(String(args.id), String(args.error ?? ''));
      if (t) toast(`Fetching “${t.info.name}” stopped: ${t.error}`);
      renderTransfers();
      break;
    }
    case 'clipboard':
      relayClipboard(String(args.text ?? ''));
      break;
    case 'log':
      log(String(args.message ?? ''));
      break;
    default:
      break;
  }
}

// ------------------------------------------------------------------ transfers

/**
 * The toasts that make a download visible without the panel being open: the
 * landing (which is the moment the old behavior showed nothing at all), the
 * file being ready with its price, and the failure. Each transition once.
 */
function announceTransfer(d: Download, before: string): void {
  switch (d.state) {
    case 'landing':
      if (before === '') {
        toast(`Downloading “${d.name}” on your server…`,
          { label: 'Transfers', run: () => openPanel('transfers') });
      }
      break;
    case 'ready': {
      const size = transfers.fmtSize(d.total);
      toast(size
        ? `“${d.name}” is on your server (${size}).`
        : `“${d.name}” is on your server.`,
      { label: size ? `Fetch (${size})` : 'Fetch', run: () => fetchTransfer(d.id) });
      break;
    }
    case 'failed':
      toast(`“${d.name}” failed to download on the server.`);
      break;
    default:
      break;
  }
}

const transferActions: transfers.TransferActions = {
  fetch: (id) => fetchTransfer(id),
  stop: (id) => {
    send('downloadStop', { id });
    transfers.stopped(id);
    renderTransfers();
  },
  discard: (id) => {
    send('downloadDiscard', { id });
  },
  save: (id) => saveTransfer(id),
};

function fetchTransfer(id: string): void {
  if (!connected) {
    toast('Offline: fetching a file needs the link. It stays safe on the server.');
    return;
  }
  transfers.fetching(id);
  send('downloadFetch', { id });
  renderTransfers();
}

/**
 * Hands a fetched file to the device's own downloads, named what the origin
 * called it. The Blob is this page's memory, so the reader is told to save
 * rather than left assuming the shell is an archive.
 */
function saveTransfer(id: string): void {
  const t = transfers.get(id);
  if (!t?.blob) return;
  const url = URL.createObjectURL(t.blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = t.info.name || 'download';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}

/** Redraws the panel body when it is showing transfers. */
function renderTransfers(): void {
  if (!el.panel.hidden && panelView === 'transfers') {
    transfers.render(el.panelBody, transferActions);
  }
}

// ------------------------------------------------------------------ clipboard

/**
 * A copy the page made landside, arriving a round trip after the click that
 * caused it (P-008). The write is tried at once — the reader's click usually
 * leaves a user-activation window wide enough to cover even this link's round
 * trip — and when the browser says no, the text is not lost: the toast offers
 * a Copy whose own click is the activation the retry needs.
 */
function relayClipboard(text: string): void {
  if (!text) return;
  const write = (): Promise<void> => navigator.clipboard.writeText(text);
  write().then(
    () => toast('The page copied text to your clipboard.'),
    () => toast('The page copied text for you.', {
      label: 'Copy',
      run: () => {
        write().then(
          () => toast('Copied.'),
          () => {
            // Last resort: the text must not vanish with the toast.
            log(`clipboard from the page: ${text}`);
            toast('The browser refused the clipboard; the text is in the log.');
          });
      },
    }),
  );
}

async function applySnapshot(tab: number, snap: Snapshot): Promise<void> {
  const host = await hostFor(tab);
  if (!host) return;
  // A whole new document arrived: whatever the open menu was pointing at is
  // gone, and half its entries would act on a node that no longer exists.
  if (tab === tabs.active) closeMenu();
  host.applySnapshot(snap);
  // The page the reader asked for is on screen. Waiting for the tab-state frame
  // behind it to say so would leave the bar running over a page that has
  // already arrived.
  if (progress.arrived(tab, snap.url)) renderProgress();
}

async function applyMutation(tab: number, m: Mutation, seq: number, cause = 0): Promise<void> {
  const host = await hostFor(tab);
  host?.applyMutation(m, seq, cause);
}

async function hostFor(tab: number): Promise<MirrorHost | null> {
  if (tabs.isClosed(tab)) return null;
  const existing = hosts.get(tab);
  if (existing) {
    await existing.whenReady();
    return existing;
  }
  const host = new MirrorHost(tab, {
    input: (t, ev) => tabs.forTab('input', t, { ...ev }),
    scroll: (t, ev) => tabs.forTab('scroll', t, { ...ev }),
    applied: (t, seq, hash, epoch) => tabs.forTab('ack', t, { seq, hash, epoch }),
    wantImages: (t, hashes) => tabs.forTab('wantImage', t, { hashes }),
    openLink: (_t, url) => openInNewTab(url),
    navigating: (t, url) => asking(t, 'Loading', url),
    menu: (t, target) => showMenu(target.x, target.y, mirrorMenu(t, target)),
    dismiss: () => {
      const was = menuIsOpen();
      closeMenu();
      // A sheet covers the page rather than sitting beside it, so touching the
      // page is how a phone says "not that, this". Reaching the × in the far
      // corner of a six-inch screen to get back to what is already on screen
      // is a gesture nobody makes twice.
      dismissSheet();
      return was;
    },
  });
  hosts.set(tab, host);
  el.frames.appendChild(host.frame);
  layout();
  await host.whenReady();
  return host;
}

// ------------------------------------------------------------------ tab strip

function renderTabs(): void {
  el.strip.textContent = '';
  for (const tab of tabs.list()) {
    const busy = isBusy(tab.id);
    const node = document.createElement('div');
    // A tab the server has not named yet is drawn like one that is not quite
    // real, because it is not quite real: it is a tab this side is holding
    // open until the answer arrives. Everything else about it works.
    node.className = `tab${tab.id === tabs.active ? ' active' : ''}`
      + `${busy ? ' loading' : ''}${tab.provisional ? ' ghost' : ''}`;
    if (tab.provisional) node.title = 'Waiting for the server to open this tab';
    node.setAttribute('role', 'tab');
    // The landside tab this row stands for, for the same reason the frame
    // carries it: a row in the strip is otherwise identified only by a title it
    // shares with every other tab on the same site.
    node.dataset.tab = String(tab.id);
    // Before the title, where a favicon goes and where a browser puts this: a
    // background tab fetching a page is the case the bar over the mirror cannot
    // show, because the mirror it would sit over is another tab's. The icon
    // takes the same seat once the page has one and the seat is free (P-104).
    if (busy) {
      node.appendChild(spinner());
    } else if (tab.favicon?.startsWith('data:image/')) {
      const ico = document.createElement('img');
      ico.className = 'favicon';
      ico.alt = '';
      ico.src = tab.favicon;
      node.appendChild(ico);
    }

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

    node.addEventListener('click', () => selectTab(tab.id));
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
  add.addEventListener('click', () => openTab('', 'focus'));
  el.strip.appendChild(add);
  syncTabsButton();
  syncToolbar();
}

/** Brings a tab to the front, from the strip or from the list that replaces it. */
function selectTab(id: number): void {
  tabs.select(id);
  // Switching tabs abandons a half-typed address: the bar belongs to whichever
  // tab is in front.
  urlbarEdited = false;
  renderTabs();
  renderProgress();
  layout();
  syncToolbar();
}

/**
 * The phone's whole tab strip: a count, and whether any of them is waiting.
 *
 * With the strip gone, this spinner is the only thing on screen that says a
 * background tab is still fetching — which on this link is a fact worth
 * minutes, because the answer to "is it here yet" decides whether the reader
 * switches to it or keeps reading.
 */
function syncTabsButton(): void {
  const count = tabs.size;
  el.tabs.textContent = String(count);
  const busy = tabs.ids().some(isBusy);
  el.tabs.classList.toggle('loading', busy);
  el.tabs.setAttribute(
    'aria-label',
    busy ? `${count} tabs, one still loading` : `${count} tabs`,
  );
  el.tabs.setAttribute('aria-expanded', String(!el.panel.hidden && panelView === 'tabs'));
  if (!el.panel.hidden && panelView === 'tabs') renderTabList();
}

function renderTabList(): void {
  tabList.render(
    tabs.list().map((t) => ({
      id: t.id, title: t.title, url: t.url, loading: isBusy(t.id),
    })),
    // Nothing here: a tab asked for and not yet named is in the list above,
    // with an id of its own and a place the reader can already reach.
    [],
    tabs.active,
    connected,
  );
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
  // The model sends the close, holding it if the tab has no server id yet, and
  // calls back to drop the frame and redraw.
  tabs.close(id);
  renderProgress();
}

/**
 * Whether the URL bar holds something the user typed and has not committed.
 * The bar tracks the tab, except while it is being edited — and *edited* is the
 * test, not *focused*: a new tab focuses the bar for the reader's convenience,
 * and a focused-but-untouched bar that stopped following its tab would sit
 * there showing the address of a page long since navigated away from.
 */
let urlbarEdited = false;

function setUrlbar(url: string): void {
  el.urlbar.value = addressText(url);
  urlbarEdited = false;
}

function syncToolbar(): void {
  const tab = tabs.current();
  const url = tab?.url ?? '';
  // A tab that has not been anywhere shows an empty address bar rather than
  // `about:blank`: the reader is about to type over it, and a placeholder that
  // has to be cleared first is a placeholder in the way.
  if (!urlbarEdited) setUrlbar(url);
  el.back.disabled = !tab?.canBack;
  el.forward.disabled = !tab?.canForward;
  syncReloadButton();
  // A back gesture let through earlier spent the trap. Now that there is a page
  // to go back to, it is worth keeping again.
  if (!centred && tab?.canBack) claimHistoryGestures();
  renderBookmarks();
}

/**
 * One button, wearing whichever of its two jobs is the useful one.
 *
 * Reload and stop are the same button in every browser because they are never
 * both wanted: a page that is coming can be stopped and not usefully reloaded,
 * and a page that has arrived is the other way round. Here the swap is worth
 * more than it is anywhere else — a page is minutes wide on this link, so the
 * moment the reader most wants a button is the moment reload is the wrong one —
 * and the toolbar is a phone's, with no room for a second.
 */
function syncReloadButton(): void {
  const busy = isBusy(tabs.active);
  el.reload.textContent = busy ? '✕' : '↻';
  el.reload.title = busy ? 'Stop' : 'Reload';
  el.reload.setAttribute('aria-label', busy ? 'Stop loading' : 'Reload');
  el.reload.classList.toggle('stop', busy);
  el.reload.disabled = !connected || !tabs.active;
}

/** A tab that has not been anywhere yet. `about:blank` is not a page. */
function isBlank(url: string): boolean {
  return !url || url.startsWith('about:');
}

/**
 * What the address bar reads while nobody is typing in it.
 *
 * On the phone shell that field is about two hundred pixels wide, and the
 * first fifty of them used to be spent on `https://` — a prefix that is the
 * same on every page the reader will ever see here, clipping the part that is
 * not: the host. So the scheme goes, the way every phone browser drops it, and
 * the field is left showing the thing the reader is checking.
 *
 * Only while it is not focused. The moment the caret lands, the real address
 * comes back (see below): what is edited, copied and sent on Enter must be the
 * URL and never a description of it.
 */
function addressText(url: string): string {
  if (isBlank(url)) return '';
  return isPhone() ? plainUrl(url) : url;
}

function layout(): void {
  for (const [id, host] of hosts) {
    host.frame.style.display = id === tabs.active ? 'block' : 'none';
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
  const busy = isBusy(tabs.active);
  el.progress.hidden = !busy;
  el.status.hidden = !busy;
  if (busy) el.status.textContent = busyLabel(tabs.active);
  for (const [id, host] of hosts) host.setBusy(isBusy(id));
  armSweep();
}

/** What the status line says: the verb the gesture meant, and where it points. */
function busyLabel(tab: number): string {
  const ask = progress.waiting(tab);
  // Only ever somewhere this side actually asked to go. A gesture whose
  // destination it does not know — back, forward, a form submitted — is
  // labelled with the verb alone, because the tab's current URL is the one
  // page the reader is definitely not going to, and it stays the tab's current
  // URL for the whole of the wait: the server confirms a navigation has
  // started long before it says where it landed.
  const url = ask?.url ?? progress.destination(tab);
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
  openTab(url);
}

/**
 * Opens a tab and, when the reader means to be in it, puts the cursor where
 * they were going to put it anyway. Everything here is plane-side and
 * immediate: the strip entry, the empty frame, the focused URL bar. The server
 * is told in the same breath, but nothing waits for it — a "+" that does
 * nothing for a second and a half is a "+" that gets pressed three times.
 * Every gesture that opens a tab comes through here.
 *
 * Which tab the reader lands in is settled here rather than when the server
 * answers: the + button and the menus mean "go there", a middle click on a link
 * means "leave it beside what I am reading", and only the gesture knows which.
 */
function openTab(url: string, where: 'background' | 'focus' = 'background'): number {
  // Only the server can make a page, so a tab asked for during an outage is a
  // tab that can never load anything. The "+" button is disabled offline for
  // the same reason; this covers the URL bar, which is reachable with no tabs
  // open at all.
  if (!connected) {
    log('offline: a new tab needs the link');
    return 0;
  }
  const background = where !== 'focus';
  const id = tabs.open(url, background);
  void hostFor(id);
  // The tab is real enough to wait on: it has an id of its own, so the spinner
  // and the status line can name what it is fetching before the server has
  // agreed the tab exists.
  if (url) asking(id, 'Opening', url);
  if (!background) {
    setUrlbar(url);
    el.urlbar.focus();
    el.urlbar.select();
  }
  renderTabs();
  layout();
  renderProgress();
  return id;
}

function navigateTo(tab: number, url: string): void {
  if (!tab) {
    openTab(url, 'focus');
    return;
  }
  // Held if the tab was opened less than a round trip ago and has no server id
  // yet — typing a URL straight into a brand new tab is the ordinary case.
  tabs.forTab('navigate', tab, { url });
  asking(tab, 'Loading', url);
}

/** Remembers that the next page this tab lands on was asked for by name. */
function markTyped(tab: number): void {
  typedAsks.set(tab, Date.now() + TYPED_TTL_MS);
}

/**
 * Whether the page a tab has just arrived at was one the reader typed. Consumes
 * the ask either way: an address is typed once, and a second page reached from
 * the first was reached by clicking something on it.
 */
function takeTyped(tab: number, first: boolean): boolean {
  const now = Date.now();
  // A tab arriving at its first page may be the one an address was typed into
  // with nothing open, which had no id to be filed under yet.
  const keys = first ? [tab, 0] : [tab];
  for (const key of keys) {
    const until = typedAsks.get(key);
    if (until === undefined) continue;
    typedAsks.delete(key);
    if (until >= now) return true;
  }
  return false;
}

/**
 * Files where a tab actually ended up, which is the only thing history is
 * written from.
 *
 * A tab reports itself several times per page — the URL first, the title when
 * the document has one — so a repeat of the address already recorded updates
 * the title rather than counting as another visit.
 */
function recordArrival(tab: TabView, before: string): void {
  if (isBlank(tab.url)) return;
  if (normalizeUrl(tab.url) === normalizeUrl(before)) {
    visits.retitle(tab.url, tab.title);
    return;
  }
  visits.record(tab.url, tab.title, takeTyped(tab.id, isBlank(before)));
}

/** Reloads a tab, from wherever the gesture came from. */
function reloadTab(tab: number): void {
  tabs.forTab('navigate', tab, { action: 'reload' });
  asking(tab, 'Reloading', tabs.get(tab)?.url);
}

/**
 * Stops a page that is still coming.
 *
 * Every other browser has had this button since 1994, and this one has more
 * need of it than any of them: a page here is minutes wide, and until now the
 * only way to call one off was to close the tab and lose whatever of it had
 * already arrived. What has landed stays on screen — the mirror is patched, not
 * replaced — so stopping a page half-drawn leaves a half-drawn page rather than
 * nothing.
 *
 * The waiting is forgotten straight away. The gesture is the reader saying they
 * are no longer waiting, and a bar that keeps running until the server answers
 * would be reporting a wait that has been called off.
 */
function stopTab(tab: number): void {
  if (!tab) return;
  send('navigate', { tab, action: 'stop' });
  if (progress.forget(tab)) renderProgress();
  const view = tabs.get(tab);
  if (view?.loading) {
    // The server confirms this a round trip from now. Until then the spinner
    // would go on turning over a page the reader has just stopped.
    // The model holds the view, so this is the strip's copy: setting it here
    // is what the redraw below reads.
    view.loading = false;
    renderTabs();
    renderProgress();
  }
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

/**
 * Where the reader has been. Separate from the saved list because it is a
 * different kind of fact — what they did, rather than what they chose to keep —
 * and because it is the one that answers "finish this address for me". Only the
 * address bar reads it.
 */
const visits = new History({
  read: () => store.readHistory(),
  write: (entries) => store.writeHistory(entries),
  onError: (message) => log(message),
});

// Writes are batched, so the last second of them would otherwise go down with
// the page. Nothing else here needs saving at this point: everything but the
// two local lists is landside and outlives the tab.
window.addEventListener('pagehide', () => visits.flush());

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

/**
 * The phone's tab strip. Selecting one closes the sheet: the reader asked for
 * a page, and leaving the list over it would cover the thing they asked for.
 */
const tabList = new TabList({
  select: (id) => {
    selectTab(id);
    closePanel();
  },
  close: (id) => closeTab(id),
  stop: (id) => stopTab(id),
  menu: (id, x, y) => showMenu(x, y, tabMenu(id)),
  open: () => {
    openTab('', 'focus');
    closePanel();
  },
});

const suggest = new Suggest(el.urlbar, {
  source: (query, limit) => completions(bookmarks.all(), visits.all(), query, limit),
  pick: (row) => openCompletion(row),
  forget: (row) => forgetVisit(row.entry),
});

bookmarks.onChange(() => renderBookmarks());

/**
 * Opens what the dropdown offered. A saved row goes through the bookmark path
 * so that opening from the address bar counts as a use — the saved list is
 * ordered by that, and a reader who reaches a bookmark by typing three letters
 * of it has used it exactly as much as one who clicked it in the panel.
 */
function openCompletion(row: Completion): void {
  if (row.kind === 'saved') {
    openBookmark(row.mark, 'here');
    return;
  }
  if (!connected) {
    toast('Offline: opening a page needs the link. The list stays readable.');
    return;
  }
  // Picking a completion is naming a destination at the address bar, which is
  // the same act as typing the whole thing — so it counts as one, and the row
  // the reader keeps choosing keeps rising.
  markTyped(tabs.active);
  navigateTo(tabs.active, row.url);
}

/**
 * Drops one address out of the completions. No confirmation, a notice with the
 * way back: the same bargain the star and the saved list make, and the right
 * one for a gesture whose whole value is being quick enough to use while
 * typing.
 */
function forgetVisit(entry: HistoryEntry): void {
  const gone = visits.forget(entry.url);
  if (!gone) return;
  toast(`Removed “${gone.title}” from history.`, {
    label: 'Undo',
    run: () => visits.restore([gone]),
  });
}

/** Empties it. The undo is the whole list, held until the notice goes away. */
function clearHistory(): void {
  const gone = visits.clear();
  if (!gone.length) {
    toast('There is no history to clear.');
    return;
  }
  toast(`Cleared ${gone.length} address(es) from history.`, {
    label: 'Undo',
    run: () => visits.restore(gone),
  });
}

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
  if (isBusy(tabs.active)) return false;
  const tab = tabs.current();
  if (!tab) return true;
  return isBlank(tab.url);
}

function syncBookmarkButton(): void {
  const url = tabs.current()?.url ?? '';
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
function toggleBookmark(id = tabs.active): void {
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
  navigateTo(tabs.active, mark.url);
}

/** The menu on a saved row, in either view. */
function bookmarkMenu(mark: Bookmark, x: number, y: number, from: 'panel' | 'start'): void {
  showMenu(x, y, [
    [
      { label: 'Open', disabled: !connected, run: () => openBookmark(mark, 'here') },
      {
        label: 'Open in new tab',
        chord: 'Middle click',
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
      // The same swap the toolbar button makes, for the same reason: a page
      // that is still coming is one the reader may want called off, and this
      // menu is the whole of the chrome on a phone in landscape.
      isBusy(tab) ? {
        label: 'Stop',
        disabled: !connected || !tab,
        run: () => stopTab(tab),
      } : {
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
        chord: 'Ctrl+D',
        disabled: isBlank(url),
        run: () => toggleBookmark(tab),
      },
      { label: 'Saved pages…', chord: 'Ctrl+B', run: () => showPanel('marks') },
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
        chord: 'Middle click',
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
    const imageGroup: MenuGroups[number] = [
      { label: 'Save image', run: () => void saveImage(image, target.imageAlt ?? '') },
      {
        label: 'Copy image description',
        disabled: !target.imageAlt,
        run: () => copyText(target.imageAlt ?? ''),
      },
    ];
    if (target.imageAnim) {
      // The still is the design; the tap is the ask (P-118). The hint is the
      // cost, the way every entry that spends the link says so.
      imageGroup.unshift({
        label: 'Play animation',
        hint: 'fetches the original',
        run: () => hosts.get(tab)?.playAnimated(image),
      });
    }
    groups.push(imageGroup);
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
      isBusy(id) ? {
        label: 'Stop',
        disabled: !connected,
        run: () => stopTab(id),
      } : {
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
          for (const other of tabs.ids()) {
            if (other !== id) closeTab(other);
          }
        },
      },
    ],
  ];
}

/**
 * Which scheme the server renders in.
 *
 * The hint is not decoration. Changing this re-renders every open tab landside
 * and re-sends its document, because a stylesheet is a delta that this side only
 * appends to and the rules already sent were written under the other answer —
 * so this costs a page each, over the link that is the whole problem. A reader
 * is owed that before they tap it, not after.
 */
function schemeGroup(): MenuItem[] {
  const choices: Array<[string, string]> = [
    ['', 'Match this device'],
    ['light', 'Light'],
    ['dark', 'Dark'],
  ];
  return choices.map(([value, label]) => ({
    label: schemePref === value ? `\u2713 ${label}` : `\u2007\u2007${label}`,
    hint: schemePref === value ? undefined : 'resends open pages',
    disabled: !connected && schemePref !== value,
    run: () => setScheme(value),
  }));
}

function setScheme(value: string): void {
  if (value === schemePref) return;
  schemePref = value;
  void store.writeScheme(value);
  send('viewport', { viewport: viewport() });
}

/** The menu for a right click on the chrome itself. */
function shellMenu(): MenuGroups {
  const groups: MenuGroups = [
    [{ label: 'New tab', disabled: !connected, run: () => openTab('', 'focus') }],
  ];
  if (tabs.active) groups.push(...pageGroups(tabs.active));
  // The bulk counterpart to the X on a single completion. It lives here rather
  // than in the dropdown because a list you are typing at is the wrong place to
  // put a gesture that empties it, and because the reader who wants it wants it
  // when they are not mid-address.
  groups.push(schemeGroup());
  groups.push([{
    label: 'Clear history',
    disabled: !visits.count(),
    run: () => clearHistory(),
  }]);
  // Files the server is holding, and the way back to one whose toast has
  // already gone. Disabled while there is nothing: an empty list teaches less
  // than a menu entry that comes alive when a download exists.
  groups.push([{
    label: 'Transfers…',
    disabled: !transfers.any(),
    run: () => showPanel('transfers'),
  }]);
  // Last, and always present: the one entry that works with the link down, and
  // the only way to find out that the app itself is behind the server. It says
  // which of the two it is in the label, because a reader who has an update
  // waiting should not have to open a dialog to discover it.
  groups.push([{
    label: needsUpdate(verdict(versionSides())) ? 'Update Skyhook…' : 'Skyhook versions…',
    run: () => openAbout(),
  }]);
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

// The full address while it is being worked on, the short one while it is only
// being read. Selecting it as well is what the field being tapped means on a
// phone: the reader who wanted to edit one character can still do it, and the
// far more common one who is replacing the whole address has it gone already.
el.urlbar.addEventListener('focus', () => {
  const url = tabs.current()?.url ?? '';
  if (isBlank(url) || el.urlbar.value === url) return;
  el.urlbar.value = url;
  el.urlbar.select();
});

// Typing is what stops the bar following its tab; focus alone is not.
el.urlbar.addEventListener('input', () => {
  urlbarEdited = true;
});

el.urlbar.addEventListener('blur', () => {
  urlbarEdited = false;
  // Not over something half-typed: a reader who tapped away mid-address is
  // coming back to it, and replacing it with where they currently are would
  // throw the typing away.
  const url = tabs.current()?.url ?? '';
  if (el.urlbar.value === url) el.urlbar.value = addressText(url);
});

el.urlbar.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter') return;
  const url = el.urlbar.value.trim();
  if (!url) return;
  suggest.close();
  // The address has been sent, so the field is done with. Left focused on a
  // phone it keeps the on-screen keyboard up over the bottom half of a page
  // that is about to cost seconds to arrive, and holds the caret at the end of
  // a URL whose beginning — the host — is the part worth looking at while
  // waiting for it.
  if (isPhone()) el.urlbar.blur();
  // Committed: the bar goes back to following the tab it belongs to.
  urlbarEdited = false;
  // Before the navigation, because with no tab open that call creates one and
  // the arrival has to find the ask already filed.
  markTyped(tabs.active);
  navigateTo(tabs.active, url);
});

el.back.addEventListener('click', () => goHistory(tabs.active, 'back'));
el.forward.addEventListener('click', () => goHistory(tabs.active, 'forward'));
el.reload.addEventListener('click', () => {
  if (isBusy(tabs.active)) stopTab(tabs.active);
  else reloadTab(tabs.active);
});
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
    if (goHistory(tabs.active, 'back')) history.forward();
    else centred = false;
    return;
  }
  if (state === HISTORY_STATES.forward) {
    goHistory(tabs.active, 'forward');
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
    // Telling the reader to reload was advice that could not work. This app is
    // served by its own service worker out of its own cache; a reload fetches
    // nothing and comes back as the same build, refused the same way. The
    // versions dialog is the only place where the update can actually happen.
    log('server refused the connection: client and server speak different protocols');
    versionRefused = true;
    if (!el.pairing.open) openAbout();
    return;
  }
  log('server refused the pairing token');
  el.pairingError.textContent =
    'The server no longer accepts this pairing. It has most likely been restarted with a '
    + 'new token: open a fresh pairing link, or paste the current pairing.json below.';
  if (!el.pairing.open) el.pairing.showModal();
}

// ------------------------------------------------------------------ versions
//
// Two halves that ship together, and one of them is a PWA: it runs whatever its
// service worker cached, it starts with no network at all — which is the entire
// point of it — and every route by which it might ask for something newer is
// answered out of that same cache. A deploy landside therefore changes nothing
// plane-side, and there is no symptom. The reader is simply, quietly, on last
// month's client, meeting bugs that were fixed weeks ago.
//
// So each half states what it is. The app's build id is compiled into its bytes;
// the server names the build it would hand out today in every Welcome. Where
// they differ the reader is told once and offered the download — offered, not
// given, because it is a few hundred kilobytes over a link that charges seconds
// for them, and nothing in this client spends the link on its own initiative.

/** What the server last told us. Persisted, so an offline start still knows. */
let serverVersions = { server: '', clientVersion: '', clientBuild: '' };
/** Set when the server hung up because the two speak different protocols. */
let versionRefused = false;
/** The served build already announced, so a reconnect does not say it again. */
let announcedBuild = '';

function versionSides(): { build: string; servedBuild: string; refused: boolean } {
  return { build: BUILD, servedBuild: serverVersions.clientBuild, refused: versionRefused };
}

/**
 * Whether an update could even be fetched right now.
 *
 * Not simply `connected`: a client refused over the protocol version has no
 * session and is not connected by any measure this shell uses, and is also the
 * one client that most needs the button — the server has just answered it, so
 * the link is demonstrably there.
 */
function canFetchUpdate(): boolean {
  return connected || versionRefused;
}

/** Records what a Welcome said, and says so once if it is not what we are. */
function noteVersions(w: Welcome): void {
  versionRefused = false;
  serverVersions = {
    server: w.server ?? '',
    clientVersion: w.clientVersion ?? '',
    clientBuild: w.clientBuild ?? '',
  };
  void store.writeVersions(serverVersions);
  if (el.about.open) renderAbout();
  if (!needsUpdate(verdict(versionSides()))) return;
  if (announcedBuild === serverVersions.clientBuild) return;
  announcedBuild = serverVersions.clientBuild;
  log(`server serves client build ${serverVersions.clientBuild}; this app is ${BUILD}`);
  toast('Skyhook can be updated to the build the server is serving.',
    { label: 'Details', run: () => openAbout() });
}

function openAbout(): void {
  renderAbout();
  if (!el.about.open) el.about.showModal();
}

function renderAbout(): void {
  const v = verdict(versionSides());
  el.aboutRows.textContent = '';
  const row = (term: string, value: string, stale = false): void => {
    const dt = document.createElement('dt');
    dt.textContent = term;
    const dd = document.createElement('dd');
    if (stale) {
      const mark = document.createElement('span');
      mark.className = 'stale';
      mark.textContent = value;
      dd.appendChild(mark);
    } else {
      dd.textContent = value;
    }
    el.aboutRows.append(dt, dd);
  };
  row('This app', `${VERSION} · ${BUILD}`, v === 'mismatch');
  row('Server', serverVersions.server || 'not connected yet');
  row("Server's app", serverVersions.clientBuild
    ? `${serverVersions.clientVersion || '?'} · ${serverVersions.clientBuild}`
    : 'not connected yet');
  row('Protocol', String(PROTOCOL_VERSION));
  el.aboutVerdict.textContent = versionHeadline(v);
  el.aboutDetail.textContent = versionDetail(v, canFetchUpdate());
  el.aboutUpdate.hidden = !needsUpdate(v);
  el.aboutUpdate.disabled = !canFetchUpdate();
  el.aboutUpdate.textContent = 'Update now';
}

/**
 * Fetches the new shell and reloads onto it, reporting whichever way it goes.
 *
 * The reader has pressed a button that costs bytes on a link where bytes are
 * the currency, so it says what is happening while it happens — and says so
 * afterwards when there was nothing to fetch, because a button that silently
 * does nothing is indistinguishable from one that is broken.
 */
/**
 * Built once, on load, rather than when the button is pressed.
 *
 * It watches for another worker taking this page over, and that can happen
 * before anybody presses anything: the browser runs its own update check on
 * navigation. An env created at the moment of the click would have missed it,
 * and missing it is the case where the page is running code its own cache no
 * longer holds.
 */
const updater = browserEnv();

async function runUpdate(): Promise<void> {
  el.aboutUpdate.disabled = true;
  el.aboutUpdate.textContent = 'Updating…';
  el.aboutDetail.textContent = 'Fetching the app from the server. The page reloads itself '
    + 'onto the new build as soon as it has arrived, which over this link may be a while.';
  const outcome = await installUpdate(updater);
  if (outcome === 'reloading') return;
  el.aboutUpdate.disabled = false;
  el.aboutUpdate.textContent = 'Try again';
  if (outcome === 'unchanged') {
    el.aboutVerdict.textContent = 'The server had nothing newer to send.';
    el.aboutDetail.textContent = 'The app was re-fetched and is the build already installed, '
      + 'so nothing was replaced. If the server has just been updated, its new client may not '
      + 'be deployed beside it yet.';
    return;
  }
  el.aboutVerdict.textContent = 'The update could not be fetched.';
  el.aboutDetail.textContent = 'The request did not reach the server. The link is the usual '
    + 'reason; nothing was changed, and this build goes on working.';
}

el.aboutUpdate.addEventListener('click', () => { void runUpdate(); });

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
  placeToast();
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(hideToast, 8000);
}

/**
 * Keeps the notice clear of the sheet.
 *
 * Both live at the bottom of a phone, which is where a thumb is and therefore
 * where both belong; a notice pinned there while a sheet is open lands on the
 * sheet's last row, and the last row of the saved list is the count and the
 * two buttons that get the list off the device. So the notice stands on top of
 * the sheet rather than in front of it. On a wide screen the panel is a column
 * beside the page and the corner is free, so nothing moves.
 */
function placeToast(): void {
  const sheet = isPhone() && !el.panel.hidden ? el.panel.getBoundingClientRect().height : 0;
  el.toast.style.bottom = sheet ? `calc(${Math.round(sheet)}px + 12px)` : '';
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

/**
 * The parity probe, reachable from outside the app the way a capture is
 * reachable from the server: the e2e parity suite evaluates this through CDP
 * and holds the answer against the landside agent's own probe. Always on and
 * read-only, the same posture as `__skyhook` landside — it reads the mirror
 * the shell already holds, in the shell's own trust domain, and changes
 * nothing.
 */
declare global {
  interface Window { __skyhookParity?: (tab?: number) => Record<string, unknown> | null }
}
window.__skyhookParity = (tab?: number): Record<string, unknown> | null => {
  const id = tab ?? tabs.active;
  return hosts.get(id)?.parityProbe() ?? null;
};

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
    activeTab: tabs.active,
    tabs: tabs.list().map((t) => ({
      tab: t.id, url: t.url, title: t.title, loading: t.loading,
      canBack: t.canBack, canForward: t.canForward,
      hasFrame: hosts.has(t.id),
      nodes: hosts.get(t.id)?.nodes ?? 0,
      // A page the reader is still waiting for is the other half of "this looks
      // wrong": a capture taken mid-navigation is a picture of the old page.
      waitingFor: waitReport(progress.waiting(t.id)),
    })),
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

type PanelView = 'chat' | 'marks' | 'tabs' | 'transfers';
let panelView: PanelView = 'chat';

const PANEL_TITLES: Record<PanelView, string> = {
  chat: 'Chat', marks: 'Saved pages', tabs: 'Tabs', transfers: 'Transfers',
};

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
  } else if (view === 'tabs') {
    el.panelBody.appendChild(tabList.root);
    renderTabList();
  } else if (view === 'transfers') {
    transfers.render(el.panelBody, transferActions);
  } else {
    el.panelBody.appendChild(marksPanel.root);
    marksPanel.render(bookmarks.all(), connected);
    // Not on a phone: the on-screen keyboard would come up over the list the
    // reader opened the sheet to look at, and there is no Escape key to send
    // it away again.
    if (!isPhone()) marksPanel.focusSearch();
  }
  syncBookmarkButton();
  syncTabsButton();
  // The sheet has just taken the bottom of the screen; a notice already down
  // there is now standing on the list it is about to be read against.
  placeToast();
}

function closePanel(): void {
  el.panel.hidden = true;
  syncBookmarkButton();
  syncTabsButton();
  placeToast();
}

/**
 * Closes the panel when it is a sheet and the reader has touched what it is
 * covering. On a wide screen the panel sits beside the page rather than over
 * it, so nothing about touching the page means "put that away".
 */
function dismissSheet(): void {
  if (isPhone() && !el.panel.hidden) closePanel();
}

// The other half of the same gesture. A touch inside a mirror frame never
// reaches this document — it is answered by the host's dismiss hook above —
// but the start page, the empty area beside a short page and the chrome
// itself all do.
document.addEventListener('pointerdown', (ev) => {
  if (el.panel.hidden || !isPhone()) return;
  const target = ev.target as HTMLElement | null;
  // Not the panel itself, and not the buttons that open it: those toggle, and
  // closing here first would make the second half of the toggle reopen it.
  if (target?.closest('#panel, #tabs, #more, .menu')) return;
  closePanel();
}, true);

el.chat.addEventListener('click', () => showPanel('chat'));
el.panelClose.addEventListener('click', () => closePanel());
el.tabs.addEventListener('click', () => showPanel('tabs'));

/**
 * The phone's ⋯, which carries everything the one-row toolbar had to drop.
 *
 * The shell menu already offers the saved list — it is one of the entries a
 * right click has always had — so the only thing this adds is chat, whose
 * button is the other one off the toolbar when there is room for four controls
 * and not eight.
 */
el.more.addEventListener('click', () => {
  const rect = el.more.getBoundingClientRect();
  showMenu(rect.left, rect.bottom + 4, [
    ...shellMenu(),
    [{ label: 'Chat', run: () => showPanel('chat') }],
  ]);
});

/**
 * The three numbers the phone HUD drops, on request.
 *
 * Collapsing the HUD to a coloured dot is the right trade for a glance — is
 * the link there — and the wrong one for the moment a reader is deciding
 * whether a page is worth asking for. That decision is made in round trips and
 * kilobytes, so the dot has to be able to say them, and saying them costs
 * nothing: every figure is already plane-side.
 */
el.hud.addEventListener('click', () => {
  if (!isPhone()) return;
  const state = el.hudState.textContent ?? '';
  const reason = el.hudState.title;
  const detail = [state, el.hudRtt.textContent, el.hudQueue.textContent, el.hudBytes.textContent]
    .filter(Boolean).join(' · ');
  toast(reason ? `${detail} — ${reason}` : detail);
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

document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && !ev.defaultPrevented && !el.panel.hidden) {
    // Not when a menu just took it: the menu handles Escape in the capture
    // phase and marks it handled, and closing the panel out from under a menu
    // dismissal is two things happening for one key.
    closePanel();
    return;
  }
  // Escape is what a browser's stop button has answered to since there were
  // browsers, and it is worth more here: the reader who wants a page called off
  // is the reader who has been staring at a spinner for a minute. Only when
  // there is nothing else for it to mean — a panel or a menu takes it first —
  // and only over a page that is actually coming, so it never quietly does
  // something on a page that has arrived.
  if (ev.key === 'Escape' && !ev.defaultPrevented && isBusy(tabs.active)) {
    stopTab(tabs.active);
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
