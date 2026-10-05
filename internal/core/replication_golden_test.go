package core

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// The replication stream's bytes, held to what develop wrote before step 2.4
// of the embedding plan (docs/embedding-plan.md) moved replication and
// failover into the engine.
//
// Each scenario runs a primary on the default engine with the log open, pulls
// its stream in-process as a replica would, then turns the same engine into a
// replica and applies what it pulled. It writes a transcript of everything a
// peer or an operator can see:
//
//   - every frame pulled, protocol 1 and 2, delta and snapshot: its JSON as
//     sent, and its body record by record;
//   - every reply to a replication, failover or refused command, KEEL.DUMP's
//     included;
//   - INFO replication, field by field, on the primary and on the replica;
//   - the replica's log, the part each applied frame added, and its restart
//     checkpoint;
//   - the term file a promotion, a fence or an observed term writes;
//   - the keyspace, which the replica must share with its primary after each
//     frame that brings it level.
//
// testdata/replication-9736d8d/ holds the transcripts develop at 9736d8d
// wrote, captured with
//
//	KEEL_CAPTURE_REPLICATION_GOLDEN=$PWD/testdata/replication-9736d8d \
//	  go test ./internal/core -run '^TestReplicationGoldenCapture$'
//
// and every run since must write the same transcript, line for line, under
// every fsync policy with synchronous and worker appends. With
// KEEL_REPLICATION_GOLDEN_DEBUG naming a directory, the capture also writes
// each mode's transcript there, uncompressed, to compare by hand.
//
// A transcript leaves out, or names by the order it first appears, only what
// is not the same from run to run:
//
//   - Epochs and snapshot identities are random. Each is written as the order
//     it first appeared in (epoch-1, snapshot-1), which keeps whether two
//     frames, or a frame and a replica, name the same one. A frame's checksum
//     is checked against its contents and written as "ok".
//   - Expiries and map order in records are normalized as the persistence
//     golden test normalizes a log (persistence_golden_test.go), and a
//     snapshot, which is a rewritten log, has the records of one collection
//     joined as that test joins them. The collections here are small enough
//     that a rewrite never cuts one.
//   - Protocol 1 seals the keys a primary changed from a map, and walks the
//     keyspace for a full frame, so the records of each key in a frame are kept
//     together and the keys put in order. The replica's log of that frame is
//     sorted the same way.
//   - The ages INFO reports in milliseconds, replica_last_update_ms and
//     replication_acked_age_ms, are left out.
//   - A checkpoint's digest is checked against the replica's log and written
//     as "log".
//
// Every opaque command here changes one key, so the record order of each
// protocol 2 frame is the order the primary published it in, and is kept.
const replicationGoldenDir = "testdata/replication-9736d8d"

// goldenPrimary is what a golden replica names as its primary.
const goldenPrimary = "primary.golden:6379"

type replicationGoldenScenario struct {
	name     string
	protocol int
	run      func(g *replicationGolden)
}

var replicationGoldenScenarios = []replicationGoldenScenario{
	{"protocol-2", 2, goldenProtocol2},
	{"protocol-1", 1, goldenProtocol1},
	{"terms", 2, goldenTerms},
}

// replicationGolden is one scenario under one mode: a goldenRun for the
// default engine's log, which is the primary's until becomeReplica, and the
// transcript it writes.
type replicationGolden struct {
	*goldenRun
	names   map[string]string
	out     bytes.Buffer
	logRead int
}

func startReplicationGolden(t *testing.T, mode goldenMode, protocol int) *replicationGolden {
	t.Helper()
	feed, replicaOf, previous := config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol
	expiry, eviction := data_structure.DefaultSpace.SuspendExpiry, data_structure.DefaultSpace.SuspendEviction
	restoreTerm := saveGoldenTerm()
	// Registered before startGoldenRun's, so it runs after the log is closed.
	t.Cleanup(func() {
		config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol = feed, replicaOf, previous
		require.NoError(t, InitReplication())
		data_structure.DefaultSpace.SuspendExpiry, data_structure.DefaultSpace.SuspendEviction = expiry, eviction
		restoreTerm()
	})
	config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol = true, "", protocol
	r, _ := startGoldenRun(t, mode, nil)
	require.NoError(t, InitReplication())
	return &replicationGolden{goldenRun: r, names: map[string]string{}}
}

// saveGoldenTerm keeps the failover state a scenario's LoadTerm replaces, and
// returns what puts it back.
func saveGoldenTerm() func() {
	saved := defaultEngine.failover
	return func() { defaultEngine.failover = saved }
}

func runReplicationGolden(t *testing.T, scenario replicationGoldenScenario, mode goldenMode) []byte {
	t.Helper()
	g := startReplicationGolden(t, mode, scenario.protocol)
	scenario.run(g)
	return g.out.Bytes()
}

func (g *replicationGolden) printf(format string, args ...any) {
	fmt.Fprintf(&g.out, format, args...)
	g.out.WriteByte('\n')
}

// id names a random identity by the order it first appeared in.
func (g *replicationGolden) id(kind, value string) string {
	if value == "" {
		return ""
	}
	name, ok := g.names[kind+":"+value]
	if !ok {
		n := 1
		for k := range g.names {
			if strings.HasPrefix(k, kind+":") {
				n++
			}
		}
		name = kind + "-" + strconv.Itoa(n)
		g.names[kind+":"+value] = name
	}
	return name
}

func (g *replicationGolden) window() goldenWindow {
	return goldenWindow{g.start, time.Now().UnixMilli()}
}

// call runs a command outside the replies the transcript keeps, then the
// cycle's flush, and returns its reply.
func (g *replicationGolden) call(parts ...string) []byte {
	g.t.Helper()
	var w replyWriter
	require.NoError(g.t, EvalAndResponse(&Command{Cmd: parts[0], Args: parts[1:]}, &w), "%q", parts)
	g.cycle()
	return w.b
}

// reply runs a command and writes its reply into the transcript.
func (g *replicationGolden) reply(parts ...string) []byte {
	g.t.Helper()
	got := g.call(parts...)
	var line bytes.Buffer
	for _, p := range parts {
		for _, kind := range []string{"epoch", "snapshot"} {
			if name, ok := g.names[kind+":"+p]; ok {
				p = name
			}
		}
		goldenValue(&line, p)
	}
	line.WriteString(" ->")
	goldenValue(&line, string(got))
	g.printf("reply%s", line.String())
	return got
}

// recordKind says how a body is normalized before its records are written.
type recordKind int

const (
	inOrder   recordKind = iota // the order published, which a replica applies in
	snapshot                    // a rewritten log, with one collection's records joined
	keyGroups                   // protocol 1: each key's records together, keys in order
)

// records writes body's records into the transcript, normalized as kind says.
func (g *replicationGolden) records(body []byte, kind recordKind) {
	g.t.Helper()
	normalized := normalizeGoldenLog(g.t, body, g.window())
	if kind == snapshot {
		normalized = joinGoldenChunks(g.t, normalized)
	}
	records := goldenRecords(g.t, normalized)
	if kind == keyGroups {
		records = sortKeyGroups(records)
	}
	g.printf("  %d records", len(records))
	for _, parts := range records {
		var line bytes.Buffer
		for _, p := range parts {
			goldenValue(&line, p)
		}
		g.printf("  %s", line.String()[1:])
	}
}

// sortKeyGroups puts the records of a protocol 1 frame in key order: a
// FLUSHDB first stays first, the records on one key stay together and in
// their order, and the groups are sorted by key.
func sortKeyGroups(records [][]string) [][]string {
	var head [][]string
	if len(records) > 0 && strings.EqualFold(records[0][0], "FLUSHDB") {
		head, records = records[:1], records[1:]
	}
	var groups [][][]string
	for _, parts := range records {
		key := ""
		if len(parts) > 1 {
			key = parts[1]
		}
		if n := len(groups); n > 0 && len(groups[n-1][0]) > 1 && groups[n-1][0][1] == key &&
			!strings.EqualFold(parts[0], "DEL") {
			groups[n-1] = append(groups[n-1], parts)
			continue
		}
		groups = append(groups, [][]string{parts})
	}
	slices.SortStableFunc(groups, func(a, b [][]string) int {
		ka, kb := "", ""
		if len(a[0]) > 1 {
			ka = a[0][1]
		}
		if len(b[0]) > 1 {
			kb = b[0][1]
		}
		return strings.Compare(ka, kb)
	})
	out := head
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// frame writes a frame as it was sent, with its identities named and its
// checksum checked, then its body.
func (g *replicationGolden) frame(what string, f ReplicationFrame, kind recordKind) {
	g.t.Helper()
	require.Equal(g.t, frameChecksum(f), f.Checksum, "%s: checksum", what)
	body := f.Body
	f.Epoch, f.SnapshotID, f.Checksum, f.Body = g.id("epoch", f.Epoch), g.id("snapshot", f.SnapshotID), "ok", nil
	encoded, err := json.Marshal(f)
	require.NoError(g.t, err)
	g.printf("frame %s %s", what, encoded)
	if kind != snapshot || len(body) > 0 {
		g.printf("  %d bytes", len(body))
	}
	if kind != snapshot {
		g.records(body, kind)
	}
}

func decodeGoldenFrame(t *testing.T, reply []byte) ReplicationFrame {
	t.Helper()
	v, err := Decode(reply)
	require.NoError(t, err)
	s, ok := v.(string)
	require.True(t, ok, "not a frame: %q", reply)
	var f ReplicationFrame
	require.NoError(t, json.Unmarshal([]byte(s), &f))
	return f
}

func utoa(n uint64) string { return strconv.FormatUint(n, 10) }

// pull2 is a protocol 2 pull, with the caller's term, as a replica sends it.
func (g *replicationGolden) pull2(epoch string, offset uint64, snapshotID string, part uint64) ReplicationFrame {
	g.t.Helper()
	return decodeGoldenFrame(g.t, g.call("KEEL.REPL.PULL2", epoch, utoa(offset), snapshotID, utoa(part), utoa(CurrentTerm())))
}

// drain2 pulls protocol 2 deltas from offset until one says the replica has
// caught up, writing each, and returns them.
func (g *replicationGolden) drain2(what, epoch string, offset uint64) []ReplicationFrame {
	return g.drain2As(what, epoch, offset, inOrder)
}

// drain2As is drain2, with the frames' records normalized as kind says.
func (g *replicationGolden) drain2As(what, epoch string, offset uint64, kind recordKind) []ReplicationFrame {
	g.t.Helper()
	var frames []ReplicationFrame
	for i := 0; ; i++ {
		require.Less(g.t, i, 1000, "the stream did not catch up")
		f := g.pull2(epoch, offset, "", 0)
		require.False(g.t, f.Full, "%s: a delta was asked for", what)
		g.frame(what, f, kind)
		frames = append(frames, f)
		if f.CaughtUp {
			return frames
		}
		offset = f.To
	}
}

// snapshot2 asks for a protocol 2 snapshot: the first pull is told to wait
// while its rewrite runs, and the rest fetch its chunks.
func (g *replicationGolden) snapshot2() []ReplicationFrame {
	g.t.Helper()
	first := g.pull2("", 0, "", 0)
	require.True(g.t, first.Pending, "the first pull starts the snapshot's rewrite")
	g.frame("snapshot-requested", first, snapshot)
	g.driveRewrite(nil)
	var frames []ReplicationFrame
	var body []byte
	for id, part := "", uint64(0); ; {
		f := g.pull2("", 0, id, part)
		require.True(g.t, f.Full && !f.Pending, "a snapshot chunk")
		g.frame("snapshot-chunk", f, snapshot)
		frames = append(frames, f)
		body = append(body, f.Body...)
		if f.SnapshotDone {
			g.printf("snapshot body")
			g.records(body, snapshot)
			return frames
		}
		id, part = f.SnapshotID, part+uint64(len(f.Body))
	}
}

// pull1 is a protocol 1 pull.
func (g *replicationGolden) pull1(what, epoch string, offset uint64) ReplicationFrame {
	g.t.Helper()
	f := decodeGoldenFrame(g.t, g.call("KEEL.REPL.PULL", epoch, utoa(offset)))
	g.frame(what, f, keyGroups)
	return f
}

// epoch is the primary's epoch now.
func (g *replicationGolden) epoch() string {
	return goldenInfo(g.t, "replication")["primary_epoch"]
}

func goldenInfo(t *testing.T, section string) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, line := range goldenInfoLines(t, section) {
		k, v, _ := strings.Cut(line, ":")
		fields[k] = v
	}
	return fields
}

func goldenInfoLines(t *testing.T, section string) []string {
	t.Helper()
	text, ok := run(t, "INFO", section).(string)
	require.True(t, ok)
	var lines []string
	for _, line := range strings.Split(text, "\r\n") {
		if strings.Contains(line, ":") && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

// info writes INFO replication as it stands, field by field and in order.
func (g *replicationGolden) info(what string) {
	g.t.Helper()
	var line bytes.Buffer
	for _, field := range goldenInfoLines(g.t, "replication") {
		k, v, _ := strings.Cut(field, ":")
		switch k {
		case "replica_last_update_ms", "replication_acked_age_ms":
			v = "-" // an age, not the same twice
		case "primary_epoch", "replica_epoch":
			v = g.id("epoch", v)
		}
		fmt.Fprintf(&line, " %s:%s", k, v)
	}
	g.printf("info %s%s", what, line.String())
}

// state writes the keyspace, and returns it.
func (g *replicationGolden) state(what string) string {
	g.t.Helper()
	state := string(goldenState(g.t, g.window()))
	g.printf("state %s\n%s", what, strings.TrimSuffix(state, "\n"))
	return state
}

// sameState requires the keyspace to be want, and says so in the transcript.
func (g *replicationGolden) sameState(what, want string) {
	g.t.Helper()
	require.Equal(g.t, want, string(goldenState(g.t, g.window())), what)
	g.printf("state %s: the same", what)
}

// termFile writes the term file beside the log at path.
func (g *replicationGolden) termFile(what, path string) {
	g.t.Helper()
	body, err := os.ReadFile(path + termFileName)
	if os.IsNotExist(err) {
		g.printf("term file %s: none", what)
		return
	}
	require.NoError(g.t, err)
	g.printf("term file %s: %q", what, body)
}

// becomeReplica turns the default engine into a replica of goldenPrimary with
// a log of its own, as a server started with -replicaof is.
func (g *replicationGolden) becomeReplica(protocol int) {
	g.t.Helper()
	g.cycle()
	require.NoError(g.t, CloseAOF())
	ResetStores()
	config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol = false, goldenPrimary, protocol
	g.path = filepath.Join(filepath.Dir(g.path), "replica.aof")
	require.NoError(g.t, OpenAOF(g.path))
	require.NoError(g.t, InitReplication())
	g.logRead = 0
	g.printf("replica of %s, protocol %d", goldenPrimary, protocol)
}

// restartReplica stops the replica and starts it again on its own log, as a
// server restarts: replay, open, then replication, which reads the checkpoint.
func (g *replicationGolden) restartReplica() {
	g.t.Helper()
	g.cycle()
	require.NoError(g.t, CloseAOF())
	ResetStores()
	_, err := LoadAOF(g.path)
	require.NoError(g.t, err)
	require.NoError(g.t, OpenAOF(g.path))
	require.NoError(g.t, InitReplication())
	info, err := os.Stat(g.path)
	require.NoError(g.t, err)
	g.logRead = int(info.Size())
	epoch, offset := ReplicaResumeCursor()
	g.printf("replica restarted: resumes at %q %d", g.id("epoch", epoch), offset)
}

// apply applies a frame on the replica, then writes what it did: its error if
// it refused it, and otherwise the records it added to the replica's log.
func (g *replicationGolden) apply(what string, f ReplicationFrame, kind recordKind) error {
	g.t.Helper()
	g.cycle() // protocol 2 waits for a pending append
	err := ApplyReplication(f)
	g.cycle()
	if err != nil {
		g.printf("apply %s: error %q", what, err.Error())
		return err
	}
	log, readErr := os.ReadFile(g.path)
	require.NoError(g.t, readErr)
	added := log[g.logRead:]
	g.logRead = len(log)
	g.printf("apply %s: the replica's log grew %d bytes", what, len(added))
	if kind == snapshot {
		kind = inOrder // a snapshot is applied in the order the rewrite wrote it
	}
	g.records(added, kind)
	return nil
}

// checkpoint writes the replica's restart checkpoint, whose digest must name
// the replica's log as it is.
func (g *replicationGolden) checkpoint(what string) {
	g.t.Helper()
	body, err := os.ReadFile(g.path + ".replica-checkpoint")
	if os.IsNotExist(err) {
		g.printf("checkpoint %s: none", what)
		return
	}
	require.NoError(g.t, err)
	log, err := os.ReadFile(g.path)
	require.NoError(g.t, err)
	var cp map[string]any
	require.NoError(g.t, json.Unmarshal(body, &cp))
	sum := sha256.Sum256(log)
	require.Equal(g.t, hex.EncodeToString(sum[:]), cp["aof_sha256"], "the checkpoint names the log's digest")
	require.EqualValues(g.t, len(log), cp["aof_bytes"], "the checkpoint names the log's length")
	epoch, _ := cp["epoch"].(string)
	text := strings.Replace(string(body), epoch, g.id("epoch", epoch), 1)
	text = strings.Replace(text, cp["aof_sha256"].(string), "log", 1)
	g.printf("checkpoint %s: %s", what, text)
}

// goldenReplicationPopulate writes small keys of every type, with expiries:
// what a snapshot holds. No collection is large enough for a rewrite to cut.
func goldenReplicationPopulate(g *replicationGolden) {
	for i := range 20 {
		g.ok("SET", "s:"+strconv.Itoa(i), "value:"+strconv.Itoa(i))
	}
	g.ok("SET", "s:abs", "v", "PXAT", goldenAbsolute+"123")
	g.ok("SET", "s:rel", "v", "EX", hours(2, time.Second))
	g.ok("MSET", "m:1", "a", "m:2", "b")
	g.ok("INCR", "c")
	g.ok("INCRBY", "c", "41")
	g.ok("HSET", "h", "f1", "v1", "f2", "v2", "f3", "v3")
	g.ok("HSET", "h:ttl", "f", "v")
	g.ok("PEXPIREAT", "h:ttl", goldenAbsolute+"321")
	g.ok("RPUSH", "l", "a", "b", "c", "d")
	g.ok("SADD", "set", "a", "b", "c")
	g.ok("ZADD", "z", "1", "a", "2.5", "b", "3", "c")
	g.ok("GEOADD", "geo", "13.361389", "38.115556", "palermo", "15.087269", "37.502669", "catania")
	g.ok("BF.RESERVE", "bf", "0.01", "1000")
	g.ok("BF.MADD", "bf", "a", "b")
	g.ok("CF.RESERVE", "cf", "1000")
	g.ok("CF.ADD", "cf", "a")
	g.ok("CMS.INITBYDIM", "cms", "100", "5")
	g.ok("CMS.INCRBY", "cms", "a", "3")
	g.ok("MORRIS.INITBYDIM", "mor", "200", "5")
	g.ok("MORRIS.INCRBY", "mor", "hits", "500")
	g.ok("PFADD", "hll", "a", "b", "c")
	g.ok("PEXPIRE", "hll", hours(3, time.Millisecond))
}

// goldenReplicationDeltas changes every type with each operation a stream
// carries, the records written in a command's place, the opaque images of
// filters and sketches, a key reaped lazily, and transactions: a block of
// writes with an opaque image inside it, a block of reads, a block with a
// command that fails, a block of expiries, and a discarded one.
func goldenReplicationDeltas(g *replicationGolden) {
	sec, ms := time.Second, time.Millisecond
	g.ok("SET", "s:1", "changed")
	g.ok("SET", "s:new", "v", "EX", hours(3, sec))
	g.ok("SETNX", "s:nx", "a")
	g.ok("SET", "s:2", "x", "XX", "GET")
	g.ok("MSET", "m:1", "c", "m:3", "d")
	g.ok("INCR", "c")
	g.ok("INCRBY", "c", "5")
	g.ok("DECR", "c")
	g.ok("DECRBY", "c", "2")
	g.ok("INCR", "c:new")
	g.ok("EXPIRE", "s:3", hours(4, sec))
	g.ok("PEXPIRE", "s:4", hours(5, ms))
	g.ok("EXPIREAT", "s:5", goldenAbsolute)
	g.ok("PERSIST", "s:rel")
	g.ok("DEL", "s:6", "missing")
	g.ok("UNLINK", "s:7")
	g.ok("PEXPIREAT", "s:8", "1000") // in the past: a DEL
	g.ok("HSET", "h", "f4", "v4")
	g.ok("HSETNX", "h", "f5", "v5")
	g.ok("HDEL", "h", "f1")
	g.ok("HINCRBY", "h", "n", "7")
	g.ok("LPUSH", "l", "z")
	g.ok("RPUSH", "l", "e")
	g.ok("LPOP", "l")
	g.ok("RPOP", "l")
	g.ok("LPOP", "l", "1")
	g.ok("LSET", "l", "0", "q")
	g.ok("LTRIM", "l", "0", "1")
	g.ok("SADD", "set", "d", "e")
	g.ok("SREM", "set", "a")
	g.ok("SADD", "set:one", "only")
	g.ok("SPOP", "set:one") // published as the SREM it was
	g.ok("ZADD", "z", "4", "d")
	g.ok("ZADD", "z", "XX", "CH", "10", "b")
	g.ok("ZINCRBY", "z", "2", "a")
	g.ok("ZREM", "z", "c")
	g.ok("ZPOPMIN", "z")
	g.ok("GEOADD", "geo", "2.3522", "48.8566", "paris")
	g.ok("BF.ADD", "bf", "c")
	g.ok("BF.MADD", "bf", "d", "e")
	g.ok("BF.ADD", "bf:auto", "x")
	g.ok("CF.ADD", "cf", "b")
	g.ok("CF.ADDNX", "cf", "c")
	g.ok("CF.DEL", "cf", "a")
	g.ok("CMS.INCRBY", "cms", "a", "1", "b", "2")
	g.ok("MORRIS.INCRBY", "mor", "hits", "10")
	g.ok("PFADD", "hll", "d")
	g.ok("PFADD", "hll2", "x")
	g.ok("PFMERGE", "hll3") // one key: PFMERGE names every key it reads as written too
	for _, key := range []string{"s:1", "h:one", "l", "set", "z", "bf", "cf", "cms", "mor", "hll"} {
		if key == "h:one" {
			g.ok("HSET", "h:one", "f", "v")
		}
		g.reply("KEEL.DUMP", key)
	}
	image, _ := Decode(g.call("KEEL.DUMP", "l"))
	g.ok("KEEL.RESTORE", "l:copy", image.(string))
	g.ok("HSET", "x:h", "f", "v")
	g.ok("SET", "x:h", "string now") // DEL, then SET
	g.ok("SET", "s:brief", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	g.ok("GET", "s:brief") // reaped: a DEL
	g.ok("MULTI")
	g.ok("SET", "t:1", "a")
	g.ok("INCR", "t:2")
	g.ok("HSET", "t:3", "f", "v")
	g.ok("BF.ADD", "bf", "t")
	g.ok("EXEC")
	g.ok("MULTI")
	g.ok("GET", "t:1")
	g.ok("EXEC")
	g.ok("MULTI")
	g.ok("SET", "t:4", "a")
	g.ok("LPUSH", "t:4", "x")
	g.ok("INCR", "t:5")
	g.ok("EXEC")
	g.ok("MULTI")
	g.ok("EXPIRE", "t:1", hours(8, sec))
	g.ok("SET", "t:6", "v", "EX", hours(9, sec))
	g.ok("EXEC")
	g.ok("MULTI")
	g.ok("SET", "t:7", "v")
	g.ok("DISCARD")
}

// goldenReplicationLater is written after a replica's restart: a flush
// inside a transaction, and keys on either side of it.
func goldenReplicationLater(g *replicationGolden) {
	g.ok("SET", "w:1", "v")
	g.ok("INCR", "c")
	g.ok("MULTI")
	g.ok("SET", "w:2", "v")
	g.ok("FLUSHDB")
	g.ok("SET", "w:3", "v")
	g.ok("HSET", "w:h", "f", "v")
	g.ok("EXEC")
	g.ok("SET", "w:4", "v", "PX", hours(1, time.Millisecond))
	g.ok("CMS.INITBYDIM", "w:cms", "10", "2")
}

// goldenProtocol2: a protocol 2 primary's stream from its start, a snapshot
// and the deltas after it, and the refusals of its pulls; then a replica
// applying the snapshot and the deltas, writing its checkpoint, restarting
// from it and catching up again.
func goldenProtocol2(g *replicationGolden) {
	goldenReplicationPopulate(g)
	epoch := g.epoch()
	g.info("primary-populated")
	g.drain2("from-start", epoch, 0)
	chunks := g.snapshot2()
	g.info("primary-snapshot")
	goldenReplicationDeltas(g)
	deltas := g.drain2("deltas", epoch, chunks[0].To)
	// PFMERGE publishes the image of every key it names, from a map, so it
	// is pulled alone and its keys put in order.
	g.ok("PFMERGE", "hll4", "hll", "hll2")
	merge := g.drain2As("merge", epoch, deltas[len(deltas)-1].To, keyGroups)
	g.info("primary-deltas")
	afterDeltas := g.state("primary-deltas")
	goldenReplicationLater(g)
	later := g.drain2("later", epoch, merge[len(merge)-1].To)
	g.info("primary-later")
	afterLater := g.state("primary-later")

	// A stale snapshot is told to start again; a delta cannot name a chunk;
	// protocol 1 is off; and the counts are checked.
	g.frame("stale-snapshot", g.pull2(epoch, 0, "00000000000000000000000000000000", 0), inOrder)
	g.reply("KEEL.REPL.PULL2", epoch, "0", "", "1", "0")
	g.reply("KEEL.REPL.PULL2", epoch, "x", "", "0", "0")
	g.reply("KEEL.REPL.PULL2", epoch)
	g.reply("KEEL.REPL.PULL", epoch, "0")

	g.becomeReplica(2)
	g.info("replica-new")
	g.reply("PING")
	g.reply("GET", "s:1")
	g.reply("SET", "s:1", "v")
	g.reply("KEEL.REPL.PULL2", epoch, "0", "", "0")
	for _, f := range chunks {
		require.NoError(g.t, g.apply("snapshot-chunk", f, snapshot))
		g.info("replica-snapshot-chunk")
	}
	g.checkpoint("after-snapshot")
	for _, f := range deltas {
		require.NoError(g.t, g.apply("deltas", f, inOrder))
		g.info("replica-deltas")
	}
	for _, f := range merge {
		require.NoError(g.t, g.apply("merge", f, keyGroups))
	}
	g.checkpoint("after-deltas")
	g.sameState("replica-deltas", afterDeltas)
	g.reply("GET", "s:1")
	g.reply("SET", "s:1", "v")

	g.restartReplica()
	g.info("replica-restarted")
	for _, f := range later {
		require.NoError(g.t, g.apply("later", f, inOrder))
		g.info("replica-later")
	}
	g.checkpoint("after-later")
	g.sameState("replica-later", afterLater)

	// Frames a replica refuses: a gap, a bad checksum, a protocol 1 frame.
	last := later[len(later)-1]
	gap := ReplicationFrame{Version: 2, Epoch: last.Epoch, From: last.To + 1, To: last.To + 1, CaughtUp: true}
	gap.Checksum = frameChecksum(gap)
	require.Error(g.t, g.apply("gap", gap, inOrder))
	g.info("replica-refused")
	corrupt := last
	corrupt.From++
	require.Error(g.t, g.apply("corrupt", corrupt, inOrder))
	old := ReplicationFrame{Version: 1, Epoch: last.Epoch, From: last.To, To: last.To}
	old.Checksum = frameChecksum(old)
	require.Error(g.t, g.apply("protocol-1", old, inOrder))
}

// goldenProtocol1: a protocol 1 primary's full frame and its deltas, then a
// replica applying them.
func goldenProtocol1(g *replicationGolden) {
	goldenReplicationPopulate(g)
	full := g.pull1("full", "", 0)
	epoch := full.Epoch
	afterPopulate := g.state("primary-populated")
	g.info("primary-populated")
	goldenReplicationDeltas(g)
	d1 := g.pull1("deltas", epoch, full.To)
	afterDeltas := g.state("primary-deltas")
	goldenReplicationLater(g)
	d2 := g.pull1("later", epoch, d1.To)
	afterLater := g.state("primary-later")
	empty := g.pull1("caught-up", epoch, d2.To)
	g.pull1("behind", epoch, d1.To)
	g.pull1("unknown-epoch", "00000000000000000000000000000000", 0)
	g.info("primary-later")
	g.reply("KEEL.REPL.PULL", epoch)
	g.reply("KEEL.REPL.PULL", epoch, "x")
	g.reply("KEEL.REPL.PULL2", epoch, "0", "", "0")

	g.becomeReplica(1)
	g.info("replica-new")
	g.reply("GET", "s:1")
	g.reply("SET", "s:1", "v")
	require.NoError(g.t, g.apply("full", full, keyGroups))
	g.info("replica-full")
	g.sameState("replica-full", afterPopulate)
	require.NoError(g.t, g.apply("deltas", d1, keyGroups))
	g.sameState("replica-deltas", afterDeltas)
	require.NoError(g.t, g.apply("later", d2, keyGroups))
	g.sameState("replica-later", afterLater)
	require.NoError(g.t, g.apply("caught-up", empty, keyGroups))
	g.info("replica-caught-up")
	g.reply("GET", "s:1")
	g.reply("SET", "s:1", "v")
	g.checkpoint("protocol-1")

	gap := d2
	gap.From, gap.To = d2.To+1, d2.To+2
	gap.Checksum = frameChecksum(gap)
	require.Error(g.t, g.apply("gap", gap, keyGroups))
	termed := empty
	termed.Term = 1
	termed.Checksum = frameChecksum(termed)
	require.Error(g.t, g.apply("termed", termed, keyGroups))
	g.info("replica-refused")
}

// goldenTerms: promotion, fencing and observed terms on a protocol 2 primary,
// the term file each writes, a restart onto it, and the terms frames carry;
// then a replica observing a term from a frame and refusing an older one.
func goldenTerms(g *replicationGolden) {
	primary := g.path
	require.NoError(g.t, LoadTerm(primary))
	g.info("fresh")
	g.termFile("fresh", primary)
	g.reply("KEEL.PROMOTE", "0")
	g.reply("KEEL.PROMOTE", "3")
	g.termFile("promoted", primary)
	g.info("promoted")
	g.ok("SET", "a", "1")
	epoch := g.epoch()
	g.reply("KEEL.REPL.PULL2", epoch, "0", "", "0")
	g.drain2("at-term-3", epoch, 0)
	g.reply("KEEL.FENCE", "2")
	g.reply("KEEL.PROMOTE", "3")
	g.info("lower-terms")
	g.reply("KEEL.REPL.PULL2", epoch, "0", "", "0", "5") // a replica that has moved on
	g.termFile("observed", primary)
	g.info("observed")
	g.reply("SET", "b", "2")
	g.reply("KEEL.REPL.PULL2", epoch, "0", "", "0", "5")

	// A restart reads the term back, and starts fenced.
	require.NoError(g.t, LoadTerm(primary))
	g.info("restarted")
	g.reply("SET", "b", "2")
	g.reply("KEEL.PROMOTE", "5")
	g.reply("KEEL.PROMOTE", "6")
	g.termFile("promoted-again", primary)
	g.info("promoted-again")
	g.ok("SET", "b", "2")
	g.drain2("at-term-6", epoch, 0)
	g.reply("KEEL.FENCE", "x")
	g.reply("KEEL.FENCE", "7")
	g.termFile("fenced", primary)
	g.info("fenced")
	g.reply("SET", "c", "3")
	config.ReplicationProtocol = 1
	g.reply("KEEL.REPL.PULL", "", "0")
	config.ReplicationProtocol = 2

	g.becomeReplica(2)
	require.NoError(g.t, LoadTerm(g.path))
	g.info("replica-fresh")
	pending := ReplicationFrame{Version: 2, Epoch: epoch, Full: true, Pending: true, Term: 7}
	pending.Checksum = frameChecksum(pending)
	require.NoError(g.t, g.apply("pending-at-term-7", pending, inOrder))
	g.termFile("replica-observed", g.path)
	g.info("replica-observed")
	older := ReplicationFrame{Version: 2, Epoch: epoch, Full: true, Pending: true, Term: 6}
	older.Checksum = frameChecksum(older)
	require.Error(g.t, g.apply("pending-at-term-6", older, inOrder))
	g.reply("KEEL.PROMOTE", "8")
	g.termFile("replica-promoted", g.path)
	g.info("replica-promoted")
	g.reply("SET", "d", "4")
}

// TestReplicationGoldenCapture writes the fixture when asked to, and is
// skipped otherwise. It was run once, on develop at 9736d8d. Every mode must
// write the same transcript before anything is written.
func TestReplicationGoldenCapture(t *testing.T) {
	dir := os.Getenv("KEEL_CAPTURE_REPLICATION_GOLDEN")
	if dir == "" {
		t.Skip("set KEEL_CAPTURE_REPLICATION_GOLDEN to rewrite the fixture")
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	manifest := map[string]any{
		"source_revision": "9736d8d",
		"note":            "Captured before step 2.4 of the embedding plan moved replication and failover into core.Engine; see replication_golden_test.go.",
	}
	sums := map[string]string{}
	for _, scenario := range replicationGoldenScenarios {
		var first []byte
		for i, mode := range goldenModes {
			var got []byte
			// A subtest each, so that each run's cleanup comes before the next.
			t.Run(scenario.name+"/"+mode.name, func(t *testing.T) { got = runReplicationGolden(t, scenario, mode) })
			require.NotEmpty(t, got)
			if debug := os.Getenv("KEEL_REPLICATION_GOLDEN_DEBUG"); debug != "" {
				require.NoError(t, os.WriteFile(filepath.Join(debug, scenario.name+"."+mode.name+".txt"), got, 0o644))
			}
			if i == 0 {
				first = got
				continue
			}
			requireSameTranscript(t, first, got, scenario.name+" under "+mode.name)
		}
		var z bytes.Buffer
		w, err := gzip.NewWriterLevel(&z, gzip.BestCompression)
		require.NoError(t, err)
		_, err = w.Write(first)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, os.WriteFile(filepath.Join(dir, scenario.name+".txt.gz"), z.Bytes(), 0o644))
		sums[scenario.name] = goldenSHA(first)
	}
	manifest["transcript_sha256"] = sums
	body, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), append(body, '\n'), 0o644))
}

// requireSameTranscript compares two transcripts and, when they differ, names
// the first line that does.
func requireSameTranscript(t *testing.T, want, got []byte, what string) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	a, b := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			t.Fatalf("%s: line %d differs:\nwant %.300s\ngot  %.300s", what, i+1, a[i], b[i])
		}
	}
	t.Fatalf("%s: %d lines, want %d; the first %d are the same", what, len(b), len(a), min(len(a), len(b)))
}

// TestReplicationGoldenTranscriptsAreUnchanged: every scenario, under every
// mode, writes develop's transcript.
func TestReplicationGoldenTranscriptsAreUnchanged(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_REPLICATION_GOLDEN") != "" {
		t.Skip("capturing")
	}
	var manifest struct {
		Sums map[string]string `json:"transcript_sha256"`
	}
	body, err := os.ReadFile(filepath.Join(replicationGoldenDir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &manifest))
	require.Len(t, manifest.Sums, len(replicationGoldenScenarios))
	for _, scenario := range replicationGoldenScenarios {
		f, err := os.Open(filepath.Join(replicationGoldenDir, scenario.name+".txt.gz"))
		require.NoError(t, err)
		z, err := gzip.NewReader(f)
		require.NoError(t, err)
		var want bytes.Buffer
		_, err = want.ReadFrom(z)
		require.NoError(t, err)
		f.Close()
		require.Equal(t, manifest.Sums[scenario.name], goldenSHA(want.Bytes()), "the fixture's transcript")
		for _, mode := range goldenModes {
			t.Run(scenario.name+"/"+mode.name, func(t *testing.T) {
				requireSameTranscript(t, want.Bytes(), runReplicationGolden(t, scenario, mode), "the transcript")
			})
		}
	}
}
