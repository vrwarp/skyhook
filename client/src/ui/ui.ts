/**
 * The chrome UI: tab strip, URL bar, link-health HUD, and the chat panel that
 * renders the adapter archive locally.
 *
 * No framework. The UI is a few hundred lines of DOM manipulation, and a
 * framework would be more bytes of runtime than the entire mirror protocol.
 */
import type { AdapterRecord, Stats, TabRef, TabState, Welcome } from '../shared/protocol.js';

declare global {
  interface Window {
    skyhookUI: {
      call(action: string, args?: Record<string, unknown>): Promise<unknown>;
      on(fn: (ev: { kind: string; args: Record<string, unknown> }) => void): void;
    };
  }
}

const ui = window.skyhookUI;

interface TabView {
  id: number;
  url: string;
  title: string;
  loading: boolean;
  canBack: boolean;
  canForward: boolean;
}

const tabs = new Map<number, TabView>();
let active = 0;
const archive: AdapterRecord[] = [];
const spaces = new Map<string, { name: string; unread: number }>();
let currentSpace = '';

const el = {
  strip: document.getElementById('tabstrip') as HTMLDivElement,
  urlbar: document.getElementById('urlbar') as HTMLInputElement,
  back: document.getElementById('back') as HTMLButtonElement,
  forward: document.getElementById('forward') as HTMLButtonElement,
  reload: document.getElementById('reload') as HTMLButtonElement,
  bookmark: document.getElementById('bookmark') as HTMLButtonElement,
  chat: document.getElementById('chat') as HTMLButtonElement,
  panel: document.getElementById('panel') as HTMLElement,
  panelBody: document.getElementById('panel-body') as HTMLDivElement,
  panelClose: document.getElementById('panel-close') as HTMLButtonElement,
  hudState: document.getElementById('hud-state') as HTMLSpanElement,
  hudRtt: document.getElementById('hud-rtt') as HTMLSpanElement,
  hudQueue: document.getElementById('hud-queue') as HTMLSpanElement,
  hudBytes: document.getElementById('hud-bytes') as HTMLSpanElement,
};

// ------------------------------------------------------------------ tab strip

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
      void ui.call('closeTab', { tab: tab.id });
    });
    node.appendChild(close);

    node.addEventListener('click', () => {
      active = tab.id;
      void ui.call('selectTab', { tab: tab.id });
      renderTabs();
      syncToolbar();
    });
    el.strip.appendChild(node);
  }
  const add = document.createElement('button');
  add.id = 'newtab';
  add.textContent = '+';
  add.title = 'New tab';
  add.addEventListener('click', () => void ui.call('openTab', { url: '' }));
  el.strip.appendChild(add);
}

function syncToolbar(): void {
  const tab = tabs.get(active);
  el.urlbar.value = tab?.url ?? '';
  el.back.disabled = !tab?.canBack;
  el.forward.disabled = !tab?.canForward;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return '';
  }
}

el.urlbar.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter') return;
  const url = el.urlbar.value.trim();
  if (!url) return;
  if (!active) {
    void ui.call('openTab', { url });
    return;
  }
  void ui.call('navigate', { tab: active, url });
});

el.back.addEventListener('click', () => void ui.call('navigate', { tab: active, action: 'back' }));
el.forward.addEventListener('click', () => void ui.call('navigate', { tab: active, action: 'forward' }));
el.reload.addEventListener('click', () => void ui.call('navigate', { tab: active, action: 'reload' }));
el.bookmark.addEventListener('click', async () => {
  const tab = tabs.get(active);
  if (!tab) return;
  const marks = (await ui.call('bookmarks')) as { title: string; url: string }[];
  marks.push({ title: tab.title || tab.url, url: tab.url });
  await ui.call('setBookmarks', { bookmarks: marks });
});

// ------------------------------------------------------------------- the HUD

function renderStats(s: Partial<Stats> & { rttMs?: number }): void {
  const rttMs = s.rttMs ?? Math.round((s.rttMicros ?? 0) / 1000);
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

// ------------------------------------------------------------- the chat panel

el.chat.addEventListener('click', () => {
  el.panel.hidden = !el.panel.hidden;
  if (!el.panel.hidden) void openChat();
});
el.panelClose.addEventListener('click', () => {
  el.panel.hidden = true;
});

async function openChat(): Promise<void> {
  // Cold open comes from the local archive, not from the network: this is the
  // difference between a chat that opens in 300 ms and one that opens in 8 s.
  const stored = (await ui.call('archive')) as AdapterRecord[];
  for (const r of stored) ingestRecord(r, false);
  renderChat();
  void ui.call('adapter', { adapter: 'googlechat', cmd: 'sync', since: lastSeq() });
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
    // An arriving message retires the optimistic ghost that stood for it.
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
      void ui.call('adapter', { adapter: 'googlechat', cmd: 'open', space: id });
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
  const send = document.createElement('button');
  send.textContent = 'Send';

  const submit = (): void => {
    const text = input.value.trim();
    if (!text) return;
    input.value = '';
    // Optimistic: the message appears now, and is retired when the server's
    // authoritative copy arrives. Sending feels instant despite the RTT.
    const ghost = document.createElement('div');
    ghost.className = 'msg pending';
    ghost.textContent = text;
    el.panelBody.insertBefore(ghost, wrap);
    pending.set(text, ghost);
    void ui.call('adapter', {
      adapter: 'googlechat', cmd: 'send', space: currentSpace, text,
      localId: `local-${Date.now()}`,
    });
  };

  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') submit();
  });
  send.addEventListener('click', submit);
  wrap.appendChild(input);
  wrap.appendChild(send);
  return wrap;
}

// ------------------------------------------------------------------- events

ui.on(({ kind, args }) => {
  switch (kind) {
    case 'welcome': {
      const w = args as unknown as Welcome;
      for (const t of w.tabs ?? []) upsertTab(t);
      renderTabs();
      syncToolbar();
      break;
    }
    case 'tabState': {
      const id = Number(args.tab);
      const st = args.state as TabState;
      const tab = tabs.get(id) ?? { id, url: '', title: '', loading: false, canBack: false, canForward: false };
      tab.url = st.url || tab.url;
      tab.title = st.title || tab.title;
      tab.loading = st.loading;
      tab.canBack = st.canBack;
      tab.canForward = st.canForward;
      tabs.set(id, tab);
      if (!active) active = id;
      renderTabs();
      syncToolbar();
      break;
    }
    case 'activeTab':
      active = Number(args.tab);
      renderTabs();
      syncToolbar();
      break;
    case 'tabClosed':
      tabs.delete(Number(args.tab));
      renderTabs();
      syncToolbar();
      break;
    case 'status':
      renderStatus(args.online === true, String(args.kind ?? ''), args.reason as string | undefined);
      break;
    case 'stats':
      renderStats(args as unknown as Stats);
      break;
    case 'adapter': {
      const records = (args.records ?? []) as AdapterRecord[];
      for (const r of records) ingestRecord(r, true);
      if (!el.panel.hidden) renderChat();
      break;
    }
    default:
      break;
  }
});

function upsertTab(t: TabRef): void {
  tabs.set(t.tab, {
    id: t.tab, url: t.url, title: t.title, loading: t.loading,
    canBack: false, canForward: false,
  });
  if (t.active) active = t.tab;
}

window.addEventListener('resize', () => void ui.call('layout'));

// Kick the layout once at start so the mirror view is positioned correctly.
void ui.call('layout');
