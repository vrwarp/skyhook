package mirror

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// Model is a server-side replica of what the client's DOM should look like
// after applying a frame stream. It exists for two reasons: the end-to-end
// tests apply real frames to it and assert on the result without an Electron
// process, and the integrity checker compares its hash against the client's.
//
// It is deliberately the same algorithm the TypeScript patcher implements; if
// the two ever disagree, one of them has a bug worth finding.
type Model struct {
	// mu guards everything below.
	//
	// A replica is written by whichever goroutine is feeding it frames and read
	// by whoever is asking what the page says — in the end-to-end client those
	// are always two different goroutines, because frames keep arriving for as
	// long as a test polls. Unsynchronised, `WaitForText` walking `Nodes` while
	// a mutation inserts into it is a "concurrent map read and map write",
	// which is not a test failure but a runtime fatal: it takes the whole suite
	// down, in whichever test happened to be running.
	mu      sync.RWMutex
	Strings []string
	Nodes   map[int64]*ModelNode
	Root    int64
	CSS     []string
	URL     string
	Title   string
	Seq     uint64
}

// ModelNode is one node in the replica.
type ModelNode struct {
	ID       int64
	Parent   int64
	Kind     uint8
	Name     string
	Text     string
	Attrs    map[string]string
	Flags    uint8
	Children []int64
}

// NewModel returns an empty replica.
func NewModel() *Model {
	return &Model{Nodes: map[int64]*ModelNode{}}
}

func (m *Model) str(ref int32) string {
	if ref < 0 || int(ref) >= len(m.Strings) {
		return ""
	}
	return m.Strings[ref]
}

// ApplySnapshot resets the replica to a full document.
func (m *Model) ApplySnapshot(s *protocol.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Strings = append([]string{}, s.Strings...)
	m.Nodes = make(map[int64]*ModelNode, len(s.Nodes))
	m.CSS = append([]string{}, s.CSS...)
	m.URL, m.Title = s.URL, s.Title
	m.Root = 0
	m.Seq = 0
	for i := range s.Nodes {
		n := s.Nodes[i]
		mn := &ModelNode{ID: n.ID, Parent: n.Parent, Kind: n.Kind, Flags: n.Flags}
		switch n.Kind {
		case protocol.KindText:
			mn.Text = m.str(n.Ref)
		default:
			mn.Name = m.str(n.Ref)
		}
		if len(n.Attrs) > 0 {
			mn.Attrs = make(map[string]string, len(n.Attrs)/2)
			for j := 0; j+1 < len(n.Attrs); j += 2 {
				mn.Attrs[m.str(n.Attrs[j])] = m.str(n.Attrs[j+1])
			}
		}
		m.Nodes[n.ID] = mn
		if n.Parent == 0 {
			m.Root = n.ID
			continue
		}
		p := m.Nodes[n.Parent]
		if p == nil {
			return fmt.Errorf("model: node %d has unknown parent %d", n.ID, n.Parent)
		}
		p.Children = append(p.Children, n.ID)
	}
	return nil
}

// ApplyMutation applies one mutation batch.
func (m *Model) ApplyMutation(mu *protocol.Mutation, seq uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Strings = append(m.Strings, mu.Strings...)
	for i := range mu.Ops {
		if err := m.applyOp(&mu.Ops[i]); err != nil {
			return err
		}
	}
	m.Seq = seq
	return nil
}

func (m *Model) applyOp(op *protocol.Op) error {
	switch op.Op {
	case protocol.OpInsert:
		parent := m.Nodes[op.Parent]
		if parent == nil {
			return fmt.Errorf("model: insert into unknown parent %d", op.Parent)
		}
		for i := range op.Nodes {
			n := op.Nodes[i]
			mn := &ModelNode{ID: n.ID, Parent: n.Parent, Kind: n.Kind, Flags: n.Flags}
			switch n.Kind {
			case protocol.KindText:
				mn.Text = m.str(n.Ref)
			default:
				mn.Name = m.str(n.Ref)
			}
			if len(n.Attrs) > 0 {
				mn.Attrs = make(map[string]string, len(n.Attrs)/2)
				for j := 0; j+1 < len(n.Attrs); j += 2 {
					mn.Attrs[m.str(n.Attrs[j])] = m.str(n.Attrs[j+1])
				}
			}
			m.Nodes[n.ID] = mn
			if i == 0 {
				mn.Parent = op.Parent
				insertChild(parent, n.ID, op.Before)
				continue
			}
			p := m.Nodes[mn.Parent]
			if p == nil {
				return fmt.Errorf("model: subtree node %d has unknown parent %d", n.ID, mn.Parent)
			}
			p.Children = append(p.Children, n.ID)
		}
	case protocol.OpRemove:
		m.remove(op.Node)
	case protocol.OpAttr:
		n := m.Nodes[op.Node]
		if n == nil {
			return nil // node already gone; the client tolerates this too
		}
		if n.Attrs == nil {
			n.Attrs = map[string]string{}
		}
		name := m.str(op.Ref)
		if op.Ref2 < 0 {
			delete(n.Attrs, name)
		} else {
			n.Attrs[name] = m.str(op.Ref2)
		}
	case protocol.OpText:
		if n := m.Nodes[op.Node]; n != nil {
			n.Text = m.str(op.Ref)
		}
	case protocol.OpSplice:
		n := m.Nodes[op.Node]
		if n == nil {
			return nil
		}
		runes := []rune(n.Text)
		off := int(op.Off)
		del := int(op.Del)
		if off > len(runes) {
			off = len(runes)
		}
		if off+del > len(runes) {
			del = len(runes) - off
		}
		ins := []rune(m.str(op.Ref))
		out := make([]rune, 0, len(runes)-del+len(ins))
		out = append(out, runes[:off]...)
		out = append(out, ins...)
		out = append(out, runes[off+del:]...)
		n.Text = string(out)
	case protocol.OpMove:
		n := m.Nodes[op.Node]
		parent := m.Nodes[op.Parent]
		if n == nil || parent == nil {
			return nil
		}
		if old := m.Nodes[n.Parent]; old != nil {
			old.Children = removeID(old.Children, op.Node)
		}
		n.Parent = op.Parent
		insertChild(parent, op.Node, op.Before)
	case protocol.OpStyle:
		m.CSS = append(m.CSS, op.Add...)
	case protocol.OpDocInfo:
		if op.Str != "" {
			m.Title = op.Str
		}
	case protocol.OpFocus, protocol.OpScroll, protocol.OpImage:
		// No structural effect on the replica.
	}
	return nil
}

func (m *Model) remove(id int64) {
	n := m.Nodes[id]
	if n == nil {
		return
	}
	if p := m.Nodes[n.Parent]; p != nil {
		p.Children = removeID(p.Children, id)
	}
	var drop func(int64)
	drop = func(x int64) {
		node := m.Nodes[x]
		if node == nil {
			return
		}
		for _, c := range node.Children {
			drop(c)
		}
		delete(m.Nodes, x)
	}
	drop(id)
}

func insertChild(parent *ModelNode, id, before int64) {
	if before != 0 {
		for i, c := range parent.Children {
			if c == before {
				parent.Children = append(parent.Children[:i],
					append([]int64{id}, parent.Children[i:]...)...)
				return
			}
		}
	}
	parent.Children = append(parent.Children, id)
}

func removeID(list []int64, id int64) []int64 {
	for i, c := range list {
		if c == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// Text renders the replica's visible text, which is what tests assert on.
func (m *Model) Text() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var b strings.Builder
	var walk func(int64)
	walk = func(id int64) {
		n := m.Nodes[id]
		if n == nil {
			return
		}
		if n.Kind == protocol.KindText {
			b.WriteString(n.Text)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(m.Root)
	return b.String()
}

// HTML renders the replica as HTML. The client builds real DOM nodes rather
// than parsing HTML, but this is invaluable for debugging a divergence.
func (m *Model) HTML() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var b strings.Builder
	var walk func(int64)
	walk = func(id int64) {
		n := m.Nodes[id]
		if n == nil {
			return
		}
		switch n.Kind {
		case protocol.KindText:
			b.WriteString(escapeHTML(n.Text))
			return
		case protocol.KindDoctype:
			b.WriteString("<!DOCTYPE " + n.Name + ">")
			return
		}
		b.WriteString("<" + n.Name)
		keys := make([]string, 0, len(n.Attrs))
		for k := range n.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" " + k + `="` + escapeHTML(n.Attrs[k]) + `"`)
		}
		b.WriteString(">")
		for _, c := range n.Children {
			walk(c)
		}
		b.WriteString("</" + n.Name + ">")
	}
	walk(m.Root)
	return b.String()
}

// Find returns the first node matching a tag and attribute value.
func (m *Model) Find(tag, attr, value string) *ModelNode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]int64, 0, len(m.Nodes))
	for id := range m.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		n := m.Nodes[id]
		if tag != "" && !strings.EqualFold(n.Name, tag) {
			continue
		}
		if attr == "" {
			return n
		}
		if v, ok := n.Attrs[attr]; ok && (value == "" || v == value) {
			return n
		}
	}
	return nil
}

// FindByText returns the innermost element whose text contains a substring.
//
// It lives here rather than in the client that wants it because the walk has to
// happen under the replica's own lock: a caller iterating `Nodes` from outside
// is the exact race this type exists to prevent.
func (m *Model) FindByText(substr string) *ModelNode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *ModelNode
	var bestLen int
	var walk func(id int64) string
	walk = func(id int64) string {
		n := m.Nodes[id]
		if n == nil {
			return ""
		}
		if n.Kind == protocol.KindText {
			return n.Text
		}
		var sb strings.Builder
		for _, ch := range n.Children {
			sb.WriteString(walk(ch))
		}
		text := sb.String()
		if strings.Contains(text, substr) && (best == nil || len(text) < bestLen) {
			best, bestLen = n, len(text)
		}
		return text
	}
	walk(m.Root)
	return best
}

// Hash fingerprints the replica with the same algorithm the agent uses, so a
// mismatch means genuine divergence rather than a hashing difference.
func (m *Model) Hash() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var h uint32 = 0x811c9dc5
	ids := make([]int64, 0, len(m.Nodes))
	for id := range m.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		n := m.Nodes[id]
		v := n.Name
		lower := true // the agent hashes tagName.toLowerCase()
		switch n.Kind {
		case protocol.KindText:
			v, lower = n.Text, false
		case protocol.KindDoctype:
			v, lower = "", false // the agent has no tagName for a doctype either
		}
		h ^= uint32(id & 0xff)
		h *= 16777619
		for i, r := range v {
			if i >= 32 {
				break
			}
			// Element names travel in the case the DOM uses — `clipPath` is not
			// `clippath` in SVG — but the agent's fingerprint lowercases them,
			// and a fingerprint that disagrees resyncs the page forever.
			if lower && r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			h ^= uint32(r) & 0xff
			h *= 16777619
		}
	}
	return uint64(h)
}

func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
