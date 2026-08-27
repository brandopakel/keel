package server

// WriteBuffered makes the event loop coalesce all replies produced from one
// read into a single write syscall.
//
// It exists to keep the epoll/kqueue-versus-netpoller comparison honest. The
// net.Listener implementation writes through a bufio.Writer and therefore gets
// reply coalescing for free, while the event loop as written issues one write
// per command. Comparing the two directly would mostly measure that difference
// rather than the I/O mechanism, so this flag lets the event loop adopt the same
// write policy and isolates the variable under test.
//
// The buffering itself lives in respondBatch, which the unbuffered path shares
// through FDComm.Write. This file previously carried its own copy - a bufComm
// sink and a writeAll loop that spun on EAGAIN. Spinning burns a core and, in a
// single-threaded event loop, stalls every other connection, so the copy was
// dropped in favour of the blocking-mode fallback in FDComm.Write.
var WriteBuffered bool
