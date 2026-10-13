package resp

// Budget is the transport's allocation budget, which a run of commands
// reserves its large replies and workspaces from before building them. The
// event loop owns it: it begins a run with the buffers earlier runs retained,
// and ends it once execution returns and the run's replies are accounted.
// Reservations include conservative copy headroom; they are not a
// measurement of the heap or of the process's RSS. The engine knows it as
// core.CommandAllocationBudget, and a Reply reserves its memory from it.
type Budget struct {
	Limit, Retained, Reserved, Peak          int
	ReplyLimit, ReplyRetained, ReplyReserved int
	Refusals                                 uint64
}

// AllocationPressure is the reply that refuses what the budget cannot hold.
var AllocationPressure = []byte("-ERR temporary command allocation budget exhausted\r\n")

// Begin starts a serial execution run with buffers retained by earlier runs.
func (b *Budget) Begin(retained int) {
	b.Retained, b.Reserved, b.ReplyReserved = retained, 0, 0
	b.ObserveRetained(retained)
}

// ObserveRetained refreshes existing buffer ownership without releasing the
// current run's reservations. Peak includes runs that allocate no large reply.
func (b *Budget) ObserveRetained(retained int) {
	b.Retained = retained
	if retained >= 0 && b.Reserved <= int(^uint(0)>>1)-retained {
		b.Peak = max(b.Peak, retained+b.Reserved)
	}
}

// End releases reservations after execution; output ownership passes to the
// transport, which accounts that output before starting another run.
func (b *Budget) End() { b.Reserved, b.ReplyReserved = 0, 0 }

// Reserve refuses before allocation and never changes the reserved amount on
// refusal. Subtraction checks avoid overflow even for untrusted sizes.
func (b *Budget) Reserve(n int) bool {
	if n < 0 || b.Retained < 0 || b.Reserved < 0 || b.Retained > b.Limit || b.Reserved > b.Limit-b.Retained || n > b.Limit-b.Retained-b.Reserved {
		b.Refusals++
		return false
	}
	b.Reserved += n
	b.Peak = max(b.Peak, b.Retained+b.Reserved)
	return true
}

// ReserveReply reserves what a reply of n encoded bytes may cost: three
// payloads, for the output, the arena's growth and detaching a partial
// reply, page-rounded for slack in the backing allocation, within the
// reply limit as well as the whole. Large outputs bypass the arena but keep
// the same conservative admission.
func (b *Budget) ReserveReply(n int) bool {
	if n < 0 || n > MaxReplyBytes {
		return false
	}
	charge := 3 * ((n + 4095) &^ 4095)
	if b.ReplyLimit > 0 && (b.ReplyRetained > b.ReplyLimit || b.ReplyReserved > b.ReplyLimit-b.ReplyRetained || charge > b.ReplyLimit-b.ReplyRetained-b.ReplyReserved) {
		b.Refusals++
		return false
	}
	if !b.Reserve(charge) {
		return false
	}
	b.ReplyReserved += charge
	return true
}
