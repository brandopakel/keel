package data_structure

import (
	"errors"
	"math"
)

// A scalable Bloom filter, after Almeida, Baquero, Preguiça and Hutchison,
// "Scalable Bloom Filters" (2007).
//
// A Bloom filter has to be sized for the number of items it will hold, and a
// key does not say in advance how many that is. The scalable form is a chain of
// filters: items go into the newest one until it holds what it was sized for,
// then a larger one is added behind it. A lookup asks every filter in the
// chain.
//
// Each filter added has a tighter error rate than the one before it, by a fixed
// ratio, so that the chain's overall false positive rate converges to a bound
// rather than growing with every filter. With the ratio r and an initial rate
// p, the bound is p / (1 - r); at the defaults here that is twice the rate the
// first filter was asked for.

const (
	// ErrorTighteningRatio is how much stricter each new filter's error rate
	// is than the last one's.
	ErrorTighteningRatio = 0.5
	// BfDefaultExpansion is how many times larger each new filter is than the
	// last one.
	BfDefaultExpansion = 2
	// BfDefaultInitCapacity and BfDefaultErrRate size a filter that was not
	// reserved before its first item.
	BfDefaultInitCapacity = 100
	BfDefaultErrRate      = 0.01
)

// SBLink is one filter in the chain and how many items it has taken.
type SBLink struct {
	bloom *Bloom
	size  uint64
}

// SBChain is a scalable Bloom filter.
type SBChain struct {
	filters []SBLink
	// size is the number of items added to the whole chain.
	size uint64
	// growthFactor multiplies the capacity of each new filter.
	growthFactor uint64
}

// ErrFilterTooLarge is answered by Add when the filter would have to grow and
// the next filter in the chain would be larger than MaxStructureBytes. The
// item is not added; everything already in the chain is still there.
var ErrFilterTooLarge = errors.New("ERR the filter cannot grow: its next filter would be larger than this server allocates for one key")

// CreateSBChain starts a chain with one filter of the given capacity and error
// rate, growing by expansion each time it fills. It returns nil for a capacity
// of zero, an error rate outside (0, 1), an expansion below one, or a first
// filter larger than MaxStructureBytes: no filter can be sized for the first
// two, the third would grow a filter of no capacity the first time the chain
// filled, and the fourth is an allocation nothing should make.
func CreateSBChain(capacity uint64, errorRate float64, expansion uint64) *SBChain {
	// NaN is refused by name: it compares false against everything, so the
	// range check alone would let it through into the sizing arithmetic.
	if capacity == 0 || math.IsNaN(errorRate) || errorRate <= 0 || errorRate >= 1 || expansion == 0 {
		return nil
	}
	if BloomBytesFor(capacity, errorRate) > MaxStructureBytes {
		return nil
	}
	sb := &SBChain{growthFactor: expansion}
	sb.grow(capacity, errorRate)
	return sb
}

// grow appends an empty filter of the given size and error rate.
func (sb *SBChain) grow(capacity uint64, errorRate float64) {
	sb.filters = append(sb.filters, SBLink{bloom: CreateBloomFilter(capacity, errorRate)})
}

// newest is the filter items are currently added to.
func (sb *SBChain) newest() *SBLink { return &sb.filters[len(sb.filters)-1] }

// hasHash asks every filter, newest first, since recent items are the likelier
// question.
func (sb *SBChain) hasHash(h bloomHash) bool {
	for i := len(sb.filters) - 1; i >= 0; i-- {
		if sb.filters[i].bloom.hasHash(h) {
			return true
		}
	}
	return false
}

// nextFilter is the capacity and error rate the chain would grow to next. The
// capacity saturates rather than wrapping: an expansion large enough to
// overflow is an allocation that must be refused, not a small one by accident.
func (sb *SBChain) nextFilter() (capacity uint64, errorRate float64) {
	current := sb.newest()
	capacity = current.bloom.Entries
	if capacity > math.MaxUint64/sb.growthFactor {
		capacity = math.MaxUint64
	} else {
		capacity *= sb.growthFactor
	}
	return capacity, current.bloom.Error * ErrorTighteningRatio
}

// Add records an item and reports whether it was new to the chain. False
// means the item was probably added before; like any Bloom filter answer, that
// can be a false positive.
//
// A new item that arrives when the newest filter is full makes the chain grow,
// and the growth is sized before it is made: a filter larger than
// MaxStructureBytes is refused with ErrFilterTooLarge and the item is not
// added. Without that, an expansion of a few billion turned the second item
// into a multi-gigabyte allocation on the command thread.
func (sb *SBChain) Add(item string) (added bool, err error) {
	h := hashItem(item)
	if sb.hasHash(h) {
		return false, nil
	}
	current := sb.newest()
	if current.size >= current.bloom.Entries {
		capacity, errorRate := sb.nextFilter()
		if BloomBytesFor(capacity, errorRate) > MaxStructureBytes {
			return false, ErrFilterTooLarge
		}
		sb.grow(capacity, errorRate)
		current = sb.newest()
	}
	current.bloom.addHash(h)
	current.size++
	sb.size++
	return true, nil
}

// Exists reports whether an item may have been added.
func (sb *SBChain) Exists(item string) bool { return sb.hasHash(hashItem(item)) }

// Capacity is how many items the chain can take before it grows again: the
// sum of what each filter was sized for.
func (sb *SBChain) Capacity() uint64 {
	var total uint64
	for i := range sb.filters {
		total += sb.filters[i].bloom.Entries
	}
	return total
}

// Count is how many items have been added.
func (sb *SBChain) Count() uint64 { return sb.size }

// Filters is how many filters the chain has grown to.
func (sb *SBChain) Filters() int { return len(sb.filters) }

// Expansion is the growth factor.
func (sb *SBChain) Expansion() uint64 { return sb.growthFactor }

// MemUsage estimates the bytes held by the whole chain.
//
// This previously returned reflect.TypeOf(*sb).Size(), which is the size of the
// SBChain struct header - three words - and not the bit arrays hanging off it.
// BF.INFO therefore reported about 40 bytes for a filter holding megabytes, and
// a memory budget built on it would have been meaningless.
func (sb *SBChain) MemUsage() uint64 {
	total := uint64(sbChainBaseBytes)
	for i := range sb.filters {
		total += bloomBaseBytes + sb.filters[i].bloom.bytes
	}
	return total
}
