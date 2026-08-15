/**
 * Which build is running on each side of the link, and how to move this side
 * onto the other's.
 *
 * The problem is particular to this client. It is a PWA whose service worker
 * answers every request for the app out of a cache it filled once, on purpose:
 * the app has to start with no network at all, because it is opened at 35,000
 * feet. The consequence is that "reload the page" does not fetch anything, a
 * deploy landside changes nothing plane-side, and a reader can spend a week on
 * a build the server replaced before they took off — with no symptom except a
 * bug that was fixed a fortnight ago.
 *
 * So the two halves say what they are. The app's build id is compiled into its
 * bytes (see shared/build.ts), the server names the build it is serving in
 * every Welcome frame, and the comparison happens here. Nothing updates by
 * itself: the update costs a shell download over a link that charges seconds
 * for it, so it is offered and not taken.
 */

/** Where the two halves stand. `unknown` is a client that has never connected. */
export type Verdict = 'match' | 'mismatch' | 'unknown' | 'incompatible';

/** What the comparison is made from. */
export interface Sides {
  /** The build id compiled into this app. */
  build: string;
  /** The build id the server says it is serving; empty if it never said. */
  servedBuild: string;
  /**
   * Set when the server hung up over the protocol version. That is the same
   * disagreement seen from the other end and one version worse: the halves
   * cannot talk at all, so there is no Welcome and no served build to compare —
   * only the refusal itself, which is evidence enough.
   */
  refused?: boolean;
}

/**
 * Whether this app is the app the server would hand out.
 *
 * An empty served build is deliberately never a mismatch. A server built before
 * this mechanism existed, or one serving an app it has no stamp for, says
 * nothing — and answering "nothing" with "you are out of date" would put an
 * update prompt in front of every reader of every such deployment, forever.
 */
export function verdict(s: Sides): Verdict {
  if (s.refused) return 'incompatible';
  if (!s.servedBuild || !s.build) return 'unknown';
  return s.servedBuild === s.build ? 'match' : 'mismatch';
}

/** Whether there is anything for the reader to do about it. */
export function needsUpdate(v: Verdict): boolean {
  return v === 'mismatch' || v === 'incompatible';
}

/**
 * One line for each verdict, in the terms the reader is in.
 *
 * "Different" rather than "newer", because the server is not always ahead: a
 * deployment rolled back is the same disagreement with the arrow reversed, and
 * the build the server is serving is the build it wants read either way.
 */
export function headline(v: Verdict): string {
  switch (v) {
    case 'match':
      return 'Up to date.';
    case 'mismatch':
      return 'The server is serving a different build of this app.';
    case 'incompatible':
      return 'This app and the server cannot talk to each other.';
    default:
      return 'There is nothing to compare this build against.';
  }
}

/** The sentence under it: what it costs and what happens. */
export function detail(v: Verdict, online: boolean): string {
  if (!needsUpdate(v)) {
    return v === 'match'
      ? 'This is the build the server would hand out.'
      : 'The server has not said which build of the app it serves — either this '
        + 'client has not connected yet, or the server is older than this check.';
  }
  const what = v === 'incompatible'
    ? 'The server refused this client: the two speak different versions of the '
      + 'protocol, and only an update fixes that.'
    : 'Skyhook keeps running on the build you have. Updating is worth doing when '
      + 'the link is cheap and not much fun when it is not.';
  return online
    ? `${what} Updating downloads the app again — a few hundred kilobytes, once.`
    : `${what} It needs the link, which is down: try again once you are connected.`;
}

// --------------------------------------------------------------- the upgrade

/** The part of a ServiceWorkerRegistration this needs, so a test can fake it. */
export interface UpdatableRegistration {
  update(): Promise<unknown>;
  readonly installing: { postMessage(message: unknown): void } | null;
  readonly waiting: { postMessage(message: unknown): void } | null;
}

/** Everything about the browser that the upgrade touches. */
export interface UpdateEnv {
  /** This page's registration, or null where there are no service workers. */
  registration(): Promise<UpdatableRegistration | null>;
  /** Resolves true if another worker takes control within ms, false if not. */
  waitForControl(ms: number): Promise<boolean>;
  /** Whether a new worker has already taken this page over. */
  tookControl(): boolean;
  /** Reloads, which is what actually swaps the running app for the new one. */
  reload(): void;
}

export type Outcome =
  /** A new build is in place; the page is reloading onto it. */
  | 'reloading'
  /** The server had nothing newer. */
  | 'unchanged'
  /** The fetch did not get there. Almost always the link. */
  | 'failed';

/** How long to wait for the new worker to take over before reloading anyway. */
const CONTROL_TIMEOUT_MS = 60_000;

/**
 * Fetches the new shell and reloads onto it.
 *
 * The dance is browser-imposed and worth spelling out, because every step of it
 * is a way this silently does nothing:
 *
 *  1. `update()` re-fetches the worker script. It is the one request in this
 *     app that deliberately goes past the cache, and it is what discovers that
 *     a new build exists at all — the worker's bytes carry the build id, so
 *     they differ exactly when the shell does.
 *  2. Installing the new worker precaches the new shell under a new cache name,
 *     whole, before any of it is served. Half a generation is what produces one
 *     build's markup drawn with another's stylesheet.
 *  3. `skipWaiting` is sent anyway, though the worker calls it itself: a worker
 *     that was already sitting in `waiting` from an earlier visit never runs
 *     that install again, and would otherwise stay parked until every tab of
 *     this app was closed — which on an installed PWA can be days.
 *  4. Only after the new worker controls the page does a reload get the new
 *     shell. Reloading first is what makes an update appear not to work, twice
 *     in a row, before it takes.
 */
export async function installUpdate(
  env: UpdateEnv, timeoutMs = CONTROL_TIMEOUT_MS,
): Promise<Outcome> {
  const reg = await env.registration();
  if (!reg) {
    // No service worker means no cache in the way: the page is served over the
    // network and a reload is the whole update.
    env.reload();
    return 'reloading';
  }
  try {
    await reg.update();
  } catch {
    return 'failed';
  }
  const pending = reg.installing ?? reg.waiting;
  if (!pending) {
    // Nothing new was fetched. Unless a worker took the page over while we were
    // asking — which happens when the browser ran its own update check on load,
    // and leaves this page running code the cache no longer holds.
    if (!env.tookControl()) return 'unchanged';
    env.reload();
    return 'reloading';
  }
  pending.postMessage({ kind: 'skip-waiting' });
  await env.waitForControl(timeoutMs);
  // Reloading even on the timeout is deliberate: the worker may have activated
  // between the timeout firing and this line, and a reload that turns out to be
  // unnecessary costs nothing — the shell it loads comes from a cache.
  env.reload();
  return 'reloading';
}

/** The real browser, wired up. */
export function browserEnv(): UpdateEnv {
  const container = navigator.serviceWorker as ServiceWorkerContainer | undefined;
  let changed = false;
  container?.addEventListener('controllerchange', () => { changed = true; });
  return {
    async registration(): Promise<UpdatableRegistration | null> {
      if (!container) return null;
      return (await container.getRegistration('/')) ?? null;
    },
    waitForControl(ms: number): Promise<boolean> {
      if (!container) return Promise.resolve(false);
      return new Promise<boolean>((resolve) => {
        const done = (value: boolean): void => {
          clearTimeout(timer);
          container.removeEventListener('controllerchange', onChange);
          resolve(value);
        };
        const onChange = (): void => done(true);
        const timer = setTimeout(() => done(false), ms);
        container.addEventListener('controllerchange', onChange);
      });
    },
    tookControl: () => changed,
    reload: () => location.reload(),
  };
}
