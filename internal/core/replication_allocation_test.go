package core

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestOversizedOpaqueReplicationUpdateIsSizedBeforeConstruction(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	e.cmsStore.Put("large", data_structure.CreateCMS((replicationCommandBytes/4)+1, 1))
	epoch := e.replication.epoch
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	runOn(t, e, "CMS.INCRBY", "large", "item", "1")
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("oversized opaque update allocated %d bytes", allocated)
	require.Less(t, allocated, uint64(256<<10), "a refused delta must not construct an oversized snapshot first")
	require.NotEqual(t, epoch, e.replication.epoch, "the replica must fall back to a fresh snapshot")
	require.Equal(t, []interface{}{int64(1)}, runOn(t, e, "CMS.QUERY", "large", "item"))
}

func TestOpaqueReplicationBodyAllocatesOneAcceptedImage(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	cms := data_structure.CreateCMS(1<<20, 1)
	cms.IncrBy("item", 7)
	e.cmsStore.Put("large", cms)
	e.replication.dirty["large"] = struct{}{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, fits := e.opaqueReplicationBody()
	runtime.ReadMemStats(&after)
	t.Logf("accepted body=%d allocated=%d", len(body), after.TotalAlloc-before.TotalAlloc)
	require.True(t, fits)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(cms.MarshalSize()+(256<<10)))
	first, n, err := ParseCmd(body)
	require.NoError(t, err)
	require.Equal(t, "DEL", first.Cmd)
	second, used, err := ParseCmd(body[n:])
	require.NoError(t, err)
	require.Equal(t, len(body), n+used)
	require.Equal(t, "KEEL.RESTORE", second.Cmd)
	require.NoError(t, e.restoreKey("restored", []byte(second.Args[1])))
	require.Equal(t, []interface{}{int64(7)}, runOn(t, e, "CMS.QUERY", "restored", "item"))
}

func TestOpaqueReplicationReplacementReplaysEveryTypeAndExpiry(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	fillOneOfEverythingOn(t, e)
	runOn(t, e, "HSET", "hash", "field", "value")
	runOn(t, e, "RPUSH", "list", "first", "second")
	want := snapshotEverythingOn(t, e)
	expiry := e.replicationKeyExpiry("living", dumpTagString)
	keys := []string{"str", "num", "living", "set", "z", "geo", "hll", "bf", "cf", "cms", "mor", "hash", "list", "absent"}
	for _, key := range keys {
		e.replication.dirty[key] = struct{}{}
	}
	body, fits := e.opaqueReplicationBody()
	require.True(t, fits)
	require.NoError(t, e.CloseAOF())
	path := filepath.Join(t.TempDir(), "replacement.aof")
	require.NoError(t, os.WriteFile(path, body, 0600))
	for restart := 0; restart < 2; restart++ {
		e.resetStores()
		runOn(t, e, "SET", "absent", "must be removed")
		_, err := e.LoadAOF(path)
		require.NoError(t, err)
		require.Equal(t, want, snapshotEverythingOn(t, e))
		require.Equal(t, expiry, e.replicationKeyExpiry("living", dumpTagString))
		require.Equal(t, "value", runOn(t, e, "HGET", "hash", "field"))
		require.Equal(t, []interface{}{"first", "second"}, runOn(t, e, "LRANGE", "list", "0", "-1"))
		require.Equal(t, int64(0), runOn(t, e, "EXISTS", "absent"))
	}
}

func TestOpaqueReplicationExpiryBelongsToSelectedValue(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	hash := data_structure.NewHash()
	hash.Set("field", "value")
	e.hashStore.Put("overlap", hash)
	e.cmsStore.Put("overlap", data_structure.CreateCMS(16, 1))
	wantExpiry := uint64(time.Now().UnixMilli() + 600000)
	e.hashStore.SetExpiryAt("overlap", wantExpiry)
	e.cmsStore.SetExpiryAt("overlap", 1)
	e.replication.dirty["overlap"] = struct{}{}
	body, fits := e.opaqueReplicationBody()
	require.True(t, fits)
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	path := filepath.Join(t.TempDir(), "replacement.aof")
	require.NoError(t, os.WriteFile(path, body, 0600))
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", runOn(t, e, "HGET", "overlap", "field"))
	expiry, exists := e.hashStore.GetExpiry("overlap")
	require.True(t, exists)
	require.Equal(t, wantExpiry, expiry)
}

func TestOpaqueReplicationSizesAggregateBeforeAllocating(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	// Each image fits alone; their combined delta does not.
	for _, key := range []string{"first", "second"} {
		e.cmsStore.Put(key, data_structure.CreateCMS(9<<20, 1))
		e.replication.dirty[key] = struct{}{}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, fits := e.opaqueReplicationBody()
	runtime.ReadMemStats(&after)
	require.False(t, fits)
	require.Nil(t, body)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
}
