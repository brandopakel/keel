package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/config"
	"github.com/stretchr/testify/require"
)

// The BF.* and CF.* replies follow RedisBloom's; what is logged, replicated
// and dumped does not depend on a reply, and these tests hold it to what it
// was before the replies changed.
//
// testdata/redisbloom-persistence-6567ca7.json was written by these tests on
// develop at 6567ca7, before any reply changed, with
//
//	KEEL_CAPTURE_REDISBLOOM_PERSISTENCE=$PWD/testdata/redisbloom-persistence-6567ca7.json \
//	  go test ./internal/core -run 'TestBloomCuckoo(Persistence|LegacyLog)'
//
// and is compared with on every run since.

const redisBloomPersistenceFixture = "testdata/redisbloom-persistence-6567ca7.json"

// bloomPersistenceSession is every BF and CF write form the build before
// RedisBloom parity accepted, with reads and refusals between them. Each was
// accepted then and means the same thing now, so the log, both replication
// streams and the dump images it leaves must be the same, byte for byte. A
// CF.DEL of a key that does not exist is not here: it answered 0 before and
// was logged, and now answers RedisBloom's "Not found" and is not -
// TestCFDELOfAMissingKeyIsNotLoggedOrReplicated covers it.
var bloomPersistenceSession = func() [][]string {
	session := [][]string{
		{"BF.RESERVE", "bf", "0.001", "1000"},
		{"BF.RESERVE", "bf:grow", "0.01", "10", "EXPANSION", "4"},
		{"BF.ADD", "bf", "a"},
		{"BF.ADD", "bf", "a"},
		{"BF.MADD", "bf", "b", "c", "a"},
		{"BF.ADD", "bf:auto", "x"},
		{"BF.MADD", "bf:auto2", "y", "z"},
	}
	grow := []string{"BF.MADD", "bf:grow"}
	for i := 0; i < 30; i++ {
		grow = append(grow, "item:"+strconv.Itoa(i))
	}
	return append(append(session, grow), [][]string{
		{"BF.EXISTS", "bf", "a"},
		{"BF.MEXISTS", "bf", "a", "q"},
		{"BF.INFO", "bf"},
		{"BF.RESERVE", "bf", "0.01", "100"},
		{"BF.RESERVE", "bf:bad", "2", "100"},
		{"CF.RESERVE", "cf", "1000"},
		{"CF.ADD", "cf", "a"},
		{"CF.ADD", "cf", "a"},
		{"CF.ADDNX", "cf", "a"},
		{"CF.ADDNX", "cf", "b"},
		{"CF.ADD", "cf:auto", "x"},
		{"CF.ADDNX", "cf:auto2", "y"},
		{"CF.DEL", "cf", "a"},
		{"CF.DEL", "cf", "never"},
		{"CF.EXISTS", "cf", "a"},
		{"CF.MEXISTS", "cf", "a", "b"},
		{"CF.COUNT", "cf", "a"},
		{"CF.INFO", "cf"},
		{"CF.RESERVE", "cf", "1000"},
		{"SET", "str", "v"},
		{"BF.ADD", "str", "x"},
		{"CF.ADD", "str", "x"},
		{"BF.EXISTS", "str", "x"},
		{"CF.DEL", "str", "x"},
	}...)
}()

var bloomPersistenceKeys = []string{"bf", "bf:grow", "bf:auto", "bf:auto2", "cf", "cf:auto", "cf:auto2"}

// legacyBloomLog is a log the build before RedisBloom parity could have
// written: commands it accepted and recorded that RedisBloom refuses, or
// answers differently. CF.RESERVE below RedisBloom's minimum capacity and with
// a leading zero, BF.RESERVE with a number Go parses and Redis does not, an
// expansion and a capacity past RedisBloom's ranges, a CF.DEL of a key that is
// not there, and reservations of keys RedisBloom would read as options -
// nonscaling, expansion, maxiterations - which were only names to the earlier
// build. A log is replayed by the build that reads it, so each has to replay
// as it did when it was written.
var legacyBloomLog = [][]string{
	{"CF.RESERVE", "cf:1", "1"},
	{"CF.RESERVE", "cf:3", "3"},
	{"CF.RESERVE", "cf:7", "007"},
	{"CF.ADD", "cf:1", "a"},
	{"CF.ADD", "cf:1", "b"},
	{"CF.ADDNX", "cf:3", "c"},
	{"CF.DEL", "missing", "x"},
	{"CF.DEL", "cf:1", "never"},
	{"CF.DEL", "cf:1", "a"},
	{"BF.RESERVE", "bf:007", "0.01", "007"},
	{"BF.RESERVE", "bf:plus", "+0.01", "100"},
	{"BF.RESERVE", "bf:hex", "0x1p-7", "100"},
	{"BF.RESERVE", "bf:wide", "0.01", "10", "EXPANSION", "100000"},
	{"BF.RESERVE", "bf:zeros", "0.01", "10", "expansion", "007"},
	{"BF.RESERVE", "bf:huge", "0.999", "2000000000"},
	{"BF.MADD", "bf:007", "a", "b", "c"},
	{"BF.MADD", "bf:wide", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
	{"BF.ADD", "bf:hex", "a"},
	{"BF.RESERVE", "nonscaling", "0.01", "2"},
	{"BF.MADD", "nonscaling", "a", "b", "c", "d"},
	{"BF.RESERVE", "NonScaling", "0.01", "1", "EXPANSION", "3"},
	{"BF.MADD", "NonScaling", "a", "b"},
	{"CF.RESERVE", "expansion", "1000"},
	{"CF.RESERVE", "maxiterations", "1000"},
	{"CF.RESERVE", "bucketsize", "100"},
	{"CF.ADD", "expansion", "a"},
}

var legacyBloomKeys = []string{"cf:1", "cf:3", "cf:7", "bf:007", "bf:plus", "bf:hex", "bf:wide", "bf:zeros", "bf:huge",
	"nonscaling", "NonScaling", "expansion", "maxiterations", "bucketsize"}

type redisBloomPersistence struct {
	SourceRevision string `json:"source_revision"`
	Note           string `json:"note"`
	// Session: sha256 of the log, the protocol 2 replication delta, the
	// protocol 1 replication deltas and the dump images the session leaves.
	SessionLog     string `json:"session_log_sha256"`
	SessionV2Delta string `json:"session_replication_v2_delta_sha256"`
	SessionV1Delta string `json:"session_replication_v1_delta_sha256"`
	SessionDumps   string `json:"session_dumps_sha256"`
	// Legacy: sha256 of the dump images legacyBloomLog replays to.
	LegacyDumps string `json:"legacy_log_dumps_sha256"`
	// LegacyEach is each of those images' sha256, so a failure names the
	// filter that moved.
	LegacyEach map[string]string `json:"legacy_log_dump_sha256_by_key"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dumpImages(t *testing.T, keys []string) ([]byte, map[string]string) {
	t.Helper()
	var all []byte
	each := map[string]string{}
	for _, key := range keys {
		image, ok := dumpKey(key)
		require.True(t, ok, "%s holds a filter", key)
		all = append(all, key...)
		all = append(all, 0)
		all = append(all, image...)
		each[key] = sha256Hex(image)
	}
	return all, each
}

// runBloomSessionV2 runs the session on a connection of the given protocol,
// with the log and the protocol 2 feed on, and returns the log, the delta the
// feed sends a replica that is already caught up with an empty primary, and
// the dumps.
func runBloomSessionV2(t *testing.T, resp3 bool) (log, delta, dumps []byte) {
	setupReplicationV2(t)
	frames := snapshotV2(t)
	offset, epoch := frames[len(frames)-1].To, frames[0].Epoch
	for _, args := range bloomPersistenceSession {
		rawReplyAs(t, resp3, args[0], args[1:]...)
	}
	require.NoError(t, FlushAOF())
	for {
		f := pullV2(t, epoch, offset, "", 0)
		require.False(t, f.Full)
		delta = append(delta, f.Body...)
		if f.To == offset || f.CaughtUp {
			break
		}
		offset = f.To
	}
	dumps, _ = dumpImages(t, bloomPersistenceKeys)
	log, err := os.ReadFile(defaultEngine.aof.path)
	require.NoError(t, err)
	return log, delta, dumps
}

// runBloomSessionV1 runs the session with the protocol 1 feed on, pulling a
// delta after every command so each batch holds one key and the order is the
// commands'.
func runBloomSessionV1(t *testing.T, resp3 bool) []byte {
	oldFeed, oldReplica, oldProtocol := config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol
	t.Cleanup(func() {
		CloseAOF()
		config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol = oldFeed, oldReplica, oldProtocol
	})
	ResetStores()
	config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol = true, "", 1
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary.aof")))
	require.NoError(t, InitReplication())
	pull := func(epoch string, offset uint64) ReplicationFrame {
		reply := run(t, "KEEL.REPL.PULL", epoch, strconv.FormatUint(offset, 10))
		encoded, ok := reply.(string)
		require.True(t, ok, "reply: %v", reply)
		var f ReplicationFrame
		require.NoError(t, json.Unmarshal([]byte(encoded), &f))
		return f
	}
	first := pull("", 0)
	require.True(t, first.Full)
	epoch, offset := first.Epoch, first.To
	var delta []byte
	for _, args := range bloomPersistenceSession {
		rawReplyAs(t, resp3, args[0], args[1:]...)
		f := pull(epoch, offset)
		require.False(t, f.Full, "%q", args)
		delta = append(delta, f.Body...)
		offset = f.To
	}
	return delta
}

func writeLegacyBloomLog(t *testing.T) string {
	t.Helper()
	var body []byte
	for _, args := range legacyBloomLog {
		body = appendCommand(body, args...)
	}
	path := filepath.Join(t.TempDir(), "legacy.aof")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	return path
}

func replayLegacyBloomLog(t *testing.T) ([]byte, map[string]string) {
	t.Helper()
	ResetStores()
	t.Cleanup(ResetStores)
	applied, err := LoadAOF(writeLegacyBloomLog(t))
	require.NoError(t, err, "a log the earlier build wrote replays")
	require.Equal(t, len(legacyBloomLog), applied)
	return dumpImages(t, legacyBloomKeys)
}

func loadRedisBloomPersistence(t *testing.T) redisBloomPersistence {
	t.Helper()
	raw, err := os.ReadFile(redisBloomPersistenceFixture)
	require.NoError(t, err)
	var fixture redisBloomPersistence
	require.NoError(t, json.Unmarshal(raw, &fixture))
	return fixture
}

// TestBloomCuckooPersistenceCapture writes the fixture when asked to, and is
// skipped otherwise. It was run once, on develop at 6567ca7.
func TestBloomCuckooPersistenceCapture(t *testing.T) {
	path := os.Getenv("KEEL_CAPTURE_REDISBLOOM_PERSISTENCE")
	if path == "" {
		t.Skip("set KEEL_CAPTURE_REDISBLOOM_PERSISTENCE to rewrite the fixture")
	}
	log, v2, dumps := runBloomSessionV2(t, false)
	v1 := runBloomSessionV1(t, false)
	legacy, legacyEach := replayLegacyBloomLog(t)
	fixture := redisBloomPersistence{
		SourceRevision: "6567ca7b844f77bb8697ff0fadec4b4b2136523a",
		Note:           "Captured before BF/CF replies changed to RedisBloom's; see redisbloom_persistence_test.go.",
		SessionLog:     sha256Hex(log), SessionV2Delta: sha256Hex(v2),
		SessionV1Delta: sha256Hex(v1), SessionDumps: sha256Hex(dumps),
		LegacyDumps: sha256Hex(legacy), LegacyEach: legacyEach,
	}
	body, err := json.MarshalIndent(fixture, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(body, '\n'), 0o644))
}

// TestBloomCuckooPersistenceIsUnchanged: the session writes the log, both
// replication streams and the dump images it wrote before the replies
// changed, from a RESP2 connection and from a RESP3 one.
func TestBloomCuckooPersistenceIsUnchanged(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_REDISBLOOM_PERSISTENCE") != "" {
		t.Skip("capturing")
	}
	want := loadRedisBloomPersistence(t)
	for _, resp3 := range []bool{false, true} {
		t.Run("resp3="+strconv.FormatBool(resp3), func(t *testing.T) {
			log, v2, dumps := runBloomSessionV2(t, resp3)
			require.Equal(t, want.SessionLog, sha256Hex(log), "the log")
			require.Equal(t, want.SessionV2Delta, sha256Hex(v2), "the protocol 2 replication delta")
			require.Equal(t, want.SessionDumps, sha256Hex(dumps), "the dump images")
			require.Equal(t, want.SessionV1Delta, sha256Hex(runBloomSessionV1(t, resp3)), "the protocol 1 replication deltas")
		})
	}
}

// TestBloomCuckooLegacyLogReplays: a log the earlier build wrote, holding
// commands RedisBloom refuses, replays to the filters it replayed to then.
func TestBloomCuckooLegacyLogReplays(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_REDISBLOOM_PERSISTENCE") != "" {
		t.Skip("capturing")
	}
	want := loadRedisBloomPersistence(t)
	got, each := replayLegacyBloomLog(t)
	require.Len(t, want.LegacyEach, len(legacyBloomKeys))
	for key, image := range want.LegacyEach {
		require.Equal(t, image, each[key], "%s", key)
	}
	require.Equal(t, want.LegacyDumps, sha256Hex(got))
	require.True(t, strings.HasPrefix(want.SourceRevision, "6567ca7"))
}

// TestBFRESERVEOfAKeyNamedNONSCALINGIsLoggedSoItReplays: RedisBloom reads a
// key named NONSCALING as the option, and so does this build, but the earlier
// build read it as a name, and a log is replayed as the earlier build read it.
// So the reservation is logged with the option spelled out - a form the
// earlier build refused - and replays to the filter it made.
func TestBFRESERVEOfAKeyNamedNONSCALINGIsLoggedSoItReplays(t *testing.T) {
	var before []byte
	path := withAOF(t, func() {
		require.Equal(t, "+OK\r\n", string(rawReply(t, "BF.RESERVE", "nonscaling", "0.000001", "50")))
		items := []string{"nonscaling"}
		for i := 0; i < 50; i++ {
			items = append(items, "item:"+strconv.Itoa(i))
		}
		run(t, "BF.MADD", items...)
		require.Equal(t, "-ERR non scaling filter is full\r\n", string(rawReply(t, "BF.ADD", "nonscaling", "over")))
		before, _ = dumpKey("nonscaling")
	})
	log, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(log), string(appendCommand(nil, "BF.RESERVE", "nonscaling", "0.000001", "50", "NONSCALING")))
	restart(t, path)
	after, ok := dumpKey("nonscaling")
	require.True(t, ok)
	require.Equal(t, before, after, "the filter that does not grow, as it was")
	require.Equal(t, "*1\r\n$-1\r\n", string(rawReply(t, "BF.INFO", "nonscaling", "EXPANSION")))
}
