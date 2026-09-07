package core

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestDumpPreservesLegacyPayloadsAcrossEveryType(t *testing.T) {
	raw, err := os.ReadFile("testdata/dump-legacy-7fc360d.json")
	require.NoError(t, err)
	var fixture struct{ Payloads map[string]string }
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Len(t, fixture.Payloads, 10)
	for kind, encoded := range fixture.Payloads {
		t.Run(kind, func(t *testing.T) {
			ResetStores()
			t.Cleanup(ResetStores)
			payload, err := hex.DecodeString(encoded)
			require.NoError(t, err)
			require.NoError(t, restoreKey("legacy", payload))
			actual, ok := dumpKey("legacy")
			require.True(t, ok)
			require.Equal(t, payload, actual, "the pre-change binary produced this payload")
			reply := cmdDUMP([]string{"legacy"})
			require.Equal(t, Encode(string(payload), false), reply, "complete RESP framing")
		})
	}
}

func TestDumpRejectsAmplifiedListBeforeAllocation(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	list := data_structure.NewList()
	value := strings.Repeat("v", 1<<20)
	for i := 0; i < 65; i++ {
		list.PushBack(value)
	}
	listStore.Put("large", list)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	reply := cmdDUMP([]string{"large"})
	runtime.ReadMemStats(&after)
	t.Logf("reply bytes=%d allocated=%d", len(reply), after.TotalAlloc-before.TotalAlloc)
	if len(reply) > 1024 {
		t.Fatalf("oversized dump was allocated: %d bytes", len(reply))
	}
	require.Equal(t, replyTooLarge, reply)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
	require.Equal(t, 65, list.Len())
}

func TestDumpAcceptedPayloadUsesOneSizedBuffer(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	list := data_structure.NewList()
	value := strings.Repeat("v", 1<<20)
	for i := 0; i < 4; i++ {
		list.PushBack(value)
	}
	listStore.Put("large", list)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	reply := cmdDUMP([]string{"large"})
	runtime.ReadMemStats(&after)
	t.Logf("reply bytes=%d allocated=%d", len(reply), after.TotalAlloc-before.TotalAlloc)
	require.Greater(t, len(reply), 4<<20)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(len(reply)+(256<<10)))
}

func TestDumpLargeSketchRejectsBeforeMarshalling(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	// A small INITBYDIM request can create a table larger than the reply limit.
	cms := data_structure.CreateCMS(17<<20, 1)
	cmsStore.Put("large", cms)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	reply := cmdDUMP([]string{"large"})
	runtime.ReadMemStats(&after)
	require.Equal(t, replyTooLarge, reply)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
	require.Equal(t, uint64(0), cms.TotalCount())
}
