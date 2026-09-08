package server

import (
	"sync/atomic"

	"github.com/brandopakel/keel/internal/core"
)

// The owner starts a reservation window before handing reads to I/O workers.
// Every worker shares its remaining aggregate/input-class allowance. Charges
// include old/new buffer overlap and all parsing allocations through the phase;
// they are replaced by retained ownership after all readers have joined.
type requestAllocationBudget struct {
	limit, base int64
	used        atomic.Int64
	refusals    atomic.Uint64
	peak        int64 // Updated by the owner after readers join.
}

func (b *requestAllocationBudget) begin(total, input, totalLimit, inputLimit int) {
	b.base = int64(max(0, total))
	b.limit = 0
	if total >= 0 && input >= 0 && total <= totalLimit && input <= inputLimit {
		b.limit = int64(min(totalLimit-total, inputLimit-input))
	}
	b.used.Store(0)
}

func (b *requestAllocationBudget) reserve(n int) bool {
	if b == nil {
		return true
	}
	for {
		used := b.used.Load()
		if n < 0 || used > b.limit || int64(n) > b.limit-used {
			b.refusals.Add(1)
			return false
		}
		if b.used.CompareAndSwap(used, used+int64(n)) {
			return true
		}
	}
}

func (b *requestAllocationBudget) reserveObject(n int) bool {
	charge, ok := core.RequestAllocationSize(n)
	if !ok {
		if b != nil {
			b.refusals.Add(1)
		}
		return false
	}
	return b.reserve(charge)
}

func (b *requestAllocationBudget) end() {
	b.peak = max(b.peak, b.base+b.used.Load())
	b.used.Store(0)
}

var requestAllocationReply = []byte("-ERR request allocation budget exhausted\r\n")

// This reports a new backing allocation, not a delta: during growth, both old
// and new arrays are live. Sliding the unparsed suffix needs no allocation.
func (b *connBuffer) growthCapacity(n int) int {
	if cap(b.data)-len(b.data) >= n || cap(b.data)-b.size() >= n {
		return 0
	}
	return max(2*cap(b.data), b.size()+n)
}
