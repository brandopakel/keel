package data_structure

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

// CreateSBChain starts a chain with one filter of the given capacity and error
// rate, growing by expansion each time it fills. It returns nil for a capacity
// of zero or an error rate outside (0, 1), which no filter can be sized for.
func CreateSBChain(capacity uint64, errorRate float64, expansion uint64) *SBChain {
	if capacity == 0 || errorRate <= 0 || errorRate >= 1 {
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

// Add records an item and reports whether it was new to the chain. False
// means the item was probably added before; like any Bloom filter answer, that
// can be a false positive.
func (sb *SBChain) Add(item string) bool {
	h := hashItem(item)
	if sb.hasHash(h) {
		return false
	}
	current := sb.newest()
	if current.size >= current.bloom.Entries {
		sb.grow(current.bloom.Entries*sb.growthFactor, current.bloom.Error*ErrorTighteningRatio)
		current = sb.newest()
	}
	current.bloom.addHash(h)
	current.size++
	sb.size++
	return true
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
