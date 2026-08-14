package diag

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Ext is the suffix a finished bundle carries.
const Ext = ".zip"

// partExt marks a bundle still being written. A capture waits on a client over
// a link that may be gone, so an unfinished bundle can outlive the process; it
// must never be mistaken for one somebody can read.
const partExt = ".zip.part"

// ErrBundleClosed is returned by Add after Close.
var ErrBundleClosed = errors.New("diag: bundle is closed")

// Bundle is one capture, being written into a zip file.
//
// Everything about it is bounded. A capture is a diagnostic, not a backup: a
// bundle that grows until the VPS runs out of disk has turned a rendering bug
// into an outage, and a bundle nobody can open because it is 400 MB is no
// better. Over-budget artifacts are dropped with a note saying so, because a
// bundle that silently omits things is worse than one that admits it.
type Bundle struct {
	mu      sync.Mutex
	id      string
	final   string
	tmp     string
	file    *os.File
	zw      *zip.Writer
	budget  int64
	written int64
	notes   []string
	names   map[string]int
	closed  bool
}

// NewBundle creates a bundle in dir. The file is named after the capture, and
// is not given its final name until Close.
func NewBundle(dir, id string, budget int64) (*Bundle, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if budget <= 0 {
		budget = 64 << 20
	}
	base := filepath.Join(dir, sanitizeFile(id))
	f, err := os.OpenFile(base+partExt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		id: id, final: base + Ext, tmp: base + partExt,
		file: f, zw: zip.NewWriter(f), budget: budget,
		names: map[string]int{},
	}, nil
}

// ID reports the capture id.
func (b *Bundle) ID() string { return b.id }

// Path reports where the finished bundle will land.
func (b *Bundle) Path() string { return b.final }

// Add stores one artifact. A name already used gets a numeric suffix rather
// than overwriting: two artifacts colliding is a bug worth seeing in the
// bundle, not one worth hiding by losing one of them.
func (b *Bundle) Add(name string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBundleClosed
	}
	name = sanitizeEntry(name)
	if n := b.names[name]; n > 0 {
		b.names[name] = n + 1
		ext := path.Ext(name)
		name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n, ext)
	}
	b.names[name] = b.names[name] + 1

	if b.written+int64(len(data)) > b.budget {
		b.notes = append(b.notes, fmt.Sprintf(
			"omitted %s (%d bytes): the bundle's %d byte budget was already spent",
			name, len(data), b.budget))
		return nil
	}
	w, err := b.zw.CreateHeader(&zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	b.written += int64(len(data))
	return nil
}

// AddText stores a text artifact.
func (b *Bundle) AddText(name, body string) error { return b.Add(name, []byte(body)) }

// AddJSON stores a value as indented JSON, which is what makes a bundle
// readable without tooling.
func (b *Bundle) AddJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return b.AddText(name, fmt.Sprintf("could not encode: %v", err))
	}
	return b.Add(name, append(data, '\n'))
}

// Note records something about the capture itself: an artifact that could not
// be gathered, a limit that was hit, a half that never answered. Notes are
// written into the bundle at Close, so reading one starts with what is missing
// from it.
func (b *Bundle) Note(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notes = append(b.notes, fmt.Sprintf(format, args...))
}

// Notes returns what has been noted so far.
func (b *Bundle) Notes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.notes...)
}

// Bytes reports the uncompressed size of everything stored so far.
func (b *Bundle) Bytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written
}

// Close seals the bundle and gives it its final name. It reports the path and
// the size of the file on disk.
func (b *Bundle) Close() (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return b.final, 0, ErrBundleClosed
	}
	b.closed = true

	notes := "none.\n"
	if len(b.notes) > 0 {
		notes = "- " + strings.Join(b.notes, "\n- ") + "\n"
	}
	if w, err := b.zw.Create("NOTES.txt"); err == nil {
		_, _ = w.Write([]byte("What is missing from this capture, and why:\n\n" + notes))
	}
	if err := b.zw.Close(); err != nil {
		_ = b.file.Close()
		return "", 0, err
	}
	if err := b.file.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(b.tmp, b.final); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(b.final)
	if err != nil {
		return b.final, 0, nil
	}
	return b.final, info.Size(), nil
}

// Abort discards a bundle that will never be finished.
func (b *Bundle) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	_ = b.zw.Close()
	_ = b.file.Close()
	_ = os.Remove(b.tmp)
}

// Prune keeps the newest bundles and deletes the rest, so a server that takes a
// capture on every divergence cannot fill a disk. Half-written bundles older
// than an hour go too: they are the remains of a capture whose process died.
func Prune(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	type bundleFile struct {
		path string
		mod  time.Time
	}
	var done []bundleFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, e.Name())
		switch {
		case strings.HasSuffix(e.Name(), partExt):
			if time.Since(info.ModTime()) > time.Hour {
				_ = os.Remove(full)
			}
		case strings.HasSuffix(e.Name(), Ext):
			done = append(done, bundleFile{path: full, mod: info.ModTime()})
		}
	}
	if len(done) <= keep {
		return nil
	}
	sort.Slice(done, func(i, j int) bool { return done[i].mod.After(done[j].mod) })
	for _, f := range done[keep:] {
		_ = os.Remove(f.path)
	}
	return nil
}

// Redact replaces a string with its shape: how long it was, and a short digest
// that still matches other occurrences of the same value.
//
// Captures carry what the reader typed, which is the difference between "the
// field diverged" and "the field diverged after these six keystrokes". It is
// also, sometimes, a password. So the default is the shape, and the operator
// opts in to the contents.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("«%d chars, %s»", utf8.RuneCountInString(s), hex.EncodeToString(sum[:4]))
}

// sanitizeEntry makes a zip entry name safe to extract: relative, forward
// slashes, no traversal.
func sanitizeEntry(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	parts := strings.Split(path.Clean("/"+name), "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			continue
		}
		out = append(out, sanitizeFile(p))
	}
	if len(out) == 0 {
		return "unnamed"
	}
	return strings.Join(out, "/")
}

// sanitizeFile reduces one path element to something every filesystem accepts.
func sanitizeFile(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unnamed"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}
