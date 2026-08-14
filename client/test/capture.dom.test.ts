/**
 * Plane-side capture tests.
 *
 * jsdom has no canvas and no Cache Storage, which is most of what the
 * screenshot path needs — so what is asserted here is everything that has to
 * work whether or not a picture can be taken: that the mirrored document, the
 * patcher's state and the fingerprint are gathered, that the upload budget is
 * respected, and that a failure to rasterise costs the screenshot rather than
 * the whole capture. The rasteriser itself is exercised against a real browser
 * by the e2e suite.
 */
// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { gunzipSync } from 'node:zlib';

import { gather, type CaptureArtifact } from '../src/app/capture.js';
import type { MirrorFreeze } from '../src/mirror/host.js';
import type { CaptureRequest } from '../src/shared/protocol.js';

function freeze(tab: number, html: string, over: Partial<MirrorFreeze> = {}): MirrorFreeze {
  return {
    tab,
    html,
    images: [],
    width: 800,
    height: 600,
    docHeight: 1200,
    scrollX: 0,
    scrollY: 240,
    state: { tab, url: `https://example.test/${tab}`, lastAppliedSeq: 12 },
    fingerprint: { total: 2, truncated: false, nodes: [[1, 1, 'html', 0], [2, 3, 'hello', 0]] },
    ...over,
  };
}

function request(over: Partial<CaptureRequest> = {}): CaptureRequest {
  return {
    id: 'cap-1', reason: 'manual', note: 'the body stopped updating',
    tabs: [1], maxBytes: 1 << 20, screenshots: false, ...over,
  };
}

/** Undoes whatever the gatherer did to an artifact so it can be read. */
function body(a: CaptureArtifact): string {
  if (a.name.endsWith('.gz')) return gunzipSync(Buffer.from(a.data)).toString('utf8');
  return Buffer.from(a.data).toString('utf8');
}

function byName(artifacts: CaptureArtifact[], name: string): CaptureArtifact | undefined {
  return artifacts.find((a) => a.name === name || a.name === `${name}.gz`);
}

describe('plane-side capture', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  it('gathers the mirrored document, the state and the fingerprint per tab', async () => {
    const artifacts = await gather({
      request: request(),
      frozen: [freeze(1, '<html><body><p>mirrored</p></body></html>')],
      shell: { connected: true },
    });

    const html = byName(artifacts, 'tabs/1/mirror.html');
    expect(html, 'the mirrored document is the artifact that explains the bug').toBeDefined();
    expect(body(html!)).toContain('mirrored');

    const state = byName(artifacts, 'tabs/1/state.json');
    expect(state).toBeDefined();
    expect(JSON.parse(body(state!)).lastAppliedSeq).toBe(12);

    const fp = byName(artifacts, 'tabs/1/fingerprint.json');
    expect(fp).toBeDefined();
    expect(JSON.parse(body(fp!)).nodes).toHaveLength(2);
  });

  it('reports what this device and this build are', async () => {
    const artifacts = await gather({
      request: request(),
      frozen: [],
      shell: { connected: true, activeTab: 3 },
    });
    const client = byName(artifacts, 'client.json');
    expect(client).toBeDefined();
    const report = JSON.parse(body(client!)) as Record<string, unknown>;
    expect(report.captureId).toBe('cap-1');
    expect(report.note).toBe('the body stopped updating');
    expect((report.shell as Record<string, unknown>).activeTab).toBe(3);
    // A bundle is a thing people send to each other. The pairing token is the
    // whole of this client's credential and has no business in one.
    expect(JSON.stringify(report)).not.toContain('token');
  });

  it('always carries the client log, so a patcher that threw is visible', async () => {
    const artifacts = await gather({ request: request(), frozen: [], shell: {} });
    expect(byName(artifacts, 'client.log')).toBeDefined();
  });

  it('stops at the upload budget rather than flooding the link', async () => {
    const big = `<html><body>${'<div>x</div>'.repeat(20000)}</body></html>`;
    const artifacts = await gather({
      request: request({ maxBytes: 4000 }),
      frozen: [freeze(1, big)],
      shell: {},
    });
    const total = artifacts.reduce((n, a) => n + a.data.length, 0);
    expect(total).toBeLessThanOrEqual(4000);
    expect(byName(artifacts, 'tabs/1/mirror.html'),
      'a document far over budget must not be sent').toBeUndefined();
    // The cheap, valuable artifacts still made it: gathering in order of value
    // is what makes a truncated capture worth having at all.
    expect(byName(artifacts, 'client.json')).toBeDefined();
    expect(byName(artifacts, 'tabs/1/state.json')).toBeDefined();
  });

  it('records a tab it could not freeze instead of dropping it silently', async () => {
    const artifacts = await gather({
      request: request(),
      frozen: [freeze(1, '', { error: 'this tab has no patchable document' })],
      shell: {},
    });
    const log = byName(artifacts, 'client.log');
    expect(body(log!)).toContain('no patchable document');
    // No document to send, so no mirror.html: an empty one would read as a
    // mirror that rendered nothing.
    expect(byName(artifacts, 'tabs/1/mirror.html')).toBeUndefined();
  });

  it('survives a browser that cannot rasterise', async () => {
    // jsdom's canvas has no 2d context, which is exactly the failure this has
    // to absorb: the screenshot is the expendable artifact.
    const artifacts = await gather({
      request: request({ screenshots: true }),
      frozen: [freeze(1, '<html><body><p>mirrored</p></body></html>')],
      shell: {},
    });
    expect(byName(artifacts, 'tabs/1/mirror.html')).toBeDefined();
    const log = byName(artifacts, 'client.log');
    expect(body(log!)).toMatch(/capture:/);
  });
});
