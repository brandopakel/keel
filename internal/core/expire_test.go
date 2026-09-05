package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// waitPast blocks until ms milliseconds have gone by, without sleeping the test
// framework's clock along with it.
func waitPast(ms int) {
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
	}
}

// TestExpiredKeysAreReclaimedWithoutBeingRead is the reason the cycle exists.
//
// Expiry used to happen only when something looked at a key, so a key written
// once with a short TTL and never read again held its memory until eviction got
// round to it - which, with no memory pressure, is never.
func TestExpiredKeysAreReclaimedWithoutBeingRead(t *testing.T) {
	ResetStores()
	for i := 0; i < 500; i++ {
		run(t, "SET", "temp"+strconv.Itoa(i), "value", "PX", "20")
	}
	assert.Equal(t, 500, data_structure.TotalKeys())
	assert.Equal(t, 500, KeysWithExpiry())

	waitPast(60)

	// Nothing reads any of them; the cycle is the only thing running.
	for i := 0; i < 200 && data_structure.TotalKeys() > 0; i++ {
		ExpireCycle()
	}

	assert.Equal(t, 0, data_structure.TotalKeys(),
		"a key nobody reads must still go away once its TTL has passed")
	assert.Equal(t, 0, KeysWithExpiry(), "and must leave no expiry behind")
	assert.EqualValues(t, 500, ExpiredKeys())
}

// TestExpireCycleLeavesLivingKeysAlone. The sampling must not be a licence to
// remove keys that merely have a TTL.
func TestExpireCycleLeavesLivingKeysAlone(t *testing.T) {
	ResetStores()
	for i := 0; i < 200; i++ {
		run(t, "SET", "alive"+strconv.Itoa(i), "v", "EX", "1000")
	}
	for i := 0; i < 200; i++ {
		run(t, "SET", "forever"+strconv.Itoa(i), "v")
	}
	for i := 0; i < 100; i++ {
		ExpireCycle()
	}
	assert.Equal(t, 400, data_structure.TotalKeys(), "nothing has fallen due yet")
	assert.EqualValues(t, 0, ExpiredKeys())
}

// TestExpireCycleCostsNothingWhenNothingExpires.
//
// The cycle runs on every turn of the event loop, so its cost when there is
// nothing to do is the number that matters: a keyspace with no TTLs at all must
// not be sampled, and one whose TTLs are all in the future must be sampled once
// rather than repeatedly.
func TestExpireCycleCostsNothingWhenNothingExpires(t *testing.T) {
	ResetStores()
	for i := 0; i < 1000; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v")
	}
	assert.Equal(t, 0, KeysWithExpiry())

	before := data_structure.TotalKeys()
	start := time.Now()
	for i := 0; i < 10000; i++ {
		ExpireCycle()
	}
	perCycle := time.Since(start) / 10000
	t.Logf("no keys with a TTL: %v per cycle", perCycle)
	assert.Equal(t, before, data_structure.TotalKeys())
	if !raceEnabled {
		assert.Less(t, perCycle, 500*time.Nanosecond,
			"a keyspace with no expiries must not be walked")
	} // The race detector instruments the registry reads; timing measures that overhead.
}

// TestExpireCycleIsBoundedPerTurn. A keyspace where everything has fallen due
// must not all be reaped in one turn of the loop, or the cycle becomes exactly
// the stall it exists to avoid.
func TestExpireCycleIsBoundedPerTurn(t *testing.T) {
	ResetStores()
	for i := 0; i < 20000; i++ {
		run(t, "SET", "doomed"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)

	first := ExpireCycle()
	assert.Greater(t, first, 0, "the cycle must find them")
	assert.LessOrEqual(t, first, config.ActiveExpireSamples*config.ActiveExpireRounds,
		"but must not empty the keyspace in a single turn")
	assert.Greater(t, data_structure.TotalKeys(), 0, "there must be some left for the next turn")
}

// TestExpireCycleKeepsGoingWhileTheSampleSaysThereIsMore is the adaptive half:
// twenty keys a turn would take a thousand turns to clear twenty thousand
// expired keys, so a sample that keeps coming up expired earns another round.
func TestExpireCycleKeepsGoingWhileTheSampleSaysThereIsMore(t *testing.T) {
	ResetStores()
	for i := 0; i < 5000; i++ {
		run(t, "SET", "d"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)

	assert.Greater(t, ExpireCycle(), config.ActiveExpireSamples,
		"a keyspace that is mostly expired must be reaped faster than one sample a turn")
}

// TestActiveExpiryCanBeTurnedOff leaves expiry lazy, as it was.
func TestActiveExpiryCanBeTurnedOff(t *testing.T) {
	original := config.ActiveExpireSamples
	defer func() { config.ActiveExpireSamples = original }()
	config.ActiveExpireSamples = 0

	ResetStores()
	for i := 0; i < 100; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)
	for i := 0; i < 50; i++ {
		ExpireCycle()
	}
	assert.Equal(t, 100, data_structure.TotalKeys(),
		"with sampling off, only a read reaps a key")
	assert.Equal(t, constant.RespNil, rawReply(t, "GET", "k0"))
	assert.Equal(t, 99, data_structure.TotalKeys(), "and it reaps exactly the one that was read")
}

// TestActiveExpiryIsRecordedInTheLog.
//
// A key reaped by the cycle has no command behind it. If the log missed it, a
// restart would replay the key back into existence carrying an expiry already
// in the past - alive again until something happened to read it.
func TestActiveExpiryIsRecordedInTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expire.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))

	run(t, "SET", "stays", "v")
	for i := 0; i < 100; i++ {
		run(t, "SET", "goes"+strconv.Itoa(i), "v", "PX", "10")
	}
	waitPast(40)
	for i := 0; i < 100 && KeysWithExpiry() > 0; i++ {
		ExpireCycle()
	}
	assert.Equal(t, 1, data_structure.TotalKeys())

	assert.NoError(t, FlushAOF())
	assert.NoError(t, CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "DEL", "the cycle's removals must reach the log")

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 1, data_structure.TotalKeys(),
		"a key the cycle expired must not come back on restart")
	assert.EqualValues(t, "v", run(t, "GET", "stays"))
}

// TestExpiredKeyFreesItsNameForAnotherType. A dead key owns nothing, so the
// keyspace check must not go on refusing its name.
func TestExpiredKeyFreesItsNameForAnotherType(t *testing.T) {
	ResetStores()
	run(t, "SET", "shared", "v", "PX", "5")
	waitPast(30)
	for i := 0; i < 50 && data_structure.TotalKeys() > 0; i++ {
		ExpireCycle()
	}
	assert.EqualValues(t, 1, run(t, "SADD", "shared", "member"))
}

// TestExpireCycleReportsWhatItReclaimed, since a cycle falling behind is
// otherwise invisible.
func TestExpireCycleReportsWhatItReclaimed(t *testing.T) {
	ResetStores()
	run(t, "SET", "a", "v", "PX", "5")
	run(t, "SET", "b", "v", "EX", "1000")
	waitPast(30)
	for i := 0; i < 50 && ExpireCycle() > 0; i++ {
	}

	info := run(t, "INFO", "").(string)
	assert.Contains(t, info, "expired_keys:1")
	assert.Contains(t, info, "expires=1", "the survivor still carries its TTL")
	assert.True(t, strings.Contains(info, "db0:keys=1"))
}
