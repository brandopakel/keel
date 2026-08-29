package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func resetCFStore() { ResetStores() }

func TestCmdCFReserveAndAdd(t *testing.T) {
	resetCFStore()

	res, err := Decode(cmdCFRESERVE([]string{"cf", "1000"}))
	assert.Nil(t, err)
	assert.EqualValues(t, "OK", res)

	res, _ = Decode(cmdCFRESERVE([]string{"cf", "1000"}))
	assert.Contains(t, res, "already exists")

	res, _ = Decode(cmdCFADD([]string{"cf", "a"}))
	assert.EqualValues(t, 1, res)
	res, _ = Decode(cmdCFEXISTS([]string{"cf", "a"}))
	assert.EqualValues(t, 1, res)
	res, _ = Decode(cmdCFEXISTS([]string{"cf", "b"}))
	assert.EqualValues(t, 0, res)
}

// TestCmdCFAddCreatesMissingKey matches RedisBloom: adding to a key that does
// not exist creates a default-sized filter rather than erroring.
func TestCmdCFAddCreatesMissingKey(t *testing.T) {
	resetCFStore()
	res, _ := Decode(cmdCFADD([]string{"fresh", "x"}))
	assert.EqualValues(t, 1, res)
	assert.True(t, cfStore.Exists("fresh"))
}

// TestCmdCFAddAllowsDuplicates is the difference from CF.ADDNX, and what makes
// CF.COUNT meaningful.
func TestCmdCFAddAllowsDuplicates(t *testing.T) {
	resetCFStore()
	for i := 0; i < 3; i++ {
		res, _ := Decode(cmdCFADD([]string{"cf", "dup"}))
		assert.EqualValues(t, 1, res)
	}
	res, _ := Decode(cmdCFCOUNT([]string{"cf", "dup"}))
	assert.EqualValues(t, 3, res)
}

func TestCmdCFAddNX(t *testing.T) {
	resetCFStore()
	res, _ := Decode(cmdCFADDNX([]string{"cf", "x"}))
	assert.EqualValues(t, 1, res, "first add should succeed")

	res, _ = Decode(cmdCFADDNX([]string{"cf", "x"}))
	assert.EqualValues(t, 0, res, "second add should be refused")

	res, _ = Decode(cmdCFCOUNT([]string{"cf", "x"}))
	assert.EqualValues(t, 1, res, "ADDNX must not have stored a second copy")
}

func TestCmdCFDel(t *testing.T) {
	resetCFStore()
	cmdCFADD([]string{"cf", "gone"})

	res, _ := Decode(cmdCFDEL([]string{"cf", "gone"}))
	assert.EqualValues(t, 1, res)

	res, _ = Decode(cmdCFEXISTS([]string{"cf", "gone"}))
	assert.EqualValues(t, 0, res)

	res, _ = Decode(cmdCFDEL([]string{"cf", "gone"}))
	assert.EqualValues(t, 0, res, "deleting what is not there reports nothing removed")

	res, _ = Decode(cmdCFDEL([]string{"nosuchkey", "x"}))
	assert.EqualValues(t, 0, res)
}

func TestCmdCFMExists(t *testing.T) {
	resetCFStore()
	cmdCFADD([]string{"cf", "a"})
	cmdCFADD([]string{"cf", "c"})

	res, err := Decode(cmdCFMEXISTS([]string{"cf", "a", "b", "c"}))
	assert.Nil(t, err)
	assert.EqualValues(t, []interface{}{"1", "0", "1"}, res)

	res, _ = Decode(cmdCFMEXISTS([]string{"nosuchkey", "a", "b"}))
	assert.EqualValues(t, []interface{}{"0", "0"}, res)
}

func TestCmdCFCountAndExistsOnMissingKey(t *testing.T) {
	resetCFStore()
	res, _ := Decode(cmdCFCOUNT([]string{"nope", "x"}))
	assert.EqualValues(t, 0, res)
	res, _ = Decode(cmdCFEXISTS([]string{"nope", "x"}))
	assert.EqualValues(t, 0, res)
}

func TestCmdCFInfo(t *testing.T) {
	resetCFStore()
	cmdCFRESERVE([]string{"cf", "1000"})
	cmdCFADD([]string{"cf", "a"})
	cmdCFADD([]string{"cf", "b"})
	cmdCFDEL([]string{"cf", "a"})

	res, err := Decode(cmdCFINFO([]string{"cf"}))
	assert.Nil(t, err)
	fields, ok := res.([]interface{})
	assert.True(t, ok)

	flat := map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		flat[fields[i].(string)] = fields[i+1].(string)
	}
	assert.Equal(t, "1000", flat["Capacity"])
	assert.Equal(t, "2", flat["Number of items inserted"])
	assert.Equal(t, "1", flat["Number of items deleted"])
	assert.Equal(t, "4", flat["Bucket size"])

	res, _ = Decode(cmdCFINFO([]string{"missing"}))
	assert.Contains(t, res, "does not exist")
}

// TestCmdCFFullFilterReportsAnError covers what a cuckoo filter does that a
// Bloom filter cannot: refuse. A Bloom filter accepts every insert and quietly
// grows less accurate; this one has a hard capacity and says so.
func TestCmdCFFullFilterReportsAnError(t *testing.T) {
	resetCFStore()
	cmdCFRESERVE([]string{"small", "8"})

	var lastErr interface{}
	for i := 0; i < 100000; i++ {
		res, _ := Decode(cmdCFADD([]string{"small", "item:" + strconv.Itoa(i)}))
		if s, ok := res.(string); ok {
			lastErr = s
			break
		}
	}
	assert.Contains(t, lastErr, "filter is full")
}

func TestCmdCFWrongArity(t *testing.T) {
	resetCFStore()
	for _, c := range []struct {
		name string
		fn   func([]string) []byte
		args []string
	}{
		{"CF.RESERVE", cmdCFRESERVE, []string{"k"}},
		{"CF.ADD", cmdCFADD, []string{"k"}},
		{"CF.ADDNX", cmdCFADDNX, []string{"k"}},
		{"CF.EXISTS", cmdCFEXISTS, []string{"k"}},
		{"CF.MEXISTS", cmdCFMEXISTS, []string{"k"}},
		{"CF.DEL", cmdCFDEL, []string{"k"}},
		{"CF.COUNT", cmdCFCOUNT, []string{"k"}},
		{"CF.INFO", cmdCFINFO, []string{}},
	} {
		res, _ := Decode(c.fn(c.args))
		assert.Contains(t, res, "wrong number of arguments", "%s should reject bad arity", c.name)
	}
}
