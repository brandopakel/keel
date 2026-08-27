package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/data_structure"
)

func resetHLLStore() {
	hllStore = make(map[string]*data_structure.HLL)
}

func TestCmdPFADD(t *testing.T) {
	resetHLLStore()

	// Creating the key is a change in itself, so the first call reports 1 even
	// with no elements to add.
	res, err := Decode(cmdPFADD([]string{"h"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 1, res)

	res, err = Decode(cmdPFADD([]string{"h"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 0, res, "an existing key with nothing added has not changed")

	res, err = Decode(cmdPFADD([]string{"h", "a", "b", "c"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 1, res)

	res, err = Decode(cmdPFADD([]string{"h", "a", "b", "c"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 0, res, "re-adding known members must not report a change")
}

func TestCmdPFADDWrongArity(t *testing.T) {
	resetHLLStore()
	res, err := Decode(cmdPFADD([]string{}))
	assert.Nil(t, err)
	assert.Contains(t, res, "wrong number of arguments")
}

func TestCmdPFCOUNT(t *testing.T) {
	resetHLLStore()

	res, err := Decode(cmdPFCOUNT([]string{"missing"}))
	assert.Nil(t, err)
	assert.EqualValues(t, 0, res, "a key that does not exist counts zero")

	for i := 0; i < 1000; i++ {
		cmdPFADD([]string{"h", "item:" + strconv.Itoa(i)})
	}
	res, err = Decode(cmdPFCOUNT([]string{"h"}))
	assert.Nil(t, err)
	count, ok := res.(int64)
	assert.True(t, ok)
	assert.InEpsilon(t, 1000, count, 0.03)
}

// TestCmdPFCOUNTOverSeveralKeys checks the union, and that asking for it does
// not quietly modify the keys being asked about.
func TestCmdPFCOUNTOverSeveralKeys(t *testing.T) {
	resetHLLStore()
	for i := 0; i < 5000; i++ {
		cmdPFADD([]string{"a", "left:" + strconv.Itoa(i)})
		cmdPFADD([]string{"b", "right:" + strconv.Itoa(i)})
	}

	res, _ := Decode(cmdPFCOUNT([]string{"a", "b"}))
	assert.InEpsilon(t, 10000, res.(int64), 0.03, "disjoint sets union to their sum")

	// PFCOUNT is a read.
	resA, _ := Decode(cmdPFCOUNT([]string{"a"}))
	resB, _ := Decode(cmdPFCOUNT([]string{"b"}))
	assert.InEpsilon(t, 5000, resA.(int64), 0.03, "counting a union must not alter its inputs")
	assert.InEpsilon(t, 5000, resB.(int64), 0.03)
}

func TestCmdPFCOUNTOfOverlappingKeysIsNotASum(t *testing.T) {
	resetHLLStore()
	for i := 0; i < 5000; i++ {
		item := "item:" + strconv.Itoa(i)
		cmdPFADD([]string{"a", item})
		cmdPFADD([]string{"b", item})
	}
	res, _ := Decode(cmdPFCOUNT([]string{"a", "b"}))
	assert.InEpsilon(t, 5000, res.(int64), 0.03, "identical sets union to one of them")
}

func TestCmdPFMERGE(t *testing.T) {
	resetHLLStore()
	for i := 0; i < 4000; i++ {
		cmdPFADD([]string{"a", "left:" + strconv.Itoa(i)})
		cmdPFADD([]string{"b", "right:" + strconv.Itoa(i)})
	}

	res, err := Decode(cmdPFMERGE([]string{"dest", "a", "b"}))
	assert.Nil(t, err)
	assert.EqualValues(t, "OK", res)

	merged, _ := Decode(cmdPFCOUNT([]string{"dest"}))
	assert.InEpsilon(t, 8000, merged.(int64), 0.03)
}

// TestCmdPFMERGEAccumulatesIntoDestination pins that the destination takes part
// in its own merge, rather than being overwritten by the sources.
func TestCmdPFMERGEAccumulatesIntoDestination(t *testing.T) {
	resetHLLStore()
	for i := 0; i < 3000; i++ {
		cmdPFADD([]string{"dest", "already:" + strconv.Itoa(i)})
		cmdPFADD([]string{"src", "new:" + strconv.Itoa(i)})
	}

	cmdPFMERGE([]string{"dest", "src"})
	res, _ := Decode(cmdPFCOUNT([]string{"dest"}))
	assert.InEpsilon(t, 6000, res.(int64), 0.03,
		"the destination's own members must survive the merge")
}

func TestCmdPFMERGEIgnoresMissingSources(t *testing.T) {
	resetHLLStore()
	for i := 0; i < 2000; i++ {
		cmdPFADD([]string{"src", "item:" + strconv.Itoa(i)})
	}
	res, _ := Decode(cmdPFMERGE([]string{"dest", "src", "nosuchkey"}))
	assert.EqualValues(t, "OK", res)

	count, _ := Decode(cmdPFCOUNT([]string{"dest"}))
	assert.InEpsilon(t, 2000, count.(int64), 0.03)
}
