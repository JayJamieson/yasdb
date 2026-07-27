package ds

// committer lands an assembled append burst durably and arranges the
// reader-visible publish and the client replies. It is the durability strategy
// injected per Server (Config.Durability): both modes return Stream-Next-Offset
// only once the data is durable, and differ only in whether the append blocks on
// durability.
type committer interface {
	commit(st *streamer, c commitBurst)
	close() error
}

// commitBurst is the finalised pending view of one group commit: the assembled
// ops plus the per-request responses to send once the outcome is known.
type commitBurst struct {
	pend        *pendingBurst
	ops         []Op
	batch       []appendReq
	responses   []appendResp
	accepted    []bool
	metaChanged bool
	seg         cacheSeg
}

func newCommitter(store Storage, cfg Config) committer {
	if cfg.Durability == "notifier" {
		return &notifierCommitter{store: store, notifier: newDurabilityNotifier(store, cfg.NotifierPollInterval)}
	}
	return syncCommitter{store: store}
}

// syncCommitter blocks each group commit on a durable write. A stream's
// throughput is capped at maxBurst/flush because the streamer serialises on the
// durable write.
type syncCommitter struct{ store Storage }

func (sc syncCommitter) commit(st *streamer, c commitBurst) {
	if err := sc.store.Commit(c.ops, true); err != nil {
		failAccepted(c.batch, c.responses, c.accepted, err)
		replyAll(c.batch, c.responses)
		return
	}
	st.applyWriter(c.pend, c.metaChanged)
	st.applyReader(c.pend.tail, c.metaChanged && c.pend.closed, c.seg)
	replyAll(c.batch, c.responses)
}

func (syncCommitter) close() error { return nil }

// notifierCommitter writes non-durably (returning at once so the next burst
// pipelines), advances the writer view immediately, and defers the
// reader-visible publish and reply to the durable-seq notifier. Appends scale
// with concurrency instead of plateauing at maxBurst/flush.
type notifierCommitter struct {
	store    Storage
	notifier *durabilityNotifier
}

func (nc *notifierCommitter) commit(st *streamer, c commitBurst) {
	seq, err := nc.store.CommitAsync(c.ops)
	if err != nil {
		failAccepted(c.batch, c.responses, c.accepted, err)
		replyAll(c.batch, c.responses)
		return
	}
	st.applyWriter(c.pend, c.metaChanged)

	targetTail := c.pend.tail
	targetClosed := c.metaChanged && c.pend.closed
	// pending gates retirement: a streamer must not retire (and let a respawn read
	// a stale durable tail) while a durability callback is still outstanding.
	st.pending.Add(1)
	nc.notifier.subscribe(seq, func(cerr error) {
		if cerr == nil {
			st.applyReader(targetTail, targetClosed, c.seg)
		} else {
			failAccepted(c.batch, c.responses, c.accepted, cerr)
		}
		replyAll(c.batch, c.responses)
		st.pending.Add(-1)
	})
}

func (nc *notifierCommitter) close() error {
	nc.notifier.shutdown()
	return nil
}
