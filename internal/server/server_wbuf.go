package server

// WriteUnbuffered makes the event loop issue one write syscall per reply
// instead of coalescing all replies produced from one read into a single write.
//
// Coalescing is the default because it is strictly better for a served
// workload: one write per command caps a pipelined client at the rate the
// machine can issue syscalls - measured on Linux, a flat ~124k ops/second no
// matter how deep the pipeline - and removing that ceiling was worth 4.1x at
// P=8, 8.1x at P=16 and 13.9x at P=64. Nothing about the unbuffered path is
// better; it is retained only so it can still be measured.
//
// That measurement is the reason this flag exists. The net.Listener
// implementation writes through a bufio.Writer and so gets coalescing for free,
// while upstream's event loop writes once per command. Comparing those two
// directly would mostly measure the write policy rather than the I/O mechanism,
// so the bench runs the event loop both ways and isolates the variable under
// test. `-mode kqueue-nobuf` selects this path and is the baseline
// representing upstream's design in everything under bench/results.
//
// Either way every write is nonblocking: a reply the socket will not take yet
// stays pending and resumes on write readiness, so a slow reader never holds
// the loop.
var WriteUnbuffered bool
