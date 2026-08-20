/**
 * The Transfers panel: every download the server has announced, and what this
 * device has done about each (P-108).
 *
 * A download has two legs and the panel says so plainly. The first leg is
 * landside — the origin's file arriving on the server at datacenter speed —
 * and costs the link nothing. The second is the expensive one: the same bytes
 * crossing the bad link, which happens only when the reader asks, with the
 * size printed on the button doing the asking. That is the shell's standing
 * grammar — nothing expensive without a cost label — applied to files.
 *
 * State lives here rather than in the shell so the rows and the toasts read
 * from one ledger. Nothing is persisted: fetched bytes are held as a Blob
 * until saved, and a reload starts the second leg over — the landside copy is
 * what survives, and re-fetching it is one ask.
 */
import type { Download } from '../shared/protocol.js';

/** One download, as the server tells it and as this device holds it. */
export interface Transfer {
  info: Download;
  /** The link leg: nothing asked, parts arriving, or all bytes in hand. */
  leg: 'idle' | 'fetching' | 'held';
  /** Bytes this device holds of it, counted from offset zero. */
  received: number;
  blob?: Blob;
  error?: string;
}

/** Everything a row can ask the shell to do. */
export interface TransferActions {
  fetch(id: string): void;
  stop(id: string): void;
  discard(id: string): void;
  save(id: string): void;
}

const transfers = new Map<string, Transfer>();

/** The server said: a download exists, or moved. Returns the previous state
 * string so the shell can toast transitions and only transitions. */
export function ingest(d: Download): string {
  const t = transfers.get(d.id);
  const before = t?.info.state ?? '';
  if (t) {
    t.info = d;
    if (d.state === 'gone' || d.state === 'failed') {
      // The landside copy is no longer fetchable; a fetch leg in flight is
      // over, but bytes already held stay held until the row is dropped.
      if (t.leg === 'fetching') t.leg = t.blob ? 'held' : 'idle';
    }
  } else {
    transfers.set(d.id, { info: d, leg: 'idle', received: 0 });
  }
  prune();
  return before;
}

/** Parts are crossing the link. */
export function progressed(id: string, received: number): void {
  const t = transfers.get(id);
  if (!t) return;
  t.leg = 'fetching';
  t.received = received;
}

/** The whole file is on this device. */
export function landed(id: string, data: Uint8Array, size: number): Transfer | undefined {
  const t = transfers.get(id);
  if (!t) return undefined;
  t.leg = 'held';
  t.received = size;
  t.error = undefined;
  t.blob = new Blob([data as BlobPart]);
  return t;
}

/** The fetch stream said no, or broke. */
export function failed(id: string, error: string): Transfer | undefined {
  const t = transfers.get(id);
  if (!t) return undefined;
  t.leg = t.blob ? 'held' : 'idle';
  t.error = error;
  return t;
}

/** The reader called the fetch off; whatever arrived stays counted, and the
 * next fetch resumes from it. */
export function stopped(id: string): void {
  const t = transfers.get(id);
  if (!t) return;
  if (t.leg === 'fetching') t.leg = t.blob ? 'held' : 'idle';
  t.error = undefined;
}

/** The shell asked; remember the leg so the row can offer Stop. */
export function fetching(id: string): void {
  const t = transfers.get(id);
  if (!t) return;
  t.leg = 'fetching';
  t.error = undefined;
}

export function get(id: string): Transfer | undefined {
  return transfers.get(id);
}

/** True when the panel has anything to show. */
export function any(): boolean {
  return transfers.size > 0;
}

/** Newest last, the order the server announced them in. */
function all(): Transfer[] {
  return Array.from(transfers.values());
}

/** Rows that are over — gone from the shelf with nothing held here — age out
 * so the panel stays the working set rather than a history. */
function prune(): void {
  for (const [id, t] of transfers) {
    if (t.info.state === 'gone' && !t.blob) transfers.delete(id);
  }
  while (transfers.size > 40) {
    const oldest = transfers.keys().next().value;
    if (oldest === undefined) break;
    transfers.delete(oldest);
  }
}

/** Bytes for a human, one decimal at most. */
export function fmtSize(n: number): string {
  if (!n || n < 0) return '';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} kB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

/** What the announcement is worth saying out loud, per state. */
function stateLine(t: Transfer): string {
  const { info } = t;
  switch (info.state) {
    case 'landing':
      return info.total
        ? `${fmtSize(info.received)} of ${fmtSize(info.total)} arriving on the server`
        : `${fmtSize(info.received) || 'arriving'} on the server`;
    case 'ready':
      switch (t.leg) {
        case 'fetching':
          return `${fmtSize(t.received)} of ${fmtSize(info.total)} over the link`;
        case 'held':
          return 'on this device — save it to keep it';
        default:
          return t.received > 0
            ? `paused at ${fmtSize(t.received)} of ${fmtSize(info.total)}`
            : `${fmtSize(info.total)} on the server, not fetched`;
      }
    case 'failed':
      return 'the download failed on the server';
    case 'gone':
      return t.blob ? 'discarded on the server; still held here' : 'discarded';
    default:
      return info.state;
  }
}

function button(label: string, run: () => void): HTMLButtonElement {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'transfer-act';
  b.textContent = label;
  b.addEventListener('click', run);
  return b;
}

function row(t: Transfer, actions: TransferActions): HTMLElement {
  const el = document.createElement('div');
  el.className = 'transfer';
  el.dataset.id = t.info.id;

  const name = document.createElement('div');
  name.className = 'transfer-name';
  name.textContent = t.info.name || 'download';
  name.title = t.info.url;
  el.appendChild(name);

  const state = document.createElement('div');
  state.className = 'transfer-state';
  state.textContent = t.error ? `${stateLine(t)} — ${t.error}` : stateLine(t);
  el.appendChild(state);

  const acts = document.createElement('div');
  acts.className = 'transfer-actions';
  const { info } = t;
  if (info.state === 'ready' && t.leg === 'idle') {
    // The ask that spends the link carries its price — what is left of it,
    // when a stopped fetch already paid for part.
    const left = Math.max(0, info.total - t.received);
    const price = fmtSize(left);
    const label = t.received > 0
      ? (price ? `Resume (${price})` : 'Resume')
      : (price ? `Fetch (${price})` : 'Fetch');
    acts.appendChild(button(label, () => actions.fetch(info.id)));
  }
  if (t.leg === 'fetching') {
    acts.appendChild(button('Stop', () => actions.stop(info.id)));
  }
  if (t.leg === 'held' && t.blob) {
    acts.appendChild(button('Save', () => actions.save(info.id)));
  }
  if (info.state !== 'gone') {
    acts.appendChild(button('Discard', () => actions.discard(info.id)));
  }
  el.appendChild(acts);
  return el;
}

/** Builds the panel body fresh. Small lists, no diffing to get wrong. */
export function render(root: HTMLElement, actions: TransferActions): void {
  root.textContent = '';
  const list = all();
  if (!list.length) {
    const empty = document.createElement('p');
    empty.className = 'transfer-empty';
    empty.textContent = 'Nothing has been downloaded. When a page starts a '
      + 'download it lands on your server and is offered here first.';
    root.appendChild(empty);
    return;
  }
  for (const t of list) root.appendChild(row(t, actions));
}
