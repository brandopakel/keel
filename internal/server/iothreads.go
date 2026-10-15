package server

import (
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/brandopakel/keel/internal/core"
)

// Threaded socket I/O, in the style of Redis's io-threads.
//
// One thread can only copy so many bytes. The read path here now moves 3866
// MB/s on a single core, which was fast enough to stop being the bottleneck for
// value size and start being the bottleneck for everything - the same wall that
// made Redis 6 add I/O threads in 2020. Beyond it the only way forward is more
// than one core, and the question is which work to move.
//
// Not command execution. Executing on one thread is what lets every store in
// core/storage.go be an unsynchronised map, and it is why a command needs no
// locking and no transaction. Giving that up to gain throughput would be a
// different database.
//
// What is left is everything either side: the read syscall, parsing bytes into
// commands, and the write syscall. That is the majority of the work for small
// commands and nearly all of it for large ones, and none of it touches a store.
// So a cycle of the event loop becomes three phases:
//
//	read    every ready connection is read and parsed, in parallel
//	execute every command runs, on the loop's own thread, in arrival order
//	write   every reply is written, in parallel
//
// Threads are handed a slice of connections each and the loop waits for all of
// them before moving on. Connections do not migrate mid-phase, so no two
// threads ever touch the same connection, and the map they live in is only
// mutated by the loop thread between phases.
//
// The phases replaced the loop for everyone, not only for a server that asks
// for threads, so the default path is not the code any earlier measurement was
// taken against. That is measured rather than assumed: bench/run-ab.sh runs the
// tree before the phases against the tree after, arms alternating rep by rep.
// Flat on small values and 10-13% faster at 256KB, which is where reading every
// ready connection before executing any of them has the most to batch.
//
// It is off by default because the measurements are mixed. On darwin/arm64,
// medians of five runs, the results that held steady inside the noise floor:
// 1.41x at 256KB SET, saturating at four threads, which is where the
// motivation was; 11% down in a band around 8KB, for GET and SET alike; and no
// change on a single connection, which is ioThreshold declining to split work
// that cannot pay for the split. Small payloads at c=50 and above measured
// 1.05x to 1.25x with 15-32% spread, so they are recorded rather than relied
// on.
//
// The barrier was the first suspect for the rest, and it is not the answer. A
// channel-and-WaitGroup rendezvous measures 688ns for three workers, which at
// these rates is a fraction of a percent. What is worth recording is what the
// alternative measured: Redis does not use a channel here, its I/O threads
// busy-wait on an atomic, because in C a spin beats parking a thread. The same
// spin in Go measured 11.4us - sixteen times worse - because goroutines are
// multiplexed onto threads rather than pinned to cores, so one that spins
// fights the scheduler instead of sidestepping it. The likelier cost is moving
// each connection's bytes between cores for a phase that is over in a
// microsecond, which is a reading the numbers are consistent with rather than
// one they establish. bench/README.md has the full table and the caveat that
// these are loopback runs sharing a machine with the client.

// ioThreshold is how many connections a phase needs before distributing them is
// worth the handoff at all. Redis uses io_threads_num*2 and stops threading
// below it; below it here the loop thread does the whole phase itself.
func ioThreshold(threads int) int { return threads * 2 }

// ioPool owns the worker goroutines and the per-thread assignment slices.
type ioPool struct {
	threads       int
	requestBudget *requestAllocationBudget
	jobs          []chan ioJob
	wg            sync.WaitGroup

	// assign[i] is thread i's share for the current phase, slot 0 being the
	// loop's own. Reused between phases: they are only ever rewritten after
	// wg.Wait has returned, so no worker is still reading one.
	assign [][]*client

	// mainScratch is the loop thread's landing area for reads. Each worker
	// allocates its own, which is why readScratch could stop being a single
	// array for the whole server: one per thread is still not one per
	// connection, which is the property that matters.
	mainScratch []byte

	// io is each worker's reads and writes, for INFO's Threads section; the
	// loop's own share is the rest of readsProcessed and writesProcessed.
	// Slot 0, the loop's, is unused.
	io []ioCounters
}

// ioCounters are one worker's reads and writes, padded to a cache line of
// their own, since each worker adds to its own while the others add to theirs.
type ioCounters struct {
	reads, writes atomic.Uint64
	_             [48]byte
}

type ioJob struct {
	clients []*client
	write   bool
}

func newIOPool(threads int) *ioPool {
	if threads < 1 {
		threads = 1
	}
	p := &ioPool{
		threads:     threads,
		assign:      make([][]*client, threads),
		mainScratch: make([]byte, readChunkSize),
		io:          make([]ioCounters, threads),
	}
	for i := 1; i < threads; i++ {
		ch := make(chan ioJob)
		p.jobs = append(p.jobs, ch)
		go p.worker(ch, &p.io[i])
	}
	if threads > 1 {
		log.Printf("io threads: %d (1 loop thread + %d workers)", threads, threads-1)
	}
	return p
}

func (p *ioPool) worker(ch chan ioJob, io *ioCounters) {
	scratch := make([]byte, readChunkSize)
	for job := range ch {
		for _, c := range job.clients {
			n := p.serve(c, job.write, scratch)
			if job.write {
				io.writes.Add(n)
			} else {
				io.reads.Add(n)
			}
		}
		p.wg.Done()
	}
}

// threadStats is INFO's Threads section: the loop's line first, holding every
// client, since the workers take a share of whichever connections are ready
// rather than clients of their own, and the reads and writes the workers did
// not make; then each worker's. A nil pool, before the server has made one,
// has none.
func (p *ioPool) threadStats(clients int) []core.IOThreadStats {
	if p == nil {
		return nil
	}
	stats := make([]core.IOThreadStats, p.threads)
	reads, writes := readsProcessed.Load(), writesProcessed.Load()
	for i := 1; i < p.threads; i++ {
		r, w := p.io[i].reads.Load(), p.io[i].writes.Load()
		stats[i] = core.IOThreadStats{Reads: r, Writes: w}
		reads, writes = reads-min(reads, r), writes-min(writes, w)
	}
	stats[0] = core.IOThreadStats{Clients: clients, Reads: reads, Writes: writes}
	return stats
}

// resetIO starts the workers' counts again, for CONFIG RESETSTAT, as Redis's
// resetServerStats zeroes each I/O thread's.
func (p *ioPool) resetIO() {
	if p == nil {
		return
	}
	for i := range p.io {
		p.io[i].reads.Store(0)
		p.io[i].writes.Store(0)
	}
}

// stop shuts the workers down. Called as the loop unwinds, so the goroutines go
// away with everything else rather than outliving the server they belong to.
func (p *ioPool) stop() {
	for _, ch := range p.jobs {
		close(ch)
	}
	p.jobs = nil
}

// serve does one phase's I/O for c, a write or a read, and reports how many
// reads or writes it made.
func (p *ioPool) serve(c *client, write bool, scratch []byte) uint64 {
	if write {
		c.err = nil
		if len(c.out) > maxOutputBuffer {
			c.err = fmt.Errorf("output buffer limit exceeded")
			return 0
		}
		part := c.out
		if len(c.frames) > 0 {
			part = part[:c.frames[0]]
		}
		n, err := syscall.Write(c.fd, part)
		writesProcessed.Add(1)
		if n > 0 {
			if c.replica {
				netReplOutputBytes.Add(uint64(n))
			} else {
				netOutputBytes.Add(uint64(n))
			}
			c.out = c.out[n:]
			if len(c.frames) > 0 {
				c.frames[0] -= n
				if c.frames[0] == 0 {
					c.frames = c.frames[1:]
				}
			}
			c.lastProgress = time.Now()
		}
		if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
			c.err = err
		}
		return 1
	}

	before := c.reads
	if len(c.cmds) == 0 {
		c.cmds, c.err = c.readCommandsReserved(scratch, p.requestBudget)
	}
	return c.reads - before
}

// run performs one phase over cs, in parallel when that is worth doing.
func (p *ioPool) run(cs []*client, write bool) {
	if len(cs) == 0 {
		return
	}
	if p.threads == 1 || len(cs) < ioThreshold(p.threads) {
		for _, c := range cs {
			p.serve(c, write, p.mainScratch)
		}
		return
	}

	for i := range p.assign {
		p.assign[i] = p.assign[i][:0]
	}
	for i, c := range cs {
		slot := i % p.threads
		p.assign[slot] = append(p.assign[slot], c)
	}

	handed := 0
	for i := 1; i < p.threads; i++ {
		if len(p.assign[i]) > 0 {
			handed++
		}
	}
	p.wg.Add(handed)
	for i := 1; i < p.threads; i++ {
		if len(p.assign[i]) > 0 {
			p.jobs[i-1] <- ioJob{clients: p.assign[i], write: write}
		}
	}

	// The loop thread takes a share too rather than only supervising, which is
	// what Redis does: with one worker and a supervisor idle, half the machine
	// would be waiting on the other half.
	for _, c := range p.assign[0] {
		p.serve(c, write, p.mainScratch)
	}
	p.wg.Wait()
}

// replyArena holds every reply produced in one cycle.
//
// Replies can no longer be written the moment they are produced: the write
// happens in a later phase, so a reply has to survive from execution until
// then. Giving each connection its own reply buffer would do it and would also
// give away the thing the event loop is chosen for - 20,000 connections holding
// a 4KB buffer each is 80MB of idle server, which is the netpoller's cost that
// bench/ exists to measure. So the replies of a whole cycle go into one array
// for the entire server and each connection keeps a window into it.
//
// Appending can move the array, so the windows are resolved into slices only
// once the last reply of the cycle has been appended.
type replyArena struct{ buf []byte }

// arenaShrinkAbove is the size past which an arena that is no longer being
// filled is given back. Without it a single deeply pipelined moment would leave
// the server holding that buffer forever.
const arenaShrinkAbove = 1 << 20

func (a *replyArena) reset() {
	if cap(a.buf) > arenaShrinkAbove && len(a.buf) < cap(a.buf)/4 {
		a.buf = nil
	}
	a.buf = a.buf[:0]
}

// arenaWriter appends a reply to the cycle's arena.
type arenaWriter struct{ a *replyArena }

func (w arenaWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w arenaWriter) Write(p []byte) (int, error) {
	w.a.buf = append(w.a.buf, p...)
	return len(p), nil
}

// captureWriter keeps the reply it is handed instead of copying it.
//
// A batch of one command needs no coalescing, and a large value is nearly
// always a batch of one because it fills a read on its own. Sending it through
// the arena would copy the whole value for nothing - the reason respondBatch
// special-cased a single reply before the phases existed, and the reason it
// still has to be special-cased now.
type captureWriter struct{ p []byte }

func (w *captureWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w *captureWriter) Write(p []byte) (int, error) {
	if w.p == nil {
		w.p = p
		return len(p), nil
	}
	// Only reachable if a command ever writes twice. Correct rather than fast,
	// since nothing does today and a silent aliasing bug if something started
	// to would be far worse than a copy.
	joined := make([]byte, 0, len(w.p)+len(p))
	w.p = append(append(joined, w.p...), p...)
	return len(p), nil
}
