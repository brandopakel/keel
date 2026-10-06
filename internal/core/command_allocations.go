package core

// CommandAllocationBudget is owned by the event loop. A run reserves its large
// reply/workspace allocations before constructing them. Reservations last until
// execution returns and the caller accounts retained replies. They include
// conservative copy headroom, not a measurement of heap or process RSS.
type CommandAllocationBudget struct {
	Limit, Retained, Reserved, Peak          int
	ReplyLimit, ReplyRetained, ReplyReserved int
	Refusals                                 uint64
}

// SetCommandAllocations is the method of that name on the default engine,
// which plan step 2.7 removes.
func SetCommandAllocations(b *CommandAllocationBudget) { defaultEngine.SetCommandAllocations(b) }

// CommandAllocations is the method of that name on the default engine, which
// plan step 2.7 removes.
func CommandAllocations() *CommandAllocationBudget { return defaultEngine.CommandAllocations() }

// SetCommandAllocations installs the event-loop transport's budget on e, or
// removes it when b is nil. Core-only calls and alternate transports retain
// their per-command limits without this budget.
func (e *Engine) SetCommandAllocations(b *CommandAllocationBudget) { e.commandAllocations = b }

// CommandAllocations is the budget installed on e, nil when there is none.
func (e *Engine) CommandAllocations() *CommandAllocationBudget { return e.commandAllocations }

// Begin starts a serial execution run with buffers retained by earlier runs.
func (b *CommandAllocationBudget) Begin(retained int) {
	b.Retained, b.Reserved, b.ReplyReserved = retained, 0, 0
	b.ObserveRetained(retained)
}

// ObserveRetained refreshes existing buffer ownership without releasing the
// current run's reservations. Peak includes runs that allocate no large reply.
func (b *CommandAllocationBudget) ObserveRetained(retained int) {
	b.Retained = retained
	if retained >= 0 && b.Reserved <= int(^uint(0)>>1)-retained {
		b.Peak = max(b.Peak, retained+b.Reserved)
	}
}

// End releases reservations after execution; output ownership passes to the
// transport, which accounts that output before starting another run.
func (b *CommandAllocationBudget) End() { b.Reserved, b.ReplyReserved = 0, 0 }

// Reserve refuses before allocation and never changes the reserved amount on
// refusal. Subtraction checks avoid overflow even for untrusted sizes.
func (b *CommandAllocationBudget) Reserve(n int) bool {
	if n < 0 || b.Retained < 0 || b.Reserved < 0 || b.Retained > b.Limit || b.Reserved > b.Limit-b.Retained || n > b.Limit-b.Retained-b.Reserved {
		b.Refusals++
		return false
	}
	b.Reserved += n
	b.Peak = max(b.Peak, b.Retained+b.Reserved)
	return true
}

var allocationPressure = []byte("-ERR temporary command allocation budget exhausted\r\n")

func (e *Engine) reserveCommandMemory(n int) bool {
	return e.commandAllocations == nil || e.commandAllocations.Reserve(n)
}

// Reserve three payloads for output, arena growth and detaching a partial reply.
// Page rounding supplies slack for the backing allocation. Large outputs bypass
// the arena, but keep the same conservative admission policy.
func (e *Engine) reserveReplyMemory(n int) bool {
	if n < 0 || n > MaxReplyBytes {
		return false
	}
	charge := 3 * ((n + 4095) &^ 4095)
	budget := e.commandAllocations
	if budget == nil {
		return true
	}
	if budget.ReplyLimit > 0 && (budget.ReplyRetained > budget.ReplyLimit || budget.ReplyReserved > budget.ReplyLimit-budget.ReplyRetained || charge > budget.ReplyLimit-budget.ReplyRetained-budget.ReplyReserved) {
		budget.Refusals++
		return false
	}
	if !budget.Reserve(charge) {
		return false
	}
	budget.ReplyReserved += charge
	return true
}

// admitReply is the last check before a reply of n encoded bytes is built: it
// has to fit what the client can still be sent - the engine's reply ceiling -
// and building it has to fit the run's allocation budget. Each refusal names
// its own reason.
func (e *Engine) admitReply(n int) []byte {
	if n > e.replyCeiling {
		return replyTooLarge
	}
	if !e.reserveReplyMemory(n) {
		return allocationPressure
	}
	return nil
}

func (e *Engine) encodeBoundedString(value string) []byte {
	size, fits := addBulkSize(0, len(value))
	if !fits {
		return replyTooLarge
	}
	if refusal := e.admitReply(size); refusal != nil {
		return refusal
	}
	return appendBulkString(make([]byte, 0, size), value)
}
