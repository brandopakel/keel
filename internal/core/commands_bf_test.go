package core

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
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

func TestBFRESERVERefusesBadSizing(t *testing.T) {
	ResetStores()
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "1", "100"), "0 < error rate range < 1")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0", "100"), "0 < error rate range < 1")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "abc", "100"), "bad error rate")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01", "0"), "capacity should be larger than 0")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01", "-5"), "bad capacity")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01", "100", "EXPANSION", "0"), "expansion should be greater or equal to 1")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01", "100", "EXPANSION", "x"), "bad expansion")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01", "100", "GROWTH", "2"), "syntax error")
	assert.Contains(t, run(t, "BF.RESERVE", "bf", "0.01"), "wrong number of arguments")
	assert.EqualValues(t, 0, run(t, "EXISTS", "bf"), "a refused reserve creates nothing")
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

// TestBFADDIsLogged: the filter cannot be rebuilt from anything but its
// items, so an add has to reach the append-only file like any other write.
func TestBFADDIsLogged(t *testing.T) {
	ResetStores()
	assert.True(t, writeCommands["BF.ADD"])
}
