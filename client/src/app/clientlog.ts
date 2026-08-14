/**
 * The plane-side log ring.
 *
 * The landside half of a capture carries the server's log because the server
 * has one. This side has a console nobody is looking at — the reader is on a
 * phone at 35,000 feet, and by the time anyone opens devtools the session is
 * over — so the client keeps its own last few hundred lines in memory, and a
 * capture carries them up.
 *
 * Uncaught errors and rejected promises are folded in here too. A mirror that
 * stopped updating because the patcher threw is the single most useful thing
 * this file can record, and it is exactly the thing that leaves no other trace.
 */

const LIMIT = 500;
const lines: string[] = [];
let dropped = 0;

/** Records one line, stamped with time since load. */
export function record(level: string, message: string): void {
  const at = Math.round(performance.now());
  lines.push(`${String(at).padStart(8)}ms ${level} ${message}`);
  if (lines.length > LIMIT) {
    lines.shift();
    dropped += 1;
  }
}

/** The ring as a log file, newest last. */
export function text(): string {
  const head = dropped > 0
    ? `[${dropped} earlier line(s) dropped: this ring holds ${LIMIT}]\n`
    : '';
  return head + lines.join('\n') + '\n';
}

/** How many lines aged out. */
export function droppedCount(): number {
  return dropped;
}

/**
 * Starts folding the window's own failures into the ring. Called once at
 * startup; safe to call again.
 */
let installed = false;
export function install(): void {
  if (installed || typeof window === 'undefined') return;
  installed = true;
  window.addEventListener('error', (ev) => {
    const err = ev as ErrorEvent;
    record('error', `uncaught ${String(err.message)} at ${err.filename}:${err.lineno}`);
  });
  window.addEventListener('unhandledrejection', (ev) => {
    record('error', `unhandled rejection: ${String((ev as PromiseRejectionEvent).reason)}`);
  });
}
