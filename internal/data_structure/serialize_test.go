package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSerialiseRoundTripsExactly.
//
// These five structures are written to the log as bytes because no command can
// rebuild them. That only helps if the bytes come back as the same structure -
// not a similar one - so every round trip is checked by asking the structure
// the questions it exists to answer, not by comparing fields.
func TestSerialiseRoundTripsExactly(t *testing.T) {
	t.Run("count-min sketch", func(t *testing.T) {
		c := CreateCMS(200, 5)
		for i := 0; i < 500; i++ {
			c.IncrBy("item:"+strconv.Itoa(i), uint32(i+1))
		}
		back, err := UnmarshalCMS(c.Marshal())
		assert.NoError(t, err)
		for i := 0; i < 500; i++ {
			item := "item:" + strconv.Itoa(i)
			assert.Equal(t, c.Count(item), back.Count(item), "count of %s", item)
		}
		assert.Equal(t, c.MemUsage(), back.MemUsage())
	})

	t.Run("morris counter", func(t *testing.T) {
		m := CreateMorris(200, 5)
		m.IncrBy("hot", 1000000)
		m.IncrBy("cold", 3)
		back, err := UnmarshalMorris(m.Marshal())
		assert.NoError(t, err)
		assert.Equal(t, m.Count("hot"), back.Count("hot"))
		assert.Equal(t, m.Count("cold"), back.Count("cold"))
		assert.Equal(t, m.TotalCount(), back.TotalCount())

		// The generator state travels with it, so counting on from a restored
		// table continues the same sequence rather than starting a new one.
		m.IncrBy("hot", 1000)
		back.IncrBy("hot", 1000)
		assert.Equal(t, m.Count("hot"), back.Count("hot"),
			"a restored counter must carry on identically, not merely similarly")
	})

	t.Run("hyperloglog", func(t *testing.T) {
		h := CreateHLL()
		for i := 0; i < 100000; i++ {
			h.Add("item:" + strconv.Itoa(i))
		}
		back, err := UnmarshalHLL(h.Marshal())
		assert.NoError(t, err)
		assert.Equal(t, h.Count(), back.Count())

		h.Add("one-more")
		back.Add("one-more")
		assert.Equal(t, h.Count(), back.Count())
	})

	t.Run("cuckoo filter", func(t *testing.T) {
		c := CreateCuckooFilter(5000)
		for i := 0; i < 4000; i++ {
			assert.True(t, c.Insert("item:"+strconv.Itoa(i)))
		}
		c.Delete("item:7")
		back, err := UnmarshalCuckoo(c.Marshal())
		assert.NoError(t, err)

		assert.Equal(t, c.Size(), back.Size())
		assert.Equal(t, c.Capacity(), back.Capacity())
		assert.Equal(t, c.NumBuckets(), back.NumBuckets())
		for i := 0; i < 4000; i++ {
			item := "item:" + strconv.Itoa(i)
			assert.Equal(t, c.Lookup(item), back.Lookup(item), "lookup of %s", item)
		}
		assert.False(t, back.Lookup("item:7"), "a deletion must survive the round trip")
	})

	t.Run("scalable bloom filter", func(t *testing.T) {
		// Enough items to force the chain to grow, so more than one link is
		// serialised - a format that only ever saw one would not be tested.
		sb := CreateSBChain(100, 0.01, 2)
		for i := 0; i < 2000; i++ {
			sb.Add("item:" + strconv.Itoa(i))
		}
		assert.Greater(t, len(sb.filters), 1, "the chain should have grown")

		back, err := UnmarshalSBChain(sb.Marshal())
		assert.NoError(t, err)
		assert.Equal(t, len(sb.filters), len(back.filters))
		for i := 0; i < 2000; i++ {
			assert.True(t, back.Exist("item:"+strconv.Itoa(i)), "item %d must still be present", i)
		}
		// And it must still be usable, not merely readable.
		back.Add("added-after-restore")
		assert.True(t, back.Exist("added-after-restore"))
	})
}

// TestSerialiseRejectsNonsense. The payload comes off disk, so its lengths and
// dimensions are input rather than fact: a corrupt one must be an error and not
// an allocation of whatever size the file happened to name.
func TestSerialiseRejectsNonsense(t *testing.T) {
	for name, fn := range map[string]func([]byte) error{
		"cms":     func(p []byte) error { _, err := UnmarshalCMS(p); return err },
		"morris":  func(p []byte) error { _, err := UnmarshalMorris(p); return err },
		"hll":     func(p []byte) error { _, err := UnmarshalHLL(p); return err },
		"cuckoo":  func(p []byte) error { _, err := UnmarshalCuckoo(p); return err },
		"sbchain": func(p []byte) error { _, err := UnmarshalSBChain(p); return err },
	} {
		assert.Error(t, fn(nil), "%s: empty payload", name)
		assert.Error(t, fn([]byte{1, 2, 3}), "%s: truncated payload", name)
		assert.Error(t, fn(make([]byte, 64)), "%s: zeroed payload", name)
	}

	// A width and depth that multiply out to more bytes than are present must
	// be refused before they become an allocation.
	huge := make([]byte, 16)
	huge[0], huge[4] = 0xFF, 0xFF
	_, err := UnmarshalCMS(huge)
	assert.Error(t, err)
	_, err = UnmarshalMorris(huge)
	assert.Error(t, err)
}

// TestSerialiseTruncatedTailIsRejected covers the payload cut short mid-field,
// which is what a torn write looks like from the inside.
func TestSerialiseTruncatedTailIsRejected(t *testing.T) {
	c := CreateCMS(50, 4)
	c.IncrBy("x", 1)
	full := c.Marshal()
	for _, cut := range []int{1, 8, 15, len(full) / 2, len(full) - 1} {
		_, err := UnmarshalCMS(full[:cut])
		assert.Error(t, err, "a payload cut at %d must be refused", cut)
	}
}
