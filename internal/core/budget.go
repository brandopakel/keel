package core

import (
	"errors"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Sizes that arrive from a client.
//
// A reserve or init names the size of the structure it wants, and that size
// is allocated in full before the memory budget ever sees the key: the budget
// is enforced after a Put, and the allocation comes first. So an unchecked
// size is a way to take the server down with one command, and the check has to
// happen here, on the number, before anything is made.

// maxStructureBytes is the most one reserve or init may allocate for one key;
// the structures enforce the same figure on their own growth.
const maxStructureBytes = data_structure.MaxStructureBytes

var (
	errTooLargeForOneKey = errors.New("ERR the requested size is more than this server will allocate for one key")
	errTooLargeForBudget = errors.New("OOM the requested size does not fit in maxmemory")
)

// affordable refuses a size that could never be held: above the per-key cap,
// or larger than the whole memory budget when one is set. A size that fits the
// budget but not what is left of it is allowed through, because making room
// for it by evicting other keys is what the budget is for.
func affordable(bytes uint64) error {
	if bytes > maxStructureBytes {
		return errTooLargeForOneKey
	}
	if !aof.replaying && config.MaxMemory > 0 && bytes > config.MaxMemory {
		return errTooLargeForBudget
	}
	return nil
}
