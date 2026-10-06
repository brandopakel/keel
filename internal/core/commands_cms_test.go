package core

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCMSINITBYDIM(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "OK", runOn(t, e, "CMS.INITBYDIM", "c", "100", "5"))
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "c", "100", "5"), "key already exists")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "0", "5"), "invalid width")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "x", "5"), "invalid width")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "100", "0"), "invalid depth")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "100", "-1"), "invalid depth")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "4000000000", "4000000000"), "more than this server will allocate for one key")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "0.00000001", "0.001"), "more than this server will allocate for one key",
		"a probability sizing is checked the same way")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "2147483648", "2147483648"), "more than this server will allocate for one key",
		"a product whose byte count would wrap to zero is caught on its cell count")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "4294967295", "4294967295"), "more than this server will allocate for one key")
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "d", "100"), "wrong number of arguments")
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "d"), "a refused init creates nothing")
}

func TestCMSINITBYPROB(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "OK", runOn(t, e, "CMS.INITBYPROB", "c", "0.001", "0.01"))
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "c", "0.001", "0.01"), "key already exists")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "1", "0.01"), "invalid overestimation value")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "x", "0.01"), "invalid overestimation value")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "nan", "0.01"), "invalid overestimation value")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "0.01", "NaN"), "invalid prob value")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "0.01", "0"), "invalid prob value")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "0.01", "1.5"), "invalid prob value")
	assert.Contains(t, runOn(t, e, "CMS.INITBYPROB", "d", "0.01"), "wrong number of arguments")

	// The sketch it sized has to be big enough to be exact on a few items.
	runOn(t, e, "CMS.INCRBY", "c", "a", "5", "b", "7")
	assert.Equal(t, []interface{}{int64(5), int64(7), int64(0)}, runOn(t, e, "CMS.QUERY", "c", "a", "b", "z"))
}

func TestCMSINCRBYAndQUERY(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Contains(t, runOn(t, e, "CMS.INCRBY", "nokey", "a", "1"), "key does not exist")
	assert.Contains(t, runOn(t, e, "CMS.QUERY", "nokey", "a"), "key does not exist")

	runOn(t, e, "CMS.INITBYDIM", "c", "1000", "5")
	assert.Equal(t, []interface{}{int64(10), int64(3)}, runOn(t, e, "CMS.INCRBY", "c", "a", "10", "b", "3"))
	assert.Equal(t, []interface{}{int64(15)}, runOn(t, e, "CMS.INCRBY", "c", "a", "5"))
	assert.Equal(t, "*1\r\n:15\r\n", string(rawReplyOn(t, e, "CMS.QUERY", "c", "a")), "integers on the wire")
	assert.Equal(t, []interface{}{int64(15), int64(3), int64(0)}, runOn(t, e, "CMS.QUERY", "c", "a", "b", "never"))

	assert.Contains(t, runOn(t, e, "CMS.INCRBY", "c", "a", "x"), "Cannot parse number")
	assert.Contains(t, runOn(t, e, "CMS.INCRBY", "c", "a", "-1"), "Cannot parse number")
	assert.Contains(t, runOn(t, e, "CMS.INCRBY", "c", "a", "1", "b", "x"), "Cannot parse number")
	assert.Equal(t, []interface{}{int64(15)}, runOn(t, e, "CMS.QUERY", "c", "a"), "a refused batch changed nothing")

	assert.Contains(t, runOn(t, e, "CMS.INCRBY", "c", "a"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "CMS.INCRBY", "c"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "CMS.QUERY", "c"), "wrong number of arguments")
}

// TestCMSSizeIsCheckedAgainstTheBudgetBeforeAllocating: the memory budget is
// enforced after a Put, and the counters are allocated before it, so a sketch
// larger than the whole budget has to be refused on its dimensions alone.
func TestCMSSizeIsCheckedAgainstTheBudgetBeforeAllocating(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	withBudget(t, e, 1<<20, EvictLRU)
	assert.Contains(t, runOn(t, e, "CMS.INITBYDIM", "c", "100000", "10"), "does not fit in maxmemory", "4MB of counters against a 1MB budget")
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "c"))
	assert.Equal(t, "OK", runOn(t, e, "CMS.INITBYDIM", "small", "1000", "5"), "one that fits is fine")
}

func TestCMSINCRBYReportsOverflowInPlace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "CMS.INITBYDIM", "c", "100", "3")
	almost := strconv.FormatUint(math.MaxUint32-1, 10)
	assert.Equal(t, []interface{}{int64(math.MaxUint32 - 1)}, runOn(t, e, "CMS.INCRBY", "c", "a", almost))
	res := runOn(t, e, "CMS.INCRBY", "c", "b", "1", "a", "10").([]interface{})
	assert.Equal(t, int64(1), res[0])
	assert.Contains(t, res[1], "INCRBY overflow", "the saturated counter answers an error in its position")
	assert.Equal(t, "*2\r\n:2\r\n-CMS: INCRBY overflow\r\n", string(rawReplyOn(t, e, "CMS.INCRBY", "c", "b", "1", "a", "1")), "b was 1 and is now 2; a stays saturated")
}
