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
	setupReplicationV2(t)
	cmsStore.Put("large", data_structure.CreateCMS((replicationCommandBytes/4)+1, 1))
	epoch := replication.epoch
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run(t, "CMS.INCRBY", "large", "item", "1")
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("oversized opaque update allocated %d bytes", allocated)
	require.Less(t, allocated, uint64(256<<10), "a refused delta must not construct an oversized snapshot first")
	require.NotEqual(t, epoch, replication.epoch, "the replica must fall back to a fresh snapshot")
	require.Equal(t, []interface{}{int64(1)}, run(t, "CMS.QUERY", "large", "item"))
}

func TestOpaqueReplicationBodyAllocatesOneAcceptedImage(t *testing.T) {
	setupReplicationV2(t)
	cms := data_structure.CreateCMS(1<<20, 1)
	cms.IncrBy("item", 7)
	cmsStore.Put("large", cms)
	replication.dirty["large"] = struct{}{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, fits := opaqueReplicationBody()
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
	require.NoError(t, restoreKey("restored", []byte(second.Args[1])))
	require.Equal(t, []interface{}{int64(7)}, run(t, "CMS.QUERY", "restored", "item"))
}

func TestOpaqueReplicationReplacementReplaysEveryTypeAndExpiry(t *testing.T) {
	setupReplicationV2(t)
	fillOneOfEverything(t)
	run(t, "HSET", "hash", "field", "value")
	run(t, "RPUSH", "list", "first", "second")
	want := snapshotEverything(t)
	expiry := replicationKeyExpiry("living", dumpTagString)
	keys := []string{"str", "num", "living", "set", "z", "geo", "hll", "bf", "cf", "cms", "mor", "hash", "list", "absent"}
	for _, key := range keys {
		replication.dirty[key] = struct{}{}
	}
	body, fits := opaqueReplicationBody()
	require.True(t, fits)
	require.NoError(t, CloseAOF())
	path := filepath.Join(t.TempDir(), "replacement.aof")
	require.NoError(t, os.WriteFile(path, body, 0600))
	for restart := 0; restart < 2; restart++ {
		ResetStores()
		run(t, "SET", "absent", "must be removed")
		_, err := LoadAOF(path)
		require.NoError(t, err)
		require.Equal(t, want, snapshotEverything(t))
		require.Equal(t, expiry, replicationKeyExpiry("living", dumpTagString))
		require.Equal(t, "value", run(t, "HGET", "hash", "field"))
		require.Equal(t, []interface{}{"first", "second"}, run(t, "LRANGE", "list", "0", "-1"))
		require.Equal(t, int64(0), run(t, "EXISTS", "absent"))
	}
}

func TestOpaqueReplicationExpiryBelongsToSelectedValue(t *testing.T) {
	setupReplicationV2(t)
	hash := data_structure.NewHash()
	hash.Set("field", "value")
	hashStore.Put("overlap", hash)
	cmsStore.Put("overlap", data_structure.CreateCMS(16, 1))
	wantExpiry := uint64(time.Now().UnixMilli() + 600000)
	hashStore.SetExpiryAt("overlap", wantExpiry)
	cmsStore.SetExpiryAt("overlap", 1)
	replication.dirty["overlap"] = struct{}{}
	body, fits := opaqueReplicationBody()
	require.True(t, fits)
	require.NoError(t, CloseAOF())
	ResetStores()
	path := filepath.Join(t.TempDir(), "replacement.aof")
	require.NoError(t, os.WriteFile(path, body, 0600))
	_, err := LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", run(t, "HGET", "overlap", "field"))
	expiry, exists := hashStore.GetExpiry("overlap")
	require.True(t, exists)
	require.Equal(t, wantExpiry, expiry)
}

func TestOpaqueReplicationSizesAggregateBeforeAllocating(t *testing.T) {
	setupReplicationV2(t)
	// Each image fits alone; their combined delta does not.
	for _, key := range []string{"first", "second"} {
		cmsStore.Put(key, data_structure.CreateCMS(9<<20, 1))
		replication.dirty[key] = struct{}{}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, fits := opaqueReplicationBody()
	runtime.ReadMemStats(&after)
	require.False(t, fits)
	require.Nil(t, body)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
}
