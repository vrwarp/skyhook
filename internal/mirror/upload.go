package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
File upload (P-007).

A file input used to be a dead end twice over. Landside, the replayed click
asked headless Chromium to show a chooser it does not have; plane-side, the
mirrored input opened the reader's own picker into a document whose value can
never cross. Nobody got a file, and nothing said so.

The chooser is intercepted instead: Page.setInterceptFileChooserDialog
suppresses the landside dialog and reports the ask, which crosses as a
TypeFileAsk naming the mirrored input. The reader picks with their device's
own picker, the bytes cross as TypeUploadPart chunks on the bulk channel —
client to server, the reverse of a download and at the same 32 kB grain — and
DOM.setFileInputFiles hands the landed files to the input, which fires the
change event the page has been waiting for. From there the page uploads to
its origin the way it always would, landside, with its own cookies.

The files land in a directory the manager wipes at startup and on the kill
switch, one subdirectory per ask. They must outlive the answer: the page
reads a chosen file whenever it likes — on submit, in a FileReader, never —
so an ask's files stay until the tab closes.
*/

// uploadMaxBytes bounds one ask's files landside. Past half a gigabyte the
// transfer is a backup job, not a form field, and the link it crosses is the
// wrong tool for it.
const uploadMaxBytes = 512 << 20

// fileAsk is one intercepted chooser waiting for the reader's answer — or,
// once done, holding the landed files until the tab closes and the page can
// no longer read them.
type fileAsk struct {
	backendNode int64
	dir         string
	files       []string
	cur         *os.File
	curPath     string
	curOff      int64
	received    int64
	done        bool
}

// onFileChooser is the browser reporting a chooser the interception ate.
func (t *Tab) onFileChooser(_ string, params json.RawMessage) {
	var p struct {
		FrameID       string `json:"frameId"`
		Mode          string `json:"mode"`
		BackendNodeID int64  `json:"backendNodeId"`
	}
	if json.Unmarshal(params, &p) != nil || p.BackendNodeID == 0 {
		return
	}
	go t.askForFiles(p.BackendNodeID, p.Mode == "selectMultiple")
}

// askForFiles names the mirrored input if it can and sends the ask. Off the
// event goroutine: naming the node is two CDP calls.
func (t *Tab) askForFiles(backendNode int64, multiple bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The mirrored id for the element, so the client can anchor its
	// affordance. Best effort: an input inside a sub-frame resolves in a
	// world this tab's agent does not own, and an ask with no node is still
	// answerable.
	var node int64
	t.mu.Lock()
	ctxID := t.ctxID
	t.mu.Unlock()
	if ctxID != 0 {
		var res struct {
			Object struct {
				ObjectID string `json:"objectId"`
			} `json:"object"`
		}
		err := t.sess.Do(ctx, "DOM.resolveNode", map[string]any{
			"backendNodeId":      backendNode,
			"executionContextId": ctxID,
		}, &res)
		if err == nil && res.Object.ObjectID != "" {
			var out struct {
				Result struct {
					Value int64 `json:"value"`
				} `json:"result"`
			}
			if t.sess.Do(ctx, "Runtime.callFunctionOn", map[string]any{
				"objectId":            res.Object.ObjectID,
				"functionDeclaration": "function () { return __skyhook.idOfNode(this); }",
				"returnByValue":       true,
			}, &out) == nil {
				node = out.Result.Value
			}
		}
	}

	if t.opts.UploadDir == "" {
		t.log.Warn("a page asked for a file and uploads are not configured", "tab", t.ID)
		return
	}
	dir, err := os.MkdirTemp(t.opts.UploadDir, "ask-")
	if err != nil {
		t.log.Warn("upload staging failed", "tab", t.ID, "err", err)
		return
	}
	id := uint32(t.askSeq.Add(1)) //nolint:gosec // a session never has 2^32 asks
	t.uploadMu.Lock()
	if t.uploads == nil {
		t.uploads = map[uint32]*fileAsk{}
	}
	t.uploads[id] = &fileAsk{backendNode: backendNode, dir: dir}
	t.uploadMu.Unlock()

	t.log.Info("page asked for a file", "tab", t.ID, "ask", id, "multiple", multiple)
	f, err := protocol.NewFrame(protocol.TypeFileAsk, t.ID,
		protocol.FileAsk{ID: id, Node: node, Multiple: multiple})
	if err != nil {
		return
	}
	t.out.EmitFrame(protocol.ChCtrl, f)
}

/*
UploadPart lands one piece of the reader's answer.

Parts arrive in order on one connection — the client sends them through an
ordered queue for exactly this reason — so assembly is sequential appends. A
part that breaks the sequence, overflows the cap, or names an ask this tab
does not hold aborts the whole ask: half a file handed to the page would be
indistinguishable from the file.
*/
func (t *Tab) UploadPart(ctx context.Context, p *protocol.UploadPart) error {
	t.uploadMu.Lock()
	ask := t.uploads[p.Ask]
	t.uploadMu.Unlock()
	if ask == nil {
		return fmt.Errorf("mirror: no file ask %d", p.Ask)
	}
	if ask.done {
		return fmt.Errorf("mirror: file ask %d is already answered", p.Ask)
	}
	if p.Err != "" {
		// The reader dismissed the picker, or the plane side broke off. The
		// page sees exactly what a dismissed chooser shows it: nothing.
		t.log.Info("file ask ended without files", "tab", t.ID, "ask", p.Ask, "reason", p.Err)
		t.dropAsk(p.Ask)
		return nil
	}
	if p.Name != "" && ask.cur == nil {
		// One subdirectory per file, so the basename the page will see stays
		// exactly what the reader's device called it: DOM.setFileInputFiles
		// names each File after the path's last element, and a de-collision
		// prefix there would reach the page's own extension checks.
		sub := filepath.Join(ask.dir, fmt.Sprintf("%d", len(ask.files)))
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.dropAsk(p.Ask)
			return err
		}
		path := filepath.Join(sub, uploadFilename(p.Name))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside our staging dir
		if err != nil {
			t.dropAsk(p.Ask)
			return err
		}
		ask.cur, ask.curPath, ask.curOff = f, path, 0
	}
	if len(p.Data) > 0 {
		if ask.cur == nil || p.Off != ask.curOff {
			t.dropAsk(p.Ask)
			return fmt.Errorf("mirror: upload part out of order for ask %d", p.Ask)
		}
		ask.received += int64(len(p.Data))
		if ask.received > uploadMaxBytes {
			t.dropAsk(p.Ask)
			return fmt.Errorf("mirror: upload larger than %d bytes refused", uploadMaxBytes)
		}
		if _, err := ask.cur.Write(p.Data); err != nil {
			t.dropAsk(p.Ask)
			return err
		}
		ask.curOff += int64(len(p.Data))
	}
	if p.Last && ask.cur != nil {
		if err := ask.cur.Close(); err != nil {
			t.dropAsk(p.Ask)
			return err
		}
		ask.files = append(ask.files, ask.curPath)
		ask.cur, ask.curPath, ask.curOff = nil, "", 0
	}
	if !p.Done {
		return nil
	}

	// The answer is complete: hand it to the input. The ask stays on the
	// books, done, holding its files — the page reads them whenever it likes,
	// so they live until the tab closes.
	ask.done = true
	if len(ask.files) == 0 {
		t.dropAsk(p.Ask)
		return nil
	}
	if err := t.sess.Do(ctx, "DOM.setFileInputFiles", map[string]any{
		"files":         ask.files,
		"backendNodeId": ask.backendNode,
	}, nil); err != nil {
		t.dropAsk(p.Ask)
		return fmt.Errorf("mirror: handing files to the page: %w", err)
	}
	t.log.Info("files handed to the page", "tab", t.ID, "ask", p.Ask, "count", len(ask.files))
	return nil
}

// dropAsk forgets one ask and deletes whatever of it landed.
func (t *Tab) dropAsk(id uint32) {
	t.uploadMu.Lock()
	ask := t.uploads[id]
	delete(t.uploads, id)
	t.uploadMu.Unlock()
	if ask == nil {
		return
	}
	if ask.cur != nil {
		_ = ask.cur.Close()
	}
	_ = os.RemoveAll(ask.dir)
}

// dropAsks clears every pending ask; the tab is closing.
func (t *Tab) dropAsks() {
	t.uploadMu.Lock()
	asks := t.uploads
	t.uploads = nil
	t.uploadMu.Unlock()
	for _, ask := range asks {
		if ask.cur != nil {
			_ = ask.cur.Close()
		}
		_ = os.RemoveAll(ask.dir)
	}
}

// uploadFilename reduces a plane-suggested filename to something safe to
// create landside: basename, control characters out, bounded, never empty.
func uploadFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "file"
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}
