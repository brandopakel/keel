package core

import "github.com/brandopakel/keel/internal/resp"

// CommandAllocationBudget is the transport's allocation budget, which a run
// of commands reserves its large replies and workspaces from before building
// them; see resp.Budget, where it lives so that a resp.Reply reserves from it
// directly.
type CommandAllocationBudget = resp.Budget

// SetCommandAllocations installs the event-loop transport's budget on e, or
// removes it when b is nil. Core-only calls and alternate transports retain
// their per-command limits without this budget.
func (e *Engine) SetCommandAllocations(b *CommandAllocationBudget) { e.commandAllocations = b }

// CommandAllocations is the budget installed on e, nil when there is none.
func (e *Engine) CommandAllocations() *CommandAllocationBudget { return e.commandAllocations }

// allocationPressure refuses what the budget cannot hold.
var allocationPressure = resp.AllocationPressure

func (e *Engine) reserveCommandMemory(n int) bool {
	return e.commandAllocations == nil || e.commandAllocations.Reserve(n)
}

// reserveReplyMemory reserves what a reply of n encoded bytes may cost from
// the run's budget (resp.Budget.ReserveReply); with no budget, any reply
// within the output limit may be built.
func (e *Engine) reserveReplyMemory(n int) bool {
	if n < 0 || n > MaxReplyBytes {
		return false
	}
	return e.commandAllocations == nil || e.commandAllocations.ReserveReply(n)
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
