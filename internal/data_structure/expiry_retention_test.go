package data_structure

import (
	"runtime"
	"strconv"
	"testing"

	"github.com/brandopakel/keel/internal/config"
	"github.com/stretchr/testify/require"
)

type retentionValue uint64

func (v retentionValue) MemUsage() uint64 { return 8 }

func TestEmptyExpiryTableReleasesRetainedHeap(t *testing.T) {
	for _, kind := range []string{"string", "collection"} {
		for _, action := range []string{"persist", "overwrite", "delete"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				withEviction(t, config.EvictFirst, 5, 1000000)
				ResetKeyspaces()
				var ks Keyspace
				var put func(string)
				if kind == "string" {
					d := CreateDict()
					ks, put = d, func(key string) { d.Put(key, d.NewObj("value")) }
				} else {
					k := NewKeyed[retentionValue]("retention")
					ks, put = k, func(key string) { k.Put(key, 1) }
				}
				RegisterKeyspace(ks)
				keys := make([]string, 100000)
				for i := range keys {
					keys[i] = "ttl:" + strconv.Itoa(i)
				}
				empty := heapBytes()
				for _, key := range keys {
					put(key)
				}
				withoutTTL := heapBytes()
				for _, key := range keys {
					ks.SetExpiryAt(key, 1<<60)
				}
				withTTL := heapBytes()
				for _, key := range keys {
					switch action {
					case "persist":
						require.True(t, ks.ClearExpiry(key))
					case "overwrite":
						put(key)
					case "delete":
						require.True(t, ks.Delete(key))
					}
				}
				after := heapBytes()
				runtime.KeepAlive(keys)
				require.Zero(t, ks.KeysWithExpiry())
				base := withoutTTL
				if action == "delete" {
					base = empty
				}
				t.Logf("loaded=%d withTTL=%d after=%d retained_above_base=%d", withoutTTL, withTTL, after, int64(after)-int64(base))
				require.Less(t, after, base+512<<10, "removing the last TTL must release its old map capacity")
				// Reusing the store must still attach, find and remove expiry.
				put("again")
				ks.SetExpiryAt("again", 1<<60)
				at, ok := ks.GetExpiry("again")
				require.True(t, ok)
				require.EqualValues(t, 1<<60, at)
				require.True(t, ks.ClearExpiry("again"))
			})
		}
	}
}

func TestSingleKeyTTLChurnAvoidsMapReallocation(t *testing.T) {
	for _, kind := range []string{"string", "collection"} {
		t.Run(kind, func(t *testing.T) {
			withEviction(t, config.EvictFirst, 5, 1000000)
			ResetKeyspaces()
			var ks Keyspace
			if kind == "string" {
				d := CreateDict()
				d.Put("key", d.NewObj("value"))
				ks = d
			} else {
				k := NewKeyed[retentionValue]("retention")
				k.Put("key", 1)
				ks = k
			}
			RegisterKeyspace(ks)
			ks.SetExpiryAt("key", 1<<60)
			allocs := testing.AllocsPerRun(1000, func() { ks.ClearExpiry("key"); ks.SetExpiryAt("key", 1<<60) })
			require.Zero(t, allocs, "hot single-key TTL churn must reuse a small table")
		})
	}
}
