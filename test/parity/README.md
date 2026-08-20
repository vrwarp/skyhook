# The parity corpus

This directory is the measured catalogue of how faithfully the plane-side
mirror renders what the landside browser is showing. `docs/PARITY.md` explains
the system; this file is the rules for working in this directory.

## Layout

```
gaps.json                 the gap registry: every known divergence, with a status
corpus/<group>/<name>/    one page: page.html + manifest.json + any assets
baselines/<group>--<name>.json   the ratchet: the page's last locked-in measurement
```

Run it with `make test-parity` (needs a browser and `client/dist`). Lock in a
deliberate change with `make parity-baseline` and commit the diff — the diff
is the review artifact.

## Writing a page

- **One page, one question.** A page exercises one feature area, prints a
  unique `waitText` marker, and stays still once loaded: the runner's settle
  barrier treats a page that never quiets as a bug, not a flake.
- **Say your ground.** Set `html { color; font-family }` and `body { margin }`
  explicitly unless the page is specifically measuring UA-default inheritance
  (P-119) or margin collapse at the boundary (P-120) — those have pages of
  their own, and their noise must not leak into yours.
- **Prove effects with landside text.** Page script runs landside only, so
  the way a page proves an interaction arrived is by writing a unique string
  into the document and letting the mirror deliver it.
- **Positive control before the measurement.** A page measuring a gap first
  proves its own wiring with an interaction that works, so a dead page cannot
  be mistaken for the gap.
- **Catalogue every failure.** A gated dimension that fails must carry an
  `expectedFail` entry naming a gap in `gaps.json`; the runner enforces both
  directions, so a fixed gap is claimed rather than silently enjoyed.
- **`{{SITE}}` and `{{CDN}}`** resolve to the page's own origin and to a
  second, genuinely cross-origin one. Files served from the CDN are listed in
  the manifest's `serve.cdn`.
- Imported real pages live under `corpus/real/` and must carry an
  `attribution`.
