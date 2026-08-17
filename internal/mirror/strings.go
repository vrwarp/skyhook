package mirror

import (
	"sync"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
One client table, several agents interning into their own.

Every string on the wire is an index rather than a string: a document repeats
its tag names and its class attributes thousands of times, and the intern table
is most of what makes a snapshot affordable. The client keeps one table per tab
and appends to it as frames arrive, which is exactly right while one agent is
writing — its table and the client's are the same list in the same order.

With a frame mirrored by an agent of its own that stops being true in the
quietest possible way. Each agent numbers from zero in a table of its own, so a
frame's `ref: 0` means the frame's first string and arrives at a client whose
zero is the page's. Nothing errors: the refs are all in range, and the client
renders the frame's document using the page's words. The first few tags of an
HTML document are the same everywhere — `html`, `head` — so it even looks like
it is working, until the tree stops matching and the rest of the frame goes
missing with no error anywhere.

So the host keeps the mapping. Each agent's table is appended to the client's as
its strings arrive, and every ref in that agent's ops is rewritten to where its
string actually landed. The client is unchanged: it goes on appending and
indexing one table, which is the thing that makes the frames small.
*/
type strTable struct {
	mu sync.Mutex
	// next is the length of the client's table: where the next string lands.
	next int32
	// maps is each agent's own index to the client's, by slot.
	maps map[int64][]int32
}

func newStrTable() *strTable {
	return &strTable{maps: map[int64][]int32{}}
}

// Reset starts again from a snapshot, whose strings are the client's whole
// table and the top agent's whole table, in the same order.
func (s *strTable) Reset(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = int32(n)
	ident := make([]int32, n)
	for i := range ident {
		ident[i] = int32(i)
	}
	s.maps = map[int64][]int32{0: ident}
}

// Forget drops an agent's mapping: its next message will be a fresh document.
func (s *strTable) Forget(slot int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.maps, slot)
}

// Adopt places an agent's newly interned strings in the client's table and
// reports where they landed, in order.
func (s *strTable) Adopt(slot int64, n int) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.maps[slot]
	for i := 0; i < n; i++ {
		m = append(m, s.next)
		s.next++
	}
	s.maps[slot] = m
}

// Ref translates one of an agent's indices into the client's. An index nothing
// was ever adopted for — a ref from before a resnapshot, or the -1 that means
// "no string" — is passed through, which the client reads as the empty string
// rather than as somebody else's word.
func (s *strTable) Ref(slot int64, i int32) int32 {
	if i < 0 {
		return i
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.maps[slot]
	if int(i) >= len(m) {
		return -1
	}
	return m[i]
}

// rebaseOps rewrites every string reference in a batch from one agent's table
// into the client's. Ops that carry text rather than a reference — a
// stylesheet's rules, a title — are already the words themselves.
func (s *strTable) rebaseOps(slot int64, ops []protocol.Op) {
	if slot == 0 && s.isIdentity(slot) {
		return
	}
	for i := range ops {
		op := &ops[i]
		op.Ref = s.Ref(slot, op.Ref)
		op.Ref2 = s.Ref(slot, op.Ref2)
		for n := range op.Nodes {
			node := &op.Nodes[n]
			node.Ref = s.Ref(slot, node.Ref)
			for a := range node.Attrs {
				node.Attrs[a] = s.Ref(slot, node.Attrs[a])
			}
		}
	}
}

// isIdentity reports whether an agent's table is still exactly the client's, so
// the common case — a page with no mirrored frames in it — rewrites nothing.
func (s *strTable) isIdentity(slot int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.maps[slot]
	if int32(len(m)) != s.next {
		return false
	}
	for i, v := range m {
		if v != int32(i) {
			return false
		}
	}
	return true
}
