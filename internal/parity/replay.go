package parity

import (
	"fmt"

	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// Replay applies a tab's frame stream to a fresh replica, the way the client
// would have. This is how a bundle's journal becomes a document to hold the
// plane side's mirror against: the client's DOM is not knowable from here,
// but the DOM the frames specify is, and a patcher bug is precisely a
// disagreement between the two.
//
// The capture pipeline does the same walk while a bundle is being written
// (internal/session/capture.go, writeJournal); this one exists so a bundle
// already on disk can be replayed without a session, which is what triage is.
func Replay(frames []protocol.Frame) (*mirror.Model, error) {
	model := mirror.NewModel()
	for i := range frames {
		f := &frames[i]
		switch f.Type {
		case protocol.TypeSnapshot:
			var snap protocol.Snapshot
			if err := f.DecodeBody(&snap); err != nil {
				return nil, fmt.Errorf("parity: frame %d: bad snapshot: %w", i, err)
			}
			if err := model.ApplySnapshot(&snap); err != nil {
				return nil, fmt.Errorf("parity: frame %d: %w", i, err)
			}
		case protocol.TypeMutation:
			var mut protocol.Mutation
			if err := f.DecodeBody(&mut); err != nil {
				return nil, fmt.Errorf("parity: frame %d: bad mutation: %w", i, err)
			}
			if err := model.ApplyMutation(&mut, f.Seq); err != nil {
				return nil, fmt.Errorf("parity: frame %d: %w", i, err)
			}
		default:
			// Everything else on the dom channel is bookkeeping the replica
			// does not model.
		}
	}
	return model, nil
}
