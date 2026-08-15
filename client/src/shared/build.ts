/**
 * Which build of the plane-side app this is.
 *
 * Both values are substituted by esbuild (see esbuild.mjs) and therefore live
 * in the bytes themselves. That is not an implementation detail, it is the
 * whole mechanism: this app is served by a service worker out of a cache it
 * filled on some earlier flight, and every route by which it could *ask* what
 * it is — fetching a manifest, reading a version file — is answered by that
 * same cache. An app that reads its own version over HTTP is reading a copy of
 * the answer it already had.
 *
 * So the build id travels in the code, and the only fresher opinion comes from
 * the server, over the live connection, in the Welcome frame. Comparing the two
 * is the whole of `upgrade.ts`.
 *
 * The `typeof` guards are for the test suite, which imports these modules
 * without going through esbuild and would otherwise hit an undeclared global.
 */
declare const SKYHOOK_BUILD: string;
declare const SKYHOOK_VERSION: string;

/** The generation of the shell: a hash of the files this app is made of. */
export const BUILD: string = typeof SKYHOOK_BUILD === 'string' ? SKYHOOK_BUILD : 'dev';

/** The version beside it, from package.json. Human-facing; not compared. */
export const VERSION: string = typeof SKYHOOK_VERSION === 'string' ? SKYHOOK_VERSION : '0.0.0-dev';

/** What the Hello frame calls this client, and what the server's log shows. */
export const CLIENT_ID = `skyhook-pwa/${VERSION}`;
