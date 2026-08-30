package data_structure

import (
	"strconv"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"

	"memkv/internal/config"
)

func TestLFUStateRoundTripsThroughOneField(t *testing.T) {
	var o Obj
	for _, decayAt := range []uint64{0, 1, 1 << 20, (1 << 56) - 1} {
		for _, freq := range []uint8{0, 1, 5, 128, 255} {
			o.Access = packLFU(decayAt, freq)
			assert.Equal(t, freq, lfuFreqOf(o.Access), "freq %d at decayAt %d", freq, decayAt)
			assert.Equal(t, decayAt, lfuDecayAtOf(o.Access), "decayAt %d with freq %d", decayAt, freq)
		}
	}
}

// TestObjStaysOneWordPerPolicyField is why the two policies share a field. The
// cost is per key, so it is multiplied by the size of the keyspace: separate
// fields measured 48 bytes against 32, which is 76MB at the default limit.
func TestObjStaysOneWordPerPolicyField(t *testing.T) {
	assert.Equal(t, uintptr(32), unsafe.Sizeof(Obj{}),
		"adding per-policy state must not grow the per-key object")
}

func TestNewKeysStartWithCredit(t *testing.T) {
	withEviction(t, config.LFU, 5, 100)
	d := newTestDict(t)
	obj := d.NewObj("v", 0, 0)
	assert.Equal(t, uint8(lfuInitVal), lfuFreqOf(obj.Access),
		"a new key needs credit, or it is by definition the least frequently used thing present")
}

// TestCounterRiseSlowsDown is the property that lets eight bits span millions of
// accesses: the counter is a rank, not a count.
func TestCounterRiseSlowsDown(t *testing.T) {
	withEviction(t, config.LFU, 5, 1000000)
	config.LFUDecayPeriod = 0 // isolate the increment from decay
	d := newTestDict(t)

	obj := d.NewObj("v", 0, 0)
	start := lfuFreqOf(obj.Access)

	accessesFor := func(target uint8) int {
		n := 0
		for lfuFreqOf(obj.Access) < target && n < 10000000 {
			touchLFU(&obj.Access)
			n++
		}
		return n
	}

	early := accessesFor(start + 3)
	late := accessesFor(start + 8)
	assert.Greater(t, late, early*2,
		"later increments must cost far more accesses than earlier ones (early %d, late %d)", early, late)
}

func TestCounterSaturatesRatherThanWrapping(t *testing.T) {
	withEviction(t, config.LFU, 5, 100)
	var o Obj
	o.Access = packLFU(0, 255)
	assert.Equal(t, uint8(255), lfuLogIncr(lfuFreqOf(o.Access)),
		"the counter must saturate; wrapping would turn the hottest key into the coldest")
}

func TestDecayLowersAnIdleCounter(t *testing.T) {
	withEviction(t, config.LFU, 5, 1000000)
	config.LFUDecayPeriod = 100
	d := newTestDict(t)

	obj := d.NewObj("v", 0, 0)
	obj.Access = packLFU(evictionClock, 50)

	assert.Equal(t, uint8(50), decayedFreq(obj.Access), "no time has passed")

	evictionClock += 100 * 10
	assert.Equal(t, uint8(40), decayedFreq(obj.Access), "ten periods should cost ten points")

	evictionClock += 100 * 1000
	assert.Equal(t, uint8(0), decayedFreq(obj.Access), "decay must floor at zero, not wrap")
}

func TestDecayIsLazyAndDoesNotMutate(t *testing.T) {
	withEviction(t, config.LFU, 5, 1000000)
	config.LFUDecayPeriod = 100
	d := newTestDict(t)
	obj := d.NewObj("v", 0, 0)
	obj.Access = packLFU(evictionClock, 50)
	stored := obj.Access

	evictionClock += 100 * 10
	_ = decayedFreq(obj.Access)
	assert.Equal(t, stored, obj.Access,
		"reading the decayed value must not write; decay is applied when the key is touched")
}

// scanResistance measures the workload LFU exists for: a hot working set,
// followed by a long stream of keys nobody will ask for again.
func scanResistance(t *testing.T, strategy, limit int) float64 {
	t.Helper()
	withEviction(t, strategy, 5, limit)

	d := newTestDict(t)
	hot := limit / 2
	for i := 0; i < limit; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v", 0, 0))
	}
	for round := 0; round < 50; round++ {
		for i := 0; i < hot; i++ {
			d.Get("k" + strconv.Itoa(i))
		}
	}
	for i := 0; i < limit*2; i++ {
		d.Put("scan"+strconv.Itoa(i), d.NewObj("v", 0, 0))
	}

	survived := 0
	for i := 0; i < hot; i++ {
		if _, ok := d.dictStore["k"+strconv.Itoa(i)]; ok {
			survived++
		}
	}
	return float64(survived) / float64(hot) * 100
}

// TestLFUSurvivesAScanThatDestroysLRU is the whole argument for the policy. To
// LRU a scan is a stream of very recently used keys, so it evicts the working
// set to make room for data nobody will read again. Measured: LRU keeps none of
// it.
func TestLFUSurvivesAScanThatDestroysLRU(t *testing.T) {
	lru := scanResistance(t, config.LRU, 1000)
	lfu := scanResistance(t, config.LFU, 1000)

	assert.Less(t, lru, 20.0, "a scan should defeat LRU, got %.1f%%", lru)
	assert.Greater(t, lfu, 90.0, "LFU should hold its working set through a scan, got %.1f%%", lfu)
}

// TestLFUFollowsAWorkingSetThatMoves is the other half, and the reason decay
// exists. Without it the first hot set's counters stand forever, newly hot keys
// starting from the initial value can never outrank them, and the cache fills
// with history it will never serve again.
func TestLFUFollowsAWorkingSetThatMoves(t *testing.T) {
	withEviction(t, config.LFU, 5, 1000)
	d := newTestDict(t)

	hammer := func(prefix string) {
		for i := 0; i < 500; i++ {
			d.Put(prefix+strconv.Itoa(i), d.NewObj("v", 0, 0))
		}
		for round := 0; round < 400; round++ {
			for i := 0; i < 500; i++ {
				d.Get(prefix + strconv.Itoa(i))
			}
		}
	}
	hammer("old")
	hammer("new")
	for i := 0; i < 1000; i++ { // pressure: the cache must choose
		d.Put("filler"+strconv.Itoa(i), d.NewObj("v", 0, 0))
	}

	stale, current := 0, 0
	for i := 0; i < 500; i++ {
		if _, ok := d.dictStore["old"+strconv.Itoa(i)]; ok {
			stale++
		}
		if _, ok := d.dictStore["new"+strconv.Itoa(i)]; ok {
			current++
		}
	}
	assert.Greater(t, current, 450, "the current working set must be kept, got %d/500", current)
	assert.Less(t, stale, 100, "the stale working set must be let go, got %d/500", stale)
}

func TestLFUHoldsTheDictAtTheLimit(t *testing.T) {
	withEviction(t, config.LFU, 5, 100)
	d := newTestDict(t)
	for i := 0; i < 1000; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v", 0, 0))
		assert.LessOrEqual(t, d.Len(), 100)
	}
	assert.Equal(t, 100, d.Len())
}
