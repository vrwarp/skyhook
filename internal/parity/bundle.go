package parity

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// A Bundle is one diagnostic capture, opened for reading. The writer lives in
// internal/diag and internal/session; until this file existed nothing in the
// tree could open one, and triage was a documented manual procedure
// (docs/OPERATIONS.md). The reader is deliberately tolerant: a bundle is
// gathered from a live system under a deadline, and half a bundle is evidence
// too — every reader here answers "absent" rather than failing the open.
type Bundle struct {
	files map[string][]byte
	// Manifest is the bundle's own manifest.json, untyped: triage passes
	// through what it finds rather than insisting on a schema, because old
	// bundles are exactly the ones worth reading. Untyped means JSON
	// numbers surface as float64 — today the manifest carries none that
	// matter, but a 64-bit hash read through this map would round (the
	// state.json hashes rotted exactly that way once); decode through a
	// typed struct instead if you ever need one exactly.
	Manifest map[string]any
}

// OpenBundle reads a capture zip into memory. Bundles are capped at tens of
// megabytes by the capture pipeline, so memory is the simple and right place
// for one.
func OpenBundle(name string) (*Bundle, error) {
	r, err := zip.OpenReader(name)
	if err != nil {
		return nil, fmt.Errorf("parity: open bundle: %w", err)
	}
	defer r.Close()
	b := &Bundle{files: map[string][]byte{}}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("parity: %s: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("parity: %s: %w", f.Name, err)
		}
		b.files[f.Name] = data
	}
	if raw, ok := b.files["manifest.json"]; ok {
		_ = json.Unmarshal(raw, &b.Manifest)
	}
	return b, nil
}

// File returns one entry's bytes, or nil.
func (b *Bundle) File(name string) []byte { return b.files[name] }

// JSON decodes one entry into out, reporting whether it was present and well
// formed.
func (b *Bundle) JSON(name string, out any) bool {
	raw, ok := b.files[name]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

// Tabs lists the tab ids the bundle holds, landside and plane-side together,
// sorted.
func (b *Bundle) Tabs() []int {
	seen := map[int]bool{}
	for name := range b.files {
		for _, prefix := range []string{"landside/tabs/", "planeside/tabs/"} {
			rest, ok := strings.CutPrefix(name, prefix)
			if !ok {
				continue
			}
			idStr, _, ok := strings.Cut(rest, "/")
			if !ok {
				continue
			}
			if id, err := strconv.Atoi(idStr); err == nil {
				seen[id] = true
			}
		}
	}
	out := make([]int, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// Frames decodes a tab's journalled frames, in the order they were sent.
// The journal stores each frame as plain canonical CBOR of the envelope
// (internal/session/capture.go writeJournal), so this is Unmarshal and sort.
func (b *Bundle) Frames(tab int) ([]protocol.Frame, error) {
	prefix := fmt.Sprintf("landside/tabs/%d/frames/", tab)
	var names []string
	for name := range b.files {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".cbor") {
			names = append(names, name)
		}
	}
	// Names are NNNN-<type>.cbor; the number is the send order.
	sort.Strings(names)
	frames := make([]protocol.Frame, 0, len(names))
	for _, name := range names {
		var f protocol.Frame
		if err := protocol.Unmarshal(b.files[name], &f); err != nil {
			return nil, fmt.Errorf("parity: %s: %w", path.Base(name), err)
		}
		frames = append(frames, f)
	}
	return frames, nil
}

// fingerprintRow is one line of the (id, kind, value, flags) list both halves
// write into a bundle — what the document hash is computed over, plus the
// flags it cannot see.
type fingerprintRow struct {
	ID    int64
	Kind  int
	Value string
	Flags int64
}

func (b *Bundle) fingerprint(name string) (map[int64]fingerprintRow, bool, error) {
	raw, ok := b.files[name]
	if !ok {
		return nil, false, nil
	}
	var fp struct {
		Truncated bool                `json:"truncated"`
		Nodes     [][]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &fp); err != nil {
		return nil, false, fmt.Errorf("parity: %s: %w", name, err)
	}
	out := make(map[int64]fingerprintRow, len(fp.Nodes))
	for _, row := range fp.Nodes {
		if len(row) < 3 {
			continue
		}
		var r fingerprintRow
		if json.Unmarshal(row[0], &r.ID) != nil ||
			json.Unmarshal(row[1], &r.Kind) != nil ||
			json.Unmarshal(row[2], &r.Value) != nil {
			continue
		}
		if len(row) > 3 {
			_ = json.Unmarshal(row[3], &r.Flags)
		}
		out[r.ID] = r
	}
	return out, fp.Truncated, nil
}
