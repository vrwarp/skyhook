/**
 * Preload for the chrome UI window. The UI is ordinary local HTML; it never
 * touches the network or the filesystem directly.
 */
import { contextBridge, ipcRenderer } from 'electron';

contextBridge.exposeInMainWorld('skyhookUI', {
  call: (action: string, args: Record<string, unknown> = {}) =>
    ipcRenderer.invoke('skyhook:ui', { action, args }),
  on: (fn: (ev: { kind: string; args: Record<string, unknown> }) => void) => {
    ipcRenderer.on('skyhook:ui-event', (_e, ev) => fn(ev));
  },
});
