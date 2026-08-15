/**
 * Which build is which, and the service-worker dance that swaps one for the
 * other.
 *
 * Both halves of this are easy to get wrong in ways nothing notices. A
 * comparison that treats "the server did not say" as "you are out of date"
 * puts an update prompt in front of every reader forever; an upgrade that
 * reloads before the new worker has taken over reloads onto the old cache, so
 * pressing Update appears to do nothing at all — twice — before it works.
 */
import { describe, expect, it, vi } from 'vitest';

import {
  installUpdate, needsUpdate, verdict, type UpdatableRegistration, type UpdateEnv,
} from '../src/app/upgrade.js';

describe('verdict', () => {
  it('matches when the server serves the build this app is', () => {
    expect(verdict({ build: 'abc', servedBuild: 'abc' })).toBe('match');
    expect(needsUpdate('match')).toBe(false);
  });

  it('is a mismatch when the server has moved on', () => {
    expect(verdict({ build: 'abc', servedBuild: 'def' })).toBe('mismatch');
    expect(needsUpdate('mismatch')).toBe(true);
  });

  // A server built before any of this existed says nothing about the app it
  // serves. Reading that as "out of date" would nag every reader of every such
  // deployment, forever, about an update that does not exist.
  it('says nothing when the server said nothing', () => {
    expect(verdict({ build: 'abc', servedBuild: '' })).toBe('unknown');
    expect(needsUpdate('unknown')).toBe(false);
  });

  // A refusal over the protocol version is the same disagreement one step
  // worse: there is no Welcome, so there is no served build to compare — the
  // refusal itself is the evidence.
  it('trusts a refusal over a missing comparison', () => {
    expect(verdict({ build: 'abc', servedBuild: '', refused: true })).toBe('incompatible');
    expect(needsUpdate('incompatible')).toBe(true);
  });
});

/** A registration whose worker fields the test controls. */
function fakeRegistration(pending: 'installing' | 'waiting' | 'none', fail = false): {
  reg: UpdatableRegistration; messages: unknown[]; updates: number;
} {
  const messages: unknown[] = [];
  const worker = { postMessage: (m: unknown): void => { messages.push(m); } };
  let updates = 0;
  const reg: UpdatableRegistration = {
    update: (): Promise<unknown> => {
      updates += 1;
      return fail ? Promise.reject(new Error('offline')) : Promise.resolve(null);
    },
    installing: pending === 'installing' ? worker : null,
    waiting: pending === 'waiting' ? worker : null,
  };
  return { reg, messages, get updates() { return updates; } };
}

function fakeEnv(over: Partial<UpdateEnv> = {}): UpdateEnv & { reloads: number } {
  const state = {
    reloads: 0,
    registration: (): Promise<UpdatableRegistration | null> => Promise.resolve(null),
    waitForControl: (): Promise<boolean> => Promise.resolve(true),
    tookControl: (): boolean => false,
    reload: (): void => { state.reloads += 1; },
    ...over,
  };
  return state as UpdateEnv & { reloads: number };
}

describe('installUpdate', () => {
  it('sends the waiting worker past the queue and reloads once it has control', async () => {
    const { reg, messages } = fakeRegistration('waiting');
    const control = vi.fn(() => Promise.resolve(true));
    const env = fakeEnv({ registration: () => Promise.resolve(reg), waitForControl: control });

    expect(await installUpdate(env)).toBe('reloading');
    // A worker parked in `waiting` from an earlier visit never runs install
    // again, so it never calls skipWaiting on its own: without this message it
    // stays parked until every tab of the app is closed.
    expect(messages).toEqual([{ kind: 'skip-waiting' }]);
    expect(control).toHaveBeenCalled();
    expect(env.reloads).toBe(1);
  });

  it('waits for the worker before reloading, not after', async () => {
    const { reg } = fakeRegistration('installing');
    const order: string[] = [];
    const env = fakeEnv({
      registration: () => Promise.resolve(reg),
      waitForControl: async () => { order.push('control'); return true; },
      reload: () => { order.push('reload'); },
    });

    await installUpdate(env);
    // The other order is the bug this whole flow exists to avoid: reloading
    // first serves the old shell out of the old cache, and the update looks
    // like it did nothing.
    expect(order).toEqual(['control', 'reload']);
  });

  it('reloads anyway when the worker takes too long', async () => {
    const { reg } = fakeRegistration('waiting');
    const env = fakeEnv({
      registration: () => Promise.resolve(reg),
      waitForControl: () => Promise.resolve(false),
    });

    expect(await installUpdate(env)).toBe('reloading');
    expect(env.reloads).toBe(1);
  });

  it('says so when the server had nothing newer', async () => {
    const { reg } = fakeRegistration('none');
    const env = fakeEnv({ registration: () => Promise.resolve(reg) });

    expect(await installUpdate(env)).toBe('unchanged');
    expect(env.reloads).toBe(0);
  });

  // The browser runs its own update check on navigation. When that one won, the
  // page is running code the cache no longer holds and nothing is pending — but
  // a reload is exactly what is needed.
  it('reloads when a worker took over while nobody was looking', async () => {
    const { reg } = fakeRegistration('none');
    const env = fakeEnv({
      registration: () => Promise.resolve(reg),
      tookControl: () => true,
    });

    expect(await installUpdate(env)).toBe('reloading');
    expect(env.reloads).toBe(1);
  });

  it('reports a fetch that never got there', async () => {
    const { reg } = fakeRegistration('waiting', true);
    const env = fakeEnv({ registration: () => Promise.resolve(reg) });

    expect(await installUpdate(env)).toBe('failed');
    // Nothing was changed and nothing was reloaded: the app the reader has is
    // the app that goes on working.
    expect(env.reloads).toBe(0);
  });

  it('just reloads where there is no service worker to get in the way', async () => {
    const env = fakeEnv({ registration: () => Promise.resolve(null) });
    expect(await installUpdate(env)).toBe('reloading');
    expect(env.reloads).toBe(1);
  });
});
