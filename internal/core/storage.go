package core

import (
	"memkv/internal/data_structure"
)

// The keyspaces.
//
// Each type has its own store, and every one of them is registered with the
// evictor so that a memory budget spans the lot: before that, only strings were
// accounted, and a keyspace full of 12KB HyperLogLogs could sail past
// -maxmemory without anything noticing.
var (
	dictStore   *data_structure.Dict
	zsetStore   *data_structure.Keyed[*data_structure.ZSet]
	setStore    *data_structure.Keyed[data_structure.Set]
	sbStore     *data_structure.Keyed[*data_structure.SBChain]
	cmsStore    *data_structure.Keyed[*data_structure.CMS]
	morrisStore *data_structure.Keyed[*data_structure.Morris]
	hllStore    *data_structure.Keyed[*data_structure.HLL]
	cfStore     *data_structure.Keyed[*data_structure.CuckooFilter]
)

func init() {
	ResetStores()
}

// ResetStores rebuilds every keyspace and re-registers them. Called at startup,
// and by tests that need to begin from empty.
func ResetStores() {
	data_structure.ResetKeyspaces()
	// The counter describes the keyspace being thrown away, so it goes with it.
	expiredKeys = 0

	dictStore = data_structure.CreateDict()
	zsetStore = data_structure.NewKeyed[*data_structure.ZSet]("zset")
	setStore = data_structure.NewKeyed[data_structure.Set]("set")
	sbStore = data_structure.NewKeyed[*data_structure.SBChain]("bloom")
	cmsStore = data_structure.NewKeyed[*data_structure.CMS]("cms")
	morrisStore = data_structure.NewKeyed[*data_structure.Morris]("morris")
	hllStore = data_structure.NewKeyed[*data_structure.HLL]("hll")
	cfStore = data_structure.NewKeyed[*data_structure.CuckooFilter]("cuckoo")

	data_structure.RegisterKeyspace(dictStore)
	data_structure.RegisterKeyspace(zsetStore)
	data_structure.RegisterKeyspace(setStore)
	data_structure.RegisterKeyspace(sbStore)
	data_structure.RegisterKeyspace(cmsStore)
	data_structure.RegisterKeyspace(morrisStore)
	data_structure.RegisterKeyspace(hllStore)
	data_structure.RegisterKeyspace(cfStore)
}
