package core

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/config"
)

func TestBFRESERVE(t *testing.T) {
	ResetStores()
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "bf", "0.01", "1000"))
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01", "1000"), "item exists")

	info := run(t, "BF.INFO", "bf").([]interface{})
	assert.Equal(t, []interface{}{
		"Capacity", int64(1000),
		"Size", info[3],
		"Number of filters", int64(1),
		"Number of items inserted", int64(0),
		"Expansion rate", int64(2),
	}, info)
	assert.Greater(t, info[3], int64(1000), "size is the bit array, not the struct")

	assert.Equal(t, "OK", run(t, "BF.RESERVE", "wide", "0.001", "10", "EXPANSION", "4"))
	assert.Equal(t, int64(4), run(t, "BF.INFO", "wide").([]interface{})[9])
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "lower", "0.1", "10", "expansion", "1"), "keywords are case-insensitive")
}

// TestBFRESERVERefusesBadSizing: RedisBloom's refusals, word for word.
func TestBFRESERVERefusesBadSizing(t *testing.T) {
	ResetStores()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"1", "100"}, "ERR error rate must be in the range (0.000000, 1.000000)"},
		{[]string{"0", "100"}, "ERR error rate must be in the range (0.000000, 1.000000)"},
		{[]string{"inf", "100"}, "ERR error rate must be in the range (0.000000, 1.000000)"},
		{[]string{"abc", "100"}, "ERR bad error rate"},
		{[]string{"nan", "100"}, "ERR bad error rate"},
		{[]string{"1e-400", "100"}, "ERR bad error rate"},
		{[]string{"0.01", "0"}, "ERR capacity must be in the range [1, 1073741824]"},
		{[]string{"0.01", "-5"}, "ERR capacity must be in the range [1, 1073741824]"},
		{[]string{"0.01", "1073741825"}, "ERR capacity must be in the range [1, 1073741824]"},
		{[]string{"0.01", "007"}, "ERR bad capacity"},
		{[]string{"0.01", "+5"}, "ERR bad capacity"},
		{[]string{"0.01", "100", "EXPANSION"}, "ERR no expansion"},
		{[]string{"0.01", "100", "EXPANSION", "x"}, "ERR bad expansion"},
		{[]string{"0.01", "100", "EXPANSION", "-1"}, "ERR expansion must be in the range [0, 32768]"},
		{[]string{"0.01", "100", "EXPANSION", "32769"}, "ERR expansion must be in the range [0, 32768]"},
		{[]string{"0.01", "100", "NONSCALING", "EXPANSION", "2"}, "Nonscaling filters cannot expand"},
		{[]string{"4.9e-324", "100"}, "ERR could not create filter"},
	} {
		args := append([]string{"bf"}, c.args...)
		assert.Equal(t, "-"+c.want+"\r\n", string(rawReply(t, "BF.RESERVE", args...)), "%q", c.args)
	}
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01"), "wrong number of arguments")
	assert.EqualValues(t, 0, run(t, "EXISTS", "bf"), "a refused reserve creates nothing")
}

// TestBFRESERVEOptions: NONSCALING and EXPANSION are found anywhere among the
// arguments, an expansion of 0 is NONSCALING, and an argument RedisBloom does
// not know is ignored, as it is there.
func TestBFRESERVEOptions(t *testing.T) {
	ResetStores()
	for key, args := range map[string][]string{
		"ns":      {"0.01", "100", "NONSCALING"},
		"zero":    {"0.01", "100", "EXPANSION", "0"},
		"both":    {"0.01", "100", "nonscaling", "expansion", "0"},
		"ignored": {"0.01", "100", "GROWTH", "2"},
		"three":   {"0.01", "100", "expansion", "3", "FOO"},
	} {
		assert.Equal(t, "OK", run(t, "BF.RESERVE", append([]string{key}, args...)...), key)
	}
	for key, want := range map[string]string{"ns": "$-1", "zero": "$-1", "both": "$-1", "ignored": ":2", "three": ":3"} {
		assert.Equal(t, "*1\r\n"+want+"\r\n", string(rawReply(t, "BF.INFO", key, "EXPANSION")), "%s", key)
	}
	assert.Equal(t, "%1\r\n+Expansion rate\r\n_\r\n", string(rawReplyAs(t, true, "BF.INFO", "ns", "EXPANSION")),
		"a filter that does not grow has no expansion rate")
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "nonscaling", "0.01", "100"), "the key is an argument like any other")
	assert.Equal(t, "*1\r\n$-1\r\n", string(rawReply(t, "BF.INFO", "nonscaling", "EXPANSION")))
}

// TestBFRESERVEChecksItsParametersBeforeTheKey: RedisBloom reads the error
// rate, capacity and options before it opens the key, so a bad one is
// reported ahead of a key of another type, or one already reserved.
func TestBFRESERVEChecksItsParametersBeforeTheKey(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "v")
	run(t, "BF.RESERVE", "bf", "0.01", "100")
	assert.Equal(t, "ERR bad error rate", run(t, "BF.RESERVE", "str", "abc", "100"))
	assert.Equal(t, "ERR no expansion", run(t, "BF.RESERVE", "bf", "0.01", "100", "EXPANSION"))
	assert.Contains(t, run(t, "BF.RESERVE", "str", "0.01", "100"), "WRONGTYPE")
	assert.Equal(t, "ERR item exists", run(t, "BF.RESERVE", "bf", "0.01", "100"))
}

// TestBFNonScalingFilterRefusesWhenFull: a filter that does not grow answers
// the item that would overfill it with RedisBloom's refusal, and BF.MADD stops
// there, as RedisBloom's does.
func TestBFNonScalingFilterRefusesWhenFull(t *testing.T) {
	ResetStores()
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "ns", "0.000001", "50", "NONSCALING"))
	items := make([]string, 0, 50)
	for i := 0; i < 49; i++ {
		items = append(items, fmt.Sprintf("item-%d", i))
	}
	run(t, "BF.MADD", append([]string{"ns"}, items...)...)
	assert.EqualValues(t, 1, run(t, "BF.ADD", "ns", "last"))
	assert.Equal(t, "-ERR non scaling filter is full\r\n", string(rawReply(t, "BF.ADD", "ns", "over")))
	assert.EqualValues(t, 0, run(t, "BF.ADD", "ns", "last"), "an item it holds is not refused")
	assert.Equal(t, "*2\r\n:0\r\n-ERR non scaling filter is full\r\n", string(rawReply(t, "BF.MADD", "ns", "item-1", "over", "after")))
	assert.EqualValues(t, 0, run(t, "BF.EXISTS", "ns", "after"), "nothing after the refusal was tried")
	assert.Equal(t, "*1\r\n$-1\r\n", string(rawReply(t, "BF.INFO", "ns", "EXPANSION")))
	assert.Equal(t, "*1\r\n:50\r\n", string(rawReply(t, "BF.INFO", "ns", "items")))
}

// TestBFRESERVESizeIsCheckedBeforeAllocating: a capacity is a request for
// memory, allocated in full before the budget sees the key, so it is checked
// on the number first - against what one key may take, and against the whole
// budget when there is one.
func TestBFRESERVESizeIsCheckedBeforeAllocating(t *testing.T) {
	ResetStores()
	assert.Contains(t, run(t, "BF.RESERVE", "huge", "0.01", "1000000000"), "more than this server will allocate for one key",
		"a billion items at 1%% is 1.2GB of bits")
	assert.EqualValues(t, 0, run(t, "EXISTS", "huge"))

	withBudget(t, 1<<20, config.LRU)
	assert.Contains(t, run(t, "BF.RESERVE", "big", "0.01", "10000000"), "does not fit in maxmemory",
		"12MB of bits against a 1MB budget")
	assert.EqualValues(t, 0, run(t, "EXISTS", "big"))
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "fits", "0.01", "10000"))
}

// TestBFGrowthIsSizedBeforeItIsAllocated: a filter reserved with a huge
// expansion turned its second distinct item into a multi-gigabyte allocation on
// the command thread. The growth is refused, the item is not added, and the
// filter is otherwise as it was.
func TestBFGrowthIsSizedBeforeItIsAllocated(t *testing.T) {
	ResetStores()
	// RedisBloom's largest expansion: a second filter of 327,680,000 items is
	// past what one key may take.
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "g", "0.000001", "10000", "EXPANSION", "32768"))
	items := []string{"g"}
	for i := 0; i < 10000; i++ {
		items = append(items, fmt.Sprintf("item-%d", i))
	}
	run(t, "BF.MADD", items...)
	first := items[1]
	assert.Contains(t, run(t, "BF.ADD", "g", "second"), "cannot grow")
	assert.EqualValues(t, 0, run(t, "BF.EXISTS", "g", "second"))
	assert.EqualValues(t, 1, run(t, "BF.EXISTS", "g", first))

	res := run(t, "BF.MADD", "g", "third", first, "fourth").([]interface{})
	assert.Contains(t, res[0], "cannot grow")
	assert.Equal(t, int64(0), res[1], "an item already present is answered, not refused")
	assert.Contains(t, res[2], "cannot grow", "and the items after a refusal are tried")

	info := run(t, "BF.INFO", "g").([]interface{})
	assert.Equal(t, int64(1), info[5], "still one filter")
	assert.Equal(t, int64(10000), info[7], "still the items it held")
}

// TestBFRESERVEWithAnErrorRateNextToOne: such a rate asks for a fraction of a
// bit per item, which used to size an array of no bits and divide by it on the
// first add.
func TestBFRESERVEWithAnErrorRateNextToOne(t *testing.T) {
	ResetStores()
	assert.Equal(t, "OK", run(t, "BF.RESERVE", "thin", "0.9999999999999999", "1"))
	assert.EqualValues(t, 1, run(t, "BF.ADD", "thin", "x"))
	assert.EqualValues(t, 1, run(t, "BF.EXISTS", "thin", "x"))
}

func TestBFADDAndMADDReportNewItems(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 1, run(t, "BF.ADD", "bf", "x"), "a filter is created on first use")
	assert.EqualValues(t, 0, run(t, "BF.ADD", "bf", "x"), "and the second add of x is nothing new")
	assert.Equal(t, []interface{}{int64(0), int64(1), int64(1)}, run(t, "BF.MADD", "bf", "x", "y", "z"))
	assert.Equal(t, "*2\r\n:1\r\n:0\r\n", string(rawReply(t, "BF.MADD", "bf", "w", "w")),
		"integers on the wire, one per item")

	info := run(t, "BF.INFO", "bf").([]interface{})
	assert.Equal(t, int64(100), info[1], "the default capacity")
	assert.Equal(t, int64(4), info[7], "x, y, z and w")

	assert.Contains(t, run(t, "BF.ADD", "bf"), "wrong number of arguments")
	assert.Contains(t, run(t, "BF.MADD", "bf"), "wrong number of arguments")
}

func TestBFEXISTSAndMEXISTS(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 0, run(t, "BF.EXISTS", "nokey", "x"))
	assert.Equal(t, []interface{}{int64(0), int64(0)}, run(t, "BF.MEXISTS", "nokey", "x", "y"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "nokey"), "asking creates nothing")

	run(t, "BF.MADD", "bf", "x", "y")
	assert.EqualValues(t, 1, run(t, "BF.EXISTS", "bf", "x"))
	assert.EqualValues(t, 0, run(t, "BF.EXISTS", "bf", "never"))
	assert.Equal(t, []interface{}{int64(1), int64(0), int64(1)}, run(t, "BF.MEXISTS", "bf", "x", "never", "y"))
	assert.Contains(t, run(t, "BF.EXISTS", "bf"), "wrong number of arguments")
}

func TestBFINFOFollowsGrowth(t *testing.T) {
	ResetStores()
	run(t, "BF.RESERVE", "bf", "0.01", "10")
	for i := 0; i < 25; i++ {
		run(t, "BF.ADD", "bf", fmt.Sprintf("item-%d", i))
	}
	info := run(t, "BF.INFO", "bf").([]interface{})
	assert.Equal(t, int64(10+20), info[1], "capacity grew by the expansion")
	assert.Equal(t, int64(2), info[5], "two filters")
	assert.Equal(t, int64(25), info[7])

	assert.Contains(t, run(t, "BF.INFO", "nokey"), "not found")
	assert.Contains(t, run(t, "BF.INFO"), "wrong number of arguments")
}

// TestBFINFOByField: one field, named in any case, as RedisBloom answers it -
// an array of the value alone in RESP2, and a map of one in RESP3.
func TestBFINFOByField(t *testing.T) {
	ResetStores()
	run(t, "BF.RESERVE", "bf", "0.01", "10", "EXPANSION", "3")
	run(t, "BF.MADD", "bf", "a", "b")
	for field, want := range map[string]string{
		"CAPACITY": ":10", "capacity": ":10", "Filters": ":1", "ITEMS": ":2", "expansion": ":3",
	} {
		assert.Equal(t, "*1\r\n"+want+"\r\n", string(rawReplyAs(t, false, "BF.INFO", "bf", field)), field)
	}
	assert.Equal(t, "%1\r\n+Capacity\r\n:10\r\n", string(rawReplyAs(t, true, "BF.INFO", "bf", "capacity")))
	assert.Equal(t, "%1\r\n+Expansion rate\r\n:3\r\n", string(rawReplyAs(t, true, "BF.INFO", "bf", "EXPANSION")))
	assert.Regexp(t, `^\*1\r\n:[0-9]+\r\n$`, string(rawReplyAs(t, false, "BF.INFO", "bf", "size")))
	for _, field := range []string{"nosuchfield", "", "cap", "Number of filters"} {
		assert.Equal(t, "-Invalid information value\r\n", string(rawReply(t, "BF.INFO", "bf", field)), "%q", field)
	}
	assert.Equal(t, "-ERR not found\r\n", string(rawReply(t, "BF.INFO", "nokey", "nosuchfield")), "the key is looked at first")
	assert.Contains(t, run(t, "BF.INFO", "bf", "capacity", "size"), "wrong number of arguments")
}

// TestBFReadsAnswerNoForAnotherType: RedisBloom's BF.EXISTS and BF.MEXISTS
// answer no for a key that is not a Bloom filter; its writes and BF.INFO
// answer WRONGTYPE.
func TestBFReadsAnswerNoForAnotherType(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "v")
	run(t, "CF.ADD", "cf", "a")
	for _, key := range []string{"str", "cf"} {
		assert.EqualValues(t, 0, run(t, "BF.EXISTS", key, "a"), key)
		assert.Equal(t, []interface{}{int64(0), int64(0)}, run(t, "BF.MEXISTS", key, "a", "b"), key)
		for _, args := range [][]string{{"BF.ADD", key, "a"}, {"BF.MADD", key, "a"}, {"BF.INFO", key}, {"BF.RESERVE", key, "0.01", "10"}} {
			assert.Equal(t, "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n", string(rawReply(t, args[0], args[1:]...)), "%q", args)
		}
	}
	assert.Equal(t, "v", run(t, "GET", "str"), "nothing replaced it")
}

// TestBFADDIsLogged: the filter cannot be rebuilt from anything but its
// items, so an add has to reach the append-only file like any other write.
func TestBFADDIsLogged(t *testing.T) {
	ResetStores()
	assert.True(t, writeCommands["BF.ADD"])
}
