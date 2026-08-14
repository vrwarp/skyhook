/**
 * Preload for the network worker window: the only bridge between the transport
 * and the main process. Everything it exposes is a plain data channel; no Node
 * API reaches the worker itself.
 */
import { contextBridge, ipcRenderer } from 'electron';

function toMain(kind: string, args: Record<string, unknown>): void {
  ipcRenderer.send('skyhook:net', { kind, args });
}

contextBridge.exposeInMainWorld('skyhookNet', {
  ready: () => toMain('ready', {}),
  status: (s: Record<string, unknown>) => toMain('status', s),
  log: (message: string) => toMain('log', { message }),
  welcome: (w: unknown) => toMain('welcome', w as Record<string, unknown>),
  snapshot: (tab: number, snapshot: unknown) => toMain('snapshot', { tab, snapshot }),
  speculative: (tab: number, snapshot: unknown) => toMain('speculative', { tab, snapshot }),
  mutation: (tab: number, seq: number, cause: number, mutation: unknown) =>
    toMain('mutation', { tab, seq, cause, mutation }),
  tabState: (tab: number, state: unknown) => toMain('tabState', { tab, state }),
  imageMeta: (tab: number, meta: unknown) => toMain('imageMeta', { tab, meta }),
  imageData: (tab: number, hash: string, mime: string, data: Uint8Array) =>
    toMain('imageData', { tab, hash, mime, data }),
  adapter: (records: unknown, backlog: boolean) => toMain('adapter', { records, backlog }),
  stats: (stats: unknown) => toMain('stats', stats as Record<string, unknown>),
  onCommand: (fn: (cmd: { name: string; args: Record<string, unknown> }) => void) => {
    ipcRenderer.on('skyhook:command', (_e, cmd) => fn(cmd));
  },
});
