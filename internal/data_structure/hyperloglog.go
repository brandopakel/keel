package data_structure

import (
	"math"
	"math/bits"
	"sort"

	"github.com/spaolacci/murmur3"
)

// Implementation of the HyperLogLog cardinality estimator.
//
// The problem it solves: counting distinct items in a stream without storing
// them. An exact count needs memory proportional to the number of distinct
// items; HyperLogLog answers within about 0.8% using a fixed 12KB, whether the
// set holds ten items or ten billion.
//
// The idea is that a uniform hash makes leading-zero runs rare in a predictable
// way. Seeing a hash prefixed by k zeros suggests roughly 2^k distinct values
// have been hashed, because one value in 2^k looks like that. A single such
// observation is a terrible estimator - it is either right or wildly wrong - so
// the hash is split: the low bits choose one of m registers, and each register
// keeps the longest zero run it has seen. Averaging m weak estimators is what
// turns the guess into a measurement, and the error falls as 1/sqrt(m).
//
// Registers hold a value in [0, 51] here, so they need 6 bits rather than 8.
// Packing them saves a quarter of the memory: 12KB instead of 16KB.

const (
	// hllP is how many hash bits select a register. 14 gives 16384 registers
	// and a standard error of 1.04/sqrt(16384), about 0.81%. It matches Redis,
	// which matters for anyone comparing results between the two.
	hllP = 14
	// hllRegisters is m, the number of registers.
	hllRegisters = 1 << hllP
	// hllQ is how many hash bits are left to count zeros in, so a register
	// value never exceeds hllQ+1.
	hllQ = 64 - hllP
	// hllBits is the width of a packed register.
	hllBits = 6
	// hllRegisterMax is the largest value a 6-bit register holds.
	hllRegisterMax = (1 << hllBits) - 1
	// hllSeed is the murmur seed. Redis uses 0xadc83b19 for the same purpose.
	hllSeed = 0xadc83b19
	// Bound insertion work and retained sparse storage before promoting once.
	hllSparseLimit = 512
	hllDenseSize   = (hllRegisters*hllBits)/8 + 1
)

// alphaInf is 1/(2*ln2), the limit of HyperLogLog's alpha_m as m grows. The
// estimator below is Ertl's, which uses this limit directly instead of the
// per-m constant and empirical bias tables the original paper needed.
var alphaInf = 1.0 / (2.0 * math.Log(2.0))

// HLL starts with sorted nonzero registers and promotes to dense storage.
// Both representations use exactly the same registers, hash and estimator.
type HLL struct {
	// regs holds hllRegisters 6-bit values, packed four to every three bytes.
	// One spare byte at the end lets the two-byte window used for reads and
	// writes run past the last register without a bounds check.
	regs []byte
	// Entries pack the 14-bit register index and 6-bit value into uint32.
	// At most 512 entries bound both binary search and insertion shifting.
	sparse []uint32

	// cachedCount avoids rescanning 16384 registers when nothing has changed;
	// Count is otherwise O(m) even when called in a loop.
	cachedCount uint64
	cacheValid  bool
}

func CreateHLL() *HLL {
	return &HLL{}
}

// getRegister reads the 6-bit register at i.
//
// A register straddles a byte boundary three times in four, so both bytes are
// loaded into a 16-bit window and the value shifted out of it. That is why regs
// carries a spare trailing byte: for the last register the window would
// otherwise read past the end.
func (h *HLL) getRegister(i int) uint8 {
	if h.regs == nil {
		at := h.sparsePosition(i)
		if at < len(h.sparse) && h.sparse[at]>>hllBits == uint32(i) {
			return uint8(h.sparse[at] & hllRegisterMax)
		}
		return 0
	}
	return denseHLLRegister(h.regs, i)
}

func denseHLLRegister(regs []byte, i int) uint8 {
	bitPos := i * hllBits
	byteIdx, bitOff := bitPos/8, bitPos%8
	window := uint16(regs[byteIdx]) | uint16(regs[byteIdx+1])<<8
	return uint8((window >> bitOff) & hllRegisterMax)
}

// setRegister writes the 6-bit register at i.
func (h *HLL) setRegister(i int, val uint8) {
	val &= hllRegisterMax
	if h.regs == nil {
		at := h.sparsePosition(i)
		entry := uint32(i)<<hllBits | uint32(val)
		if at < len(h.sparse) && h.sparse[at]>>hllBits == uint32(i) {
			if val == 0 {
				copy(h.sparse[at:], h.sparse[at+1:])
				h.sparse = h.sparse[:len(h.sparse)-1]
			} else {
				h.sparse[at] = entry
			}
			return
		}
		if val == 0 {
			return
		}
		if len(h.sparse) < hllSparseLimit {
			if len(h.sparse) == cap(h.sparse) {
				grown := make([]uint32, len(h.sparse), max(4, 2*cap(h.sparse)))
				copy(grown, h.sparse)
				h.sparse = grown
			}
			h.sparse = append(h.sparse, 0)
			copy(h.sparse[at+1:], h.sparse[at:])
			h.sparse[at] = entry
			return
		}
		h.promoteDense()
	}
	setDenseHLLRegister(h.regs, i, val)
}

func setDenseHLLRegister(regs []byte, i int, val uint8) {
	bitPos := i * hllBits
	byteIdx, bitOff := bitPos/8, bitPos%8
	window := uint16(regs[byteIdx]) | uint16(regs[byteIdx+1])<<8
	window &^= uint16(hllRegisterMax) << bitOff
	window |= uint16(val&hllRegisterMax) << bitOff
	regs[byteIdx] = byte(window)
	regs[byteIdx+1] = byte(window >> 8)
}

func (h *HLL) sparsePosition(i int) int {
	return sort.Search(len(h.sparse), func(at int) bool { return h.sparse[at]>>hllBits >= uint32(i) })
}

func (h *HLL) promoteDense() {
	h.regs = make([]byte, hllDenseSize)
	for _, entry := range h.sparse {
		setDenseHLLRegister(h.regs, int(entry>>hllBits), uint8(entry&hllRegisterMax))
	}
	h.sparse = nil
}

// patLen splits a hash into the register it addresses and the value it observes.
//
// The low hllP bits pick the register. The rest is scanned for its first set
// bit, and the count of zeros before it, plus one, is what the register records.
// Setting bit hllQ before scanning guarantees the scan terminates on an
// all-zero remainder and caps the result at hllQ+1.
func patLen(hash uint64) (index int, count uint8) {
	index = int(hash & (hllRegisters - 1))
	rest := hash>>hllP | 1<<hllQ
	return index, uint8(bits.TrailingZeros64(rest) + 1)
}

// Add records item and reports whether the sketch changed.
//
// A register only ever moves up, to the longest run it has seen, so adding the
// same item twice changes nothing the second time. That is what makes the
// structure order-independent and idempotent, and why two sketches can be
// merged by taking the larger of each register pair.
func (h *HLL) Add(item string) bool {
	index, count := patLen(murmur3.Sum64WithSeed([]byte(item), hllSeed))
	if h.regs != nil {
		if count <= denseHLLRegister(h.regs, index) {
			return false
		}
		setDenseHLLRegister(h.regs, index, count)
		h.cacheValid = false
		return true
	}
	if count <= h.getRegister(index) {
		return false
	}
	h.setRegister(index, count)
	h.cacheValid = false
	return true
}

// Count estimates the number of distinct items added.
func (h *HLL) Count() uint64 {
	if h.cacheValid {
		return h.cachedCount
	}

	// How many registers hold each value. Everything below is a function of
	// this histogram rather than of the registers themselves.
	var histogram [hllQ + 2]int
	if h.regs == nil {
		histogram[0] = hllRegisters - len(h.sparse)
		for _, entry := range h.sparse {
			histogram[entry&hllRegisterMax]++
		}
	} else {
		for i := 0; i < hllRegisters; i++ {
			histogram[denseHLLRegister(h.regs, i)]++
		}
	}

	// Ertl's estimator. The original HyperLogLog took a harmonic mean of the
	// registers and needed two special cases - linear counting for small
	// cardinalities, a correction for large ones - with empirically tabulated
	// bias in between. This form handles the whole range in one expression:
	// tau corrects for registers that have saturated, sigma for registers still
	// at zero, and the loop folds the rest in from the top down.
	m := float64(hllRegisters)
	z := m * tau((m-float64(histogram[hllQ+1]))/m)
	for j := hllQ; j >= 1; j-- {
		z += float64(histogram[j])
		z *= 0.5
	}
	z += m * sigma(float64(histogram[0])/m)

	h.cachedCount = uint64(math.Round(alphaInf * m * m / z))
	h.cacheValid = true
	return h.cachedCount
}

// Merge folds other into h, so h estimates the union of both.
//
// Union is exact in the sense that matters: the merged sketch is identical to
// one built by adding every item from both sets, because each register holds a
// maximum and max is associative. Intersections have no such property, which is
// why HyperLogLog offers a union and not an intersection.
func (h *HLL) Merge(other *HLL) {
	if other.regs == nil {
		for _, entry := range other.sparse {
			i, v := int(entry>>hllBits), uint8(entry&hllRegisterMax)
			if v > h.getRegister(i) {
				h.setRegister(i, v)
			}
		}
	} else {
		for i := 0; i < hllRegisters; i++ {
			if v := denseHLLRegister(other.regs, i); v > h.getRegister(i) {
				h.setRegister(i, v)
			}
		}
	}
	h.cacheValid = false
}

// sigma and tau are the correction terms of Ertl's estimator. Both are defined
// as infinite series and both converge fast; iterating until the running sum
// stops changing is exactly the fixed point of float64 addition, which is the
// termination condition the paper's reference implementation uses.
func sigma(x float64) float64 {
	if x == 1.0 {
		return math.Inf(1)
	}
	y, z := 1.0, x
	for {
		x *= x
		prev := z
		z += x * y
		y += y
		if z == prev {
			return z
		}
	}
}

func tau(x float64) float64 {
	if x == 0.0 || x == 1.0 {
		return 0.0
	}
	y, z := 1.0, 1-x
	for {
		x = math.Sqrt(x)
		prev := z
		y *= 0.5
		z -= (1 - x) * (1 - x) * y
		if z == prev {
			return z / 3
		}
	}
}

// MemUsage charges capacity, including the allocator's dense size class.
func (h *HLL) MemUsage() uint64 {
	if h.regs == nil {
		return 64 + uint64(cap(h.sparse))*4
	}
	return hllBaseBytes + uint64(len(h.regs))
}
