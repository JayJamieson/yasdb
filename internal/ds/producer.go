package ds

// producerHeaders carries the parsed Producer-* headers for one append.
type producerHeaders struct {
	id    string
	epoch uint64
	seq   uint64
}

// producerActionKind enumerates the outcome of idempotent-producer validation
// (protocol §5.2.1).
type producerActionKind int

const (
	pAccept      producerActionKind = iota // new data, commit and update state
	pDuplicate                             // seq already applied -> 204 idempotent success
	pStaleEpoch                            // epoch < state.epoch -> 403
	pBadSeqStart                           // epoch > state.epoch with seq != 0 -> 400
	pGap                                   // seq > lastSeq+1 -> 409
)

type producerAction struct {
	kind         producerActionKind
	newState     producerState // valid when kind == pAccept
	currentEpoch uint64        // echoed on pStaleEpoch
	expectedSeq  uint64        // on pGap
	receivedSeq  uint64        // on pGap
}

// validateProducer implements the exact validation logic from protocol §5.2.1.
// `exists` is false when there is no persisted state for this producer yet.
func validateProducer(st producerState, exists bool, h producerHeaders) producerAction {
	if !exists {
		// No prior state. A fresh producer must start a session at seq 0. Any
		// epoch is acceptable as the initial epoch (auto-claim/bootstrap).
		if h.seq != 0 {
			// A brand-new producer id presenting seq != 0 is treated as a gap
			// from the implicit lastSeq=-1 baseline.
			return producerAction{kind: pGap, expectedSeq: 0, receivedSeq: h.seq}
		}
		return producerAction{kind: pAccept, newState: producerState{epoch: h.epoch, lastSeq: 0}}
	}

	if h.epoch < st.epoch {
		return producerAction{kind: pStaleEpoch, currentEpoch: st.epoch}
	}
	if h.epoch > st.epoch {
		if h.seq != 0 {
			return producerAction{kind: pBadSeqStart}
		}
		// New epoch established; reset lastSeq to 0.
		return producerAction{kind: pAccept, newState: producerState{epoch: h.epoch, lastSeq: 0}}
	}

	// Same epoch: sequence validation.
	switch {
	case h.seq <= st.lastSeq:
		return producerAction{kind: pDuplicate}
	case h.seq == st.lastSeq+1:
		return producerAction{kind: pAccept, newState: producerState{epoch: h.epoch, lastSeq: h.seq}}
	default: // h.seq > st.lastSeq+1
		return producerAction{kind: pGap, expectedSeq: st.lastSeq + 1, receivedSeq: h.seq}
	}
}
