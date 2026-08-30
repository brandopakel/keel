package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/config"
	"memkv/internal/constant"
)

func setString(key, value string) {
	oType, oEnc := deduceTypeString(value)
	dictStore.Put(key, dictStore.NewObj(value, oType, oEnc))
}

// TestCmdLCSDocumentedExample is the example from the Redis documentation,
// checked all the way out to the wire so that the reply shape - not just the
// answer - is what a Redis client expects.
func TestCmdLCSDocumentedExample(t *testing.T) {
	ResetStores()
	setString("key1", "ohmytext")
	setString("key2", "mynewtext")

	res, _ := Decode(cmdLCS([]string{"key1", "key2"}))
	assert.EqualValues(t, "mytext", res)

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "LEN"}))
	assert.EqualValues(t, 6, res)

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "IDX"}))
	assert.Equal(t, []interface{}{
		"matches",
		[]interface{}{
			[]interface{}{[]interface{}{int64(4), int64(7)}, []interface{}{int64(5), int64(8)}},
			[]interface{}{[]interface{}{int64(2), int64(3)}, []interface{}{int64(0), int64(1)}},
		},
		"len", int64(6),
	}, res, "IDX must report matches from the end of the strings backwards")
}

func TestCmdLCSWithMatchLen(t *testing.T) {
	ResetStores()
	setString("key1", "ohmytext")
	setString("key2", "mynewtext")

	res, _ := Decode(cmdLCS([]string{"key1", "key2", "IDX", "WITHMATCHLEN"}))
	matches := res.([]interface{})[1].([]interface{})
	assert.Len(t, matches, 2)
	assert.EqualValues(t, 4, matches[0].([]interface{})[2], "text")
	assert.EqualValues(t, 2, matches[1].([]interface{})[2], "my")
}

// TestCmdLCSMinMatchLenFiltersRangesNotTheLength. MINMATCHLEN says which
// matches are worth listing; it does not change what was found, so the reported
// length still covers the whole subsequence.
func TestCmdLCSMinMatchLenFiltersRangesNotTheLength(t *testing.T) {
	ResetStores()
	setString("key1", "ohmytext")
	setString("key2", "mynewtext")

	res, _ := Decode(cmdLCS([]string{"key1", "key2", "IDX", "MINMATCHLEN", "4"}))
	reply := res.([]interface{})
	assert.Len(t, reply[1].([]interface{}), 1, "only the four-character match survives")
	assert.EqualValues(t, 6, reply[3], "the length is still the whole subsequence")

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "IDX", "MINMATCHLEN", "99"}))
	reply = res.([]interface{})
	assert.Empty(t, reply[1])
	assert.EqualValues(t, 6, reply[3])
}

func TestCmdLCSMissingKeyIsAnEmptyString(t *testing.T) {
	ResetStores()
	setString("key1", "hello")

	res, _ := Decode(cmdLCS([]string{"key1", "nosuchkey"}))
	assert.EqualValues(t, "", res)

	res, _ = Decode(cmdLCS([]string{"nosuchkey", "alsomissing", "LEN"}))
	assert.EqualValues(t, 0, res)
}

// TestCmdLCSOnAKeyOfAnotherType pins the behaviour rather than endorsing it.
//
// Redis would answer WRONGTYPE. memkv keeps a separate map per type, so a key
// holding a set is simply not in the string dictionary, and LCS sees it as
// absent - which is what GET already does for the same key, and what every
// other command here does. Diverging in this one command would be the odd one
// out; the honest fix is a shared key directory, which is a change to the
// keyspace rather than to LCS.
func TestCmdLCSOnAKeyOfAnotherType(t *testing.T) {
	ResetStores()
	setString("str", "hello")
	assert.EqualValues(t, 1, mustDecode(t, cmdSADD([]string{"aset", "hello"})))

	res, _ := Decode(cmdLCS([]string{"str", "aset"}))
	assert.EqualValues(t, "", res, "a set key reads as absent, as it does for GET")

	// Compared as raw bytes: the decoder renders a RESP null bulk string as an
	// empty Go string, so decoding would not tell a nil reply from "".
	assert.Equal(t, constant.RespNil, cmdGET([]string{"aset"}),
		"which is the behaviour LCS is being consistent with")
}

func TestCmdLCSSyntax(t *testing.T) {
	ResetStores()
	setString("key1", "abc")
	setString("key2", "abd")

	res, _ := Decode(cmdLCS([]string{"key1"}))
	assert.Contains(t, res, "wrong number of arguments")

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "LEN", "IDX"}))
	assert.Contains(t, res, "please just use IDX")

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "NOPE"}))
	assert.Contains(t, res, "syntax error")

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "IDX", "MINMATCHLEN"}))
	assert.Contains(t, res, "syntax error")

	res, _ = Decode(cmdLCS([]string{"key1", "key2", "IDX", "MINMATCHLEN", "wat"}))
	assert.Contains(t, res, "not an integer")
}

// TestCmdLCSRefusesWhatWouldStallTheServer. The guard is the whole reason a
// computing command is safe to expose on a single-threaded server, so it has to
// be reachable from the command and not just from the library beneath it.
func TestCmdLCSRefusesWhatWouldStallTheServer(t *testing.T) {
	ResetStores()
	original := config.LCSMaxCells
	defer func() { config.LCSMaxCells = original }()

	setString("big1", strings.Repeat("a", 1000))
	setString("big2", strings.Repeat("b", 1000))

	config.LCSMaxCells = 999999
	res, _ := Decode(cmdLCS([]string{"big1", "big2"}))
	assert.Contains(t, res, "String too long for LCS")

	config.LCSMaxCells = 1000000
	res, _ = Decode(cmdLCS([]string{"big1", "big2", "LEN"}))
	assert.EqualValues(t, 0, res, "exactly at the limit is allowed")

	config.LCSMaxCells = 0
	res, _ = Decode(cmdLCS([]string{"big1", "big2", "LEN"}))
	assert.EqualValues(t, 0, res, "zero removes the bound")
}

func TestCmdLCSGoesThroughEval(t *testing.T) {
	ResetStores()
	setString("key1", "ohmytext")
	setString("key2", "mynewtext")

	var out strings.Builder
	err := EvalAndResponse(&MemKVCmd{Cmd: "LCS", Args: []string{"key1", "key2", "LEN"}}, &writerOnly{&out})
	assert.Nil(t, err)
	assert.Equal(t, ":6\r\n", out.String())
}

type writerOnly struct{ b *strings.Builder }

func (w *writerOnly) Read([]byte) (int, error)    { return 0, nil }
func (w *writerOnly) Write(p []byte) (int, error) { return w.b.Write(p) }
