package core

import (
	"fmt"
	"strings"
)

// INFO's Threads, Hotkeys and Modules sections, and Stats' io_threaded_*
// lines, as Redis 8.10.1 writes them (server.c, genRedisInfoString).
//
// The file's name sorts after every other of the package's, so that the
// functions it adds come last in the package's code and move none of the
// commands' before them: the paired command-path job reads such a move as a
// change in the commands' speed. The transport's threads come through a hook
// of their own, SetIOThreads, rather than ClientBufferStats, whose copies in
// SampleCommandRate and INFO would otherwise change size.

// IOThreadStats is one I/O thread's line in INFO's Threads section: the
// clients it serves, and the reads and writes it has made.
type IOThreadStats struct {
	Clients       int
	Reads, Writes uint64
}

// SetIOThreads installs on e the hook INFO reads the transport's I/O threads
// through, the loop's own first, whose reads and writes sum to
// ClientBufferStats' ReadsProcessed and WritesProcessed; or removes it when f
// is nil.
func (e *Engine) SetIOThreads(f func() []IOThreadStats) { e.ioThreads = f }

// threadsInfo is Redis's Threads section: a line for each I/O thread, the
// loop's own first, with the clients it serves and the reads and writes it
// made (server.c). Keel's workers take a share of whichever connections are
// ready at each turn rather than clients of their own, so the loop's line
// holds every client and the workers' none; their reads and writes are what
// each made. With no transport there are no threads to report.
func (e *Engine) threadsInfo(b *strings.Builder) {
	if e.ioThreads == nil {
		return
	}
	b.WriteString("# Threads\r\n")
	for i, t := range e.ioThreads() {
		fmt.Fprintf(b, "io_thread_%d:clients=%d,reads=%d,writes=%d\r\n", i, t.Clients, t.Reads, t.Writes)
	}
	b.WriteString("\r\n")
}

// ioThreadedInfo is Stats' io_threaded_reads_processed and
// io_threaded_writes_processed: what the I/O threads other than the loop's
// made, as Redis sums its threads but the main one.
func (e *Engine) ioThreadedInfo(b *strings.Builder) {
	var reads, writes uint64
	if e.ioThreads != nil {
		threads := e.ioThreads()
		for i := 1; i < len(threads); i++ {
			reads, writes = reads+threads[i].Reads, writes+threads[i].Writes
		}
	}
	fmt.Fprintf(b, "io_threaded_reads_processed:%d\r\nio_threaded_writes_processed:%d\r\n", reads, writes)
}

// hotkeysInfo is Redis's Hotkeys section, which has fields only while HOTKEYS
// tracking runs (server.c); Keel has no hot key tracking, so it is the header.
func (e *Engine) hotkeysInfo(b *strings.Builder) {
	b.WriteString("# Hotkeys\r\n\r\n")
}

// modulesInfo is Redis's Modules section, a line for each module loaded. Keel
// loads none: its Bloom, cuckoo and count-min commands are its own, and the
// owner decided they are not listed as RedisBloom's, which would also commit
// MODULE LIST to answering for a module (docs/info-compatibility.md).
func (e *Engine) modulesInfo(b *strings.Builder) {
	b.WriteString("# Modules\r\n\r\n")
}
