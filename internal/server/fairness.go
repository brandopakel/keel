package server

import "time"

const maxCommandsPerTurn = 64
const runReplyTarget = 64 << 10
const runTimeTarget = time.Millisecond

// A connection is queued only after its prior replies have drained. Thus
// execution order, slow-reader backpressure and ordered append reply gating all
// precede continuation of its pipeline. Closed/reused descriptors are checked
// against connection identity before resuming.
var queuedClientReads []*client

func queueClientRead(c *client) {
	if c.readQueued || (!c.bufferedReady && len(c.cmds) == 0) {
		return
	}
	c.readQueued = true
	queuedClientReads = append(queuedClientReads, c)
	wake()
}

func takeQueuedReads(dst []*client) []*client {
	for _, c := range queuedClientReads {
		c.readQueued = false
		if clients[c.fd] != c || c.appendHeld || c.appendDeferred || len(c.out) > 0 {
			continue
		}
		dst = append(dst, c)
	}
	clear(queuedClientReads)
	if cap(queuedClientReads) > 4096 {
		queuedClientReads = nil
	} else {
		queuedClientReads = queuedClientReads[:0]
	}
	return dst
}
