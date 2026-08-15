// Package appver reads the build stamp of the plane-side app this server is
// serving.
//
// The client's build writes `version.json` next to the shell it belongs to (see
// client/esbuild.mjs): a semantic version from package.json, and a build id
// that is a hash of the shell files themselves. The id is the interesting half.
// It changes exactly when the app changes, it is what the client's service
// worker names its cache after, and it is compiled into the app's own bytes —
// so a client can say which build it is running and this server can say which
// build it would hand out, and the two are comparable.
//
// They have to be comparable because nothing else about a PWA is. The app in a
// browser is whatever the service worker cached, possibly weeks ago and on
// another continent; asking the server for the app returns that same cached
// copy, because answering out of the cache is the service worker's entire job.
// A version the server states over the live connection is the one channel the
// cache does not sit in front of.
package appver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StampFile is the name the client's build writes, at the root of the web app.
const StampFile = "version.json"

// Stamp is the build stamp of one generation of the client app.
type Stamp struct {
	Version string `json:"version"`
	Build   string `json:"build"`
	Built   string `json:"built,omitempty"`
}

// Known reports whether the stamp says anything useful. An app built before
// stamping existed, or a web root with no app in it, produces a blank one — and
// a blank build id must never be compared against a client's, or every client
// would be told it is out of date.
func (s Stamp) Known() bool { return s.Build != "" }

// String renders the stamp the way both halves' logs and the client's own
// "about" panel do: a version with the build id that disambiguates it.
func (s Stamp) String() string {
	switch {
	case s.Version != "" && s.Build != "":
		return s.Version + " (" + s.Build + ")"
	case s.Build != "":
		return s.Build
	default:
		return s.Version
	}
}

// Reader serves the current stamp, re-reading the file when it changes.
//
// It is re-read rather than read once at startup because a deploy replaces the
// web root under a running server — that is what `docker compose up -d` with a
// mounted build directory does, and what every `npm run build` during
// development does. A server that answered from a stamp it read at boot would
// tell clients they were stale forever after, or up to date forever after,
// depending on which side of the deploy it started on.
type Reader struct {
	root string

	mu    sync.Mutex
	stamp Stamp
	size  int64
	mod   time.Time
	read  bool
}

// NewReader watches the app at root. An empty root — no client built, or none
// found — yields a reader that always reports an unknown stamp.
func NewReader(root string) *Reader { return &Reader{root: root} }

// Stamp returns the build stamp of the app currently on disk.
func (r *Reader) Stamp() Stamp {
	if r == nil || r.root == "" {
		return Stamp{}
	}
	path := filepath.Join(r.root, StampFile)

	r.mu.Lock()
	defer r.mu.Unlock()

	st, err := os.Stat(path)
	if err != nil {
		// A missing stamp is not an error worth propagating: it means an app
		// built before stamping existed, or no app at all. Either way the
		// honest answer is "unknown", which is the one thing a client will not
		// act on.
		r.stamp, r.read, r.size, r.mod = Stamp{}, true, 0, time.Time{}
		return r.stamp
	}
	if r.read && st.Size() == r.size && st.ModTime().Equal(r.mod) {
		return r.stamp
	}

	r.size, r.mod, r.read = st.Size(), st.ModTime(), true
	r.stamp = Stamp{}
	data, err := os.ReadFile(path) //nolint:gosec // fixed name under a configured root
	if err != nil {
		return r.stamp
	}
	var s Stamp
	if err := json.Unmarshal(data, &s); err != nil {
		return r.stamp
	}
	r.stamp = s
	return r.stamp
}
