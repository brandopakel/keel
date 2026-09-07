package core

// CommandAllocationBudget is owned by the event loop. A run reserves its large
// reply/workspace allocations before constructing them. Reservations last until
// execution returns and the caller accounts retained replies. They include
// conservative copy headroom, not a measurement of heap or process RSS.
type CommandAllocationBudget struct {
	Limit, Retained, Reserved, Peak int
	Refusals                        uint64
}

// CommandAllocations is installed by the event-loop transport. Core-only calls
// and alternate transports retain their per-command limits without this budget.
var CommandAllocations *CommandAllocationBudget

// Begin starts a serial execution run with buffers retained by earlier runs.
func (b *CommandAllocationBudget) Begin(retained int) {
	b.Retained, b.Reserved = retained, 0
}

// End releases reservations after execution; output ownership passes to the
// transport, which accounts that output before starting another run.
func (b *CommandAllocationBudget) End() { b.Reserved = 0 }

// Reserve refuses before allocation and never changes the reserved amount on
// refusal. Subtraction checks avoid overflow even for untrusted sizes.
func (b *CommandAllocationBudget) Reserve(n int) bool {
	if n < 0 || b.Retained < 0 || b.Retained > b.Limit || b.Reserved > b.Limit-b.Retained || n > b.Limit-b.Retained-b.Reserved {
		b.Refusals++
		return false
	}
	b.Reserved += n
	b.Peak = max(b.Peak, b.Retained+b.Reserved)
	return true
}

var allocationPressure = []byte("-ERR temporary command allocation budget exhausted\r\n")

func reserveCommandMemory(n int) bool {
	return CommandAllocations == nil || CommandAllocations.Reserve(n)
}

// Reserve three payloads for output, arena growth and detaching a partial reply.
// Page rounding supplies slack for the backing allocation. Large outputs bypass
// the arena, but keep the same conservative admission policy.
func reserveReplyMemory(n int) bool {
	if n < 0 || n > MaxReplyBytes {
		return false
	}
	return reserveCommandMemory(3 * ((n + 4095) &^ 4095))
}

func encodeBoundedString(value string) []byte {
	size, fits := addBulkSize(0, len(value))
	if !fits {
		return replyTooLarge
	}
	if !reserveReplyMemory(size) {
		return allocationPressure
	}
	return appendBulkString(make([]byte, 0, size), value)
}
