package server

import (
	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
	"time"
)

// orderedAppend belongs to the event loop. The disk worker owns immutable bytes
// only. Each client has at most one held reply or deferred parsed run, bounding
// queue entries by the connection limit; admission separately bounds bytes.
type orderedAppend struct {
	held          []*client
	deferred      []*client
	drain         bool
	exclusive     bool
	maintenanceAt time.Time
}

func (q *orderedAppend) begin(pool *ioPool, mux io_multiplexing.IOMultiplexer) ([]*client, error) {
	if time.Now().After(q.maintenanceAt) {
		q.drain = true
	}
	if _, err := core.FlushAOFAsync(wake); err != nil {
		return nil, err
	}
	ready := core.AppendReadyOffset()
	var writable []*client
	kept := q.held[:0]
	for _, c := range q.held {
		if clients[c.fd] != c {
			continue
		}
		if c.appendOffset <= ready {
			c.appendHeld = false
			writable = append(writable, c)
		} else {
			kept = append(kept, c)
		}
	}
	clear(q.held[len(kept):])
	q.held = kept
	flushClientReplies(pool, mux, writable)
	if core.AppendPending() || core.AppendBufferedBytes() != 0 {
		return nil, nil
	}
	if time.Now().After(q.maintenanceAt) {
		core.ExpireCycle()
		q.maintenanceAt = time.Now().Add(100 * time.Millisecond)
		if _, err := core.FlushAOFAsync(wake); err != nil {
			return nil, err
		}
	}
	q.drain, q.exclusive = false, false
	pending := q.deferred
	q.deferred = nil
	live := pending[:0]
	for _, c := range pending {
		if clients[c.fd] == c {
			c.appendDeferred = false
			live = append(live, c)
		}
	}
	return live, nil
}

func (q *orderedAppend) admit(c *client, mux io_multiplexing.IOMultiplexer) bool {
	if len(c.cmds) == 0 {
		return true
	}
	logBytes, replyBytes, bounded := core.AppendAdmission(c.cmds)
	// Three budgets, each of which can refuse on its own: the encoded log, the
	// aggregate of everything retained, and the replies alone. The last is what
	// stops a run being admitted whose replies would crowd out the requests
	// that have to be read for the queue to drain at all.
	fits := bounded && core.AppendHasRoom(logBytes) &&
		retainedClientBytes+core.AppendRetainedBytes()+2*logBytes+3*replyBytes <= maxRetainedClientBytes &&
		retainedReplyBytes+3*replyBytes <= maxRetainedClassBytes
	if !q.drain && !q.exclusive {
		if fits {
			return true
		}
		if !core.AppendPending() && core.AppendBufferedBytes() == 0 {
			// Keep the previous barrier semantics for unmodelled/large runs.
			// No subsequent run executes until this one drains.
			q.exclusive = true
			return true
		}
	}
	q.drain = true
	if err := mux.Monitor(io_multiplexing.Event{Fd: c.fd, Op: io_multiplexing.OpNone}); err != nil {
		closeClient(c)
		return false
	}
	c.appendDeferred = true
	q.deferred = append(q.deferred, c)
	return false
}

func (q *orderedAppend) gate(writable []*client, arena *replyArena, mux io_multiplexing.IOMultiplexer) []*client {
	ready := core.AppendReadyOffset()
	live := writable[:0]
	for _, c := range writable {
		if c.appendOffset <= ready {
			live = append(live, c)
			continue
		}
		if c.inArena {
			c.out = append([]byte(nil), arena.buf[c.outStart:c.outEnd]...)
			c.outBytes = cap(c.out)
			c.inArena = false
		}
		if !accountClient(c) {
			closeClient(c)
			continue
		}
		if err := mux.Monitor(io_multiplexing.Event{Fd: c.fd, Op: io_multiplexing.OpNone}); err != nil {
			closeClient(c)
			continue
		}
		c.appendHeld = true
		q.held = append(q.held, c)
	}
	return live
}

func parsedBytes(commands []*core.Command) int {
	used := cap(commands) * 8
	for _, cmd := range commands {
		used += 64 + len(cmd.Cmd) + cap(cmd.Args)*16
		for _, arg := range cmd.Args {
			used += len(arg)
		}
	}
	return used
}
