package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 500; i++ {
		runOn(t, e, "SET", "temp"+strconv.Itoa(i), "value", "PX", "20")
	}
	assert.Equal(t, 500, e.space.TotalKeys())
	assert.Equal(t, 500, e.KeysWithExpiry())

	waitPast(60)

	// Nothing reads any of them; the cycle is the only thing running.
	for i := 0; i < 200 && e.space.TotalKeys() > 0; i++ {
		e.ExpireCycle()
	}

	assert.Equal(t, 0, e.space.TotalKeys(),
		"a key nobody reads must still go away once its TTL has passed")
	assert.Equal(t, 0, e.KeysWithExpiry(), "and must leave no expiry behind")
	assert.EqualValues(t, 500, e.ExpiredKeys())
}

// TestExpireCycleLeavesLivingKeysAlone. The sampling must not be a licence to
// remove keys that merely have a TTL.
func TestExpireCycleLeavesLivingKeysAlone(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 200; i++ {
		runOn(t, e, "SET", "alive"+strconv.Itoa(i), "v", "EX", "1000")
	}
	for i := 0; i < 200; i++ {
		runOn(t, e, "SET", "forever"+strconv.Itoa(i), "v")
	}
	// A keyspace that says it holds TTLs, to see that the cycles sampled.
	probe := &expirySamplingProbe{expires: true}
	e.space.RegisterKeyspace(probe)
	for i := 0; i < 100; i++ {
		e.ExpireCycle()
	}
	assert.Positive(t, probe.samples, "the cycles sampled the keyspaces with TTLs")
	assert.Equal(t, 400, e.space.TotalKeys(), "nothing has fallen due yet")
	assert.EqualValues(t, 0, e.ExpiredKeys())
}

// TestExpireCycleCostsNothingWhenNothingExpires.
//
// The cycle runs on every turn of the event loop, so its cost when there is
// nothing to do is the number that matters: a keyspace with no TTLs at all must
// not be sampled, and one whose TTLs are all in the future must be sampled once
// rather than repeatedly.
type expirySamplingProbe struct {
	data_structure.Keyspace
	samples int
	expires bool
}

func (p *expirySamplingProbe) KeysWithExpiry() int {
	if p.expires {
		return 1
	}
	return 0
}
func (p *expirySamplingProbe) Len() int { return 0 }
func (p *expirySamplingProbe) ActiveExpire(int) (int, int) {
	p.samples++
	return 0, 0
}

func TestExpireCycleCostsNothingWhenNothingExpires(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 1000; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
	}
	assert.Equal(t, 0, e.KeysWithExpiry())

	before := e.space.TotalKeys()
	// Observe the forbidden traversal directly. A nanosecond threshold on a
	// shared runner tests its scheduling, not whether expiry sampled a store.
	probe := &expirySamplingProbe{}
	e.space.RegisterKeyspace(probe)
	for i := 0; i < 10000; i++ {
		e.ExpireCycle()
	}
	assert.Zero(t, probe.samples, "a keyspace with no expiries must not be sampled")
	assert.Equal(t, before, e.space.TotalKeys())
	probe.expires = true
	e.ExpireCycle()
	assert.Equal(t, 1, probe.samples, "the probe must observe sampling once a TTL exists")
}

// TestExpireCycleIsBoundedPerTurn. A keyspace where everything has fallen due
// must not all be reaped in one turn of the loop, or the cycle becomes exactly
// the stall it exists to avoid.
func TestExpireCycleIsBoundedPerTurn(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 20000; i++ {
		runOn(t, e, "SET", "doomed"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)

	first := e.ExpireCycle()
	assert.Greater(t, first, 0, "the cycle must find them")
	held := e.options.WithDefaults()
	assert.LessOrEqual(t, first, held.ActiveExpireSamples*held.ActiveExpireRounds,
		"but must not empty the keyspace in a single turn")
	assert.Greater(t, e.space.TotalKeys(), 0, "there must be some left for the next turn")
}

// TestExpireCycleKeepsGoingWhileTheSampleSaysThereIsMore is the adaptive half:
// twenty keys a turn would take a thousand turns to clear twenty thousand
// expired keys, so a sample that keeps coming up expired earns another round.
func TestExpireCycleKeepsGoingWhileTheSampleSaysThereIsMore(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 5000; i++ {
		runOn(t, e, "SET", "d"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)

	assert.Greater(t, e.ExpireCycle(), e.options.WithDefaults().ActiveExpireSamples,
		"a keyspace that is mostly expired must be reaped faster than one sample a turn")
}

// TestActiveExpiryCanBeTurnedOff leaves expiry lazy, as it was.
func TestActiveExpiryCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{ActiveExpireSamples: Off})
	// The control: the same keys and cycles on an engine that samples.
	control := newTestEngine(t, Options{})
	for i := 0; i < 100; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v", "PX", "5")
		runOn(t, control, "SET", "k"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)
	for i := 0; i < 50; i++ {
		e.ExpireCycle()
		control.ExpireCycle()
	}
	assert.Less(t, control.space.TotalKeys(), 100, "with sampling on, the same cycles reap keys nobody read")
	assert.Equal(t, 100, e.space.TotalKeys(),
		"with sampling off, only a read reaps a key")
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "k0"))
	assert.Equal(t, 99, e.space.TotalKeys(), "and it reaps exactly the one that was read")
}

// TestActiveExpiryIsRecordedInTheLog.
//
// A key reaped by the cycle has no command behind it. If the log missed it, a
// restart would replay the key back into existence carrying an expiry already
// in the past - alive again until something happened to read it.
func TestActiveExpiryIsRecordedInTheLog(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "expire.aof")
	e.resetStores()
	assert.NoError(t, e.OpenAOF(path))

	runOn(t, e, "SET", "stays", "v")
	for i := 0; i < 100; i++ {
		runOn(t, e, "SET", "goes"+strconv.Itoa(i), "v", "PX", "10")
	}
	waitPast(40)
	for i := 0; i < 100 && e.KeysWithExpiry() > 0; i++ {
		e.ExpireCycle()
	}
	assert.Equal(t, 1, e.space.TotalKeys())

	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "DEL", "the cycle's removals must reach the log")

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 1, e.space.TotalKeys(),
		"a key the cycle expired must not come back on restart")
	assert.EqualValues(t, "v", runOn(t, e, "GET", "stays"))
}

// TestExpiredKeyFreesItsNameForAnotherType. A dead key owns nothing, so the
// keyspace check must not go on refusing its name.
func TestExpiredKeyFreesItsNameForAnotherType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "shared", "v", "PX", "5")
	waitPast(30)
	for i := 0; i < 50 && e.space.TotalKeys() > 0; i++ {
		e.ExpireCycle()
	}
	require.Zero(t, e.space.TotalKeys(), "the cycle reaped the key, before any command could")
	require.EqualValues(t, 1, e.ExpiredKeys())
	assert.EqualValues(t, 1, runOn(t, e, "SADD", "shared", "member"))
}

// TestExpireCycleReportsWhatItReclaimed, since a cycle falling behind is
// otherwise invisible.
func TestExpireCycleReportsWhatItReclaimed(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "a", "v", "PX", "5")
	runOn(t, e, "SET", "b", "v", "EX", "1000")
	waitPast(30)
	for i := 0; i < 50 && e.ExpireCycle() > 0; i++ {
	}

	info := runOn(t, e, "INFO").(string)
	assert.Contains(t, info, "expired_keys:1")
	assert.Contains(t, info, "expires=1", "the survivor still carries its TTL")
	assert.True(t, strings.Contains(info, "db0:keys=1"))
}
