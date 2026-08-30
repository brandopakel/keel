package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/data_structure"
)

func TestCmdMorrisInitByDim(t *testing.T) {
	ResetStores()

	res, err := Decode(cmdMORRISINITBYDIM([]string{"m", "1000", "5"}))
	assert.Nil(t, err)
	assert.EqualValues(t, "OK", res)

	res, _ = Decode(cmdMORRISINITBYDIM([]string{"m", "1000", "5"}))
	assert.Contains(t, res, "already exists")

	for _, bad := range [][]string{
		{"m2", "0", "5"},
		{"m2", "1000", "0"},
		{"m2", "wide", "5"},
		{"m2", "1000"},
	} {
		res, _ = Decode(cmdMORRISINITBYDIM(bad))
		assert.IsType(t, "", res, "MORRIS.INITBYDIM %v must be refused", bad)
		assert.False(t, morrisStore.Exists("m2"))
	}
}

func TestCmdMorrisInitByProbMatchesCountMinDimensions(t *testing.T) {
	ResetStores()

	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYPROB([]string{"m", "0.001", "0.01"})))
	assert.EqualValues(t, "OK", mustDecode(t, cmdCMSINITBYPROB([]string{"c", "0.001", "0.01"})))

	m, ok := morrisStore.Peek("m")
	assert.True(t, ok)
	w, d := data_structure.CalcCMSDim(0.001, 0.01)
	assert.Equal(t, w, m.Width())
	assert.Equal(t, d, m.Depth())
}

// TestCmdMorrisCountsWithinItsStatedError is the command-level contract. The
// estimate is not exact - that is the whole point - so what has to hold is that
// it lands within the error the structure advertises.
func TestCmdMorrisCountsWithinItsStatedError(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYDIM([]string{"m", "20000", "7"})))

	const items = 300
	const each = 100000
	for i := 0; i < items; i++ {
		res, _ := Decode(cmdMORRISINCRBY([]string{"m", "item:" + strconv.Itoa(i), strconv.Itoa(each)}))
		assert.Len(t, res, 1)
	}

	var mean float64
	for i := 0; i < items; i++ {
		res, _ := Decode(cmdMORRISQUERY([]string{"m", "item:" + strconv.Itoa(i)}))
		counts := res.([]interface{})
		got, err := strconv.ParseFloat(counts[0].(string), 64)
		assert.Nil(t, err)
		mean += got / each
	}
	mean /= items
	t.Logf("mean estimate over %d items counted %d times: %.3f of the truth", items, each, mean)
	assert.InDelta(t, 1.0, mean, 0.05, "the estimate must be unbiased across items")
}

// TestCmdMorrisIncrByIsNotLinearInTheIncrement. A million-count increment must
// not cost a million coin flips; if the geometric shortcut in bump were ever
// replaced by a loop, this is the test that would notice.
func TestCmdMorrisIncrByIsNotLinearInTheIncrement(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYDIM([]string{"m", "100", "5"})))

	// Counting to a billion in one command, which a per-increment loop could
	// not finish inside this test.
	res, _ := Decode(cmdMORRISINCRBY([]string{"m", "heavy", "1000000000"}))
	counts := res.([]interface{})
	got, err := strconv.ParseUint(counts[0].(string), 10, 64)
	assert.Nil(t, err)
	assert.InEpsilon(t, 1e9, float64(got), 3*data_structure.MorrisRelativeError)
}

func TestCmdMorrisMultipleItemsPerCall(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYDIM([]string{"m", "2000", "5"})))

	res, _ := Decode(cmdMORRISINCRBY([]string{"m", "a", "1", "b", "2", "c", "3"}))
	assert.Len(t, res, 3)

	res, _ = Decode(cmdMORRISQUERY([]string{"m", "a", "b", "c"}))
	assert.Len(t, res, 3)

	// Small counts land exactly: the first increments always take, so the
	// counter is only approximate once it is well off the ground.
	counts := res.([]interface{})
	assert.EqualValues(t, "1", counts[0])
	assert.EqualValues(t, "2", counts[1])
	assert.EqualValues(t, "3", counts[2])
}

func TestCmdMorrisOnMissingKey(t *testing.T) {
	ResetStores()
	res, _ := Decode(cmdMORRISINCRBY([]string{"nope", "x", "1"}))
	assert.Contains(t, res, "does not exist")
	res, _ = Decode(cmdMORRISQUERY([]string{"nope", "x"}))
	assert.Contains(t, res, "does not exist")
	res, _ = Decode(cmdMORRISINFO([]string{"nope"}))
	assert.Contains(t, res, "does not exist")
}

func TestCmdMorrisWrongArity(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYDIM([]string{"m", "100", "5"})))

	for _, bad := range [][]string{{"m"}, {"m", "item"}, {"m", "item", "1", "other"}} {
		res, _ := Decode(cmdMORRISINCRBY(bad))
		assert.Contains(t, res, "wrong number of arguments", "MORRIS.INCRBY %v", bad)
	}
	res, _ := Decode(cmdMORRISQUERY([]string{"m"}))
	assert.Contains(t, res, "wrong number of arguments")
	res, _ = Decode(cmdMORRISINFO([]string{"m", "extra"}))
	assert.Contains(t, res, "wrong number of arguments")
}

// TestCmdMorrisInfoReportsTheTradeItIsMaking. The estimate is meaningless
// without its error bar, and the memory comparison is the reason to accept one.
func TestCmdMorrisInfoReportsTheTradeItIsMaking(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYDIM([]string{"m", "2000", "7"})))
	_, _ = Decode(cmdMORRISINCRBY([]string{"m", "x", "42"}))

	res, _ := Decode(cmdMORRISINFO([]string{"m"}))
	fields := map[string]string{}
	pairs := res.([]interface{})
	for i := 0; i+1 < len(pairs); i += 2 {
		fields[pairs[i].(string)] = pairs[i+1].(string)
	}

	assert.Equal(t, "2000", fields["Width"])
	assert.Equal(t, "7", fields["Depth"])
	assert.Equal(t, "42", fields["Total count"])
	assert.Equal(t, strconv.FormatUint(data_structure.MorrisMaxCount, 10), fields["Max count"])

	size, _ := strconv.ParseUint(fields["Size"], 10, 64)
	cmsSize, _ := strconv.ParseUint(fields["Count-Min equivalent size"], 10, 64)
	assert.Less(t, size*3, cmsSize, "a Morris table must cost well under a quarter of the sketch it replaces")
}

// TestMorrisKeyspaceIsAccountedAndEvictable. A keyspace invisible to the memory
// bound is how a server sails past -maxmemory, which is the bug the Keyed store
// was introduced to fix; a new one has to be wired in the same way.
func TestMorrisKeyspaceIsAccountedAndEvictable(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdMORRISINITBYDIM([]string{"m", "5000", "5"})))

	res, _ := Decode(cmdMEMORY([]string{"USAGE", "m"}))
	assert.EqualValues(t, 25000+64+len("m")+100, res,
		"MEMORY USAGE must find a Morris key, not report nil for it")

	assert.Greater(t, data_structure.TotalMemUsed(), uint64(25000),
		"the table must count towards the memory bound")
	assert.Equal(t, 1, data_structure.TotalKeys())
}

func mustDecode(t *testing.T, b []byte) interface{} {
	t.Helper()
	res, err := Decode(b)
	assert.Nil(t, err)
	return res
}
