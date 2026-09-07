package core

import (
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestGeoSearchRefusesOversizedReplyBeforeEncoding(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	z := data_structure.CreateZSet()
	score, ok := data_structure.GeoScore(0, 0)
	require.True(t, ok)
	for i := 0; i < 6; i++ {
		z.Add(float64(score), fmt.Sprintf("%d", i)+strings.Repeat("x", 11<<20), 0)
	}
	zsetStore.Put("geo", z)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	reply := cmdGEOSEARCH([]string{"geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1000", "km"})
	runtime.ReadMemStats(&after)
	t.Logf("oversized GEOSEARCH allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
	require.Equal(t, replyTooLarge, reply)
	require.Equal(t, 6, z.Len())
}

func TestGeoSearchSelectionAndEncodingAgainstFullSort(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	z := data_structure.CreateZSet()
	rng := rand.New(rand.NewSource(741))
	points := make([]data_structure.GeoPoint, 0, 1000)
	for i := 0; i < 1000; i++ {
		score, ok := data_structure.GeoScore(-5+rng.Float64()*10, -5+rng.Float64()*10)
		require.True(t, ok)
		lon, lat, ok := data_structure.GeoDecodeScore(float64(score))
		require.True(t, ok)
		member := fmt.Sprintf("member:%d", i)
		z.Add(float64(score), member, 0)
		points = append(points, data_structure.GeoPoint{Longitude: lon, Latitude: lat,
			Dist: data_structure.GeohashGetDistance(0, 0, lon, lat), Score: float64(score), Member: member})
	}
	zsetStore.Put("geo", z)
	for _, order := range []string{"ASC", "DESC"} {
		sort.Slice(points, func(i, j int) bool {
			if order == "ASC" {
				return points[i].Dist < points[j].Dist
			}
			return points[i].Dist > points[j].Dist
		})
		for _, count := range []int{1, 7, 100, 999, 1000, 10000000} {
			for flags := 0; flags < 8; flags++ {
				args := []string{"geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1000", "km", order, "COUNT", strconv.Itoa(count)}
				for bit, option := range []string{"WITHDIST", "WITHHASH", "WITHCOORD"} {
					if flags&(1<<bit) != 0 {
						args = append(args, option)
					}
				}
				want := make([]interface{}, 0, min(count, len(points)))
				for _, p := range points[:min(count, len(points))] {
					if flags == 0 {
						want = append(want, p.Member)
						continue
					}
					entry := []interface{}{p.Member}
					if flags&1 != 0 {
						entry = append(entry, formatDistance(p.Dist/1000))
					}
					if flags&2 != 0 {
						entry = append(entry, int64(p.Score))
					}
					if flags&4 != 0 {
						entry = append(entry, []string{formatCoordinate(p.Longitude), formatCoordinate(p.Latitude)})
					}
					want = append(want, entry)
				}
				require.Equal(t, Encode(want, false), cmdGEOSEARCH(args), "order=%s count=%d flags=%d", order, count, flags)
			}
		}
	}
	// ANY chooses the first matching points before applying an explicit order.
	base := []string{"geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1000", "km", "COUNT", "7", "ANY"}
	first := run(t, "GEOSEARCH", base...).([]interface{})
	selected := map[string]bool{}
	for _, name := range first {
		selected[name.(string)] = true
	}
	for _, order := range []string{"ASC", "DESC"} {
		args := append(append([]string{}, base...), order)
		got := run(t, "GEOSEARCH", args...).([]interface{})
		require.ElementsMatch(t, first, got)
		var last float64
		for i, name := range got {
			require.True(t, selected[name.(string)])
			for _, p := range points {
				if p.Member != name {
					continue
				}
				if i > 0 {
					if order == "ASC" {
						require.GreaterOrEqual(t, p.Dist, last)
					} else {
						require.LessOrEqual(t, p.Dist, last)
					}
				}
				last = p.Dist
			}
		}
	}
}

func TestGeoSearchCountDoesNotCollectEveryMatch(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	z := data_structure.CreateZSet()
	for i := 0; i < 50000; i++ {
		score, ok := data_structure.GeoScore(float64(i)/10000, 0)
		require.True(t, ok)
		z.Add(float64(score), fmt.Sprintf("%05d", i), 0)
	}
	zsetStore.Put("geo", z)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	reply := cmdGEOSEARCH([]string{"geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1000", "km", "COUNT", "1"})
	runtime.ReadMemStats(&after)
	t.Logf("COUNT 1 across 50000 matches allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<10))
	require.Equal(t, "*1\r\n$5\r\n00000\r\n", string(reply))
}

func TestGeoSearchLargeCountWithSparseMatches(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	z := data_structure.CreateZSet()
	far, ok := data_structure.GeoScore(10, 10)
	require.True(t, ok)
	for i := 0; i < 50000; i++ {
		z.Add(float64(far), fmt.Sprintf("far:%d", i), 0)
	}
	near, ok := data_structure.GeoScore(0, 0)
	require.True(t, ok)
	z.Add(float64(near), "near", 0)
	zsetStore.Put("geo", z)
	for _, count := range []string{"50000", "1000000", "10000000"} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		reply := cmdGEOSEARCH([]string{"geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1", "m", "COUNT", count})
		runtime.ReadMemStats(&after)
		t.Logf("sparse COUNT %s allocated %d bytes", count, after.TotalAlloc-before.TotalAlloc)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10))
		require.Equal(t, "*1\r\n$4\r\nnear\r\n", string(reply))
	}
}
