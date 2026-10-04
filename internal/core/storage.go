package core

import (
	"github.com/brandopakel/keel/internal/data_structure"
)

// The keyspaces.
//
// Each type has its own store, and every one of them is registered with the
// evictor so that a memory budget spans the lot: before that, only strings were
// accounted, and a keyspace full of 12KB HyperLogLogs could sail past
// -maxmemory without anything noticing.
//
// The stores are moving into Engine a command family at a time (plan step
// 2.1). The ones below have yet to; Engine holds those that have.
var (
	dictStore   *data_structure.Dict
	zsetStore   *data_structure.Keyed[*data_structure.ZSet]
	setStore    *data_structure.Keyed[*data_structure.Set]
	sbStore     *data_structure.Keyed[*data_structure.SBChain]
	cmsStore    *data_structure.Keyed[*data_structure.CMS]
	morrisStore *data_structure.Keyed[*data_structure.Morris]
	hllStore    *data_structure.Keyed[*data_structure.HLL]
	cfStore     *data_structure.Keyed[*data_structure.CuckooFilter]
)

func init() { ResetStores() }

// ResetStores rebuilds every keyspace and re-registers them. Called at startup,
// and by tests that need to begin from empty.
//
// The order they register in is the order OwnerOf asks them, eviction samples
// them and SCAN and the rewrite walk them, so a store that moves into the
// engine keeps its place in it.
func ResetStores() {
	e := defaultEngine
	space := e.space
	space.ResetKeyspaces()
	// The counter describes the keyspace being thrown away, so it goes with it.
	expiredKeys = 0
	aof.recovered = nil

	dictStore = data_structure.CreateDict(space)
	zsetStore = data_structure.NewKeyed[*data_structure.ZSet](space, "zset")
	setStore = data_structure.NewKeyed[*data_structure.Set](space, "set")
	e.hashStore = data_structure.NewKeyed[*data_structure.Hash](space, "hash")
	e.listStore = data_structure.NewKeyed[*data_structure.List](space, "list")
	sbStore = data_structure.NewKeyed[*data_structure.SBChain](space, "bloom")
	cmsStore = data_structure.NewKeyed[*data_structure.CMS](space, "cms")
	morrisStore = data_structure.NewKeyed[*data_structure.Morris](space, "morris")
	hllStore = data_structure.NewKeyed[*data_structure.HLL](space, "hll")
	cfStore = data_structure.NewKeyed[*data_structure.CuckooFilter](space, "cuckoo")

	space.RegisterKeyspace(dictStore)
	space.RegisterKeyspace(zsetStore)
	space.RegisterKeyspace(setStore)
	space.RegisterKeyspace(e.hashStore)
	space.RegisterKeyspace(e.listStore)
	space.RegisterKeyspace(sbStore)
	space.RegisterKeyspace(cmsStore)
	space.RegisterKeyspace(morrisStore)
	space.RegisterKeyspace(hllStore)
	space.RegisterKeyspace(cfStore)
}
